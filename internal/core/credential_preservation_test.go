package core

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/clickety-clacks/lachesis/internal/model"
	"github.com/clickety-clacks/lachesis/internal/provider"
	"github.com/clickety-clacks/lachesis/internal/teach"
)

type preservationAdapter struct {
	provider    model.Provider
	marker      string
	binding     func(string) model.StoreBinding
	start       func(string) (provider.LoginProcess, *model.ErrorDetail)
	parseDetail *model.ErrorDetail
	usageDetail *model.ErrorDetail
	mu          sync.Mutex
	homes       []string
}

func (a *preservationAdapter) Name() model.Provider { return a.provider }
func (*preservationAdapter) CLIAvailable() bool     { return true }
func (*preservationAdapter) DefaultBinding() (model.StoreBinding, *model.ErrorDetail) {
	return model.StoreBinding{}, nil
}
func (a *preservationAdapter) ManagedBinding(home string) model.StoreBinding {
	if a.binding != nil {
		return a.binding(home)
	}
	return model.StoreBinding{Kind: "file", Home: home, CredentialPath: filepath.Join(home, a.marker)}
}
func (a *preservationAdapter) StartLogin(_ context.Context, home string) (provider.LoginProcess, *model.ErrorDetail) {
	a.mu.Lock()
	a.homes = append(a.homes, home)
	a.mu.Unlock()
	return a.start(home)
}
func (a *preservationAdapter) ParseCredential(raw []byte) (provider.Credential, *model.ErrorDetail) {
	if a.parseDetail != nil {
		return provider.Credential{}, a.parseDetail
	}
	return provider.Credential{Raw: append([]byte(nil), raw...)}, nil
}
func (a *preservationAdapter) Usage(context.Context, provider.Credential) (*model.UsageSample, *model.ErrorDetail) {
	if a.usageDetail != nil {
		return nil, a.usageDetail
	}
	return &model.UsageSample{Provider: a.provider, ObservedAt: time.Unix(1, 0), Windows: []model.Window{{ID: "primary", Name: "Primary", UsedPercent: 1}}}, nil
}
func (*preservationAdapter) Refresh(context.Context, provider.Credential) ([]byte, *model.ErrorDetail) {
	return []byte("synthetic"), nil
}
func (a *preservationAdapter) lastHome() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.homes[len(a.homes)-1]
}

func testPreservationAdapter(providerName model.Provider) *preservationAdapter {
	marker := "auth.json"
	if providerName == model.ProviderClaude {
		marker = ".credentials.json"
	}
	return &preservationAdapter{provider: providerName, marker: marker}
}

func assertSkipEvent(t *testing.T, output []byte, providerName model.Provider, reason string) {
	t.Helper()
	exact := `{"event":"provider_home_cleanup_skipped","provider":"` + string(providerName) + `","reason":"` + reason + `","action":"preserved"}` + "\n"
	if string(output) != exact {
		t.Fatalf("event bytes = %q", output)
	}
	lines := bytes.Split(bytes.TrimSpace(output), []byte("\n"))
	if len(lines) != 1 {
		t.Fatalf("event lines = %q", output)
	}
	var event map[string]string
	if err := json.Unmarshal(lines[0], &event); err != nil {
		t.Fatal(err)
	}
	expected := map[string]string{
		"event":    "provider_home_cleanup_skipped",
		"provider": string(providerName),
		"reason":   reason,
		"action":   "preserved",
	}
	if len(event) != len(expected) {
		t.Fatalf("event = %#v", event)
	}
	for key, value := range expected {
		if event[key] != value {
			t.Fatalf("event[%q] = %q", key, event[key])
		}
	}
}

func TestStartupRestartPreservesProviderCredentials(t *testing.T) {
	for _, providerName := range []model.Provider{model.ProviderCodex, model.ProviderClaude} {
		t.Run(string(providerName), func(t *testing.T) {
			stateDir := t.TempDir()
			adapter := testPreservationAdapter(providerName)
			home := filepath.Join(stateDir, "providers", string(providerName), "synthetic-account")
			if err := os.MkdirAll(home, 0700); err != nil {
				t.Fatal(err)
			}
			marker := filepath.Join(home, adapter.marker)
			original := []byte("SYNTHETIC_CREDENTIAL_BYTES")
			if err := os.WriteFile(marker, original, 0600); err != nil {
				t.Fatal(err)
			}
			for restart := 0; restart < 2; restart++ {
				var events bytes.Buffer
				service, detail := openService(stateDir, []provider.Adapter{adapter}, idleChecker{}, &events, os.Lstat)
				if detail != nil {
					t.Fatal(detail)
				}
				service.Close()
				assertSkipEvent(t, events.Bytes(), providerName, "credential_present")
				got, err := os.ReadFile(marker)
				if err != nil || !bytes.Equal(got, original) {
					t.Fatalf("marker = %q, %v", got, err)
				}
				info, err := os.Stat(marker)
				if err != nil {
					t.Fatal(err)
				}
				if info.Mode().Perm() != 0600 {
					t.Fatalf("marker mode = %v", info.Mode())
				}
			}
		})
	}
}

func TestStartupPreservesAbsentSymlinkAndNonDirectoryEntries(t *testing.T) {
	stateDir := t.TempDir()
	adapter := testPreservationAdapter(model.ProviderCodex)
	root := filepath.Join(stateDir, "providers", "codex")
	home := filepath.Join(root, "empty-home")
	if err := os.MkdirAll(home, 0700); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(stateDir, "outside")
	if err := os.Mkdir(outside, 0700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "linked-home")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	nonDirectory := filepath.Join(root, "plain-entry")
	if err := os.WriteFile(nonDirectory, []byte("synthetic"), 0600); err != nil {
		t.Fatal(err)
	}
	var events bytes.Buffer
	service, detail := openService(stateDir, []provider.Adapter{adapter}, idleChecker{}, &events, os.Lstat)
	if detail != nil {
		t.Fatal(detail)
	}
	service.Close()
	if events.Len() != 0 {
		t.Fatalf("events = %q", events.Bytes())
	}
	for _, path := range []string{home, link, nonDirectory} {
		if _, err := os.Lstat(path); err != nil {
			t.Fatalf("preserved entry %q: %v", filepath.Base(path), err)
		}
	}
}

func TestCleanupClassificationFailsClosed(t *testing.T) {
	for _, classificationError := range []error{
		os.ErrPermission,
		&os.PathError{Op: "lstat", Path: "PRIVATE_PATH_SENTINEL", Err: syscall.EIO},
	} {
		t.Run(classificationError.Error(), func(t *testing.T) {
			stateDir := t.TempDir()
			adapter := testPreservationAdapter(model.ProviderCodex)
			home := filepath.Join(stateDir, "providers", "codex", "synthetic-account")
			if err := os.MkdirAll(home, 0700); err != nil {
				t.Fatal(err)
			}
			marker := filepath.Join(home, adapter.marker)
			var events bytes.Buffer
			service, detail := openService(stateDir, []provider.Adapter{adapter}, idleChecker{}, &events, func(path string) (os.FileInfo, error) {
				if path == marker {
					return nil, classificationError
				}
				return os.Lstat(path)
			})
			if detail != nil {
				t.Fatal(detail)
			}
			service.Close()
			assertSkipEvent(t, events.Bytes(), model.ProviderCodex, "credential_status_unknown")
			if bytes.Contains(events.Bytes(), []byte(classificationError.Error())) || bytes.Contains(events.Bytes(), []byte(home)) {
				t.Fatalf("private value in event = %q", events.Bytes())
			}
		})
	}
}

func TestCleanupRejectsUnsafeHomesAndBindingsWithoutMarkerLookup(t *testing.T) {
	stateDir := t.TempDir()
	root := filepath.Join(stateDir, "providers", "codex")
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	realHome := filepath.Join(root, "real-home")
	if err := os.Mkdir(realHome, 0700); err != nil {
		t.Fatal(err)
	}
	linkedHome := filepath.Join(root, "linked-home")
	if err := os.Symlink(realHome, linkedHome); err != nil {
		t.Fatal(err)
	}
	outsideHome := filepath.Join(stateDir, "outside-home")
	if err := os.Mkdir(outsideHome, 0700); err != nil {
		t.Fatal(err)
	}
	adapter := testPreservationAdapter(model.ProviderCodex)
	adapter.binding = func(home string) model.StoreBinding {
		return model.StoreBinding{Kind: "file", Home: home, CredentialPath: filepath.Join(filepath.Dir(home), "outside-marker")}
	}
	service, detail := openService(stateDir, []provider.Adapter{adapter}, idleChecker{}, io.Discard, os.Lstat)
	if detail != nil {
		t.Fatal(detail)
	}
	defer service.Close()
	var lookups atomic.Int32
	service.lstat = func(path string) (os.FileInfo, error) {
		lookups.Add(1)
		return os.Lstat(path)
	}
	if state := service.providerHomeMarkerState(providerHomeCleanupTarget{provider: model.ProviderCodex, home: outsideHome}); state != markerUnknown || lookups.Load() != 0 {
		t.Fatalf("outside state = %q, lookups = %d", state, lookups.Load())
	}
	if state := service.providerHomeMarkerState(providerHomeCleanupTarget{provider: model.ProviderCodex, home: linkedHome}); state != markerUnknown || lookups.Load() != 1 {
		t.Fatalf("link state = %q, lookups = %d", state, lookups.Load())
	}
	if state := service.providerHomeMarkerState(providerHomeCleanupTarget{provider: model.ProviderCodex, home: realHome}); state != markerUnknown || lookups.Load() != 2 {
		t.Fatalf("binding state = %q, lookups = %d", state, lookups.Load())
	}
}

func TestMarkerSymlinkIsPresentWithoutFollowingTarget(t *testing.T) {
	stateDir := t.TempDir()
	adapter := testPreservationAdapter(model.ProviderCodex)
	home := filepath.Join(stateDir, "providers", "codex", "synthetic-account")
	if err := os.MkdirAll(home, 0700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(stateDir, "PRIVATE_TARGET_SENTINEL")
	if err := os.WriteFile(target, []byte("unchanged"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(home, adapter.marker)); err != nil {
		t.Fatal(err)
	}
	var events bytes.Buffer
	service, detail := openService(stateDir, []provider.Adapter{adapter}, idleChecker{}, &events, os.Lstat)
	if detail != nil {
		t.Fatal(detail)
	}
	service.Close()
	assertSkipEvent(t, events.Bytes(), model.ProviderCodex, "credential_present")
	got, err := os.ReadFile(target)
	if err != nil || string(got) != "unchanged" {
		t.Fatalf("target = %q, %v", got, err)
	}
}

func TestFailedOnboardingPreservesProviderHomes(t *testing.T) {
	tests := []struct {
		name        string
		provider    model.Provider
		writeMarker bool
		parseDetail *model.ErrorDetail
		usageDetail *model.ErrorDetail
		exitErr     error
		reason      string
	}{
		{name: "codex absent after nonzero exit", provider: model.ProviderCodex, exitErr: errors.New("synthetic exit")},
		{name: "codex present after nonzero exit", provider: model.ProviderCodex, writeMarker: true, exitErr: errors.New("synthetic exit"), reason: "credential_present"},
		{name: "claude present after parse rejection", provider: model.ProviderClaude, writeMarker: true, parseDetail: teach.New(teach.CredentialRejected, "Synthetic parse rejection.", "usage", nil, nil, nil), reason: "credential_present"},
		{name: "codex present after usage contract change", provider: model.ProviderCodex, writeMarker: true, usageDetail: teach.New(teach.UpstreamContractChanged, "Synthetic upstream change.", "usage", nil, nil, nil), reason: "credential_present"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			adapter := testPreservationAdapter(tt.provider)
			adapter.parseDetail = tt.parseDetail
			adapter.usageDetail = tt.usageDetail
			adapter.start = func(home string) (provider.LoginProcess, *model.ErrorDetail) {
				if tt.writeMarker {
					if err := os.WriteFile(filepath.Join(home, adapter.marker), []byte("SYNTHETIC_CREDENTIAL_SENTINEL"), 0600); err != nil {
						t.Fatal(err)
					}
				}
				process := newControlledLogin()
				process.finish(tt.exitErr)
				return process, nil
			}
			var events bytes.Buffer
			service, detail := openService(t.TempDir(), []provider.Adapter{adapter}, idleChecker{}, &events, os.Lstat)
			if detail != nil {
				t.Fatal(detail)
			}
			defer service.Close()
			service.Jobs().removeAll = func(string) error {
				t.Fatal("provider home reached recursive removal")
				return nil
			}
			job, detail := service.Jobs().StartOnboard(tt.provider, "PRIVATE_LABEL_SENTINEL")
			if detail != nil {
				t.Fatal(detail)
			}
			job = waitForJobState(t, service, job.ID, "failed")
			if job.Error == nil {
				t.Fatalf("job = %#v", job)
			}
			home := adapter.lastHome()
			if info, err := os.Lstat(home); err != nil || !info.IsDir() {
				t.Fatalf("home = %v, %v", info, err)
			}
			if tt.writeMarker {
				if _, err := os.Lstat(filepath.Join(home, adapter.marker)); err != nil {
					t.Fatal(err)
				}
				assertSkipEvent(t, events.Bytes(), tt.provider, tt.reason)
			} else if events.Len() != 0 {
				t.Fatalf("events = %q", events.Bytes())
			}
			if strings.Contains(events.String(), "PRIVATE_") || strings.Contains(events.String(), home) {
				t.Fatalf("private value in event = %q", events.Bytes())
			}
			if _, detail = service.Jobs().StartOnboard(tt.provider, "replacement"); detail != nil {
				t.Fatalf("replacement = %v", detail)
			}
		})
	}
}

func TestFailedOnboardingUnknownMarkerStatePreservesHome(t *testing.T) {
	process := newControlledLogin()
	adapter := testPreservationAdapter(model.ProviderCodex)
	adapter.start = func(string) (provider.LoginProcess, *model.ErrorDetail) {
		go process.devicePrompt()
		return process, nil
	}
	var events bytes.Buffer
	service, detail := openService(t.TempDir(), []provider.Adapter{adapter}, idleChecker{}, &events, os.Lstat)
	if detail != nil {
		t.Fatal(detail)
	}
	defer service.Close()
	job, detail := service.Jobs().StartOnboard(model.ProviderCodex, "synthetic")
	if detail != nil {
		t.Fatal(detail)
	}
	waitForJobState(t, service, job.ID, "awaiting_user")
	home := adapter.lastHome()
	service.lstat = func(path string) (os.FileInfo, error) {
		if path == filepath.Join(home, adapter.marker) {
			return nil, os.ErrPermission
		}
		return os.Lstat(path)
	}
	process.finish(errors.New("synthetic exit"))
	job = waitForJobState(t, service, job.ID, "failed")
	if job.Error == nil {
		t.Fatalf("job = %#v", job)
	}
	if _, err := os.Lstat(home); err != nil {
		t.Fatal(err)
	}
	assertSkipEvent(t, events.Bytes(), model.ProviderCodex, "credential_status_unknown")
}

func TestInvalidBindingFailurePreservesHomeWithoutStartingProvider(t *testing.T) {
	adapter := testPreservationAdapter(model.ProviderClaude)
	adapter.binding = func(home string) model.StoreBinding {
		return model.StoreBinding{Kind: "file", Home: home, CredentialPath: filepath.Join(filepath.Dir(home), "outside-marker")}
	}
	adapter.start = func(string) (provider.LoginProcess, *model.ErrorDetail) {
		t.Fatal("provider start called for invalid binding")
		return nil, nil
	}
	var events bytes.Buffer
	service, detail := openService(t.TempDir(), []provider.Adapter{adapter}, idleChecker{}, &events, os.Lstat)
	if detail != nil {
		t.Fatal(detail)
	}
	defer service.Close()
	job, detail := service.Jobs().StartOnboard(model.ProviderClaude, "synthetic")
	if detail != nil {
		t.Fatal(detail)
	}
	job = waitForJobState(t, service, job.ID, "failed")
	if job.Error == nil || job.Error.Code != teach.KeychainSourceUnsupported {
		t.Fatalf("job = %#v", job)
	}
	root := filepath.Join(service.stateDir, "providers", "claude")
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 1 || !entries[0].IsDir() {
		t.Fatalf("provider homes = %#v, %v", entries, err)
	}
	assertSkipEvent(t, events.Bytes(), model.ProviderClaude, "credential_status_unknown")
}

func TestCancellationPreservesEachMarkerState(t *testing.T) {
	for _, providerName := range []model.Provider{model.ProviderCodex, model.ProviderClaude} {
		for _, state := range []markerState{markerPresent, markerAbsent, markerUnknown} {
			t.Run(string(providerName)+"/"+string(state), func(t *testing.T) {
				process := newControlledLogin()
				process.terminateExit = true
				adapter := testPreservationAdapter(providerName)
				adapter.start = func(home string) (provider.LoginProcess, *model.ErrorDetail) {
					if state == markerPresent {
						if err := os.WriteFile(filepath.Join(home, adapter.marker), []byte("synthetic"), 0600); err != nil {
							t.Fatal(err)
						}
					}
					go func() {
						if providerName == model.ProviderCodex {
							process.devicePrompt()
							return
						}
						process.line("open https://example.invalid/login")
					}()
					return process, nil
				}
				var events bytes.Buffer
				service, detail := openService(t.TempDir(), []provider.Adapter{adapter}, idleChecker{}, &events, os.Lstat)
				if detail != nil {
					t.Fatal(detail)
				}
				defer service.Close()
				service.Jobs().removeAll = func(string) error {
					t.Fatal("provider home reached recursive removal")
					return nil
				}
				job, detail := service.Jobs().StartOnboard(providerName, "synthetic")
				if detail != nil {
					t.Fatal(detail)
				}
				waitForJobState(t, service, job.ID, "awaiting_user")
				home := adapter.lastHome()
				if state == markerUnknown {
					service.lstat = func(path string) (os.FileInfo, error) {
						if path == filepath.Join(home, adapter.marker) {
							return nil, errors.New("PRIVATE_LSTAT_SENTINEL")
						}
						return os.Lstat(path)
					}
				}
				job, detail = service.Jobs().Cancel(job.ID)
				if detail != nil || job.State != "canceled" {
					t.Fatalf("job = %#v, detail = %#v", job, detail)
				}
				if _, err := os.Lstat(home); err != nil {
					t.Fatal(err)
				}
				switch state {
				case markerPresent:
					assertSkipEvent(t, events.Bytes(), providerName, "credential_present")
				case markerUnknown:
					assertSkipEvent(t, events.Bytes(), providerName, "credential_status_unknown")
				case markerAbsent:
					if events.Len() != 0 {
						t.Fatalf("events = %q", events.Bytes())
					}
				}
			})
		}
	}
}

func TestFailureCleanupOrdersExitClassificationTerminalAndIndexRelease(t *testing.T) {
	process := newControlledLogin()
	adapter := testPreservationAdapter(model.ProviderCodex)
	adapter.start = func(string) (provider.LoginProcess, *model.ErrorDetail) {
		go process.devicePrompt()
		return process, nil
	}
	service, detail := openService(t.TempDir(), []provider.Adapter{adapter}, idleChecker{}, io.Discard, os.Lstat)
	if detail != nil {
		t.Fatal(detail)
	}
	defer service.Close()
	job, detail := service.Jobs().StartOnboard(model.ProviderCodex, "synthetic")
	if detail != nil {
		t.Fatal(detail)
	}
	waitForJobState(t, service, job.ID, "awaiting_user")
	manager := service.Jobs()
	manager.mu.RLock()
	managed := manager.jobs[job.ID]
	manager.mu.RUnlock()
	classified := make(chan struct{})
	releaseClassification := make(chan struct{})
	service.lstat = func(path string) (os.FileInfo, error) {
		if path == filepath.Join(adapter.lastHome(), adapter.marker) {
			manager.mu.RLock()
			lifecycle := managed.lifecycle
			state := managed.model.State
			active := manager.activeProvider[model.ProviderCodex]
			manager.mu.RUnlock()
			if lifecycle != processExited || state != "cleaning" || active != job.ID {
				t.Errorf("classification order: lifecycle=%q state=%q active=%q", lifecycle, state, active)
			}
			close(classified)
			<-releaseClassification
		}
		return os.Lstat(path)
	}
	manager.recordFailure(managed, teach.New(teach.CredentialRejected, "Synthetic failure.", "onboard", nil, nil, nil))
	process.finish(errors.New("synthetic exit"))
	<-classified
	if _, blocked := manager.StartOnboard(model.ProviderCodex, "blocked"); blocked == nil || blocked.Code != teach.JobActive {
		t.Fatalf("blocked detail = %#v", blocked)
	}
	close(releaseClassification)
	job = waitForJobState(t, service, job.ID, "failed")
	if _, replacement := manager.StartOnboard(model.ProviderCodex, "replacement"); replacement != nil {
		t.Fatalf("replacement = %#v", replacement)
	}
}

func TestRecursiveRemovalIsTransactionOnly(t *testing.T) {
	service := openJobService(t, &jobAdapter{start: func(string) (provider.LoginProcess, *model.ErrorDetail) { return nil, nil }})
	defer service.Close()
	transaction := filepath.Join(service.stateDir, "transactions", "job_synthetic")
	if err := os.MkdirAll(transaction, 0700); err != nil {
		t.Fatal(err)
	}
	var removed string
	service.Jobs().removeAll = func(path string) error {
		removed = path
		return os.RemoveAll(path)
	}
	if err := service.Jobs().removeTransactionOnce(transactionCleanupTarget{path: transaction}); err != nil {
		t.Fatal(err)
	}
	if removed != transaction {
		t.Fatalf("removed = %q", removed)
	}
	if _, err := os.Lstat(transaction); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("transaction remains: %v", err)
	}
}
