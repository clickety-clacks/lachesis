package core

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/clickety-clacks/lachesis/internal/model"
	"github.com/clickety-clacks/lachesis/internal/provider"
	"github.com/clickety-clacks/lachesis/internal/store"
	"github.com/clickety-clacks/lachesis/internal/teach"
)

type idleChecker struct{}

func (idleChecker) Busy(context.Context, model.Provider) (bool, error) { return false, nil }

type fakeAdapter struct {
	usageDetail *model.ErrorDetail
	loginValue  []byte
	provider    model.Provider
	credential  string
	managed     *model.StoreBinding
	outsideHome bool
	defaultErr  *model.ErrorDetail
}

func (a *fakeAdapter) Name() model.Provider {
	if a.provider.Valid() {
		return a.provider
	}
	return model.ProviderCodex
}
func (*fakeAdapter) CLIAvailable() bool { return true }
func (a *fakeAdapter) DefaultBinding() (model.StoreBinding, *model.ErrorDetail) {
	return model.StoreBinding{}, a.defaultErr
}
func (a *fakeAdapter) ManagedBinding(home string) model.StoreBinding {
	if a.managed != nil {
		return *a.managed
	}
	if a.outsideHome {
		return model.StoreBinding{Kind: "file", Home: home, CredentialPath: filepath.Join(filepath.Dir(home), "auth.json")}
	}
	credential := a.credential
	if credential == "" {
		credential = "auth.json"
	}
	return model.StoreBinding{Kind: "file", Home: home, CredentialPath: filepath.Join(home, credential)}
}
func (*fakeAdapter) ParseCredential(raw []byte) (provider.Credential, *model.ErrorDetail) {
	if len(raw) == 0 {
		return provider.Credential{}, teach.New(teach.CredentialRejected, "The credential is empty.", "usage", nil, nil, nil, "retry the exact call")
	}
	return provider.Credential{Raw: append([]byte(nil), raw...)}, nil
}
func (a *fakeAdapter) Usage(context.Context, provider.Credential) (*model.UsageSample, *model.ErrorDetail) {
	if a.usageDetail != nil {
		return nil, a.usageDetail
	}
	return &model.UsageSample{Provider: model.ProviderCodex, ObservedAt: time.Unix(1, 0), Windows: []model.Window{{ID: "primary", Name: "Primary", UsedPercent: 10}}, Raw: []byte(`{"ok":true}`)}, nil
}
func (*fakeAdapter) Refresh(context.Context, provider.Credential) ([]byte, *model.ErrorDetail) {
	return []byte("refreshed"), nil
}
func (a *fakeAdapter) StartLogin(_ context.Context, home string) (provider.LoginProcess, *model.ErrorDetail) {
	credential := a.credential
	if credential == "" {
		credential = "auth.json"
	}
	if err := os.WriteFile(filepath.Join(home, credential), a.loginValue, 0600); err != nil {
		return nil, teach.New(teach.CredentialCommitFailed, "The synthetic login could not write its candidate.", "re-onboard", nil, nil, nil, "retry the exact call")
	}
	return &fakeLogin{output: io.NopCloser(strings.NewReader("https://example.invalid/login\n"))}, nil
}

func TestKeychainSourceTeachesFileOnlyRemedies(t *testing.T) {
	adapter := &fakeAdapter{provider: model.ProviderClaude}
	service, detail := OpenService(t.TempDir(), []provider.Adapter{adapter}, idleChecker{})
	if detail != nil {
		t.Fatal(detail)
	}
	defer service.Close()
	binding, detail := service.ResolveSource(model.ProviderClaude, "work", "keychain", "")
	if detail == nil || detail.Code != teach.KeychainSourceUnsupported || binding.Kind != "" {
		t.Fatalf("binding = %#v, detail = %#v", binding, detail)
	}
	if len(detail.Remedy.Calls) != 2 || detail.Remedy.Calls[0].Method != "POST" || detail.Remedy.Calls[0].Path != "/api/v1/accounts" || detail.Remedy.Calls[1].Path != "/api/v1/accounts/adopt" {
		t.Fatalf("remedy = %#v", detail.Remedy)
	}
	body, ok := detail.Remedy.Calls[1].Body.(map[string]any)
	if !ok || body["provider"] != model.ProviderClaude || body["label"] != "work" {
		t.Fatalf("adopt body = %#v", detail.Remedy.Calls[1].Body)
	}
	source, ok := body["source"].(map[string]any)
	if !ok || source["kind"] != "home" || source["path"] != "/absolute/provider/home" {
		t.Fatalf("source remedy = %#v", body["source"])
	}
}

func TestClaudeDefaultSourceTeachesFileOnlyRemedies(t *testing.T) {
	adapter := &fakeAdapter{
		provider:   model.ProviderClaude,
		defaultErr: teach.New(teach.KeychainSourceUnsupported, "The default Claude login is not an MVP file store.", "adopt", nil, nil, nil, "use an explicit file home"),
	}
	service, detail := OpenService(t.TempDir(), []provider.Adapter{adapter}, idleChecker{})
	if detail != nil {
		t.Fatal(detail)
	}
	defer service.Close()
	binding, detail := service.ResolveSource(model.ProviderClaude, "work", "default", "")
	if detail == nil || detail.Code != teach.KeychainSourceUnsupported || binding.Kind != "" {
		t.Fatalf("binding = %#v, detail = %#v", binding, detail)
	}
	if len(detail.Remedy.Calls) != 2 || detail.Remedy.Calls[0].Path != "/api/v1/accounts" || detail.Remedy.Calls[1].Path != "/api/v1/accounts/adopt" {
		t.Fatalf("remedy = %#v", detail.Remedy)
	}
}

func TestDirectAdoptCannotReachKeychainStore(t *testing.T) {
	adapter := &fakeAdapter{provider: model.ProviderClaude}
	service, detail := OpenService(t.TempDir(), []provider.Adapter{adapter}, idleChecker{})
	if detail != nil {
		t.Fatal(detail)
	}
	defer service.Close()
	service.SetStoreFactoryForTests(func(model.StoreBinding) (store.Adapter, error) {
		t.Fatal("keychain binding reached the store factory")
		return nil, nil
	})
	_, detail = service.Adopt(context.Background(), model.ProviderClaude, "work", model.StoreBinding{Kind: "keychain", Service: "legacy", Account: "default"})
	if detail == nil || detail.Code != teach.KeychainSourceUnsupported {
		t.Fatalf("detail = %#v", detail)
	}
}

func TestNewOnboardingPersistsProviderFileBindings(t *testing.T) {
	tests := []struct {
		provider   model.Provider
		credential string
	}{{model.ProviderCodex, "auth.json"}, {model.ProviderClaude, ".credentials.json"}}
	for _, tt := range tests {
		t.Run(string(tt.provider), func(t *testing.T) {
			adapter := &fakeAdapter{provider: tt.provider, credential: tt.credential, loginValue: []byte("synthetic")}
			service, detail := OpenService(t.TempDir(), []provider.Adapter{adapter}, idleChecker{})
			if detail != nil {
				t.Fatal(detail)
			}
			defer service.Close()
			job, detail := service.Jobs().StartOnboard(tt.provider, "work")
			if detail != nil {
				t.Fatal(detail)
			}
			job = waitForJob(t, service, job.ID)
			if job.State != "succeeded" || job.ResultAccount == nil || job.ResultAccount.StoreKind != "file" {
				t.Fatalf("job = %#v", job)
			}
			row, ok := service.registry.Find(job.ResultAccount.ID)
			if !ok || row.Store.Kind != "file" || filepath.Base(row.Store.CredentialPath) != tt.credential {
				t.Fatalf("row = %#v", row)
			}
		})
	}
}

func TestNewOnboardingRejectsNonFileManagedBinding(t *testing.T) {
	managed := model.StoreBinding{Kind: "keychain", Service: "legacy", Account: "default"}
	adapter := &fakeAdapter{provider: model.ProviderClaude, credential: ".credentials.json", loginValue: []byte("synthetic"), managed: &managed}
	service, detail := OpenService(t.TempDir(), []provider.Adapter{adapter}, idleChecker{})
	if detail != nil {
		t.Fatal(detail)
	}
	defer service.Close()
	job, detail := service.Jobs().StartOnboard(model.ProviderClaude, "work")
	if detail != nil {
		t.Fatal(detail)
	}
	job = waitForJob(t, service, job.ID)
	if job.State != "failed" || job.Error == nil || job.Error.Code != teach.KeychainSourceUnsupported || len(service.registry.Snapshot().Accounts) != 0 {
		t.Fatalf("job = %#v, registry = %#v", job, service.registry.Snapshot())
	}
}

func TestNewOnboardingRejectsCredentialOutsideManagedHome(t *testing.T) {
	adapter := &fakeAdapter{provider: model.ProviderCodex, loginValue: []byte("synthetic"), outsideHome: true}
	service, detail := OpenService(t.TempDir(), []provider.Adapter{adapter}, idleChecker{})
	if detail != nil {
		t.Fatal(detail)
	}
	defer service.Close()
	job, detail := service.Jobs().StartOnboard(model.ProviderCodex, "work")
	if detail != nil {
		t.Fatal(detail)
	}
	job = waitForJob(t, service, job.ID)
	if job.State != "failed" || job.Error == nil || job.Error.Code != teach.KeychainSourceUnsupported {
		t.Fatalf("job = %#v", job)
	}
	if len(service.List()) != 0 {
		t.Fatalf("accounts = %#v", service.List())
	}
}

func waitForJob(t *testing.T, service *Service, id string) model.Job {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		job, detail := service.Jobs().Get(id)
		if detail != nil {
			t.Fatal(detail)
		}
		if job.State == "succeeded" || job.State == "failed" {
			return job
		}
		if time.Now().After(deadline) {
			t.Fatalf("job did not finish: %#v", job)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

type fakeLogin struct{ output io.ReadCloser }

func (f *fakeLogin) Output() io.ReadCloser { return f.output }
func (*fakeLogin) Wait() error             { return nil }
func (*fakeLogin) Terminate() error        { return nil }
func (*fakeLogin) Kill() error             { return nil }

func TestCredentialRejectionTeachesAccountReOnboard(t *testing.T) {
	adapter := &fakeAdapter{}
	service, detail := OpenService(t.TempDir(), []provider.Adapter{adapter}, idleChecker{})
	if detail != nil {
		t.Fatal(detail)
	}
	defer service.Close()
	home := t.TempDir()
	path := filepath.Join(home, "auth.json")
	if err := os.WriteFile(path, []byte("credential"), 0600); err != nil {
		t.Fatal(err)
	}
	account, detail := service.Adopt(context.Background(), model.ProviderCodex, "test", model.StoreBinding{Kind: "file", Home: home, CredentialPath: path})
	if detail != nil {
		t.Fatal(detail)
	}
	adapter.usageDetail = teach.New(teach.CredentialRejected, "The provider rejected the credential.", "usage", nil, nil, nil, "retry the exact call")
	_, detail = service.Verify(context.Background(), account.ID)
	if detail == nil || detail.Code != teach.CredentialRejected {
		t.Fatalf("detail = %#v", detail)
	}
	if len(detail.Remedy.Calls) != 1 || detail.Remedy.Calls[0].Method != "POST" || detail.Remedy.Calls[0].Path != "/api/v1/accounts/"+account.ID+"/re-onboard" {
		t.Fatalf("remedy = %#v", detail.Remedy)
	}
	if len(detail.Remedy.Commands) != 0 || detail.Help != "/api/v1/help/re-onboard" {
		t.Fatalf("teaching detail = %#v", detail)
	}
}

func TestReOnboardRecreatesMissingFileCredential(t *testing.T) {
	adapter := &fakeAdapter{loginValue: []byte("new")}
	service, detail := OpenService(t.TempDir(), []provider.Adapter{adapter}, idleChecker{})
	if detail != nil {
		t.Fatal(detail)
	}
	defer service.Close()
	home := t.TempDir()
	path := filepath.Join(home, "auth.json")
	if err := os.WriteFile(path, []byte("old"), 0600); err != nil {
		t.Fatal(err)
	}
	account, detail := service.Adopt(context.Background(), model.ProviderCodex, "test", model.StoreBinding{Kind: "file", Home: home, CredentialPath: path})
	if detail != nil {
		t.Fatal(detail)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	job, detail := service.Jobs().StartReOnboard(account.ID)
	if detail != nil {
		t.Fatal(detail)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		job, detail = service.Jobs().Get(job.ID)
		if detail != nil {
			t.Fatal(detail)
		}
		if job.State == "succeeded" {
			break
		}
		if job.State == "failed" {
			t.Fatalf("job failed: %#v", job.Error)
		}
		if time.Now().After(deadline) {
			t.Fatalf("job did not finish: %#v", job)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got, err := os.ReadFile(path); err != nil || string(got) != "new" {
		t.Fatalf("recreated credential = %q, %v", got, err)
	}
}

func TestAdoptPersistsResolvedCredentialAuthority(t *testing.T) {
	adapter := &fakeAdapter{}
	service, detail := OpenService(t.TempDir(), []provider.Adapter{adapter}, idleChecker{})
	if detail != nil {
		t.Fatal(detail)
	}
	defer service.Close()
	dir := t.TempDir()
	target := filepath.Join(dir, "target.json")
	link := filepath.Join(dir, "link.json")
	if err := os.WriteFile(target, []byte("credential"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	account, detail := service.Adopt(context.Background(), model.ProviderCodex, "test", model.StoreBinding{Kind: "file", Home: dir, CredentialPath: link})
	if detail != nil {
		t.Fatal(detail)
	}
	row, ok := service.registry.Find(account.ID)
	if !ok || row.Store.CredentialPath != target {
		t.Fatalf("registry row = %#v", row)
	}
}
