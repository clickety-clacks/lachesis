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
	"github.com/clickety-clacks/lachesis/internal/processcheck"
	"github.com/clickety-clacks/lachesis/internal/provider"
	"github.com/clickety-clacks/lachesis/internal/store"
	"github.com/clickety-clacks/lachesis/internal/teach"
)

type idleChecker struct{}

func (idleChecker) Busy(context.Context, processcheck.Target) (bool, error) { return false, nil }

type fakeAdapter struct {
	usageDetail *model.ErrorDetail
	usageSample *model.UsageSample
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
	if a.usageSample != nil {
		sample := *a.usageSample
		return &sample, nil
	}
	return &model.UsageSample{Provider: a.Name(), ObservedAt: time.Unix(1, 0), Windows: []model.Window{{ID: "primary", Name: "Primary", UsedPercent: 10}}, Diagnostics: []model.Diagnostic{}, Raw: []byte(`{"ok":true}`)}, nil
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
			checker := &recordingChecker{busy: tt.provider == model.ProviderClaude}
			service, detail := OpenService(t.TempDir(), []provider.Adapter{adapter}, checker)
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
			if tt.provider == model.ProviderClaude && len(checker.snapshot()) != 0 {
				t.Fatalf("Claude onboarding added busy targets: %#v", checker.snapshot())
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

func TestRefreshBusyCheckUsesRegisteredProviderHome(t *testing.T) {
	checker := &recordingChecker{busy: true}
	adapter := &fakeAdapter{}
	service, detail := OpenService(t.TempDir(), []provider.Adapter{adapter}, checker)
	if detail != nil {
		t.Fatal(detail)
	}
	defer service.Close()
	home := t.TempDir()
	credentialPath := filepath.Join(home, "auth.json")
	if err := os.WriteFile(credentialPath, []byte("synthetic"), 0600); err != nil {
		t.Fatal(err)
	}
	account, detail := service.Adopt(context.Background(), model.ProviderCodex, "work", model.StoreBinding{Kind: "file", Home: home, CredentialPath: credentialPath})
	if detail != nil {
		t.Fatal(detail)
	}
	_, detail = service.Refresh(context.Background(), account.ID)
	if detail == nil || detail.Code != teach.CredentialStoreBusy || detail.Message != "The provider CLI is running." || detail.Help != "/api/v1/help/refresh" || len(detail.Remedy.Commands) != 1 || detail.Remedy.Commands[0] != "stop the provider CLI and retry when mutation_state is idle" {
		t.Fatalf("detail = %#v", detail)
	}
	targets := checker.snapshot()
	if len(targets) != 1 || targets[0] != (processcheck.Target{Provider: model.ProviderCodex, Home: home}) {
		t.Fatalf("targets = %#v", targets)
	}
}

func TestAggregatePassesDiagnosticsAndIsolatesFatalAccount(t *testing.T) {
	degradedAdapter := &fakeAdapter{
		provider: model.ProviderCodex,
		usageSample: &model.UsageSample{
			Provider:    model.ProviderCodex,
			ObservedAt:  time.Unix(1, 0),
			Windows:     []model.Window{{ID: "primary", Name: "Primary", UsedPercent: 10}},
			Diagnostics: []model.Diagnostic{{Code: "CODEX_USAGE_WINDOW_OMITTED", Message: "Codex omitted an invalid or unrecognized usage window."}},
			Raw:         []byte(`{"ok":true}`),
		},
	}
	failedAdapter := &fakeAdapter{provider: model.ProviderClaude}
	service, detail := OpenService(t.TempDir(), []provider.Adapter{degradedAdapter, failedAdapter}, idleChecker{})
	if detail != nil {
		t.Fatal(detail)
	}
	defer service.Close()
	adopt := func(providerName model.Provider, label string) model.Account {
		t.Helper()
		home := t.TempDir()
		credentialPath := filepath.Join(home, "credential.json")
		if err := os.WriteFile(credentialPath, []byte("synthetic"), 0600); err != nil {
			t.Fatal(err)
		}
		account, detail := service.Adopt(context.Background(), providerName, label, model.StoreBinding{Kind: "file", Home: home, CredentialPath: credentialPath})
		if detail != nil {
			t.Fatal(detail)
		}
		return account
	}
	degraded := adopt(model.ProviderCodex, "degraded")
	failed := adopt(model.ProviderClaude, "failed")
	failedAdapter.usageDetail = teach.New(
		teach.UpstreamContractChanged,
		"Claude usage contained no valid recognized limit window.",
		"usage",
		[]model.Prerequisite{{Code: "VALID_RECOGNIZED_WINDOW", Description: "The provider response contains at least one valid recognized usage window.", Met: false}},
		map[string]any{"provider": model.ProviderClaude},
		nil,
		"retry the exact call",
	)
	service.cache.Clear(degraded.ID)
	service.cache.Clear(failed.ID)

	aggregate, detail := service.Aggregate(context.Background(), "wait")
	if detail != nil {
		t.Fatal(detail)
	}
	if len(aggregate.Results) != 2 || aggregate.Results[0].AccountID != degraded.ID || aggregate.Results[1].AccountID != failed.ID {
		t.Fatalf("results = %#v", aggregate.Results)
	}
	if aggregate.Results[0].Status != "live" || aggregate.Results[0].Sample == nil || len(aggregate.Results[0].Sample.Diagnostics) != 1 || aggregate.Results[0].Sample.Diagnostics[0].Code != "CODEX_USAGE_WINDOW_OMITTED" {
		t.Fatalf("degraded result = %#v", aggregate.Results[0])
	}
	if aggregate.Results[1].Status != "error" || aggregate.Results[1].Sample != nil || aggregate.Results[1].Error == nil || aggregate.Results[1].Error.Code != teach.UpstreamContractChanged {
		t.Fatalf("failed result = %#v", aggregate.Results[1])
	}
	if aggregate.Counts["live"] != 1 || aggregate.Counts["error"] != 1 || aggregate.Counts["cache"] != 0 || aggregate.Counts["stale"] != 0 {
		t.Fatalf("counts = %#v", aggregate.Counts)
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
