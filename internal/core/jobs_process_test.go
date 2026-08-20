//go:build !windows

package core

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/clickety-clacks/lachesis/internal/model"
	"github.com/clickety-clacks/lachesis/internal/provider"
	"github.com/clickety-clacks/lachesis/internal/teach"
)

type realCommandAdapter struct {
	jobAdapter
	marker  string
	barrier <-chan struct{}
	starts  atomic.Int32
	waits   atomic.Int32
}

type countedLoginProcess struct {
	provider.LoginProcess
	waits *atomic.Int32
}

func (p *countedLoginProcess) Wait() error {
	p.waits.Add(1)
	return p.LoginProcess.Wait()
}

func (a *realCommandAdapter) StartLogin(ctx context.Context, home string) (provider.LoginProcess, *model.ErrorDetail) {
	if a.barrier != nil {
		<-a.barrier
	}
	if a.starts.Add(1) > 1 {
		if err := os.WriteFile(filepath.Join(home, "auth.json"), []byte("synthetic"), 0600); err != nil {
			return nil, teach.New(teach.CredentialCommitFailed, "Synthetic setup failed.", "onboard", nil, nil, nil)
		}
		process := newControlledLogin()
		process.finish(nil)
		return process, nil
	}
	ready := a.marker + ".ready"
	process, err := provider.StartCommand(ctx, os.Args[0], []string{"-test.run=^TestJobManagerProcessHelper$"}, append(os.Environ(),
		"GO_WANT_JOB_MANAGER_PROCESS_HELPER=1",
		"JOB_MANAGER_SIGNAL_MARKER="+a.marker,
		"JOB_MANAGER_READY_MARKER="+ready,
	))
	if err != nil {
		return nil, teach.New(teach.CLIMissing, "Synthetic helper failed to start.", "onboard", nil, nil, nil)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, err = os.Stat(ready); err == nil {
			break
		}
		if time.Now().After(deadline) {
			_ = process.Kill()
			_ = process.Wait()
			return nil, teach.New(teach.CLIMissing, "Synthetic helper did not become ready.", "onboard", nil, nil, nil)
		}
		time.Sleep(time.Millisecond)
	}
	return &countedLoginProcess{LoginProcess: process, waits: &a.waits}, nil
}

func TestCancelOwnsRealStartCommandTermination(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "signals")
	adapter := &realCommandAdapter{marker: marker}
	service := openJobService(t, adapter)
	defer service.Close()
	manager := service.Jobs()
	manager.interruptGrace = 250 * time.Millisecond
	manager.killGrace = 2 * time.Second
	job, detail := manager.StartOnboard(model.ProviderCodex, "work")
	if detail != nil {
		t.Fatal(detail)
	}
	waitForJobState(t, service, job.ID, "awaiting_user")
	manager.mu.RLock()
	managed := manager.jobs[job.ID]
	manager.mu.RUnlock()
	type result struct {
		job    model.Job
		detail *model.ErrorDetail
	}
	cancelDone := make(chan result, 1)
	go func() {
		canceled, cancelDetail := manager.Cancel(job.ID)
		cancelDone <- result{job: canceled, detail: cancelDetail}
	}()
	waitForJobState(t, service, job.ID, "canceling")
	manager.mu.RLock()
	activeDuringStop := manager.activeProvider[model.ProviderCodex]
	manager.mu.RUnlock()
	if activeDuringStop != job.ID {
		t.Fatalf("active provider released before child exit: %q", activeDuringStop)
	}
	var canceled result
	select {
	case canceled = <-cancelDone:
	case <-time.After(5 * time.Second):
		t.Fatal("Cancel did not reap the real helper process")
	}
	if canceled.detail != nil || canceled.job.State != "canceled" {
		t.Fatalf("job = %#v, detail = %#v", canceled.job, canceled.detail)
	}
	if adapter.waits.Load() != 1 {
		t.Fatalf("Wait calls = %d", adapter.waits.Load())
	}
	raw, err := os.ReadFile(marker)
	if err != nil || string(raw) != "interrupt\n" {
		t.Fatalf("signal marker = %q, %v", raw, err)
	}
	manager.mu.RLock()
	lifecycle := managed.lifecycle
	exitDone := managed.exitDone
	activeAfterStop := manager.activeProvider[model.ProviderCodex]
	manager.mu.RUnlock()
	if lifecycle != processExited || activeAfterStop != "" {
		t.Fatalf("lifecycle = %q, active provider = %q", lifecycle, activeAfterStop)
	}
	select {
	case <-exitDone:
	default:
		t.Fatal("exit observer did not publish the real child exit")
	}
}

func TestCancelDuringStartingOwnsLateRealProcess(t *testing.T) {
	releaseStart := make(chan struct{})
	marker := filepath.Join(t.TempDir(), "signals")
	adapter := &realCommandAdapter{marker: marker, barrier: releaseStart}
	service := openJobService(t, adapter)
	defer service.Close()
	manager := service.Jobs()
	manager.interruptGrace = 250 * time.Millisecond
	manager.killGrace = 2 * time.Second
	job, detail := manager.StartOnboard(model.ProviderCodex, "work")
	if detail != nil {
		t.Fatal(detail)
	}
	waitForJobState(t, service, job.ID, "starting")
	type result struct {
		job    model.Job
		detail *model.ErrorDetail
	}
	cancelDone := make(chan result, 1)
	go func() {
		canceled, cancelDetail := manager.Cancel(job.ID)
		cancelDone <- result{job: canceled, detail: cancelDetail}
	}()
	waitForJobState(t, service, job.ID, "canceling")
	close(releaseStart)
	var canceled result
	select {
	case canceled = <-cancelDone:
	case <-time.After(5 * time.Second):
		t.Fatal("Cancel did not register and reap the late helper process")
	}
	if canceled.detail != nil || canceled.job.State != "canceled" || adapter.waits.Load() != 1 {
		t.Fatalf("job = %#v, detail = %#v, waits = %d", canceled.job, canceled.detail, adapter.waits.Load())
	}
	raw, err := os.ReadFile(marker)
	if err != nil || string(raw) != "interrupt\n" {
		t.Fatalf("signal marker = %q, %v", raw, err)
	}
	manager.mu.RLock()
	active := manager.activeProvider[model.ProviderCodex]
	manager.mu.RUnlock()
	if active != "" {
		t.Fatalf("active provider retained by %q", active)
	}
	replacement, detail := manager.StartOnboard(model.ProviderCodex, "replacement")
	if detail != nil {
		t.Fatal(detail)
	}
	waitForJobState(t, service, replacement.ID, "succeeded")
}

func TestJobManagerProcessHelper(t *testing.T) {
	if os.Getenv("GO_WANT_JOB_MANAGER_PROCESS_HELPER") != "1" {
		return
	}
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGTERM)
	if err := os.WriteFile(os.Getenv("JOB_MANAGER_READY_MARKER"), []byte("ready\n"), 0600); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	fmt.Println(codexVerificationURL)
	fmt.Println("TEST-CODE")
	<-signals
	if err := os.WriteFile(os.Getenv("JOB_MANAGER_SIGNAL_MARKER"), []byte("interrupt\n"), 0600); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	select {}
}
