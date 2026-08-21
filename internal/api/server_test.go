package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/clickety-clacks/lachesis/internal/core"
	"github.com/clickety-clacks/lachesis/internal/model"
	"github.com/clickety-clacks/lachesis/internal/processcheck"
	"github.com/clickety-clacks/lachesis/internal/provider"
	"github.com/clickety-clacks/lachesis/internal/provider/claude"
	"github.com/clickety-clacks/lachesis/internal/teach"
)

type checker struct{}

func (checker) Busy(_ context.Context, _ processcheck.Target) (bool, error) { return false, nil }

type readError struct{}

func (readError) Read([]byte) (int, error) { return 0, errors.New("synthetic read failure") }
func TestEmptyUsageTeaches(t *testing.T) {
	svc, d := core.OpenService(t.TempDir(), nil, checker{})
	if d != nil {
		t.Fatal(d)
	}
	defer svc.Close()
	rr := httptest.NewRecorder()
	New(svc).Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/v1/usage", nil))
	if rr.Code != http.StatusConflict {
		t.Fatalf("status %d", rr.Code)
	}
	var env model.ErrorEnvelope
	if json.Unmarshal(rr.Body.Bytes(), &env) != nil || env.Error.Code != "NO_ACCOUNTS_ONBOARDED" || len(env.Error.Remedy.Calls) != 2 {
		t.Fatalf("body %s", rr.Body.String())
	}
}

func TestKeychainAdoptionReturnsStructuralFileOnlyRemedy(t *testing.T) {
	svc, d := core.OpenService(t.TempDir(), []provider.Adapter{claude.New(nil)}, checker{})
	if d != nil {
		t.Fatal(d)
	}
	defer svc.Close()
	body := `{"provider":"claude","label":"work","source":{"kind":"keychain","service":"legacy","account":"default"}}`
	rr := httptest.NewRecorder()
	New(svc).Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/api/v1/accounts/adopt", strings.NewReader(body)))
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status %d: %s", rr.Code, rr.Body.String())
	}
	var env model.ErrorEnvelope
	if json.Unmarshal(rr.Body.Bytes(), &env) != nil || env.Error == nil || env.Error.Code != "KEYCHAIN_SOURCE_UNSUPPORTED" {
		t.Fatalf("body %s", rr.Body.String())
	}
	if len(env.Error.Remedy.Calls) != 2 || env.Error.Remedy.Calls[0].Path != "/api/v1/accounts" || env.Error.Remedy.Calls[1].Path != "/api/v1/accounts/adopt" {
		t.Fatalf("remedy %#v", env.Error.Remedy)
	}
}

type cancelProcess struct {
	r       *io.PipeReader
	w       *io.PipeWriter
	exit    chan struct{}
	exitOne sync.Once
	codes   chan string
	submit  func(string) error
}

func newCancelProcess() *cancelProcess {
	r, w := io.Pipe()
	return &cancelProcess{r: r, w: w, exit: make(chan struct{}), codes: make(chan string, 1)}
}
func (p *cancelProcess) Output() io.ReadCloser { return p.r }
func (p *cancelProcess) Wait() error {
	<-p.exit
	_ = p.w.Close()
	return nil
}
func (p *cancelProcess) Terminate() error { p.exitOne.Do(func() { close(p.exit) }); return nil }
func (p *cancelProcess) Kill() error      { p.exitOne.Do(func() { close(p.exit) }); return nil }
func (p *cancelProcess) SubmitCode(code string) error {
	p.codes <- code
	if p.submit != nil {
		return p.submit(code)
	}
	return nil
}

type cancelAdapter struct{ process *cancelProcess }

func (*cancelAdapter) Name() model.Provider { return model.ProviderCodex }
func (*cancelAdapter) CLIAvailable() bool   { return true }
func (*cancelAdapter) DefaultBinding() (model.StoreBinding, *model.ErrorDetail) {
	return model.StoreBinding{}, nil
}
func (*cancelAdapter) ManagedBinding(home string) model.StoreBinding {
	return model.StoreBinding{Kind: "file", Home: home, CredentialPath: filepath.Join(home, "auth.json")}
}
func (*cancelAdapter) ParseCredential(raw []byte) (provider.Credential, *model.ErrorDetail) {
	return provider.Credential{Raw: raw}, nil
}
func (*cancelAdapter) Usage(context.Context, provider.Credential) (*model.UsageSample, *model.ErrorDetail) {
	return &model.UsageSample{}, nil
}
func (*cancelAdapter) Refresh(context.Context, provider.Credential) ([]byte, *model.ErrorDetail) {
	return nil, nil
}
func (a *cancelAdapter) StartLogin(_ context.Context, home string) (provider.LoginProcess, *model.ErrorDetail) {
	if err := os.WriteFile(filepath.Join(home, "auth.json"), []byte("synthetic"), 0600); err != nil {
		return nil, teach.New(teach.CredentialCommitFailed, "Synthetic setup failed.", "onboard", nil, nil, nil)
	}
	go func() { _, _ = io.WriteString(a.process.w, "https://auth.openai.com/codex/device\nTEST-CODE\n") }()
	return a.process, nil
}

type claudeCodeAdapter struct{ *cancelAdapter }

func (*claudeCodeAdapter) Name() model.Provider { return model.ProviderClaude }
func (*claudeCodeAdapter) ManagedBinding(home string) model.StoreBinding {
	return model.StoreBinding{Kind: "file", Home: home, CredentialPath: filepath.Join(home, ".credentials.json")}
}
func (a *claudeCodeAdapter) StartLogin(_ context.Context, home string) (provider.LoginProcess, *model.ErrorDetail) {
	a.process.submit = func(string) error {
		if err := os.WriteFile(filepath.Join(home, ".credentials.json"), []byte("synthetic"), 0600); err != nil {
			return err
		}
		a.process.exitOne.Do(func() { close(a.process.exit) })
		return nil
	}
	go func() { _, _ = io.WriteString(a.process.w, "open https://example.invalid/login\n") }()
	return a.process, nil
}

func TestCancelJobEndpoint(t *testing.T) {
	adapter := &cancelAdapter{process: newCancelProcess()}
	svc, detail := core.OpenService(t.TempDir(), []provider.Adapter{adapter}, checker{})
	if detail != nil {
		t.Fatal(detail)
	}
	defer svc.Close()
	job, detail := svc.Jobs().StartOnboard(model.ProviderCodex, "work")
	if detail != nil {
		t.Fatal(detail)
	}
	deadline := time.Now().Add(3 * time.Second)
	var active model.Job
	for {
		current, getDetail := svc.Jobs().Get(job.ID)
		if getDetail != nil {
			t.Fatal(getDetail)
		}
		if current.State == "awaiting_user" {
			active = current
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("job = %#v", current)
		}
		time.Sleep(time.Millisecond)
	}
	if active.AuthorizationURL != nil || active.VerificationURL == nil || *active.VerificationURL != "https://auth.openai.com/codex/device" || active.UserCode == nil || *active.UserCode != "TEST-CODE" {
		t.Fatalf("active job = %#v", active)
	}
	rr := httptest.NewRecorder()
	New(svc).Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/api/v1/jobs/"+job.ID+"/cancel", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rr.Code, rr.Body.String())
	}
	var canceled model.Job
	if json.Unmarshal(rr.Body.Bytes(), &canceled) != nil || canceled.State != "canceled" || canceled.Error == nil || canceled.Error.Code != teach.JobCanceled || canceled.VerificationURL != nil || canceled.UserCode != nil || strings.Contains(rr.Body.String(), "TEST-CODE") {
		t.Fatalf("body %s", rr.Body.String())
	}
	updated := canceled.UpdatedAt
	rr = httptest.NewRecorder()
	New(svc).Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/api/v1/jobs/"+job.ID+"/cancel", nil))
	if rr.Code != http.StatusOK || json.Unmarshal(rr.Body.Bytes(), &canceled) != nil || !canceled.UpdatedAt.Equal(updated) {
		t.Fatalf("repeat status %d: %s", rr.Code, rr.Body.String())
	}
}

func TestSubmitClaudeJobCodeEndpointCompletesOnboardingWithoutReturningCode(t *testing.T) {
	process := newCancelProcess()
	adapter := &claudeCodeAdapter{cancelAdapter: &cancelAdapter{process: process}}
	svc, detail := core.OpenService(t.TempDir(), []provider.Adapter{adapter}, checker{})
	if detail != nil {
		t.Fatal(detail)
	}
	defer svc.Close()
	handler := New(svc).Handler()
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/api/v1/accounts", strings.NewReader(`{"provider":"claude","label":"work"}`)))
	if rr.Code != http.StatusAccepted {
		t.Fatalf("start status %d: %s", rr.Code, rr.Body.String())
	}
	var job model.Job
	if err := json.Unmarshal(rr.Body.Bytes(), &job); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		rr = httptest.NewRecorder()
		handler.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/v1/jobs/"+job.ID, nil))
		if rr.Code != http.StatusOK || json.Unmarshal(rr.Body.Bytes(), &job) != nil {
			t.Fatalf("job status %d: %s", rr.Code, rr.Body.String())
		}
		if job.State == "awaiting_user" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("job = %#v", job)
		}
		time.Sleep(time.Millisecond)
	}
	const code = "SYNTHETIC-AUTHORIZATION-CODE"
	rr = httptest.NewRecorder()
	handler.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/api/v1/jobs/"+job.ID+"/code", strings.NewReader(`{"code":"`+code+`"}`)))
	if rr.Code != http.StatusNoContent || rr.Body.Len() != 0 {
		t.Fatalf("status %d: %s", rr.Code, rr.Body.String())
	}
	select {
	case got := <-process.codes:
		if got != code {
			t.Fatal("submitted code changed")
		}
	case <-time.After(time.Second):
		t.Fatal("code was not written to the login child")
	}
	for {
		rr = httptest.NewRecorder()
		handler.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/v1/jobs/"+job.ID, nil))
		if rr.Code != http.StatusOK || json.Unmarshal(rr.Body.Bytes(), &job) != nil {
			t.Fatalf("job status %d: %s", rr.Code, rr.Body.String())
		}
		if job.State == "succeeded" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("job = %#v", job)
		}
		time.Sleep(time.Millisecond)
	}
	if job.ResultAccount == nil || job.AuthorizationURL != nil || strings.Contains(rr.Body.String(), code) {
		t.Fatalf("completed body %s", rr.Body.String())
	}
	rr = httptest.NewRecorder()
	handler.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/api/v1/jobs/"+job.ID+"/code", strings.NewReader(`{"code":"`+code+`"}`)))
	if rr.Code != http.StatusConflict || strings.Contains(rr.Body.String(), code) {
		t.Fatalf("duplicate status %d: %s", rr.Code, rr.Body.String())
	}
	select {
	case <-process.codes:
		t.Fatal("duplicate code reached the login child")
	default:
	}
}

func TestCancelJobEndpointRejectsBodyAndMissingJob(t *testing.T) {
	svc, detail := core.OpenService(t.TempDir(), nil, checker{})
	if detail != nil {
		t.Fatal(detail)
	}
	defer svc.Close()
	handler := New(svc).Handler()
	for name, body := range map[string]string{
		"zero bytes":           "",
		"ASCII whitespace":     " \t\r\n",
		"non-ASCII whitespace": "\u2003\u00a0",
		"one MiB whitespace":   strings.Repeat(" ", 1<<20),
	} {
		t.Run(name, func(t *testing.T) {
			rr := httptest.NewRecorder()
			handler.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/api/v1/jobs/job_missing/cancel", strings.NewReader(body)))
			if rr.Code != http.StatusNotFound {
				t.Fatalf("body status %d: %s", rr.Code, rr.Body.String())
			}
		})
	}
	for name, body := range map[string]string{
		"one byte":                   `x`,
		"object":                     `{}`,
		"whitespace prefixed object": `  {}`,
		"oversized whitespace":       strings.Repeat(" ", (1<<20)+1),
	} {
		t.Run(name, func(t *testing.T) {
			rr := httptest.NewRecorder()
			handler.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/api/v1/jobs/job_missing/cancel", strings.NewReader(body)))
			if rr.Code != http.StatusBadRequest {
				t.Fatalf("body status %d: %s", rr.Code, rr.Body.String())
			}
		})
	}
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/api/v1/jobs/job_missing/cancel", readError{}))
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("read error status %d: %s", rr.Code, rr.Body.String())
	}
	rr = httptest.NewRecorder()
	handler.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/api/v1/jobs/job_missing/cancel", nil))
	if rr.Code != http.StatusNotFound {
		t.Fatalf("missing status %d: %s", rr.Code, rr.Body.String())
	}
}

func TestJobsHelpDescribesCancelAndHeldLocks(t *testing.T) {
	topic := topics["jobs"]
	if len(topic.Examples) != 3 || topic.Examples[1].Method != http.MethodPost || topic.Examples[1].Path != "/api/v1/jobs/{id}/code" || topic.Examples[2].Method != http.MethodPost || topic.Examples[2].Path != "/api/v1/jobs/{id}/cancel" || !strings.Contains(topic.Summary, teach.JobProcessStopFailed) || !strings.Contains(topic.Summary, teach.CredentialCleanupPending) {
		t.Fatalf("topic = %#v", topic)
	}
}

func TestOnboardHelpDescribesRemoteCodexDeviceAuthorization(t *testing.T) {
	onboard := topics["onboard"]
	jobs := topics["jobs"]
	reOnboard := topics["re-onboard"]
	for name, summary := range map[string]string{"onboard": onboard.Summary, "jobs": jobs.Summary, "re-onboard": reOnboard.Summary} {
		if !strings.Contains(summary, "verification_url") || !strings.Contains(summary, "user_code") || !strings.Contains(summary, "any device") || !strings.Contains(summary, "continue polling") || !strings.Contains(summary, "no callback") || !strings.Contains(summary, "SSH-forwarding") {
			t.Fatalf("%s summary = %q", name, summary)
		}
	}
	if !strings.Contains(onboard.Summary, "Claude keeps its browser-login flow") || !strings.Contains(reOnboard.Summary, "Claude uses browser login") {
		t.Fatalf("Claude help changed: onboard = %q, re-onboard = %q", onboard.Summary, reOnboard.Summary)
	}
	if len(onboard.Prerequisites) != 2 || onboard.Prerequisites[1].Code != "CODEX_DEVICE_AUTHORIZATION_ENABLED" {
		t.Fatalf("onboard prerequisites = %#v", onboard.Prerequisites)
	}
}
