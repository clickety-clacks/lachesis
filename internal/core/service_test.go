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
	"github.com/clickety-clacks/lachesis/internal/teach"
)

type idleChecker struct{}

func (idleChecker) Busy(context.Context, model.Provider) (bool, error) { return false, nil }

type fakeAdapter struct {
	usageDetail *model.ErrorDetail
	loginValue  []byte
}

func (*fakeAdapter) Name() model.Provider { return model.ProviderCodex }
func (*fakeAdapter) CLIAvailable() bool   { return true }
func (*fakeAdapter) DefaultBinding() (model.StoreBinding, *model.ErrorDetail) {
	return model.StoreBinding{}, nil
}
func (*fakeAdapter) ManagedBinding(home string) model.StoreBinding {
	return model.StoreBinding{Kind: "file", Home: home, CredentialPath: filepath.Join(home, "auth.json")}
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
	if err := os.WriteFile(filepath.Join(home, "auth.json"), a.loginValue, 0600); err != nil {
		return nil, teach.New(teach.CredentialCommitFailed, "The synthetic login could not write its candidate.", "re-onboard", nil, nil, nil, "retry the exact call")
	}
	return &fakeLogin{output: io.NopCloser(strings.NewReader("https://example.invalid/login\n"))}, nil
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
