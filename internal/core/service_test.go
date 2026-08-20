package core

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
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
	usageSample *model.UsageSample
	loginValue  []byte
	provider    model.Provider
	credential  string
	managed     *model.StoreBinding
	outsideHome bool
	defaultErr  *model.ErrorDetail
	usageFunc   func(context.Context, provider.Credential) (*model.UsageSample, *model.ErrorDetail)
	refreshFunc func(context.Context, provider.Credential) ([]byte, *model.ErrorDetail)
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
func (a *fakeAdapter) Usage(ctx context.Context, credential provider.Credential) (*model.UsageSample, *model.ErrorDetail) {
	if a.usageFunc != nil {
		return a.usageFunc(ctx, credential)
	}
	if a.usageDetail != nil {
		return nil, a.usageDetail
	}
	if a.usageSample != nil {
		sample := *a.usageSample
		return &sample, nil
	}
	return &model.UsageSample{Provider: a.Name(), ObservedAt: time.Unix(1, 0), Windows: []model.Window{{ID: "primary", Name: "Primary", UsedPercent: 10}}, Diagnostics: []model.Diagnostic{}, Raw: []byte(`{"ok":true}`)}, nil
}
func (a *fakeAdapter) Refresh(ctx context.Context, credential provider.Credential) ([]byte, *model.ErrorDetail) {
	if a.refreshFunc != nil {
		return a.refreshFunc(ctx, credential)
	}
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
	if aggregate.Results[0].Status != "fresh" || aggregate.Results[0].Sample == nil || len(aggregate.Results[0].Sample.Diagnostics) != 1 || aggregate.Results[0].Sample.Diagnostics[0].Code != "CODEX_USAGE_WINDOW_OMITTED" {
		t.Fatalf("degraded result = %#v", aggregate.Results[0])
	}
	if aggregate.Results[1].Status != "error" || aggregate.Results[1].Sample != nil || aggregate.Results[1].Error == nil || aggregate.Results[1].Error.Code != teach.UpstreamContractChanged {
		t.Fatalf("failed result = %#v", aggregate.Results[1])
	}
	if aggregate.Counts["fresh"] != 1 || aggregate.Counts["error"] != 1 || aggregate.Counts["pending"] != 0 || aggregate.Counts["stale"] != 0 {
		t.Fatalf("counts = %#v", aggregate.Counts)
	}
}

type busyChecker struct{ calls atomic.Int32 }

func (c *busyChecker) Busy(context.Context, model.Provider) (bool, error) {
	c.calls.Add(1)
	return true, nil
}

func adoptSyntheticAccount(t *testing.T, service *Service, adapter *fakeAdapter, label string, credential []byte) (model.Account, string) {
	t.Helper()
	home := t.TempDir()
	name := adapter.credential
	if name == "" {
		name = "auth.json"
	}
	path := filepath.Join(home, name)
	if err := os.WriteFile(path, credential, 0600); err != nil {
		t.Fatal(err)
	}
	account, detail := service.Adopt(context.Background(), adapter.Name(), label, model.StoreBinding{Kind: "file", Home: home, CredentialPath: path})
	if detail != nil {
		t.Fatal(detail)
	}
	return account, path
}

func TestUsageRunsBesideRefreshAndIgnoresUnrelatedClaudeProcess(t *testing.T) {
	now := time.Unix(1_000, 0).UTC()
	refreshStarted := make(chan struct{})
	releaseRefresh := make(chan struct{})
	claude := &fakeAdapter{
		provider:   model.ProviderClaude,
		credential: ".credentials.json",
		usageSample: &model.UsageSample{
			Provider: model.ProviderClaude, ObservedAt: now,
			Windows:     []model.Window{{ID: "five-hour", Name: "Five hour", UsedPercent: 20}},
			Diagnostics: []model.Diagnostic{}, Raw: []byte(`{"provider":"claude"}`),
		},
		refreshFunc: func(context.Context, provider.Credential) ([]byte, *model.ErrorDetail) {
			close(refreshStarted)
			<-releaseRefresh
			return []byte("claude-refreshed"), nil
		},
	}
	codex := &fakeAdapter{
		provider: model.ProviderCodex,
		usageSample: &model.UsageSample{
			Provider: model.ProviderCodex, ObservedAt: now,
			Windows:     []model.Window{{ID: "primary", Name: "Primary", UsedPercent: 10}},
			Diagnostics: []model.Diagnostic{}, Raw: []byte(`{"provider":"codex"}`),
		},
	}
	checker := &busyChecker{}
	service, detail := OpenService(t.TempDir(), []provider.Adapter{codex, claude}, checker)
	if detail != nil {
		t.Fatal(detail)
	}
	defer service.Close()
	service.SetClockForTests(func() time.Time { return now })
	claudeAccount, _ := adoptSyntheticAccount(t, service, claude, "claude", []byte("claude-original"))
	codexAccount, _ := adoptSyntheticAccount(t, service, codex, "codex", []byte("codex-original"))
	service.cache.Clear(claudeAccount.ID)
	service.cache.Clear(codexAccount.ID)

	type refreshOutcome struct {
		account model.Account
		detail  *model.ErrorDetail
	}
	refreshDone := make(chan refreshOutcome, 1)
	go func() {
		account, detail := service.Refresh(context.Background(), claudeAccount.ID)
		refreshDone <- refreshOutcome{account: account, detail: detail}
	}()
	select {
	case <-refreshStarted:
	case <-time.After(time.Second):
		t.Fatal("refresh did not reach the adapter; a host-wide process guard blocked it")
	}
	if checker.calls.Load() != 0 {
		t.Fatalf("provider-wide process checks = %d", checker.calls.Load())
	}
	if account, detail := service.Get(claudeAccount.ID); detail != nil || account.MutationState != model.MutationRefreshing || !account.Working || account.RefreshError != nil {
		t.Fatalf("refreshing account = %#v, detail = %#v", account, detail)
	}
	pending, detail := service.Usage(context.Background(), claudeAccount.ID, "background")
	if detail != nil || pending.Status != "pending" || pending.Sample != nil || pending.Error == nil || pending.Error.Code != teach.UsageRefreshPending || pending.Error.State["mutation_state"] != model.MutationRefreshing {
		t.Fatalf("refresh-pending usage = %#v, detail = %#v", pending, detail)
	}
	for _, accountID := range []string{claudeAccount.ID, codexAccount.ID} {
		result, detail := service.Usage(context.Background(), accountID, "wait")
		if detail != nil || result.Status != "fresh" || result.Sample == nil || result.Sample.AccountID != accountID || result.Sample.AgeSeconds != 0 {
			t.Fatalf("usage %s = %#v, detail = %#v", accountID, result, detail)
		}
	}
	close(releaseRefresh)
	outcome := <-refreshDone
	if outcome.detail != nil || !outcome.account.Working || outcome.account.MutationState != model.MutationIdle || outcome.account.RefreshError != nil {
		t.Fatalf("refresh outcome = %#v, detail = %#v", outcome.account, outcome.detail)
	}
}

func TestUsageReportsFreshAndStaleWithAgeAndTeachingDetail(t *testing.T) {
	observed := time.Unix(2_000, 0).UTC()
	adapter := &fakeAdapter{usageSample: &model.UsageSample{
		Provider: model.ProviderCodex, ObservedAt: observed,
		Windows:     []model.Window{{ID: "primary", Name: "Primary", UsedPercent: 10}},
		Diagnostics: []model.Diagnostic{}, Raw: []byte(`{"raw":"preserved"}`),
	}}
	service, detail := OpenService(t.TempDir(), []provider.Adapter{adapter}, idleChecker{})
	if detail != nil {
		t.Fatal(detail)
	}
	defer service.Close()
	now := observed.Add(10 * time.Second)
	service.SetClockForTests(func() time.Time { return now })
	account, _ := adoptSyntheticAccount(t, service, adapter, "codex", []byte("credential"))
	result, detail := service.Usage(context.Background(), account.ID, "background")
	if detail != nil || result.Status != "fresh" || result.Sample == nil || result.Sample.AgeSeconds != 10 || string(result.Sample.Raw) != `{"raw":"preserved"}` {
		t.Fatalf("fresh result = %#v, detail = %#v", result, detail)
	}

	now = observed.Add(75 * time.Second)
	adapter.usageDetail = teach.New(teach.UpstreamUnavailable, "Synthetic usage source is unavailable.", "usage", nil, map[string]any{"provider": model.ProviderCodex}, nil, "retry the exact call")
	result, detail = service.Usage(context.Background(), account.ID, "wait")
	if detail != nil || result.Status != "stale" || result.Sample == nil || result.Sample.AgeSeconds != 75 || result.Error == nil || result.Error.Code != teach.UpstreamUnavailable || result.Error.Help != "/api/v1/help/usage" {
		t.Fatalf("stale result = %#v, detail = %#v", result, detail)
	}
}

func TestUsagePendingCoalescesWithoutImplyingFreshness(t *testing.T) {
	observed := time.Unix(3_000, 0).UTC()
	adapter := &fakeAdapter{usageSample: &model.UsageSample{Provider: model.ProviderCodex, ObservedAt: observed, Windows: []model.Window{{ID: "primary", Name: "Primary", UsedPercent: 10}}, Diagnostics: []model.Diagnostic{}, Raw: []byte(`{"ok":true}`)}}
	service, detail := OpenService(t.TempDir(), []provider.Adapter{adapter}, idleChecker{})
	if detail != nil {
		t.Fatal(detail)
	}
	defer service.Close()
	service.SetClockForTests(func() time.Time { return observed })
	account, _ := adoptSyntheticAccount(t, service, adapter, "codex", []byte("credential"))
	service.cache.Clear(account.ID)
	started := make(chan struct{})
	release := make(chan struct{})
	var reads atomic.Int32
	adapter.usageFunc = func(context.Context, provider.Credential) (*model.UsageSample, *model.ErrorDetail) {
		if reads.Add(1) == 1 {
			close(started)
		}
		<-release
		return &model.UsageSample{Provider: model.ProviderCodex, ObservedAt: observed, Windows: []model.Window{{ID: "primary", Name: "Primary", UsedPercent: 10}}, Diagnostics: []model.Diagnostic{}, Raw: []byte(`{"ok":true}`)}, nil
	}
	first, detail := service.Usage(context.Background(), account.ID, "background")
	if detail != nil || first.Status != "pending" || first.Sample != nil || first.Error == nil || first.Error.Code != teach.UsageRefreshPending || first.Error.State["usage_read"] != "in_progress" {
		t.Fatalf("first pending = %#v, detail = %#v", first, detail)
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("background usage read did not start")
	}
	second, detail := service.Usage(context.Background(), account.ID, "background")
	if detail != nil || second.Status != "pending" || second.Sample != nil || second.Error == nil || second.Error.Code != teach.UsageRefreshPending || reads.Load() != 1 {
		t.Fatalf("coalesced pending = %#v, reads = %d, detail = %#v", second, reads.Load(), detail)
	}
	close(release)
	deadline := time.Now().Add(time.Second)
	for {
		result, resultDetail := service.Usage(context.Background(), account.ID, "background")
		if resultDetail == nil && result.Status == "fresh" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("usage did not become fresh: %#v, detail = %#v", result, resultDetail)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestExactStoreConflictPreservesExternalWriteAndUsageHealth(t *testing.T) {
	observed := time.Unix(4_000, 0).UTC()
	adapter := &fakeAdapter{usageSample: &model.UsageSample{Provider: model.ProviderCodex, ObservedAt: observed, Windows: []model.Window{{ID: "primary", Name: "Primary", UsedPercent: 10}}, Diagnostics: []model.Diagnostic{}, Raw: []byte(`{"ok":true}`)}}
	service, detail := OpenService(t.TempDir(), []provider.Adapter{adapter}, idleChecker{})
	if detail != nil {
		t.Fatal(detail)
	}
	defer service.Close()
	service.SetClockForTests(func() time.Time { return observed })
	account, credentialPath := adoptSyntheticAccount(t, service, adapter, "codex", []byte("original"))
	unrelated := filepath.Join(t.TempDir(), "unrelated.json")
	if err := os.WriteFile(unrelated, []byte("untouched"), 0600); err != nil {
		t.Fatal(err)
	}
	adapter.refreshFunc = func(context.Context, provider.Credential) ([]byte, *model.ErrorDetail) {
		if err := os.WriteFile(credentialPath, []byte("external-writer"), 0600); err != nil {
			t.Fatal(err)
		}
		return []byte("refresh-candidate"), nil
	}
	_, detail = service.Refresh(context.Background(), account.ID)
	if detail == nil || detail.Code != teach.CredentialChanged {
		t.Fatalf("refresh detail = %#v", detail)
	}
	if got, err := os.ReadFile(credentialPath); err != nil || string(got) != "external-writer" {
		t.Fatalf("exact store = %q, %v", got, err)
	}
	if got, err := os.ReadFile(unrelated); err != nil || string(got) != "untouched" {
		t.Fatalf("unrelated store = %q, %v", got, err)
	}
	view, getDetail := service.Get(account.ID)
	if getDetail != nil || !view.Working || view.Status != model.StatusReady || view.LastError != nil || view.RefreshError == nil || view.RefreshError.Code != teach.CredentialChanged {
		t.Fatalf("account view = %#v, detail = %#v", view, getDetail)
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
