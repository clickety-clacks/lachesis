package core

import (
	"bufio"
	"context"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/clickety-clacks/lachesis/internal/model"
	"github.com/clickety-clacks/lachesis/internal/provider"
	"github.com/clickety-clacks/lachesis/internal/store"
	"github.com/clickety-clacks/lachesis/internal/teach"
)

var urlPattern = regexp.MustCompile(`https://[^\s]+`)

type managedJob struct {
	model   model.Job
	cancel  context.CancelFunc
	process provider.LoginProcess
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
}

func NewJobManager(s *Service) *JobManager {
	ctx, cancel := context.WithCancel(context.Background())
	j := &JobManager{service: s, jobs: map[string]*managedJob{}, activeProvider: map[model.Provider]string{}, activeAccount: map[string]string{}, now: time.Now, ctx: ctx, cancel: cancel}
	go j.evict()
	return j
}
func (j *JobManager) Close() {
	j.cancel()
	j.mu.Lock()
	jobs := make([]*managedJob, 0, len(j.jobs))
	for _, job := range j.jobs {
		jobs = append(jobs, job)
		if job.cancel != nil {
			job.cancel()
		}
	}
	j.mu.Unlock()
	for _, job := range jobs {
		if job.process == nil {
			continue
		}
		_ = job.process.Terminate()
		done := make(chan struct{})
		go func(p provider.LoginProcess) { _ = p.Wait(); close(done) }(job.process)
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			_ = job.process.Kill()
			<-done
		}
	}
}
func (j *JobManager) Get(id string) (model.Job, *model.ErrorDetail) {
	j.mu.RLock()
	defer j.mu.RUnlock()
	job := j.jobs[id]
	if job == nil {
		return model.Job{}, teach.New(teach.JobNotFound, "The job does not exist or its retention period ended.", "onboard", nil, map[string]any{"job_id": id}, nil, "start the original onboarding call again")
	}
	return job.model, nil
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
	id := "job_" + newUUID()[0:8]
	accountID := newUUID()
	now := j.now().UTC()
	job := &managedJob{model: model.Job{ID: id, Kind: "onboard", Provider: p, State: "queued", CreatedAt: now, UpdatedAt: now}}
	ctx, cancel := context.WithTimeout(j.ctx, 20*time.Minute)
	job.cancel = cancel
	j.jobs[id] = job
	j.activeProvider[p] = id
	j.mu.Unlock()
	go j.runOnboard(ctx, job, accountID, label)
	return job.model, nil
}

func (j *JobManager) StartReOnboard(id string) (model.Job, *model.ErrorDetail) {
	row, r, d := j.service.lockAccount(id)
	if d != nil {
		return model.Job{}, d
	}
	r.mu.Lock()
	if r.mutation != model.MutationIdle {
		r.mu.Unlock()
		r.op.Unlock()
		return model.Job{}, j.activeDetail(j.activeAccount[id])
	}
	r.mutation = model.MutationReOnboarding
	r.mu.Unlock()
	j.mu.Lock()
	if active := j.activeAccount[id]; active != "" {
		j.mu.Unlock()
		r.mu.Lock()
		r.mutation = model.MutationIdle
		r.mu.Unlock()
		r.op.Unlock()
		return model.Job{}, j.activeDetail(active)
	}
	jobID := "job_" + newUUID()[0:8]
	now := j.now().UTC()
	accountID := id
	job := &managedJob{model: model.Job{ID: jobID, Kind: "re_onboard", Provider: row.Provider, AccountID: &accountID, State: "queued", CreatedAt: now, UpdatedAt: now}}
	ctx, cancel := context.WithTimeout(j.ctx, 20*time.Minute)
	job.cancel = cancel
	j.jobs[jobID] = job
	j.activeAccount[id] = jobID
	j.mu.Unlock()
	go j.runReOnboard(ctx, job, row)
	return job.model, nil
}

func (j *JobManager) runOnboard(ctx context.Context, job *managedJob, accountID, label string) {
	home := filepath.Join(j.service.stateDir, "providers", string(job.model.Provider), accountID)
	if err := os.MkdirAll(home, 0700); err != nil {
		j.fail(job, teach.CredentialCleanupPending, "The managed store could not be created.", home)
		return
	}
	adapter := j.service.adapters[job.model.Provider]
	proc, d := adapter.StartLogin(ctx, home)
	if d != nil {
		j.cleanupFail(job, d, home)
		return
	}
	j.setProcess(job, proc)
	if d = j.observeLogin(ctx, job, proc); d != nil {
		j.cleanupFail(job, d, home)
		return
	}
	j.transition(job, "verifying", nil, nil)
	binding := adapter.ManagedBinding(home)
	st, err := j.service.stores(binding)
	if err != nil {
		j.cleanupFail(job, teach.New(teach.CredentialMissing, "The managed credential cannot be opened.", "onboard", nil, map[string]any{}, nil, "retry onboarding"), home)
		return
	}
	raw, err := st.Read(ctx)
	if err != nil {
		j.cleanupFail(job, teach.New(teach.CredentialMissing, "The provider CLI did not write a credential.", "onboard", nil, map[string]any{}, nil, "retry onboarding"), home)
		return
	}
	cred, d := adapter.ParseCredential(raw)
	if d != nil {
		j.cleanupFail(job, d, home)
		return
	}
	sample, d := adapter.Usage(ctx, cred)
	if d != nil {
		j.cleanupFail(job, d, home)
		return
	}
	j.transition(job, "committing", nil, nil)
	row := model.RegistryAccount{ID: accountID, Label: label, Provider: job.model.Provider, Store: binding}
	if err = j.service.registry.Add(row); err != nil {
		j.cleanupFail(job, teach.New(teach.RegistryCommitFailed, "The onboarded account could not be committed.", "onboard", nil, map[string]any{}, nil, "preserve state and retry"), home)
		return
	}
	j.service.mu.Lock()
	j.service.state[accountID] = &runtimeAccount{status: model.StatusReady, mutation: model.MutationIdle}
	j.service.mu.Unlock()
	sample.AccountID = accountID
	sample.Label = label
	j.service.cache.Install(accountID, *sample)
	account := j.service.accountView(row)
	j.succeed(job, account)
}

func (j *JobManager) runReOnboard(ctx context.Context, job *managedJob, row model.RegistryAccount) {
	j.service.mu.RLock()
	runtime := j.service.state[row.ID]
	j.service.mu.RUnlock()
	defer runtime.op.Unlock()
	tx := filepath.Join(j.service.stateDir, "transactions", job.model.ID)
	if err := os.MkdirAll(tx, 0700); err != nil {
		j.cleanupFail(job, teach.New(teach.CredentialCleanupPending, "The transaction directory could not be created.", "re-onboard", nil, map[string]any{}, nil, "retry re-onboarding"), tx)
		return
	}
	busy, err := j.service.process.Busy(ctx, row.Provider)
	if err != nil || busy {
		d := teach.New(teach.CredentialStoreBusy, "The provider CLI is running or cannot be inspected.", "re-onboard", nil, map[string]any{"provider": row.Provider}, nil, "stop the provider CLI and retry when mutation_state is idle")
		j.cleanupFail(job, d, tx)
		return
	}
	adapter := j.service.adapters[row.Provider]
	proc, d := adapter.StartLogin(ctx, tx)
	if d != nil {
		j.cleanupFail(job, d, tx)
		return
	}
	j.setProcess(job, proc)
	if d = j.observeLogin(ctx, job, proc); d != nil {
		j.cleanupFail(job, d, tx)
		return
	}
	j.transition(job, "verifying", nil, nil)
	candidateStore, err := j.service.stores(adapter.ManagedBinding(tx))
	if err != nil {
		j.cleanupFail(job, teach.New(teach.CredentialMissing, "The candidate store cannot be opened.", "re-onboard", nil, map[string]any{}, nil, "retry re-onboarding"), tx)
		return
	}
	candidate, err := candidateStore.Read(ctx)
	if err != nil {
		j.cleanupFail(job, teach.New(teach.CredentialMissing, "The login did not produce a credential.", "re-onboard", nil, map[string]any{}, nil, "retry re-onboarding"), tx)
		return
	}
	cred, d := adapter.ParseCredential(candidate)
	if d != nil {
		j.cleanupFail(job, accountAwareDetail(row.ID, d), tx)
		return
	}
	if _, d = adapter.Usage(ctx, cred); d != nil {
		j.cleanupFail(job, accountAwareDetail(row.ID, d), tx)
		return
	}
	original, err := j.service.stores(row.Store)
	if err != nil {
		j.cleanupFail(job, teach.New(teach.CredentialCommitFailed, "The original store cannot be opened.", "re-onboard", nil, map[string]any{}, nil, "verify the original store"), tx)
		return
	}
	old, readErr := original.Read(ctx)
	expected := store.DigestBytes(old)
	if errors.Is(readErr, os.ErrNotExist) {
		expected = [32]byte{}
	} else if readErr != nil {
		j.cleanupFail(job, teach.New(teach.CredentialCommitFailed, "The original store cannot be read.", "re-onboard", nil, map[string]any{"store_kind": row.Store.Kind}, []model.RemedyCall{{Method: "POST", Path: "/api/v1/accounts/" + row.ID + "/verify"}}), tx)
		return
	}
	j.transition(job, "committing", nil, nil)
	if err = original.Commit(ctx, expected, candidate); err != nil {
		code := teach.CredentialCommitFailed
		if errors.Is(err, store.ErrAtomicUnavailable) {
			code = teach.KeychainAtomicCommitUnavailable
		}
		j.cleanupFail(job, teach.New(code, "The re-onboarded credential could not be committed.", "re-onboard", nil, map[string]any{"store_kind": row.Store.Kind}, nil, "preserve the original store"), tx)
		return
	}
	j.service.cache.Clear(row.ID)
	sample, d := j.service.fetchDirect(ctx, row)
	if d != nil {
		j.finishAccountMutation(row.ID, d)
		j.cleanupFail(job, d, tx)
		return
	}
	j.service.cache.Install(row.ID, *sample)
	j.finishAccountMutation(row.ID, nil)
	if !j.removeUntilGone(j.ctx, tx) {
		return
	}
	account := j.service.accountView(row)
	j.succeed(job, account)
}

func (j *JobManager) observeLogin(ctx context.Context, job *managedJob, proc provider.LoginProcess) *model.ErrorDetail {
	reader := proc.Output()
	defer reader.Close()
	scanner := bufio.NewScanner(reader)
	found := false
	for scanner.Scan() {
		if !found {
			if u := urlPattern.FindString(scanner.Text()); u != "" {
				u = strings.TrimRight(u, ".,;)")
				found = true
				j.transition(job, "awaiting_user", &u, nil)
			}
		}
	}
	waitErr := proc.Wait()
	j.clearProcess(job)
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return teach.New(teach.LoginTimeout, "The browser login exceeded its 20-minute deadline.", "onboard", nil, map[string]any{}, nil, "start the onboarding call again")
	}
	if waitErr != nil {
		return teach.New(teach.CredentialRejected, "The provider login command failed.", "onboard", nil, map[string]any{}, nil, "start the onboarding call again")
	}
	if !found {
		return teach.New(teach.LoginURLUnavailable, "The provider CLI did not expose an authorization URL.", "onboard", nil, map[string]any{"provider": job.model.Provider}, nil, "upgrade the provider CLI or retry after adapter support changes")
	}
	return nil
}

func (j *JobManager) setProcess(job *managedJob, p provider.LoginProcess) {
	j.mu.Lock()
	job.process = p
	j.mu.Unlock()
}
func (j *JobManager) clearProcess(job *managedJob) { j.mu.Lock(); job.process = nil; j.mu.Unlock() }
func (j *JobManager) transition(job *managedJob, state string, url *string, detail *model.ErrorDetail) {
	j.mu.Lock()
	defer j.mu.Unlock()
	job.model.State = state
	job.model.UpdatedAt = j.now().UTC()
	job.model.AuthorizationURL = url
	job.model.Error = detail
}
func (j *JobManager) activeDetail(id string) *model.ErrorDetail {
	return teach.New(teach.JobActive, "A login job is already active.", "onboard", nil, map[string]any{"job_id": id}, []model.RemedyCall{{Method: "GET", Path: "/api/v1/jobs/" + id}})
}
func (j *JobManager) fail(job *managedJob, code, message, path string) {
	j.cleanupFail(job, teach.New(code, message, "onboard", nil, map[string]any{"managed_path": path}, nil, "retry onboarding"), path)
}
func (j *JobManager) cleanupFail(job *managedJob, detail *model.ErrorDetail, path string) {
	j.transition(job, "cleaning", nil, detail)
	if job.process != nil {
		_ = job.process.Terminate()
	}
	_ = j.removeUntilGone(j.ctx, path)
	j.mu.Lock()
	job.model.State = "failed"
	job.model.UpdatedAt = j.now().UTC()
	job.model.AuthorizationURL = nil
	delete(j.activeProvider, job.model.Provider)
	if job.model.AccountID != nil {
		delete(j.activeAccount, *job.model.AccountID)
	}
	j.mu.Unlock()
	if job.model.AccountID != nil {
		j.finishAccountMutation(*job.model.AccountID, detail)
	}
}
func (j *JobManager) removeUntilGone(ctx context.Context, path string) bool {
	if path == "" {
		return true
	}
	for {
		err := os.RemoveAll(path)
		if err == nil {
			if _, statErr := os.Stat(path); errors.Is(statErr, os.ErrNotExist) {
				return true
			}
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(time.Second):
		}
	}
}
func (j *JobManager) finishAccountMutation(id string, detail *model.ErrorDetail) {
	j.service.mu.RLock()
	r := j.service.state[id]
	j.service.mu.RUnlock()
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.mutation = model.MutationIdle
	now := j.now().UTC()
	if detail == nil {
		r.status = model.StatusReady
		r.checked = &now
		r.lastErr = nil
	} else {
		r.lastErr = detail
		r.checked = &now
		switch detail.Code {
		case teach.CredentialMissing, teach.CredentialRejected, teach.TokenScopeInsufficient, teach.RefreshRejected:
			r.status = model.StatusReauthRequired
		default:
			r.status = model.StatusDegraded
		}
	}
}
func (j *JobManager) succeed(job *managedJob, account model.Account) {
	j.mu.Lock()
	defer j.mu.Unlock()
	job.model.State = "succeeded"
	job.model.UpdatedAt = j.now().UTC()
	job.model.AuthorizationURL = nil
	job.model.ResultAccount = &account
	job.model.Error = nil
	delete(j.activeProvider, job.model.Provider)
	if job.model.AccountID != nil {
		delete(j.activeAccount, *job.model.AccountID)
	}
	if job.cancel != nil {
		job.cancel()
	}
}
func (j *JobManager) evict() {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-j.ctx.Done():
			return
		case <-ticker.C:
			cutoff := j.now().Add(-15 * time.Minute)
			j.mu.Lock()
			for id, job := range j.jobs {
				if (job.model.State == "succeeded" || job.model.State == "failed") && job.model.UpdatedAt.Before(cutoff) {
					delete(j.jobs, id)
				}
			}
			j.mu.Unlock()
		}
	}
}
