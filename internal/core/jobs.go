package core

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/clickety-clacks/lachesis/internal/model"
	"github.com/clickety-clacks/lachesis/internal/processcheck"
	"github.com/clickety-clacks/lachesis/internal/provider"
	"github.com/clickety-clacks/lachesis/internal/store"
	"github.com/clickety-clacks/lachesis/internal/teach"
)

var urlPattern = regexp.MustCompile(`https://[^\s]+`)

const (
	processNotStarted = "not_started"
	processStarting   = "starting"
	processRegistered = "registered"
	processExited     = "exited"
)

type terminalClaim struct {
	state  string
	detail *model.ErrorDetail
}

type managedJob struct {
	model          model.Job
	label          string
	cancel         context.CancelFunc
	process        provider.LoginProcess
	lifecycle      string
	startDone      chan struct{}
	exitDone       chan struct{}
	exitErr        error
	outputDone     chan outputResult
	claim          *terminalClaim
	commitClaim    bool
	codeSubmitted  bool
	terminalDone   chan struct{}
	workerDone     chan struct{}
	stopRunning    bool
	stopDone       chan struct{}
	cleanup        cleanupTarget
	expected       string
	accountRuntime *runtimeAccount
}

type JobManager struct {
	service        *Service
	mu             sync.RWMutex
	jobs           map[string]*managedJob
	activeProvider map[model.Provider]string
	activeAccount  map[string]string
	now            func() time.Time
	ctx            context.Context
	cancel         context.CancelFunc
	closed         bool
	interruptGrace time.Duration
	killGrace      time.Duration
	removeAll      func(string) error
	stat           func(string) (os.FileInfo, error)
	beforeStart    func(*managedJob)
	beforeVerify   func(*managedJob)
	beforeCommit   func(*managedJob)
	afterCommit    func(*managedJob)
	onCommitWait   func(*managedJob)
}

func NewJobManager(s *Service) *JobManager {
	ctx, cancel := context.WithCancel(context.Background())
	j := &JobManager{
		service: s, jobs: map[string]*managedJob{}, activeProvider: map[model.Provider]string{}, activeAccount: map[string]string{},
		now: time.Now, ctx: ctx, cancel: cancel, interruptGrace: 5 * time.Second, killGrace: 5 * time.Second,
		removeAll: os.RemoveAll, stat: os.Stat,
	}
	go j.evict()
	return j
}

func (j *JobManager) Close() {
	j.mu.Lock()
	if j.closed {
		j.mu.Unlock()
		return
	}
	j.closed = true
	jobs := make([]*managedJob, 0, len(j.jobs))
	for _, job := range j.jobs {
		jobs = append(jobs, job)
		if !terminalState(job.model.State) && job.claim == nil && !job.commitClaim {
			j.recordClaimLocked(job, &terminalClaim{state: "canceled", detail: j.canceledDetail(job)}, "canceling")
		}
		if job.cancel != nil {
			job.cancel()
		}
	}
	j.mu.Unlock()
	j.cancel()
	for _, job := range jobs {
		j.ensureStop(job)
		j.mu.RLock()
		lifecycle := job.lifecycle
		exitDone := job.exitDone
		process := job.process
		j.mu.RUnlock()
		if lifecycle == processRegistered {
			_ = process.Kill()
			<-exitDone
			j.ensureStop(job)
		}
		<-job.workerDone
	}
}

func (j *JobManager) Get(id string) (model.Job, *model.ErrorDetail) {
	j.mu.RLock()
	defer j.mu.RUnlock()
	job := j.jobs[id]
	if job == nil {
		return model.Job{}, j.jobNotFound(id)
	}
	return job.model, nil
}

func (j *JobManager) Cancel(id string) (model.Job, *model.ErrorDetail) {
	j.mu.Lock()
	job := j.jobs[id]
	if job == nil {
		j.mu.Unlock()
		return model.Job{}, j.jobNotFound(id)
	}
	if terminalState(job.model.State) {
		result := job.model
		j.mu.Unlock()
		return result, nil
	}
	if job.commitClaim {
		if j.onCommitWait != nil {
			j.onCommitWait(job)
		}
		workerDone := job.workerDone
		j.mu.Unlock()
		<-workerDone
		j.mu.RLock()
		result := job.model
		if result.State == "stop_failed" {
			detail := result.Error
			j.mu.RUnlock()
			return model.Job{}, detail
		}
		j.mu.RUnlock()
		return result, nil
	}
	if job.claim == nil {
		j.recordClaimLocked(job, &terminalClaim{state: "canceled", detail: j.canceledDetail(job)}, "canceling")
	}
	j.mu.Unlock()
	j.ensureStop(job)
	<-job.workerDone
	j.mu.RLock()
	result := job.model
	if result.State == "stop_failed" {
		detail := result.Error
		j.mu.RUnlock()
		return model.Job{}, detail
	}
	j.mu.RUnlock()
	return result, nil
}

func (j *JobManager) SubmitCode(id, code string) *model.ErrorDetail {
	if strings.TrimSpace(code) == "" || strings.ContainsAny(code, "\r\n") {
		return teach.New(teach.InvalidRequest, "Send one non-empty authorization code without a line break.", "jobs", nil, nil, nil)
	}
	j.mu.Lock()
	job := j.jobs[id]
	if job == nil {
		j.mu.Unlock()
		return j.jobNotFound(id)
	}
	active := j.activeProvider[model.ProviderClaude] == id
	if job.model.AccountID != nil {
		active = j.activeAccount[*job.model.AccountID] == id
	}
	submitter, canSubmit := job.process.(provider.CodeSubmitter)
	switch {
	case job.model.Provider != model.ProviderClaude:
		detail := j.codeSubmissionDetail(job, "provider_mismatch")
		j.mu.Unlock()
		return detail
	case job.codeSubmitted:
		detail := j.codeSubmissionDetail(job, "code_already_submitted")
		j.mu.Unlock()
		return detail
	case !active || job.model.State != "awaiting_user" || job.lifecycle != processRegistered || job.claim != nil || job.commitClaim || !canSubmit:
		detail := j.codeSubmissionDetail(job, "job_not_awaiting_code")
		j.mu.Unlock()
		return detail
	}
	// Reserve the only delivery while the exact active process and state are
	// locked, then release the manager before the external stdin write.
	job.codeSubmitted = true
	j.mu.Unlock()
	if err := submitter.SubmitCode(code); err != nil {
		j.mu.RLock()
		detail := j.codeSubmissionDetail(job, "process_did_not_accept_code")
		j.mu.RUnlock()
		return detail
	}
	return nil
}

func (j *JobManager) StartOnboard(p model.Provider, label string) (model.Job, *model.ErrorDetail) {
	if d := validateAccountInput(p, label); d != nil {
		return model.Job{}, d
	}
	j.mu.Lock()
	if id := j.activeProvider[p]; id != "" {
		j.mu.Unlock()
		return model.Job{}, j.activeDetail(id)
	}
	if j.closed {
		j.mu.Unlock()
		return model.Job{}, teach.New(teach.InvalidRequest, "The job manager is closed.", "onboard", nil, nil, nil, "restart the service")
	}
	id := "job_" + newUUID()[0:8]
	accountID := newUUID()
	now := j.now().UTC()
	job := newManagedJob(model.Job{ID: id, Kind: "onboard", Provider: p, State: "queued", CreatedAt: now, UpdatedAt: now}, label)
	ctx, cancel := context.WithTimeout(j.ctx, 20*time.Minute)
	job.cancel = cancel
	j.jobs[id] = job
	j.activeProvider[p] = id
	result := job.model
	j.mu.Unlock()
	go j.runOnboard(ctx, job, accountID, label)
	return result, nil
}

func (j *JobManager) StartReOnboard(id string) (model.Job, *model.ErrorDetail) {
	row, runtime, d := j.service.lockAccount(id)
	if d != nil {
		return model.Job{}, d
	}
	runtime.mu.Lock()
	if runtime.mutation != model.MutationIdle {
		runtime.mu.Unlock()
		runtime.op.Unlock()
		j.mu.RLock()
		active := j.activeAccount[id]
		j.mu.RUnlock()
		return model.Job{}, j.activeDetail(active)
	}
	runtime.mutation = model.MutationReOnboarding
	runtime.mu.Unlock()
	j.mu.Lock()
	if active := j.activeAccount[id]; active != "" {
		j.mu.Unlock()
		runtime.mu.Lock()
		runtime.mutation = model.MutationIdle
		runtime.mu.Unlock()
		runtime.op.Unlock()
		return model.Job{}, j.activeDetail(active)
	}
	if j.closed {
		j.mu.Unlock()
		runtime.mu.Lock()
		runtime.mutation = model.MutationIdle
		runtime.mu.Unlock()
		runtime.op.Unlock()
		return model.Job{}, teach.New(teach.InvalidRequest, "The job manager is closed.", "re-onboard", nil, nil, nil, "restart the service")
	}
	jobID := "job_" + newUUID()[0:8]
	now := j.now().UTC()
	accountID := id
	job := newManagedJob(model.Job{ID: jobID, Kind: "re_onboard", Provider: row.Provider, AccountID: &accountID, State: "queued", CreatedAt: now, UpdatedAt: now}, "")
	job.accountRuntime = runtime
	ctx, cancel := context.WithTimeout(j.ctx, 20*time.Minute)
	job.cancel = cancel
	j.jobs[jobID] = job
	j.activeAccount[id] = jobID
	result := job.model
	j.mu.Unlock()
	go j.runReOnboard(ctx, job, row, runtime)
	return result, nil
}

func newManagedJob(m model.Job, label string) *managedJob {
	return &managedJob{
		model: m, label: label, lifecycle: processNotStarted, startDone: make(chan struct{}),
		terminalDone: make(chan struct{}), workerDone: make(chan struct{}),
	}
}

func validManagedBinding(providerName model.Provider, label, home string, binding model.StoreBinding) *model.ErrorDetail {
	cleanHome := filepath.Clean(home)
	cleanCredential := filepath.Clean(binding.CredentialPath)
	if binding.Kind != "file" || filepath.Clean(binding.Home) != cleanHome || !filepath.IsAbs(cleanCredential) || filepath.Dir(cleanCredential) != cleanHome {
		return fileOnlySourceDetail(providerName, label, binding.Kind)
	}
	return nil
}

func (j *JobManager) runOnboard(ctx context.Context, job *managedJob, accountID, label string) {
	defer close(job.workerDone)
	if j.beforeStart != nil {
		j.beforeStart(job)
	}
	if !j.beginStart(job) {
		j.ensureStop(job)
		return
	}
	home := filepath.Join(j.service.stateDir, "providers", string(job.model.Provider), accountID)
	busy, err := j.service.process.Busy(ctx, processcheck.Target{Provider: job.model.Provider, Home: home})
	if err != nil || busy {
		claimed := j.finishStart(job, nil, nil, "")
		if claimed {
			j.ensureStop(job)
			return
		}
		j.failJob(job, teach.New(teach.CredentialStoreBusy, "The provider CLI is running or cannot be inspected.", "onboard", nil, map[string]any{"provider": job.model.Provider}, nil, "stop the provider CLI and retry when mutation_state is idle"))
		return
	}
	if err := os.MkdirAll(home, 0700); err != nil {
		j.finishStart(job, nil, providerHomeCleanupTarget{provider: job.model.Provider, home: home}, "")
		j.failJob(job, teach.New(teach.CredentialCleanupPending, "The managed store could not be created.", "onboard", nil, map[string]any{"managed_path": home}, nil, "retry onboarding"))
		return
	}
	adapter := j.service.adapters[job.model.Provider]
	binding := adapter.ManagedBinding(home)
	if detail := validManagedBinding(job.model.Provider, label, home, binding); detail != nil {
		j.finishStart(job, nil, providerHomeCleanupTarget{provider: job.model.Provider, home: home}, binding.CredentialPath)
		j.failJob(job, detail)
		return
	}
	proc, detail := adapter.StartLogin(ctx, home)
	claimed := j.finishStart(job, proc, providerHomeCleanupTarget{provider: job.model.Provider, home: home}, binding.CredentialPath)
	if proc == nil && errors.Is(ctx.Err(), context.DeadlineExceeded) {
		j.recordFailure(job, j.timeoutDetail(job))
		claimed = true
	}
	if claimed {
		j.ensureStop(job)
		return
	}
	if detail != nil {
		j.failJob(job, detail)
		return
	}
	if detail = j.observeLogin(ctx, job); detail != nil {
		return
	}
	if j.beforeVerify != nil {
		j.beforeVerify(job)
	}
	verificationCtx, ok := j.beginVerification(job)
	if !ok {
		j.ensureStop(job)
		return
	}
	st, err := j.service.stores(binding)
	if err != nil {
		j.failJob(job, teach.New(teach.CredentialMissing, "The managed credential cannot be opened.", "onboard", nil, nil, nil, "retry onboarding"))
		return
	}
	raw, err := st.Read(verificationCtx)
	if err != nil {
		j.failJob(job, teach.New(teach.CredentialMissing, "The provider CLI did not write a credential.", "onboard", nil, nil, nil, "retry onboarding"))
		return
	}
	cred, detail := adapter.ParseCredential(raw)
	if detail != nil {
		j.failJob(job, detail)
		return
	}
	sample, detail := adapter.Usage(verificationCtx, cred)
	if detail != nil && !degradedClaudeVerification(job.model.Provider, detail) {
		j.failJob(job, detail)
		return
	}
	if j.beforeCommit != nil {
		j.beforeCommit(job)
	}
	if !j.beginCommit(job) {
		j.ensureStop(job)
		return
	}
	if j.afterCommit != nil {
		j.afterCommit(job)
	}
	row := model.RegistryAccount{ID: accountID, Label: label, Provider: job.model.Provider, Store: binding}
	if err = j.service.registry.Add(row); err != nil {
		j.failJob(job, teach.New(teach.RegistryCommitFailed, "The onboarded account could not be committed.", "onboard", nil, nil, nil, "preserve state and retry"))
		return
	}
	runtime := &runtimeAccount{status: model.StatusReady, mutation: model.MutationIdle}
	if detail != nil {
		now := j.now().UTC()
		runtime.status = model.StatusDegraded
		runtime.checked = &now
		runtime.lastErr = detail
	}
	j.service.mu.Lock()
	j.service.state[accountID] = runtime
	j.service.mu.Unlock()
	if sample != nil {
		sample.AccountID = accountID
		sample.Label = label
		j.service.cache.Install(accountID, *sample)
	}
	j.completeSuccess(job, row, detail)
}

func (j *JobManager) runReOnboard(ctx context.Context, job *managedJob, row model.RegistryAccount, runtime *runtimeAccount) {
	defer close(job.workerDone)
	defer runtime.op.Unlock()
	if j.beforeStart != nil {
		j.beforeStart(job)
	}
	if !j.beginStart(job) {
		j.ensureStop(job)
		return
	}
	tx := filepath.Join(j.service.stateDir, "transactions", job.model.ID)
	if err := os.MkdirAll(tx, 0700); err != nil {
		j.finishStart(job, nil, transactionCleanupTarget{path: tx}, "")
		j.failJob(job, teach.New(teach.CredentialCleanupPending, "The transaction directory could not be created.", "re-onboard", nil, nil, nil, "retry re-onboarding"))
		return
	}
	busy, err := j.service.process.Busy(ctx, processcheck.Target{Provider: row.Provider, Home: row.Store.Home})
	if err != nil || busy {
		j.finishStart(job, nil, transactionCleanupTarget{path: tx}, "")
		j.failJob(job, teach.New(teach.CredentialStoreBusy, "The provider CLI is running or cannot be inspected.", "re-onboard", nil, map[string]any{"provider": row.Provider}, nil, "stop the provider CLI and retry when mutation_state is idle"))
		return
	}
	adapter := j.service.adapters[row.Provider]
	binding := adapter.ManagedBinding(tx)
	if detail := validManagedBinding(job.model.Provider, row.Label, tx, binding); detail != nil {
		j.finishStart(job, nil, transactionCleanupTarget{path: tx}, binding.CredentialPath)
		j.failJob(job, detail)
		return
	}
	proc, detail := adapter.StartLogin(ctx, tx)
	claimed := j.finishStart(job, proc, transactionCleanupTarget{path: tx}, binding.CredentialPath)
	if proc == nil && errors.Is(ctx.Err(), context.DeadlineExceeded) {
		j.recordFailure(job, j.timeoutDetail(job))
		claimed = true
	}
	if claimed {
		j.ensureStop(job)
		return
	}
	if detail != nil {
		j.failJob(job, detail)
		return
	}
	if detail = j.observeLogin(ctx, job); detail != nil {
		return
	}
	if j.beforeVerify != nil {
		j.beforeVerify(job)
	}
	verificationCtx, ok := j.beginVerification(job)
	if !ok {
		j.ensureStop(job)
		return
	}
	candidateStore, err := j.service.stores(binding)
	if err != nil {
		j.failJob(job, teach.New(teach.CredentialMissing, "The candidate store cannot be opened.", "re-onboard", nil, nil, nil, "retry re-onboarding"))
		return
	}
	candidate, err := candidateStore.Read(verificationCtx)
	if err != nil {
		j.failJob(job, teach.New(teach.CredentialMissing, "The login did not produce a credential.", "re-onboard", nil, nil, nil, "retry re-onboarding"))
		return
	}
	cred, detail := adapter.ParseCredential(candidate)
	if detail != nil {
		j.failJob(job, accountAwareDetail(row.ID, detail))
		return
	}
	if _, detail = adapter.Usage(verificationCtx, cred); detail != nil && !degradedClaudeVerification(job.model.Provider, detail) {
		j.failJob(job, accountAwareDetail(row.ID, detail))
		return
	}
	original, err := j.service.stores(row.Store)
	if err != nil {
		j.failJob(job, teach.New(teach.CredentialCommitFailed, "The original store cannot be opened.", "re-onboard", nil, nil, nil, "verify the original store"))
		return
	}
	old, readErr := original.Read(verificationCtx)
	expected := store.DigestBytes(old)
	if errors.Is(readErr, os.ErrNotExist) {
		expected = [32]byte{}
	} else if readErr != nil {
		j.failJob(job, teach.New(teach.CredentialCommitFailed, "The original store cannot be read.", "re-onboard", nil, map[string]any{"store_kind": row.Store.Kind}, []model.RemedyCall{{Method: "POST", Path: "/api/v1/accounts/" + row.ID + "/verify"}}))
		return
	}
	if j.beforeCommit != nil {
		j.beforeCommit(job)
	}
	if !j.beginCommit(job) {
		j.ensureStop(job)
		return
	}
	if j.afterCommit != nil {
		j.afterCommit(job)
	}
	if err = original.Commit(verificationCtx, expected, candidate); err != nil {
		code := teach.CredentialCommitFailed
		if errors.Is(err, store.ErrAtomicUnavailable) {
			code = teach.KeychainAtomicCommitUnavailable
		}
		j.failJob(job, teach.New(code, "The re-onboarded credential could not be committed.", "re-onboard", nil, map[string]any{"store_kind": row.Store.Kind}, nil, "preserve the original store"))
		return
	}
	j.service.cache.Clear(row.ID)
	sample, detail := j.service.fetchDirect(verificationCtx, row)
	if detail != nil && !degradedClaudeVerification(job.model.Provider, detail) {
		j.failJob(job, detail)
		return
	}
	if sample != nil {
		j.service.cache.Install(row.ID, *sample)
	}
	if err := j.removeTransactionOnce(transactionCleanupTarget{path: tx}); err != nil {
		j.recordFailure(job, teach.New(teach.CredentialCleanupPending, "The re-onboard transaction could not be removed.", "jobs", nil, map[string]any{"job_id": job.model.ID, "transaction_path": tx}, j.jobRemedyCalls(job)))
		stopDetail := j.cleanupPendingDetail(job, tx)
		j.mu.Lock()
		if !terminalState(job.model.State) {
			job.model.State = "stop_failed"
			job.model.UpdatedAt = j.now().UTC()
			clearLoginPrompt(&job.model)
			job.model.Error = stopDetail
		}
		j.mu.Unlock()
		return
	}
	j.completeSuccess(job, row, detail)
}

func degradedClaudeVerification(providerName model.Provider, detail *model.ErrorDetail) bool {
	if providerName != model.ProviderClaude || detail == nil || detail.Code != teach.UpstreamContractChanged || len(detail.Prerequisites) != 1 {
		return false
	}
	return detail.Prerequisites[0] == (model.Prerequisite{
		Code:        "VALID_RECOGNIZED_WINDOW",
		Description: "The provider response contains at least one valid recognized usage window.",
		Met:         false,
	})
}

func (j *JobManager) beginStart(job *managedJob) bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.closed || job.claim != nil || job.commitClaim || terminalState(job.model.State) {
		job.lifecycle = processExited
		close(job.startDone)
		return false
	}
	job.lifecycle = processStarting
	job.model.State = "starting"
	job.model.UpdatedAt = j.now().UTC()
	clearLoginPrompt(&job.model)
	return true
}

func (j *JobManager) finishStart(job *managedJob, proc provider.LoginProcess, cleanup cleanupTarget, expected string) bool {
	j.mu.Lock()
	job.cleanup = cleanup
	job.expected = expected
	if proc == nil {
		job.lifecycle = processExited
	} else {
		job.process = proc
		job.lifecycle = processRegistered
		job.exitDone = make(chan struct{})
		job.outputDone = make(chan outputResult, 1)
		go j.waitProcess(job, proc)
		go j.scanLoginOutput(job, proc, job.outputDone)
	}
	close(job.startDone)
	claimed := job.claim != nil
	j.mu.Unlock()
	return claimed
}

func (j *JobManager) waitProcess(job *managedJob, proc provider.LoginProcess) {
	err := proc.Wait()
	j.mu.Lock()
	job.exitErr = err
	job.lifecycle = processExited
	close(job.exitDone)
	claimed := job.claim != nil
	j.mu.Unlock()
	if claimed {
		j.ensureStop(job)
		j.mu.RLock()
		resumeAfterLateExit := job.model.State == "stop_failed" && job.model.Error != nil && job.model.Error.Code == teach.JobProcessStopFailed
		j.mu.RUnlock()
		if resumeAfterLateExit {
			j.ensureStop(job)
		}
	}
}

func (j *JobManager) observeLogin(ctx context.Context, job *managedJob) *model.ErrorDetail {
	j.mu.RLock()
	exitDone := job.exitDone
	outputDone := job.outputDone
	j.mu.RUnlock()
	select {
	case <-exitDone:
	case <-ctx.Done():
		select {
		case <-exitDone:
		default:
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				j.recordFailure(job, j.timeoutDetail(job))
			}
			j.ensureStop(job)
			return j.claimDetail(job)
		}
	}
	result := <-outputDone
	j.mu.RLock()
	claim := job.claim
	waitErr := job.exitErr
	expected := job.expected
	j.mu.RUnlock()
	if claim != nil {
		j.ensureStop(job)
		return claim.detail
	}
	credentialExists := false
	if expected != "" {
		_, err := j.stat(expected)
		credentialExists = err == nil
	}
	var detail *model.ErrorDetail
	switch {
	case waitErr == nil && credentialExists:
		return nil
	case waitErr != nil && credentialExists:
		detail = teach.New(teach.CredentialRejected, "The provider login command failed.", "onboard", nil, nil, nil, "start the onboarding call again")
	case result.expired:
		detail = j.timeoutDetail(job)
	case result.unavailable && job.model.Provider == model.ProviderCodex:
		detail = j.deviceAuthorizationUnavailableDetail(job)
	case result.found && job.model.Provider == model.ProviderCodex && waitErr == nil:
		detail = teach.New(teach.CredentialMissing, "Codex device authorization completed without writing a credential.", "onboard", nil, map[string]any{"provider": job.model.Provider}, j.jobRemedyCalls(job))
	case result.found && job.model.Provider == model.ProviderCodex:
		detail = teach.New(teach.CredentialRejected, "Codex device authorization did not complete.", "onboard", nil, map[string]any{"provider": job.model.Provider}, j.jobRemedyCalls(job))
	case result.found:
		detail = j.listenerExitedDetail(job)
	case waitErr == nil:
		detail = teach.New(teach.LoginURLUnavailable, "The provider CLI did not expose an authorization URL.", "onboard", nil, map[string]any{"provider": job.model.Provider}, nil, "upgrade the provider CLI or retry after adapter support changes")
	default:
		detail = teach.New(teach.CredentialRejected, "The provider login command failed.", "onboard", nil, nil, nil, "start the onboarding call again")
	}
	j.failJob(job, detail)
	return detail
}

func (j *JobManager) scanLoginOutput(job *managedJob, proc provider.LoginProcess, done chan<- outputResult) {
	reader := proc.Output()
	defer reader.Close()
	if job.model.Provider == model.ProviderCodex {
		done <- scanCodexDeviceOutput(reader, func(url, code string) { j.setDeviceAuthorization(job, url, code) })
		return
	}
	done <- scanBrowserLoginOutput(reader, func(url string) { j.setAuthorizationURL(job, url) })
}

func (j *JobManager) setAuthorizationURL(job *managedJob, url string) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if job.claim != nil || job.commitClaim || terminalState(job.model.State) || job.model.AuthorizationURL != nil {
		return
	}
	job.model.State = "awaiting_user"
	job.model.UpdatedAt = j.now().UTC()
	job.model.AuthorizationURL = &url
}

func (j *JobManager) setDeviceAuthorization(job *managedJob, url, code string) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if job.claim != nil || job.commitClaim || terminalState(job.model.State) || job.model.VerificationURL != nil || job.model.UserCode != nil {
		return
	}
	job.model.State = "awaiting_user"
	job.model.UpdatedAt = j.now().UTC()
	job.model.VerificationURL = &url
	job.model.UserCode = &code
}

// beginVerification replaces the deadline-limited login context in the same
// locked transition that exposes verifying, so later work cannot inherit it.
func (j *JobManager) beginVerification(job *managedJob) (context.Context, bool) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if job.claim != nil || job.commitClaim || terminalState(job.model.State) {
		return nil, false
	}
	if job.cancel != nil {
		job.cancel()
	}
	verificationCtx, cancel := context.WithCancel(j.ctx)
	job.cancel = cancel
	job.model.State = "verifying"
	job.model.UpdatedAt = j.now().UTC()
	clearLoginPrompt(&job.model)
	job.model.Error = nil
	return verificationCtx, true
}

func (j *JobManager) beginCommit(job *managedJob) bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	if job.claim != nil || terminalState(job.model.State) {
		return false
	}
	job.commitClaim = true
	job.model.State = "committing"
	job.model.UpdatedAt = j.now().UTC()
	clearLoginPrompt(&job.model)
	return true
}

func (j *JobManager) recordFailure(job *managedJob, detail *model.ErrorDetail) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if job.claim != nil || terminalState(job.model.State) {
		return
	}
	job.commitClaim = false
	j.recordClaimLocked(job, &terminalClaim{state: "failed", detail: detail}, "cleaning")
}

func (j *JobManager) failJob(job *managedJob, detail *model.ErrorDetail) {
	j.recordFailure(job, detail)
	j.ensureStop(job)
}

func (j *JobManager) recordClaimLocked(job *managedJob, claim *terminalClaim, state string) {
	job.claim = claim
	job.model.State = state
	job.model.UpdatedAt = j.now().UTC()
	clearLoginPrompt(&job.model)
	job.model.Error = claim.detail
	job.model.ResultAccount = nil
	if job.cancel != nil {
		job.cancel()
	}
}

func (j *JobManager) ensureStop(job *managedJob) {
	for {
		j.mu.Lock()
		if terminalState(job.model.State) || job.claim == nil {
			j.mu.Unlock()
			return
		}
		if job.stopRunning {
			done := job.stopDone
			j.mu.Unlock()
			<-done
			return
		}
		job.stopRunning = true
		job.stopDone = make(chan struct{})
		done := job.stopDone
		j.mu.Unlock()

		detail := j.stopAndFinalize(job)

		j.mu.Lock()
		if detail != nil && !terminalState(job.model.State) {
			job.model.State = "stop_failed"
			job.model.UpdatedAt = j.now().UTC()
			clearLoginPrompt(&job.model)
			job.model.Error = detail
		}
		job.stopRunning = false
		close(done)
		j.mu.Unlock()
		return
	}
}

func (j *JobManager) stopAndFinalize(job *managedJob) *model.ErrorDetail {
	j.mu.RLock()
	lifecycle := job.lifecycle
	startDone := job.startDone
	j.mu.RUnlock()
	if lifecycle == processNotStarted || lifecycle == processStarting {
		<-startDone
	}
	j.mu.RLock()
	lifecycle = job.lifecycle
	proc := job.process
	exitDone := job.exitDone
	j.mu.RUnlock()
	if lifecycle == processRegistered {
		_ = proc.Terminate()
		if !waitFor(exitDone, j.interruptGrace) {
			_ = proc.Kill()
			if !waitFor(exitDone, j.killGrace) {
				return j.processStopDetail(job)
			}
		}
	}
	j.mu.RLock()
	claim := job.claim
	cleanup := job.cleanup
	j.mu.RUnlock()
	if claim == nil {
		return nil
	}
	switch target := cleanup.(type) {
	case providerHomeCleanupTarget:
		j.service.preserveProviderHome(target)
	case transactionCleanupTarget:
		if err := j.removeTransactionOnce(target); err != nil {
			return j.cleanupPendingDetail(job, target.path)
		}
	}
	j.mu.Lock()
	if !terminalState(job.model.State) && job.claim != nil {
		j.finalizeClaimLocked(job)
	}
	j.mu.Unlock()
	return nil
}

func waitFor(done <-chan struct{}, duration time.Duration) bool {
	if done == nil {
		return true
	}
	select {
	case <-done:
		return true
	case <-time.After(duration):
		return false
	}
}

func (j *JobManager) removeTransactionOnce(target transactionCleanupTarget) error {
	if err := j.removeAll(target.path); err != nil {
		return err
	}
	if _, err := j.stat(target.path); !errors.Is(err, os.ErrNotExist) {
		if err == nil {
			return fmt.Errorf("path remains after removal")
		}
		return err
	}
	return nil
}

func (j *JobManager) finalizeClaimLocked(job *managedJob) {
	claim := job.claim
	job.model.State = claim.state
	job.model.UpdatedAt = j.now().UTC()
	clearLoginPrompt(&job.model)
	job.model.ResultAccount = nil
	job.model.Error = claim.detail
	if j.activeProvider[job.model.Provider] == job.model.ID {
		delete(j.activeProvider, job.model.Provider)
	}
	if job.model.AccountID != nil {
		if j.activeAccount[*job.model.AccountID] == job.model.ID {
			delete(j.activeAccount, *job.model.AccountID)
		}
		j.finishAccountMutationLocked(job, claim.detail, claim.state == "canceled")
	}
	close(job.terminalDone)
}

func (j *JobManager) completeSuccess(job *managedJob, row model.RegistryAccount, verificationDetail *model.ErrorDetail) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if job.claim != nil || terminalState(job.model.State) {
		return
	}
	job.commitClaim = false
	job.model.State = "succeeded"
	job.model.UpdatedAt = j.now().UTC()
	clearLoginPrompt(&job.model)
	job.model.Error = nil
	if j.activeProvider[job.model.Provider] == job.model.ID {
		delete(j.activeProvider, job.model.Provider)
	}
	if job.model.AccountID != nil {
		if j.activeAccount[*job.model.AccountID] == job.model.ID {
			delete(j.activeAccount, *job.model.AccountID)
		}
		j.finishAccountMutationLocked(job, verificationDetail, false)
	}
	account := j.service.accountView(row)
	job.model.ResultAccount = &account
	if job.cancel != nil {
		job.cancel()
	}
	close(job.terminalDone)
}

func (j *JobManager) finishAccountMutationLocked(job *managedJob, detail *model.ErrorDetail, canceled bool) {
	runtime := job.accountRuntime
	if runtime == nil {
		return
	}
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	runtime.mutation = model.MutationIdle
	if canceled {
		return
	}
	now := j.now().UTC()
	runtime.checked = &now
	runtime.lastErr = detail
	if detail == nil {
		runtime.status = model.StatusReady
		return
	}
	switch detail.Code {
	case teach.CredentialMissing, teach.CredentialRejected, teach.TokenScopeInsufficient, teach.RefreshRejected:
		runtime.status = model.StatusReauthRequired
	default:
		runtime.status = model.StatusDegraded
	}
}

func (j *JobManager) claimDetail(job *managedJob) *model.ErrorDetail {
	j.mu.RLock()
	defer j.mu.RUnlock()
	if job.claim != nil {
		return job.claim.detail
	}
	return job.model.Error
}

func (j *JobManager) activeDetail(id string) *model.ErrorDetail {
	return teach.New(teach.JobActive, "A login job is already active.", "onboard", nil, map[string]any{"job_id": id}, []model.RemedyCall{{Method: "GET", Path: "/api/v1/jobs/" + id}, {Method: "POST", Path: "/api/v1/jobs/" + id + "/cancel"}})
}

func (j *JobManager) jobNotFound(id string) *model.ErrorDetail {
	return teach.New(teach.JobNotFound, "The job does not exist or its retention period ended.", "onboard", nil, map[string]any{"job_id": id}, nil, "start the original onboarding call again")
}

func (j *JobManager) codeSubmissionDetail(job *managedJob, reason string) *model.ErrorDetail {
	return teach.New(teach.JobCodeNotAccepted, "The authorization code was not accepted for this job.", "jobs", nil,
		map[string]any{"job_id": job.model.ID, "provider": job.model.Provider, "job_state": job.model.State, "reason": reason},
		[]model.RemedyCall{{Method: "GET", Path: "/api/v1/jobs/" + job.model.ID}})
}

func (j *JobManager) listenerExitedDetail(job *managedJob) *model.ErrorDetail {
	return teach.New(teach.LoginListenerExited, "The provider login listener exited before writing a credential.", "onboard", nil,
		map[string]any{"provider": job.model.Provider, "job_id": job.model.ID}, j.jobRemedyCalls(job))
}

func (j *JobManager) canceledDetail(job *managedJob) *model.ErrorDetail {
	return teach.New(teach.JobCanceled, "The login job was canceled.", "jobs", nil,
		map[string]any{"provider": job.model.Provider, "job_id": job.model.ID}, j.jobRemedyCalls(job))
}

func (j *JobManager) timeoutDetail(job *managedJob) *model.ErrorDetail {
	if job.model.Provider == model.ProviderCodex {
		return teach.New(teach.LoginTimeout, "The Codex device authorization code expired before login completed.", "onboard", nil,
			map[string]any{"provider": job.model.Provider, "job_id": job.model.ID}, j.jobRemedyCalls(job))
	}
	return teach.New(teach.LoginTimeout, "The browser login exceeded its 20-minute deadline.", "onboard", nil,
		map[string]any{"provider": job.model.Provider, "job_id": job.model.ID}, j.jobRemedyCalls(job))
}

func (j *JobManager) deviceAuthorizationUnavailableDetail(job *managedJob) *model.ErrorDetail {
	return teach.New(teach.DeviceAuthorizationUnavailable, "Codex device authorization is disabled or unavailable.", "onboard",
		[]model.Prerequisite{
			{Code: "CODEX_DEVICE_AUTHORIZATION_SUPPORTED", Description: "The installed Codex CLI supports codex login --device-auth.", Met: false},
			{Code: "CODEX_DEVICE_AUTHORIZATION_ENABLED", Description: "Device code authentication is enabled in ChatGPT security settings or workspace permissions.", Met: false},
		}, map[string]any{"provider": job.model.Provider}, j.jobRemedyCalls(job),
		"enable device code authentication for the account or workspace, then retry the same onboarding call")
}

func clearLoginPrompt(job *model.Job) {
	job.AuthorizationURL = nil
	job.VerificationURL = nil
	job.UserCode = nil
}

func (j *JobManager) processStopDetail(job *managedJob) *model.ErrorDetail {
	j.mu.RLock()
	claimedCode := ""
	if job.claim != nil && job.claim.detail != nil {
		claimedCode = job.claim.detail.Code
	}
	lifecycle := job.lifecycle
	j.mu.RUnlock()
	return teach.New(teach.JobProcessStopFailed, "The login child could not be stopped and reaped.", "jobs", nil,
		map[string]any{"job_id": job.model.ID, "provider": job.model.Provider, "process_lifecycle": lifecycle, "claimed_terminal_code": claimedCode},
		[]model.RemedyCall{{Method: "GET", Path: "/api/v1/jobs/" + job.model.ID}, {Method: "POST", Path: "/api/v1/jobs/" + job.model.ID + "/cancel"}})
}

func (j *JobManager) cleanupPendingDetail(job *managedJob, path string) *model.ErrorDetail {
	j.mu.RLock()
	claimedCode := ""
	if job.claim != nil && job.claim.detail != nil {
		claimedCode = job.claim.detail.Code
	}
	j.mu.RUnlock()
	return teach.New(teach.CredentialCleanupPending, "The re-onboard transaction could not be removed.", "jobs", nil,
		map[string]any{"job_id": job.model.ID, "transaction_path": path, "claimed_terminal_code": claimedCode},
		[]model.RemedyCall{{Method: "GET", Path: "/api/v1/jobs/" + job.model.ID}, {Method: "POST", Path: "/api/v1/jobs/" + job.model.ID + "/cancel"}})
}

func (j *JobManager) jobRemedyCalls(job *managedJob) []model.RemedyCall {
	if job.model.Kind == "re_onboard" && job.model.AccountID != nil {
		return []model.RemedyCall{{Method: "POST", Path: "/api/v1/accounts/" + *job.model.AccountID + "/re-onboard"}}
	}
	return []model.RemedyCall{{Method: "POST", Path: "/api/v1/accounts", Body: map[string]any{"provider": job.model.Provider, "label": job.label}}}
}

func terminalState(state string) bool {
	return state == "succeeded" || state == "failed" || state == "canceled"
}

func (j *JobManager) evict() {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-j.ctx.Done():
			return
		case <-ticker.C:
			j.evictBefore(j.now().Add(-15 * time.Minute))
		}
	}
}

func (j *JobManager) evictBefore(cutoff time.Time) {
	j.mu.Lock()
	defer j.mu.Unlock()
	for id, job := range j.jobs {
		if terminalState(job.model.State) && job.model.UpdatedAt.Before(cutoff) {
			delete(j.jobs, id)
		}
	}
}
