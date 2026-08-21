package core

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/clickety-clacks/lachesis/internal/model"
	"github.com/clickety-clacks/lachesis/internal/processcheck"
	"github.com/clickety-clacks/lachesis/internal/provider"
	"github.com/clickety-clacks/lachesis/internal/teach"
)

type controlledLogin struct {
	reader        *io.PipeReader
	writer        *io.PipeWriter
	exit          chan error
	exitOnce      sync.Once
	terminateExit bool
	killExit      bool
	waits         atomic.Int32
	terminates    atomic.Int32
	kills         atomic.Int32
}

func newControlledLogin() *controlledLogin {
	r, w := io.Pipe()
	return &controlledLogin{reader: r, writer: w, exit: make(chan error, 1)}
}

func (p *controlledLogin) Output() io.ReadCloser { return p.reader }
func (p *controlledLogin) Wait() error {
	p.waits.Add(1)
	err := <-p.exit
	_ = p.writer.Close()
	return err
}
func (p *controlledLogin) Terminate() error {
	p.terminates.Add(1)
	if p.terminateExit {
		p.finish(nil)
	}
	return nil
}
func (p *controlledLogin) Kill() error {
	p.kills.Add(1)
	if p.killExit {
		p.finish(errors.New("killed"))
	}
	return nil
}
func (p *controlledLogin) finish(err error) { p.exitOnce.Do(func() { p.exit <- err }) }
func (p *controlledLogin) line(line string) {
	_, _ = io.WriteString(p.writer, line+"\n")
}
func (p *controlledLogin) devicePrompt() {
	p.line("1. Open this link in your browser and sign in to your account")
	p.line(" \x1b[94m" + codexVerificationURL + "\x1b[0m")
	p.line("2. Enter this one-time code (expires in 15 minutes)")
	p.line(" \x1b[94mTEST-CODE\x1b[0m")
}

type jobAdapter struct {
	mu      sync.Mutex
	name    model.Provider
	starts  int
	homes   []string
	start   func(string) (provider.LoginProcess, *model.ErrorDetail)
	usage   func(context.Context, provider.Credential) (*model.UsageSample, *model.ErrorDetail)
	barrier chan struct{}
}

func (a *jobAdapter) Name() model.Provider {
	if a.name == "" {
		return model.ProviderCodex
	}
	return a.name
}
func (*jobAdapter) CLIAvailable() bool { return true }
func (*jobAdapter) DefaultBinding() (model.StoreBinding, *model.ErrorDetail) {
	return model.StoreBinding{}, nil
}
func (*jobAdapter) ManagedBinding(home string) model.StoreBinding {
	return model.StoreBinding{Kind: "file", Home: home, CredentialPath: filepath.Join(home, "auth.json")}
}
func (*jobAdapter) ParseCredential(raw []byte) (provider.Credential, *model.ErrorDetail) {
	if len(raw) == 0 {
		return provider.Credential{}, teach.New(teach.CredentialRejected, "Synthetic credential is empty.", "usage", nil, nil, nil)
	}
	return provider.Credential{Raw: append([]byte(nil), raw...)}, nil
}

func (a *jobAdapter) Usage(ctx context.Context, credential provider.Credential) (*model.UsageSample, *model.ErrorDetail) {
	if a.usage != nil {
		return a.usage(ctx, credential)
	}
	return &model.UsageSample{Provider: model.ProviderCodex, ObservedAt: time.Unix(1, 0), Windows: []model.Window{{ID: "primary", Name: "Primary", UsedPercent: 10}}, Raw: []byte(`{"ok":true}`)}, nil
}
func (*jobAdapter) Refresh(context.Context, provider.Credential) ([]byte, *model.ErrorDetail) {
	return []byte("synthetic"), nil
}
func (a *jobAdapter) StartLogin(_ context.Context, home string) (provider.LoginProcess, *model.ErrorDetail) {
	a.mu.Lock()
	a.starts++
	a.homes = append(a.homes, home)
	start := a.start
	barrier := a.barrier
	a.mu.Unlock()
	if barrier != nil {
		<-barrier
	}
	return start(home)
}
func (a *jobAdapter) startCount() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.starts
}
func (a *jobAdapter) lastHome() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.homes[len(a.homes)-1]
}

func openJobService(t *testing.T, adapter provider.Adapter) *Service {
	t.Helper()
	service, detail := OpenService(t.TempDir(), []provider.Adapter{adapter}, idleChecker{})
	if detail != nil {
		t.Fatal(detail)
	}
	return service
}

type recordingChecker struct {
	mu      sync.Mutex
	targets []processcheck.Target
	busy    bool
	err     error
}

func (c *recordingChecker) Busy(_ context.Context, target processcheck.Target) (bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.targets = append(c.targets, target)
	return c.busy, c.err
}

func (c *recordingChecker) snapshot() []processcheck.Target {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]processcheck.Target(nil), c.targets...)
}

func waitForJobState(t *testing.T, service *Service, id string, states ...string) model.Job {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		job, detail := service.Jobs().Get(id)
		if detail != nil {
			t.Fatal(detail)
		}
		for _, state := range states {
			if job.State == state {
				return job
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("job did not reach %v: %#v", states, job)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestOnboardBusyCheckUsesAllocatedProviderHome(t *testing.T) {
	stateDir := t.TempDir()
	checker := &recordingChecker{busy: true}
	adapter := &jobAdapter{start: func(string) (provider.LoginProcess, *model.ErrorDetail) {
		t.Fatal("StartLogin called")
		return nil, nil
	}}
	service, detail := OpenService(stateDir, []provider.Adapter{adapter}, checker)
	if detail != nil {
		t.Fatal(detail)
	}
	defer service.Close()
	job, detail := service.Jobs().StartOnboard(model.ProviderCodex, "work")
	if detail != nil {
		t.Fatal(detail)
	}
	job = waitForJobState(t, service, job.ID, "failed")
	if job.Error == nil || job.Error.Code != teach.CredentialStoreBusy || job.Error.Message != "The provider CLI is running or cannot be inspected." || job.Error.Help != "/api/v1/help/onboard" || len(job.Error.Remedy.Commands) != 1 || job.Error.Remedy.Commands[0] != "stop the provider CLI and retry when mutation_state is idle" || job.Error.State["provider"] != model.ProviderCodex {
		t.Fatalf("job = %#v", job)
	}
	targets := checker.snapshot()
	if len(targets) != 1 || targets[0].Provider != model.ProviderCodex || filepath.Dir(targets[0].Home) != filepath.Join(stateDir, "providers", "codex") {
		t.Fatalf("targets = %#v", targets)
	}
	if adapter.startCount() != 0 {
		t.Fatalf("starts = %d", adapter.startCount())
	}
}

func TestReOnboardBusyCheckUsesRegisteredProviderHome(t *testing.T) {
	checker := &recordingChecker{busy: true}
	adapter := &jobAdapter{start: func(string) (provider.LoginProcess, *model.ErrorDetail) {
		t.Fatal("StartLogin called")
		return nil, nil
	}}
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
	job, detail := service.Jobs().StartReOnboard(account.ID)
	if detail != nil {
		t.Fatal(detail)
	}
	job = waitForJobState(t, service, job.ID, "failed")
	if job.Error == nil || job.Error.Code != teach.CredentialStoreBusy || job.Error.Message != "The provider CLI is running or cannot be inspected." || job.Error.Help != "/api/v1/help/re-onboard" || len(job.Error.Remedy.Commands) != 1 || job.Error.Remedy.Commands[0] != "stop the provider CLI and retry when mutation_state is idle" || job.Error.State["provider"] != model.ProviderCodex {
		t.Fatalf("job = %#v", job)
	}
	targets := checker.snapshot()
	if len(targets) != 1 || targets[0] != (processcheck.Target{Provider: model.ProviderCodex, Home: home}) {
		t.Fatalf("targets = %#v", targets)
	}
	if adapter.startCount() != 0 {
		t.Fatalf("starts = %d", adapter.startCount())
	}
}

func TestLoginSuccessWaitsConcurrentlyAndNeedsNoURL(t *testing.T) {
	adapter := &jobAdapter{start: func(home string) (provider.LoginProcess, *model.ErrorDetail) {
		if err := os.WriteFile(filepath.Join(home, "auth.json"), []byte("synthetic"), 0600); err != nil {
			t.Fatal(err)
		}
		process := newControlledLogin()
		process.finish(nil)
		return process, nil
	}}
	service := openJobService(t, adapter)
	defer service.Close()
	job, detail := service.Jobs().StartOnboard(model.ProviderCodex, "work")
	if detail != nil {
		t.Fatal(detail)
	}
	job = waitForJobState(t, service, job.ID, "succeeded")
	if job.AuthorizationURL != nil || job.VerificationURL != nil || job.UserCode != nil || job.Error != nil {
		t.Fatalf("job = %#v", job)
	}
}

func TestListenerExitFailsAndReleasesProvider(t *testing.T) {
	adapter := &jobAdapter{name: model.ProviderClaude, start: func(string) (provider.LoginProcess, *model.ErrorDetail) {
		process := newControlledLogin()
		go func() {
			process.line("open https://example.invalid/login")
			process.finish(nil)
		}()
		return process, nil
	}}
	service := openJobService(t, adapter)
	defer service.Close()
	job, detail := service.Jobs().StartOnboard(model.ProviderClaude, "work")
	if detail != nil {
		t.Fatal(detail)
	}
	job = waitForJobState(t, service, job.ID, "failed")
	if job.Error == nil || job.Error.Code != teach.LoginListenerExited || job.AuthorizationURL != nil {
		t.Fatalf("job = %#v", job)
	}
	if _, detail = service.Jobs().StartOnboard(model.ProviderClaude, "replacement"); detail != nil {
		t.Fatalf("replacement = %v", detail)
	}
}

func TestLoginExitClassification(t *testing.T) {
	tests := []struct {
		name       string
		url        bool
		credential bool
		exitErr    error
		state      string
		code       string
	}{
		{name: "success with URL and credential", url: true, credential: true, state: "succeeded"},
		{name: "nonzero with URL and credential", url: true, credential: true, exitErr: errors.New("exit 1"), state: "failed", code: teach.CredentialRejected},
		{name: "nonzero without URL with credential", credential: true, exitErr: errors.New("exit 1"), state: "failed", code: teach.CredentialRejected},
		{name: "zero with device prompt without credential", url: true, state: "failed", code: teach.CredentialMissing},
		{name: "nonzero with device prompt without credential", url: true, exitErr: errors.New("exit 1"), state: "failed", code: teach.CredentialRejected},
		{name: "zero without URL or credential", state: "failed", code: teach.LoginURLUnavailable},
		{name: "nonzero without URL or credential", exitErr: errors.New("exit 1"), state: "failed", code: teach.CredentialRejected},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			adapter := &jobAdapter{start: func(home string) (provider.LoginProcess, *model.ErrorDetail) {
				if tt.credential {
					if err := os.WriteFile(filepath.Join(home, "auth.json"), []byte("synthetic"), 0600); err != nil {
						t.Fatal(err)
					}
				}
				process := newControlledLogin()
				go func() {
					if tt.url {
						process.devicePrompt()
					}
					process.finish(tt.exitErr)
				}()
				return process, nil
			}}
			service := openJobService(t, adapter)
			defer service.Close()
			job, detail := service.Jobs().StartOnboard(model.ProviderCodex, "work")
			if detail != nil {
				t.Fatal(detail)
			}
			job = waitForJobState(t, service, job.ID, tt.state)
			if tt.code == "" {
				if job.Error != nil {
					t.Fatalf("job = %#v", job)
				}
			} else if job.Error == nil || job.Error.Code != tt.code {
				t.Fatalf("job = %#v", job)
			}
		})
	}
}

func TestCodexDeviceAuthorizationFieldsAreTransient(t *testing.T) {
	process := newControlledLogin()
	adapter := &jobAdapter{start: func(home string) (provider.LoginProcess, *model.ErrorDetail) {
		go process.devicePrompt()
		return process, nil
	}}
	service := openJobService(t, adapter)
	defer service.Close()
	job, detail := service.Jobs().StartOnboard(model.ProviderCodex, "work")
	if detail != nil {
		t.Fatal(detail)
	}
	job = waitForJobState(t, service, job.ID, "awaiting_user")
	if job.AuthorizationURL != nil || job.VerificationURL == nil || *job.VerificationURL != codexVerificationURL || job.UserCode == nil || *job.UserCode != "TEST-CODE" {
		t.Fatalf("job = %#v", job)
	}
	home := adapter.lastHome()
	if err := os.WriteFile(filepath.Join(home, "auth.json"), []byte("synthetic"), 0600); err != nil {
		t.Fatal(err)
	}
	job, detail = service.Jobs().Get(job.ID)
	if detail != nil || job.State != "awaiting_user" {
		t.Fatalf("credential advanced before child exit: job = %#v, detail = %#v", job, detail)
	}
	process.finish(nil)
	job = waitForJobState(t, service, job.ID, "succeeded")
	assertNoRetainedLoginPrompt(t, job, "TEST-CODE", "PRIVATE_RAW_LOGIN_SENTINEL")
}

func TestCodexDeviceAuthorizationUnavailableTeachesEnableAndRetry(t *testing.T) {
	phrases := []string{
		"device code login is not enabled",
		"please contact your workspace admin to enable device code authentication",
		"device code request failed",
		"unexpected argument '--device-auth'",
		"unrecognized option '--device-auth'",
	}
	for _, phrase := range phrases {
		t.Run(phrase, func(t *testing.T) {
			adapter := &jobAdapter{start: func(string) (provider.LoginProcess, *model.ErrorDetail) {
				process := newControlledLogin()
				go func() {
					process.line(phrase + " PRIVATE_RAW_LOGIN_SENTINEL")
					process.line("TEST-CODE")
					process.finish(errors.New("exit 1"))
				}()
				return process, nil
			}}
			service := openJobService(t, adapter)
			defer service.Close()
			job, detail := service.Jobs().StartOnboard(model.ProviderCodex, "work")
			if detail != nil {
				t.Fatal(detail)
			}
			job = waitForJobState(t, service, job.ID, "failed")
			if job.Error == nil || job.Error.Code != teach.DeviceAuthorizationUnavailable || job.Error.Message != "Codex device authorization is disabled or unavailable." || len(job.Error.Prerequisites) != 2 ||
				job.Error.Prerequisites[0] != (model.Prerequisite{Code: "CODEX_DEVICE_AUTHORIZATION_SUPPORTED", Description: "The installed Codex CLI supports codex login --device-auth.", Met: false}) ||
				job.Error.Prerequisites[1] != (model.Prerequisite{Code: "CODEX_DEVICE_AUTHORIZATION_ENABLED", Description: "Device code authentication is enabled in ChatGPT security settings or workspace permissions.", Met: false}) ||
				job.Error.Remedy.Summary != "Enable Codex device authorization, then retry the same onboarding call." || len(job.Error.Remedy.Calls) != 1 {
				t.Fatalf("job = %#v", job)
			}
			call := job.Error.Remedy.Calls[0]
			body, ok := call.Body.(map[string]any)
			if call.Method != "POST" || call.Path != "/api/v1/accounts" || !ok || body["provider"] != model.ProviderCodex || body["label"] != "work" {
				t.Fatalf("retry call = %#v", call)
			}
			assertNoRetainedLoginPrompt(t, job, "TEST-CODE", "PRIVATE_RAW_LOGIN_SENTINEL", phrase)
		})
	}
}

func TestCodexDeviceAuthorizationExpiryClearsCode(t *testing.T) {
	process := newControlledLogin()
	adapter := &jobAdapter{start: func(string) (provider.LoginProcess, *model.ErrorDetail) {
		go func() {
			process.devicePrompt()
			process.line("Error logging in with device code: device auth timed out after 15 minutes PRIVATE_RAW_LOGIN_SENTINEL")
			process.finish(errors.New("exit 1"))
		}()
		return process, nil
	}}
	service := openJobService(t, adapter)
	defer service.Close()
	job, detail := service.Jobs().StartOnboard(model.ProviderCodex, "work")
	if detail != nil {
		t.Fatal(detail)
	}
	job = waitForJobState(t, service, job.ID, "failed")
	if job.Error == nil || job.Error.Code != teach.LoginTimeout || job.Error.Message != "The Codex device authorization code expired before login completed." {
		t.Fatalf("job = %#v", job)
	}
	assertNoRetainedLoginPrompt(t, job, "TEST-CODE", "PRIVATE_RAW_LOGIN_SENTINEL")
}

func TestCodexIncompletePromptIsNeverSurfaced(t *testing.T) {
	for name, lines := range map[string][]string{
		"callback and code":         {"https://localhost:1455/auth/callback", "TEST-CODE"},
		"official URL without code": {codexVerificationURL},
	} {
		t.Run(name, func(t *testing.T) {
			process := newControlledLogin()
			adapter := &jobAdapter{start: func(string) (provider.LoginProcess, *model.ErrorDetail) {
				go func() {
					for _, line := range lines {
						process.line(line)
					}
					process.finish(nil)
				}()
				return process, nil
			}}
			service := openJobService(t, adapter)
			defer service.Close()
			job, detail := service.Jobs().StartOnboard(model.ProviderCodex, "work")
			if detail != nil {
				t.Fatal(detail)
			}
			job = waitForJobState(t, service, job.ID, "failed")
			if job.Error == nil || job.Error.Code != teach.LoginURLUnavailable {
				t.Fatalf("job = %#v", job)
			}
			assertNoRetainedLoginPrompt(t, job, lines...)
		})
	}
}

func assertNoRetainedLoginPrompt(t *testing.T, job model.Job, forbidden ...string) {
	t.Helper()
	if job.AuthorizationURL != nil || job.VerificationURL != nil || job.UserCode != nil {
		t.Fatalf("retained login prompt: %#v", job)
	}
	raw, err := json.Marshal(job)
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range forbidden {
		if strings.Contains(string(raw), value) {
			t.Fatalf("retained private output %q in %s", value, raw)
		}
	}
}

func TestCompletedLoginSuccessWinsObservableDeadline(t *testing.T) {
	service := openJobService(t, &jobAdapter{start: func(string) (provider.LoginProcess, *model.ErrorDetail) { return nil, nil }})
	defer service.Close()
	expected := filepath.Join(t.TempDir(), "auth.json")
	if err := os.WriteFile(expected, []byte("synthetic"), 0600); err != nil {
		t.Fatal(err)
	}
	job := newManagedJob(model.Job{ID: "job_success_deadline", Provider: model.ProviderCodex, State: "awaiting_user"}, "work")
	job.lifecycle = processExited
	job.expected = expected
	job.exitDone = make(chan struct{})
	close(job.exitDone)
	job.outputDone = make(chan outputResult, 1)
	job.outputDone <- outputResult{}
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	cancel()
	if detail := service.Jobs().observeLogin(ctx, job); detail != nil {
		t.Fatalf("completed success lost to deadline: %v", detail)
	}
	verificationCtx, ok := service.Jobs().beginVerification(job)
	if !ok || verificationCtx.Err() != nil || job.model.State != "verifying" {
		t.Fatalf("job = %#v", job.model)
	}
}

func TestExpiredLoginContextHandsOffToVerification(t *testing.T) {
	for _, kind := range []string{"onboard", "re_onboard"} {
		t.Run(kind, func(t *testing.T) {
			var verificationPhase atomic.Bool
			adapter := &jobAdapter{start: func(home string) (provider.LoginProcess, *model.ErrorDetail) {
				if err := os.WriteFile(filepath.Join(home, "auth.json"), []byte("synthetic"), 0600); err != nil {
					t.Fatal(err)
				}
				process := newControlledLogin()
				go func() {
					process.devicePrompt()
					process.finish(nil)
				}()
				return process, nil
			}}
			adapter.usage = func(ctx context.Context, _ provider.Credential) (*model.UsageSample, *model.ErrorDetail) {
				if verificationPhase.Load() && ctx.Err() != nil {
					return nil, teach.New(teach.UpstreamTimeout, "Synthetic verification inherited the login deadline.", "onboard", nil, nil, nil)
				}
				return &model.UsageSample{Provider: model.ProviderCodex, ObservedAt: time.Unix(1, 0), Windows: []model.Window{{ID: "primary", Name: "Primary", UsedPercent: 10}}, Raw: []byte(`{"ok":true}`)}, nil
			}
			service := openJobService(t, adapter)
			defer service.Close()

			manager := service.Jobs()
			var account model.Account
			if kind == "re_onboard" {
				originalHome := t.TempDir()
				originalPath := filepath.Join(originalHome, "auth.json")
				if err := os.WriteFile(originalPath, []byte("original"), 0600); err != nil {
					t.Fatal(err)
				}
				var detail *model.ErrorDetail
				account, detail = service.Adopt(context.Background(), model.ProviderCodex, "work", model.StoreBinding{Kind: "file", Home: originalHome, CredentialPath: originalPath})
				if detail != nil {
					t.Fatal(detail)
				}
			}
			manager.beforeVerify = func(job *managedJob) {
				job.cancel()
				verificationPhase.Store(true)
			}
			var job model.Job
			var detail *model.ErrorDetail
			if kind == "onboard" {
				job, detail = manager.StartOnboard(model.ProviderCodex, "work")
			} else {
				job, detail = manager.StartReOnboard(account.ID)
			}
			if detail != nil {
				t.Fatal(detail)
			}
			job = waitForJobState(t, service, job.ID, "succeeded")
			if !verificationPhase.Load() || job.Error != nil {
				t.Fatalf("job = %#v", job)
			}
		})
	}
}

func TestCancelUsesInstalledVerificationContext(t *testing.T) {
	for _, kind := range []string{"onboard", "re_onboard"} {
		t.Run(kind, func(t *testing.T) {
			verificationPhase := atomic.Bool{}
			verificationContext := make(chan context.Context, 1)
			adapter := &jobAdapter{start: func(home string) (provider.LoginProcess, *model.ErrorDetail) {
				if err := os.WriteFile(filepath.Join(home, "auth.json"), []byte("candidate"), 0600); err != nil {
					t.Fatal(err)
				}
				process := newControlledLogin()
				go func() {
					process.devicePrompt()
					process.finish(nil)
				}()
				return process, nil
			}}
			adapter.usage = func(ctx context.Context, _ provider.Credential) (*model.UsageSample, *model.ErrorDetail) {
				if !verificationPhase.Load() {
					return &model.UsageSample{Provider: model.ProviderCodex, ObservedAt: time.Unix(1, 0), Windows: []model.Window{{ID: "primary", Name: "Primary", UsedPercent: 10}}, Raw: []byte(`{"ok":true}`)}, nil
				}
				verificationContext <- ctx
				<-ctx.Done()
				return nil, teach.New(teach.UpstreamTimeout, "Synthetic verification was canceled.", "onboard", nil, nil, nil)
			}
			service := openJobService(t, adapter)
			defer service.Close()
			manager := service.Jobs()
			var account model.Account
			originalPath := ""
			if kind == "re_onboard" {
				originalHome := t.TempDir()
				originalPath = filepath.Join(originalHome, "auth.json")
				if err := os.WriteFile(originalPath, []byte("original"), 0600); err != nil {
					t.Fatal(err)
				}
				var detail *model.ErrorDetail
				account, detail = service.Adopt(context.Background(), model.ProviderCodex, "work", model.StoreBinding{Kind: "file", Home: originalHome, CredentialPath: originalPath})
				if detail != nil {
					t.Fatal(detail)
				}
			}
			manager.beforeVerify = func(*managedJob) { verificationPhase.Store(true) }
			var job model.Job
			var detail *model.ErrorDetail
			if kind == "onboard" {
				job, detail = manager.StartOnboard(model.ProviderCodex, "work")
			} else {
				job, detail = manager.StartReOnboard(account.ID)
			}
			if detail != nil {
				t.Fatal(detail)
			}
			ctx := <-verificationContext
			if ctx.Err() != nil {
				t.Fatalf("verification context started canceled: %v", ctx.Err())
			}
			job, detail = manager.Cancel(job.ID)
			if detail != nil || job.State != "canceled" || !errors.Is(ctx.Err(), context.Canceled) {
				t.Fatalf("job = %#v, detail = %#v, context = %v", job, detail, ctx.Err())
			}
			if originalPath != "" {
				raw, err := os.ReadFile(originalPath)
				if err != nil || string(raw) != "original" {
					t.Fatalf("original credential = %q, %v", raw, err)
				}
			}
		})
	}
}

func TestRegisteredTimeoutPreservesClaimAndLaterCancel(t *testing.T) {
	process := newControlledLogin()
	process.terminateExit = true
	adapter := &jobAdapter{start: func(string) (provider.LoginProcess, *model.ErrorDetail) {
		go process.devicePrompt()
		return process, nil
	}}
	service := openJobService(t, adapter)
	defer service.Close()
	job, detail := service.Jobs().StartOnboard(model.ProviderCodex, "work")
	if detail != nil {
		t.Fatal(detail)
	}
	waitForJobState(t, service, job.ID, "awaiting_user")
	manager := service.Jobs()
	manager.mu.RLock()
	managed := manager.jobs[job.ID]
	manager.mu.RUnlock()
	manager.recordFailure(managed, manager.timeoutDetail(managed))
	manager.ensureStop(managed)
	job = waitForJobState(t, service, job.ID, "failed")
	if job.Error == nil || job.Error.Code != teach.LoginTimeout || job.AuthorizationURL != nil || job.VerificationURL != nil || job.UserCode != nil || process.waits.Load() != 1 {
		t.Fatalf("job = %#v, waits = %d", job, process.waits.Load())
	}
	updated := job.UpdatedAt
	again, detail := manager.Cancel(job.ID)
	if detail != nil || again.State != "failed" || again.Error == nil || again.Error.Code != teach.LoginTimeout || !again.UpdatedAt.Equal(updated) {
		t.Fatalf("again = %#v, detail = %#v", again, detail)
	}
}

func TestCancelPreservesProviderCredentialAndIsIdempotent(t *testing.T) {
	var processes []*controlledLogin
	adapter := &jobAdapter{start: func(home string) (provider.LoginProcess, *model.ErrorDetail) {
		if err := os.WriteFile(filepath.Join(home, "auth.json"), []byte("synthetic-preserved"), 0600); err != nil {
			t.Fatal(err)
		}
		process := newControlledLogin()
		process.terminateExit = true
		processes = append(processes, process)
		go process.devicePrompt()
		return process, nil
	}}
	service := openJobService(t, adapter)
	defer service.Close()
	job, detail := service.Jobs().StartOnboard(model.ProviderCodex, "work")
	if detail != nil {
		t.Fatal(detail)
	}
	waitForJobState(t, service, job.ID, "awaiting_user")
	home := adapter.lastHome()
	before, err := os.ReadFile(filepath.Join(home, "auth.json"))
	if err != nil {
		t.Fatal(err)
	}
	job, detail = service.Jobs().Cancel(job.ID)
	if detail != nil || job.State != "canceled" || job.Error == nil || job.Error.Code != teach.JobCanceled || job.AuthorizationURL != nil || job.VerificationURL != nil || job.UserCode != nil {
		t.Fatalf("job = %#v, detail = %#v", job, detail)
	}
	after, err := os.ReadFile(filepath.Join(home, "auth.json"))
	if err != nil || string(after) != string(before) {
		t.Fatalf("credential = %q, %v", after, err)
	}
	updated := job.UpdatedAt
	for range 2 {
		again, retryDetail := service.Jobs().Cancel(job.ID)
		if retryDetail != nil || again.State != "canceled" || !again.UpdatedAt.Equal(updated) {
			t.Fatalf("again = %#v, detail = %#v", again, retryDetail)
		}
	}
	if _, detail = service.Jobs().StartOnboard(model.ProviderCodex, "replacement"); detail != nil {
		t.Fatalf("replacement = %v", detail)
	}
	if processes[0].waits.Load() != 1 {
		t.Fatalf("Wait calls = %d", processes[0].waits.Load())
	}
}

func TestQueuedCancellationPreventsEveryStartEffect(t *testing.T) {
	adapter := &jobAdapter{start: func(string) (provider.LoginProcess, *model.ErrorDetail) {
		t.Fatal("StartLogin called")
		return nil, nil
	}}
	service := openJobService(t, adapter)
	defer service.Close()
	reached := make(chan struct{})
	release := make(chan struct{})
	service.Jobs().beforeStart = func(*managedJob) { close(reached); <-release }
	job, detail := service.Jobs().StartOnboard(model.ProviderCodex, "work")
	if detail != nil {
		t.Fatal(detail)
	}
	<-reached
	done := make(chan struct{})
	go func() { _, _ = service.Jobs().Cancel(job.ID); close(done) }()
	waitForJobState(t, service, job.ID, "canceling")
	close(release)
	<-done
	if adapter.startCount() != 0 {
		t.Fatalf("starts = %d", adapter.startCount())
	}
	if _, err := os.Stat(filepath.Join(service.stateDir, "providers")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("provider directory err = %v", err)
	}
}

func TestCommitAndCancelHaveOneWinner(t *testing.T) {
	newAdapter := func() *jobAdapter {
		return &jobAdapter{start: func(home string) (provider.LoginProcess, *model.ErrorDetail) {
			if err := os.WriteFile(filepath.Join(home, "auth.json"), []byte("synthetic"), 0600); err != nil {
				t.Fatal(err)
			}
			process := newControlledLogin()
			process.finish(nil)
			return process, nil
		}}
	}
	t.Run("cancel before claim", func(t *testing.T) {
		service := openJobService(t, newAdapter())
		defer service.Close()
		reached := make(chan struct{})
		release := make(chan struct{})
		service.Jobs().beforeCommit = func(*managedJob) { close(reached); <-release }
		job, detail := service.Jobs().StartOnboard(model.ProviderCodex, "work")
		if detail != nil {
			t.Fatal(detail)
		}
		<-reached
		done := make(chan model.Job, 1)
		go func() { canceled, _ := service.Jobs().Cancel(job.ID); done <- canceled }()
		waitForJobState(t, service, job.ID, "canceling", "canceled")
		close(release)
		if canceled := <-done; canceled.State != "canceled" {
			t.Fatalf("canceled = %#v", canceled)
		}
		if len(service.registry.Snapshot().Accounts) != 0 {
			t.Fatalf("registry = %#v", service.registry.Snapshot())
		}
	})
	t.Run("commit before cancel", func(t *testing.T) {
		service := openJobService(t, newAdapter())
		defer service.Close()
		reached := make(chan struct{})
		release := make(chan struct{})
		service.Jobs().afterCommit = func(*managedJob) { close(reached); <-release }
		job, detail := service.Jobs().StartOnboard(model.ProviderCodex, "work")
		if detail != nil {
			t.Fatal(detail)
		}
		<-reached
		done := make(chan model.Job, 1)
		go func() { result, _ := service.Jobs().Cancel(job.ID); done <- result }()
		close(release)
		if result := <-done; result.State != "succeeded" {
			t.Fatalf("result = %#v", result)
		}
		if len(service.registry.Snapshot().Accounts) != 1 {
			t.Fatalf("registry = %#v", service.registry.Snapshot())
		}
	})
}

func TestReOnboardCleanupFailureRetainsAndRetryReleasesLock(t *testing.T) {
	process := newControlledLogin()
	process.terminateExit = true
	adapter := &jobAdapter{start: func(home string) (provider.LoginProcess, *model.ErrorDetail) {
		if err := os.WriteFile(filepath.Join(home, "auth.json"), []byte("candidate"), 0600); err != nil {
			t.Fatal(err)
		}
		go process.devicePrompt()
		return process, nil
	}}
	service := openJobService(t, adapter)
	defer service.Close()
	originalHome := t.TempDir()
	originalPath := filepath.Join(originalHome, "auth.json")
	if err := os.WriteFile(originalPath, []byte("original"), 0600); err != nil {
		t.Fatal(err)
	}
	account, detail := service.Adopt(context.Background(), model.ProviderCodex, "work", model.StoreBinding{Kind: "file", Home: originalHome, CredentialPath: originalPath})
	if detail != nil {
		t.Fatal(detail)
	}
	job, detail := service.Jobs().StartReOnboard(account.ID)
	if detail != nil {
		t.Fatal(detail)
	}
	waitForJobState(t, service, job.ID, "awaiting_user")
	service.Jobs().removeAll = func(string) error { return errors.New("synthetic removal refusal") }
	if _, detail = service.Jobs().Cancel(job.ID); detail == nil || detail.Code != teach.CredentialCleanupPending {
		t.Fatalf("detail = %#v", detail)
	}
	job = waitForJobState(t, service, job.ID, "stop_failed")
	if accountView := service.accountView(model.RegistryAccount{ID: account.ID, Label: account.Label, Provider: account.Provider, Store: model.StoreBinding{Kind: "file", Home: originalHome, CredentialPath: originalPath}}); accountView.MutationState != model.MutationReOnboarding {
		t.Fatalf("account = %#v", accountView)
	}
	if raw, err := os.ReadFile(originalPath); err != nil || string(raw) != "original" {
		t.Fatalf("original = %q, %v", raw, err)
	}
	service.Jobs().removeAll = os.RemoveAll
	job, detail = service.Jobs().Cancel(job.ID)
	if detail != nil || job.State != "canceled" {
		t.Fatalf("job = %#v, detail = %#v", job, detail)
	}
	if accountView := service.accountView(model.RegistryAccount{ID: account.ID, Label: account.Label, Provider: account.Provider, Store: model.StoreBinding{Kind: "file", Home: originalHome, CredentialPath: originalPath}}); accountView.MutationState != model.MutationIdle {
		t.Fatalf("account = %#v", accountView)
	}
}

func TestReOnboardNoProcessCancellationCleansBeforeTerminal(t *testing.T) {
	releaseStart := make(chan struct{})
	adapter := &jobAdapter{
		barrier: releaseStart,
		start: func(string) (provider.LoginProcess, *model.ErrorDetail) {
			return nil, teach.New(teach.CLIMissing, "Synthetic start returned no process.", "onboard", nil, nil, nil)
		},
	}
	service := openJobService(t, adapter)
	defer service.Close()
	originalHome := t.TempDir()
	originalPath := filepath.Join(originalHome, "auth.json")
	if err := os.WriteFile(originalPath, []byte("original"), 0600); err != nil {
		t.Fatal(err)
	}
	account, detail := service.Adopt(context.Background(), model.ProviderCodex, "work", model.StoreBinding{Kind: "file", Home: originalHome, CredentialPath: originalPath})
	if detail != nil {
		t.Fatal(detail)
	}
	job, detail := service.Jobs().StartReOnboard(account.ID)
	if detail != nil {
		t.Fatal(detail)
	}
	waitForJobState(t, service, job.ID, "starting")
	service.Jobs().removeAll = func(string) error { return errors.New("synthetic removal refusal") }
	cancelDone := make(chan *model.ErrorDetail, 1)
	go func() { _, cancelDetail := service.Jobs().Cancel(job.ID); cancelDone <- cancelDetail }()
	waitForJobState(t, service, job.ID, "canceling")
	close(releaseStart)
	if cancelDetail := <-cancelDone; cancelDetail == nil || cancelDetail.Code != teach.CredentialCleanupPending {
		t.Fatalf("cancel detail = %#v", cancelDetail)
	}
	waitForJobState(t, service, job.ID, "stop_failed")
	service.Jobs().removeAll = os.RemoveAll
	job, detail = service.Jobs().Cancel(job.ID)
	if detail != nil || job.State != "canceled" {
		t.Fatalf("job = %#v, detail = %#v", job, detail)
	}
	if raw, err := os.ReadFile(originalPath); err != nil || string(raw) != "original" {
		t.Fatalf("original = %q, %v", raw, err)
	}
}

func TestTimeoutClaimSurvivesNoProcessCleanupRetry(t *testing.T) {
	releaseStart := make(chan struct{})
	adapter := &jobAdapter{
		barrier: releaseStart,
		start: func(string) (provider.LoginProcess, *model.ErrorDetail) {
			return nil, teach.New(teach.CLIMissing, "Synthetic start returned no process.", "onboard", nil, nil, nil)
		},
	}
	service := openJobService(t, adapter)
	defer service.Close()
	originalHome := t.TempDir()
	originalPath := filepath.Join(originalHome, "auth.json")
	if err := os.WriteFile(originalPath, []byte("original"), 0600); err != nil {
		t.Fatal(err)
	}
	account, detail := service.Adopt(context.Background(), model.ProviderCodex, "work", model.StoreBinding{Kind: "file", Home: originalHome, CredentialPath: originalPath})
	if detail != nil {
		t.Fatal(detail)
	}
	job, detail := service.Jobs().StartReOnboard(account.ID)
	if detail != nil {
		t.Fatal(detail)
	}
	waitForJobState(t, service, job.ID, "starting")
	manager := service.Jobs()
	manager.mu.RLock()
	managed := manager.jobs[job.ID]
	manager.mu.RUnlock()
	manager.removeAll = func(string) error { return errors.New("synthetic removal refusal") }
	manager.recordFailure(managed, manager.timeoutDetail(managed))
	close(releaseStart)
	waitForJobState(t, service, job.ID, "stop_failed")
	<-managed.workerDone
	manager.removeAll = os.RemoveAll
	job, detail = manager.Cancel(job.ID)
	if detail != nil || job.State != "failed" || job.Error == nil || job.Error.Code != teach.LoginTimeout {
		t.Fatalf("job = %#v, detail = %#v", job, detail)
	}
	updated := job.UpdatedAt
	again, detail := manager.Cancel(job.ID)
	if detail != nil || again.State != "failed" || again.Error == nil || again.Error.Code != teach.LoginTimeout || !again.UpdatedAt.Equal(updated) {
		t.Fatalf("again = %#v, detail = %#v", again, detail)
	}
	if raw, err := os.ReadFile(originalPath); err != nil || string(raw) != "original" {
		t.Fatalf("original = %q, %v", raw, err)
	}
}

func TestPostCommitCleanupFailureDoesNotRetryInternally(t *testing.T) {
	adapter := &jobAdapter{start: func(home string) (provider.LoginProcess, *model.ErrorDetail) {
		if err := os.WriteFile(filepath.Join(home, "auth.json"), []byte("candidate"), 0600); err != nil {
			t.Fatal(err)
		}
		process := newControlledLogin()
		process.finish(nil)
		return process, nil
	}}
	service := openJobService(t, adapter)
	defer service.Close()
	originalHome := t.TempDir()
	originalPath := filepath.Join(originalHome, "auth.json")
	if err := os.WriteFile(originalPath, []byte("original"), 0600); err != nil {
		t.Fatal(err)
	}
	account, detail := service.Adopt(context.Background(), model.ProviderCodex, "work", model.StoreBinding{Kind: "file", Home: originalHome, CredentialPath: originalPath})
	if detail != nil {
		t.Fatal(detail)
	}
	var removals atomic.Int32
	cleanupStarted := make(chan struct{})
	releaseCleanup := make(chan struct{})
	commitWaitStarted := make(chan struct{})
	service.Jobs().onCommitWait = func(*managedJob) { close(commitWaitStarted) }
	service.Jobs().removeAll = func(string) error {
		removals.Add(1)
		close(cleanupStarted)
		<-releaseCleanup
		return errors.New("synthetic removal refusal")
	}
	job, detail := service.Jobs().StartReOnboard(account.ID)
	if detail != nil {
		t.Fatal(detail)
	}
	<-cleanupStarted
	cancelDone := make(chan *model.ErrorDetail, 1)
	go func() {
		_, cancelDetail := service.Jobs().Cancel(job.ID)
		cancelDone <- cancelDetail
	}()
	<-commitWaitStarted
	close(releaseCleanup)
	select {
	case cancelDetail := <-cancelDone:
		if cancelDetail == nil || cancelDetail.Code != teach.CredentialCleanupPending {
			t.Fatalf("cancel detail = %#v", cancelDetail)
		}
	case <-time.After(time.Second):
		t.Fatal("Cancel remained blocked after committed cleanup entered stop_failed")
	}
	job = waitForJobState(t, service, job.ID, "stop_failed")
	service.Jobs().mu.RLock()
	managed := service.Jobs().jobs[job.ID]
	service.Jobs().mu.RUnlock()
	<-managed.workerDone
	if removals.Load() != 1 {
		t.Fatalf("cleanup attempts before retry = %d", removals.Load())
	}
	service.Jobs().removeAll = func(path string) error {
		removals.Add(1)
		return os.RemoveAll(path)
	}
	job, detail = service.Jobs().Cancel(job.ID)
	if detail != nil || job.State != "failed" || job.Error == nil || job.Error.Code != teach.CredentialCleanupPending || removals.Load() != 2 {
		t.Fatalf("job = %#v, detail = %#v, removals = %d", job, detail, removals.Load())
	}
	if raw, err := os.ReadFile(originalPath); err != nil || string(raw) != "candidate" {
		t.Fatalf("committed original = %q, %v", raw, err)
	}
}

func TestProcessStopFailureHoldsCapacityUntilLateExit(t *testing.T) {
	process := newControlledLogin()
	var starts atomic.Int32
	adapter := &jobAdapter{start: func(string) (provider.LoginProcess, *model.ErrorDetail) {
		if starts.Add(1) == 1 {
			go process.devicePrompt()
			return process, nil
		}
		replacement := newControlledLogin()
		replacement.terminateExit = true
		go replacement.devicePrompt()
		return replacement, nil
	}}
	service := openJobService(t, adapter)
	defer service.Close()
	service.Jobs().interruptGrace = time.Millisecond
	service.Jobs().killGrace = time.Millisecond
	job, detail := service.Jobs().StartOnboard(model.ProviderCodex, "work")
	if detail != nil {
		t.Fatal(detail)
	}
	waitForJobState(t, service, job.ID, "awaiting_user")
	if _, detail = service.Jobs().Cancel(job.ID); detail == nil || detail.Code != teach.JobProcessStopFailed {
		t.Fatalf("detail = %#v", detail)
	}
	if _, detail = service.Jobs().StartOnboard(model.ProviderCodex, "blocked"); detail == nil || detail.Code != teach.JobActive {
		t.Fatalf("active detail = %#v", detail)
	}
	process.finish(errors.New("late exit"))
	job = waitForJobState(t, service, job.ID, "canceled")
	if process.waits.Load() != 1 || process.terminates.Load() == 0 || process.kills.Load() == 0 {
		t.Fatalf("waits=%d terminates=%d kills=%d", process.waits.Load(), process.terminates.Load(), process.kills.Load())
	}
	if _, detail = service.Jobs().StartOnboard(model.ProviderCodex, "replacement"); detail != nil {
		t.Fatalf("replacement = %v", detail)
	}
}

func TestRegisteredTimeoutStopFailureRecoversOriginalClaim(t *testing.T) {
	process := newControlledLogin()
	adapter := &jobAdapter{start: func(string) (provider.LoginProcess, *model.ErrorDetail) {
		go process.devicePrompt()
		return process, nil
	}}
	service := openJobService(t, adapter)
	defer service.Close()
	manager := service.Jobs()
	manager.interruptGrace = time.Millisecond
	manager.killGrace = time.Millisecond
	job, detail := manager.StartOnboard(model.ProviderCodex, "work")
	if detail != nil {
		t.Fatal(detail)
	}
	waitForJobState(t, service, job.ID, "awaiting_user")
	manager.mu.RLock()
	managed := manager.jobs[job.ID]
	manager.mu.RUnlock()
	manager.recordFailure(managed, manager.timeoutDetail(managed))
	manager.ensureStop(managed)
	job = waitForJobState(t, service, job.ID, "stop_failed")
	if job.Error == nil || job.Error.Code != teach.JobProcessStopFailed || job.Error.State["claimed_terminal_code"] != teach.LoginTimeout {
		t.Fatalf("job = %#v", job)
	}
	if _, activeDetail := manager.StartOnboard(model.ProviderCodex, "blocked"); activeDetail == nil || activeDetail.Code != teach.JobActive {
		t.Fatalf("active detail = %#v", activeDetail)
	}
	process.finish(errors.New("late timeout exit"))
	job = waitForJobState(t, service, job.ID, "failed")
	if job.Error == nil || job.Error.Code != teach.LoginTimeout || process.waits.Load() != 1 {
		t.Fatalf("job = %#v, waits = %d", job, process.waits.Load())
	}
	updated := job.UpdatedAt
	again, detail := manager.Cancel(job.ID)
	if detail != nil || again.State != "failed" || again.Error == nil || again.Error.Code != teach.LoginTimeout || !again.UpdatedAt.Equal(updated) {
		t.Fatalf("again = %#v, detail = %#v", again, detail)
	}
}

func TestCloseJoinsRegisteredProcessWithoutSecondWait(t *testing.T) {
	process := newControlledLogin()
	adapter := &jobAdapter{start: func(string) (provider.LoginProcess, *model.ErrorDetail) {
		go process.devicePrompt()
		return process, nil
	}}
	service := openJobService(t, adapter)
	service.Jobs().interruptGrace = time.Millisecond
	service.Jobs().killGrace = time.Millisecond
	job, detail := service.Jobs().StartOnboard(model.ProviderCodex, "work")
	if detail != nil {
		t.Fatal(detail)
	}
	waitForJobState(t, service, job.ID, "awaiting_user")
	closed := make(chan struct{})
	go func() { service.Close(); close(closed) }()
	deadline := time.Now().Add(time.Second)
	for process.kills.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	select {
	case <-closed:
		t.Fatal("Close returned before the registered child exited")
	default:
	}
	process.finish(errors.New("shutdown exit"))
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("Close did not join the registered child")
	}
	if process.waits.Load() != 1 {
		t.Fatalf("Wait calls = %d", process.waits.Load())
	}
}

func TestCanceledRetentionCutoffAndStopFailedRetention(t *testing.T) {
	service := openJobService(t, &jobAdapter{start: func(string) (provider.LoginProcess, *model.ErrorDetail) { return nil, nil }})
	defer service.Close()
	manager := service.Jobs()
	updated := time.Unix(100, 0).UTC()
	canceled := newManagedJob(model.Job{ID: "job_canceled", Provider: model.ProviderCodex, State: "canceled", UpdatedAt: updated}, "")
	close(canceled.workerDone)
	stopFailed := newManagedJob(model.Job{ID: "job_stop", Provider: model.ProviderCodex, State: "stop_failed", UpdatedAt: updated}, "")
	stopFailed.lifecycle = processExited
	close(stopFailed.startDone)
	close(stopFailed.workerDone)
	manager.mu.Lock()
	manager.jobs[canceled.model.ID] = canceled
	manager.jobs[stopFailed.model.ID] = stopFailed
	manager.mu.Unlock()
	manager.evictBefore(updated)
	if _, detail := manager.Get(canceled.model.ID); detail != nil {
		t.Fatalf("exact cutoff evicted: %v", detail)
	}
	manager.evictBefore(updated.Add(time.Nanosecond))
	if _, detail := manager.Get(canceled.model.ID); detail == nil || detail.Code != teach.JobNotFound {
		t.Fatalf("canceled detail = %#v", detail)
	}
	if _, detail := manager.Get(stopFailed.model.ID); detail != nil {
		t.Fatalf("stop_failed evicted: %v", detail)
	}
}
