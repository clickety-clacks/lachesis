package core

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/clickety-clacks/lachesis/internal/model"
	"github.com/clickety-clacks/lachesis/internal/processcheck"
	"github.com/clickety-clacks/lachesis/internal/provider"
	"github.com/clickety-clacks/lachesis/internal/store"
	"github.com/clickety-clacks/lachesis/internal/teach"
)

type StoreFactory func(model.StoreBinding) (store.Adapter, error)

type runtimeAccount struct {
	mu       sync.Mutex
	op       sync.Mutex
	status   model.AccountStatus
	mutation model.MutationState
	checked  *time.Time
	lastErr  *model.ErrorDetail
}

type Service struct {
	stateDir string
	registry *RegistryStore
	cache    *Cache
	adapters map[model.Provider]provider.Adapter
	stores   StoreFactory
	process  processcheck.Checker
	mu       sync.RWMutex
	state    map[string]*runtimeAccount
	jobs     *JobManager
	now      func() time.Time
	cancel   context.CancelFunc
}

func OpenService(stateDir string, adapters []provider.Adapter, checker processcheck.Checker) (*Service, *model.ErrorDetail) {
	reg, err := OpenRegistry(stateDir)
	if err != nil {
		return nil, teach.New(teach.RegistryCommitFailed, "The registry cannot be opened.", "health", nil, map[string]any{"registry_path": filepath.Join(stateDir, "accounts.json")}, nil, "fix the registry before restart")
	}
	s := &Service{stateDir: stateDir, registry: reg, cache: NewCache(), adapters: map[model.Provider]provider.Adapter{}, process: checker, state: map[string]*runtimeAccount{}, now: time.Now}
	for _, a := range adapters {
		s.adapters[a.Name()] = a
	}
	s.stores = defaultStoreFactory
	if detail := s.reconcile(); detail != nil {
		return nil, detail
	}
	for _, a := range reg.Snapshot().Accounts {
		s.state[a.ID] = &runtimeAccount{status: model.StatusUnknown, mutation: model.MutationIdle}
	}
	s.jobs = NewJobManager(s)
	return s, nil
}

func defaultStoreFactory(b model.StoreBinding) (store.Adapter, error) {
	switch b.Kind {
	case "file":
		return store.NewFile(b.Home, b.CredentialPath)
	case "keychain":
		return store.NewKeychain(b.Service, b.Account)
	default:
		return nil, fmt.Errorf("unsupported store kind %q", b.Kind)
	}
}

func (s *Service) SetStoreFactoryForTests(f StoreFactory) { s.stores = f }
func (s *Service) SetClockForTests(now func() time.Time) {
	s.now = now
	s.cache.now = now
	s.jobs.now = now
}

func (s *Service) Jobs() *JobManager { return s.jobs }

func (s *Service) ResolveSource(p model.Provider, kind, path, serviceName, account string) (model.StoreBinding, *model.ErrorDetail) {
	a := s.adapters[p]
	if a == nil {
		return model.StoreBinding{}, teach.New(teach.InvalidRequest, "The provider is unsupported.", "adopt", nil, map[string]any{"provider": p}, nil, "use codex or claude")
	}
	switch kind {
	case "default":
		return a.DefaultBinding()
	case "home":
		if !filepath.IsAbs(path) {
			return model.StoreBinding{}, teach.New(teach.InvalidRequest, "A home source requires an absolute path.", "adopt", nil, map[string]any{}, nil, "supply an absolute path")
		}
		return a.ManagedBinding(filepath.Clean(path)), nil
	case "keychain":
		if p != model.ProviderClaude || serviceName == "" || account == "" {
			return model.StoreBinding{}, teach.New(teach.InvalidRequest, "Keychain requires Claude, service, and account.", "adopt", nil, map[string]any{}, nil, "correct the source")
		}
		return model.StoreBinding{Kind: "keychain", Service: serviceName, Account: account}, nil
	default:
		return model.StoreBinding{}, teach.New(teach.InvalidRequest, "Source kind must be default, home, or keychain.", "adopt", nil, map[string]any{}, nil, "correct the source kind")
	}
}

func (s *Service) reconcile() *model.ErrorDetail {
	known := map[string]bool{}
	for _, a := range s.registry.Snapshot().Accounts {
		if a.Store.Kind == "file" && strings.HasPrefix(a.Store.Home, filepath.Join(s.stateDir, "providers")+string(os.PathSeparator)) {
			known[filepath.Clean(a.Store.Home)] = true
		}
	}
	for _, p := range []model.Provider{model.ProviderCodex, model.ProviderClaude} {
		root := filepath.Join(s.stateDir, "providers", string(p))
		entries, err := os.ReadDir(root)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return cleanupDetail(root)
		}
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			path := filepath.Join(root, e.Name())
			if !known[path] {
				if err = os.RemoveAll(path); err != nil {
					return cleanupDetail(path)
				}
			}
		}
	}
	return nil
}
func cleanupDetail(path string) *model.ErrorDetail {
	return teach.New(teach.CredentialCleanupPending, "An unregistered managed credential directory could not be removed.", "health", nil, map[string]any{"managed_path": path}, nil, "remove the managed directory, then restart")
}

func (s *Service) Start(ctx context.Context) {
	ctx, s.cancel = context.WithCancel(ctx)
	go s.scheduler(ctx)
	for _, a := range s.registry.Snapshot().Accounts {
		a := a
		go func() { _, _ = s.Verify(ctx, a.ID) }()
	}
}
func (s *Service) Close() {
	if s.cancel != nil {
		s.cancel()
	}
	s.jobs.Close()
}

func (s *Service) List() []model.Account {
	reg := s.registry.Snapshot()
	out := make([]model.Account, 0, len(reg.Accounts))
	for _, a := range reg.Accounts {
		out = append(out, s.accountView(a))
	}
	return out
}
func (s *Service) KnownIDs() []string {
	reg := s.registry.Snapshot()
	out := make([]string, len(reg.Accounts))
	for i, a := range reg.Accounts {
		out[i] = a.ID
	}
	return out
}
func (s *Service) Get(id string) (model.Account, *model.ErrorDetail) {
	a, ok := s.registry.Find(id)
	if !ok {
		return model.Account{}, teach.AccountMissing(id, s.KnownIDs())
	}
	return s.accountView(a), nil
}
func (s *Service) accountView(a model.RegistryAccount) model.Account {
	s.mu.RLock()
	r := s.state[a.ID]
	s.mu.RUnlock()
	if r == nil {
		r = &runtimeAccount{status: model.StatusUnknown, mutation: model.MutationIdle}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	base := "/api/v1/accounts/" + a.ID
	return model.Account{ID: a.ID, Label: a.Label, Provider: a.Provider, StoreKind: a.Store.Kind, Status: r.status, Working: r.status == model.StatusReady, MutationState: r.mutation, LastCheckedAt: r.checked, LastError: r.lastErr, Links: map[string]string{"verify": base + "/verify", "re_onboard": base + "/re-onboard", "usage": base + "/usage"}}
}

func (s *Service) Adopt(ctx context.Context, p model.Provider, label string, binding model.StoreBinding) (model.Account, *model.ErrorDetail) {
	if d := validateAccountInput(p, label); d != nil {
		return model.Account{}, d
	}
	if existing, ok := s.registry.FindStore(binding.CanonicalKey()); ok {
		return model.Account{}, teach.New(teach.StoreAlreadyRegistered, "The credential store is already registered.", "accounts", nil, map[string]any{"account_id": existing.ID}, []model.RemedyCall{{Method: "GET", Path: "/api/v1/accounts/" + existing.ID}})
	}
	a := s.adapters[p]
	if a == nil {
		return model.Account{}, teach.New(teach.InvalidRequest, "The provider is unsupported.", "adopt", nil, map[string]any{"provider": p}, nil, "use codex or claude")
	}
	st, err := s.stores(binding)
	if err != nil {
		return model.Account{}, teach.New(teach.InvalidRequest, "The credential store binding is invalid.", "adopt", nil, map[string]any{}, nil, "correct the source")
	}
	raw, err := st.Read(ctx)
	if err != nil {
		return model.Account{}, teach.New(teach.CredentialMissing, "The credential store cannot be read.", "adopt", nil, map[string]any{"store_kind": binding.Kind}, nil, "correct the source or onboard a new account")
	}
	cred, d := a.ParseCredential(raw)
	if d != nil {
		return model.Account{}, d
	}
	sample, d := a.Usage(ctx, cred)
	if d != nil {
		return model.Account{}, d
	}
	id := newUUID()
	row := model.RegistryAccount{ID: id, Label: strings.TrimSpace(label), Provider: p, Store: binding}
	if err = s.registry.Add(row); err != nil {
		return model.Account{}, teach.New(teach.RegistryCommitFailed, "The verified account could not be committed.", "adopt", nil, map[string]any{"registry_path": filepath.Join(s.stateDir, "accounts.json")}, nil, "preserve state and retry")
	}
	s.mu.Lock()
	s.state[id] = &runtimeAccount{status: model.StatusReady, mutation: model.MutationIdle}
	s.mu.Unlock()
	sample.AccountID = id
	sample.Label = row.Label
	s.cache.Install(id, *sample)
	now := s.now().UTC()
	s.setResult(id, model.StatusReady, &now, nil)
	return s.accountView(row), nil
}

func (s *Service) Verify(ctx context.Context, id string) (model.Account, *model.ErrorDetail) {
	row, r, d := s.lockAccount(id)
	if d != nil {
		return model.Account{}, d
	}
	defer r.op.Unlock()
	sample, d := s.fetchDirect(ctx, row)
	now := s.now().UTC()
	if d != nil {
		s.applyError(r, d, &now)
		return model.Account{}, d
	}
	s.cache.Install(id, *sample)
	r.mu.Lock()
	r.status = model.StatusReady
	r.checked = &now
	r.lastErr = nil
	r.mu.Unlock()
	return s.accountView(row), nil
}

func (s *Service) Refresh(ctx context.Context, id string) (model.Account, *model.ErrorDetail) {
	row, r, d := s.lockAccount(id)
	if d != nil {
		return model.Account{}, d
	}
	defer r.op.Unlock()
	r.mu.Lock()
	r.mutation = model.MutationRefreshing
	r.mu.Unlock()
	defer func() { r.mu.Lock(); r.mutation = model.MutationIdle; r.mu.Unlock() }()
	busy, err := s.process.Busy(ctx, row.Provider)
	if err != nil {
		d = teach.New(teach.UpstreamUnavailable, "Provider process state cannot be inspected.", "refresh", nil, map[string]any{"provider": row.Provider}, nil, "retry the exact call")
		s.applyError(r, d, nil)
		return model.Account{}, d
	}
	if busy {
		d = teach.New(teach.CredentialStoreBusy, "The provider CLI is running.", "refresh", nil, map[string]any{"provider": row.Provider, "mutation_state": "refreshing"}, nil, "stop the provider CLI and retry when mutation_state is idle")
		s.applyError(r, d, nil)
		return model.Account{}, d
	}
	st, err := s.stores(row.Store)
	if err != nil {
		return model.Account{}, s.commitError(r, err)
	}
	raw, err := st.Read(ctx)
	if err != nil {
		d = teach.New(teach.CredentialMissing, "The credential store cannot be read.", "re-onboard", nil, map[string]any{"account_id": id}, []model.RemedyCall{{Method: "POST", Path: "/api/v1/accounts/" + id + "/re-onboard"}})
		s.applyError(r, d, nil)
		return model.Account{}, d
	}
	digest := store.DigestBytes(raw)
	adapter := s.adapters[row.Provider]
	original, d := adapter.ParseCredential(raw)
	if d != nil {
		s.applyError(r, d, nil)
		return model.Account{}, d
	}
	candidate, d := adapter.Refresh(ctx, original)
	if d != nil {
		s.applyError(r, d, nil)
		return model.Account{}, d
	}
	parsed, d := adapter.ParseCredential(candidate)
	if d != nil {
		s.applyError(r, d, nil)
		return model.Account{}, d
	}
	if original.AccountID != "" && parsed.AccountID != original.AccountID {
		d = teach.New(teach.CredentialChanged, "The refreshed credential identity changed.", "refresh", nil, map[string]any{"account_id": id}, nil, "verify, then retry refresh")
		s.applyError(r, d, nil)
		return model.Account{}, d
	}
	if err = st.Commit(ctx, digest, candidate); err != nil {
		if errors.Is(err, store.ErrChanged) {
			d = teach.New(teach.CredentialChanged, "The credential store changed during refresh.", "refresh", nil, map[string]any{"account_id": id}, nil, "verify, then retry refresh")
		} else if errors.Is(err, store.ErrAtomicUnavailable) {
			d = teach.New(teach.KeychainAtomicCommitUnavailable, "Native atomic Keychain commit is unavailable.", "refresh", nil, map[string]any{"store_kind": "keychain"}, nil, "preserve Keychain state")
		} else {
			d = teach.New(teach.CredentialCommitFailed, "The credential candidate could not be committed.", "refresh", nil, map[string]any{"store_kind": row.Store.Kind}, []model.RemedyCall{{Method: "POST", Path: "/api/v1/accounts/" + id + "/verify"}})
		}
		s.applyError(r, d, nil)
		return model.Account{}, d
	}
	committed, err := st.Read(ctx)
	if err != nil {
		return model.Account{}, s.commitError(r, err)
	}
	cred, d := adapter.ParseCredential(committed)
	if d != nil {
		s.applyError(r, d, nil)
		return model.Account{}, d
	}
	sample, d := adapter.Usage(ctx, cred)
	now := s.now().UTC()
	if d != nil {
		s.cache.Clear(id)
		s.applyError(r, d, &now)
		return model.Account{}, d
	}
	sample.AccountID = id
	sample.Label = row.Label
	s.cache.Install(id, *sample)
	r.mu.Lock()
	r.status = model.StatusReady
	r.checked = &now
	r.lastErr = nil
	r.mu.Unlock()
	return s.accountView(row), nil
}

func (s *Service) Delete(id string) *model.ErrorDetail {
	row, r, d := s.lockAccount(id)
	if d != nil {
		return d
	}
	defer r.op.Unlock()
	r.mu.Lock()
	mutation := r.mutation
	r.mu.Unlock()
	if mutation != model.MutationIdle {
		return teach.New(teach.JobActive, "An account mutation is active.", "accounts", nil, map[string]any{"account_id": row.ID}, nil, "wait for the active job")
	}
	ok, err := s.registry.Remove(id)
	if err != nil {
		return teach.New(teach.RegistryCommitFailed, "The registry row could not be removed.", "accounts", nil, map[string]any{"registry_path": filepath.Join(s.stateDir, "accounts.json")}, nil, "preserve state and retry")
	}
	if !ok {
		return teach.AccountMissing(id, s.KnownIDs())
	}
	s.mu.Lock()
	delete(s.state, id)
	s.mu.Unlock()
	s.cache.Clear(id)
	return nil
}

func (s *Service) Usage(ctx context.Context, id, mode string) (model.UsageResult, *model.ErrorDetail) {
	row, ok := s.registry.Find(id)
	if !ok {
		return model.UsageResult{}, teach.AccountMissing(id, s.KnownIDs())
	}
	result := s.usageResult(ctx, row, mode)
	if result.Status == "error" {
		return model.UsageResult{}, result.Error
	}
	return result, nil
}
func (s *Service) Aggregate(ctx context.Context, mode string) (model.AggregateUsage, *model.ErrorDetail) {
	rows := s.registry.Snapshot().Accounts
	if len(rows) == 0 {
		return model.AggregateUsage{}, teach.New(
			teach.NoAccountsOnboarded,
			"Usage is unavailable because the registry has no accounts.",
			"usage",
			[]model.Prerequisite{{Code: "ACCOUNT_EXISTS", Description: "Register one account.", Met: false}},
			map[string]any{"accounts_onboarded": 0, "providers_available": s.availableProviders()},
			[]model.RemedyCall{
				{Method: "POST", Path: "/api/v1/accounts/adopt", Body: map[string]any{"provider": "codex", "label": "personal", "source": map[string]any{"kind": "default"}}},
				{Method: "POST", Path: "/api/v1/accounts", Body: map[string]any{"provider": "claude", "label": "work"}},
			},
		)
	}
	type indexed struct {
		i int
		r model.UsageResult
	}
	ch := make(chan indexed, len(rows))
	for i, row := range rows {
		go func(i int, row model.RegistryAccount) { ch <- indexed{i, s.usageResult(ctx, row, mode)} }(i, row)
	}
	out := model.AggregateUsage{GeneratedAt: s.now().UTC(), Results: make([]model.UsageResult, len(rows)), Counts: map[string]int{"live": 0, "cache": 0, "stale": 0, "error": 0}}
	for range rows {
		x := <-ch
		out.Results[x.i] = x.r
		out.Counts[x.r.Status]++
	}
	return out, nil
}

func (s *Service) usageResult(ctx context.Context, row model.RegistryAccount, mode string) model.UsageResult {
	sample, lastErr := s.cache.Peek(row.ID)
	fresh := sample != nil && sample.AgeSeconds <= 30
	if mode == "background" && fresh {
		return model.UsageResult{AccountID: row.ID, Status: "cache", Sample: sample}
	}
	if mode == "background" && sample != nil {
		go s.cache.Fetch(context.Background(), row.ID, func(ctx context.Context) (*model.UsageSample, *model.ErrorDetail) { return s.fetchAccount(ctx, row) })
		return model.UsageResult{AccountID: row.ID, Status: "stale", Sample: sample, Error: lastErr}
	}
	waitCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	got, detail, started := s.cache.Fetch(waitCtx, row.ID, func(ctx context.Context) (*model.UsageSample, *model.ErrorDetail) { return s.fetchAccount(ctx, row) })
	if got != nil && detail == nil {
		status := "cache"
		if started {
			status = "live"
		}
		return model.UsageResult{AccountID: row.ID, Status: status, Sample: got}
	}
	if sample != nil {
		return model.UsageResult{AccountID: row.ID, Status: "stale", Sample: sample, Error: detail}
	}
	return model.UsageResult{AccountID: row.ID, Status: "error", Error: detail}
}

func (s *Service) fetchDirect(ctx context.Context, row model.RegistryAccount) (*model.UsageSample, *model.ErrorDetail) {
	st, err := s.stores(row.Store)
	if err != nil {
		return nil, teach.New(teach.InvalidRequest, "The store binding is invalid.", "accounts", nil, map[string]any{}, nil, "re-onboard the account")
	}
	raw, err := st.Read(ctx)
	if err != nil {
		return nil, teach.New(teach.CredentialMissing, "The credential store cannot be read.", "re-onboard", nil, map[string]any{"account_id": row.ID}, []model.RemedyCall{{Method: "POST", Path: "/api/v1/accounts/" + row.ID + "/re-onboard"}})
	}
	cred, d := s.adapters[row.Provider].ParseCredential(raw)
	if d != nil {
		return nil, d
	}
	sample, d := s.adapters[row.Provider].Usage(ctx, cred)
	if sample != nil {
		sample.AccountID = row.ID
		sample.Label = row.Label
	}
	return sample, d
}

func (s *Service) fetchAccount(ctx context.Context, row model.RegistryAccount) (*model.UsageSample, *model.ErrorDetail) {
	s.mu.RLock()
	r := s.state[row.ID]
	s.mu.RUnlock()
	if r == nil {
		return nil, teach.AccountMissing(row.ID, s.KnownIDs())
	}
	r.op.Lock()
	defer r.op.Unlock()
	sample, detail := s.fetchDirect(ctx, row)
	now := s.now().UTC()
	if detail != nil {
		s.applyError(r, detail, &now)
		return nil, detail
	}
	r.mu.Lock()
	r.status = model.StatusReady
	r.checked = &now
	r.lastErr = nil
	r.mu.Unlock()
	return sample, nil
}

func (s *Service) lockAccount(id string) (model.RegistryAccount, *runtimeAccount, *model.ErrorDetail) {
	row, ok := s.registry.Find(id)
	if !ok {
		return model.RegistryAccount{}, nil, teach.AccountMissing(id, s.KnownIDs())
	}
	s.mu.RLock()
	r := s.state[id]
	s.mu.RUnlock()
	r.op.Lock()
	return row, r, nil
}
func (s *Service) setResult(id string, status model.AccountStatus, checked *time.Time, d *model.ErrorDetail) {
	s.mu.RLock()
	r := s.state[id]
	s.mu.RUnlock()
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.status = status
	r.checked = checked
	r.lastErr = d
}
func (s *Service) applyError(r *runtimeAccount, d *model.ErrorDetail, checked *time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lastErr = d
	r.checked = checked
	switch d.Code {
	case teach.CredentialMissing, teach.CredentialRejected, teach.TokenScopeInsufficient, teach.RefreshRejected:
		r.status = model.StatusReauthRequired
	default:
		r.status = model.StatusDegraded
	}
}
func (s *Service) commitError(r *runtimeAccount, err error) *model.ErrorDetail {
	code := teach.CredentialCommitFailed
	if errors.Is(err, store.ErrAtomicUnavailable) {
		code = teach.KeychainAtomicCommitUnavailable
	}
	d := teach.New(code, "The credential store operation failed.", "refresh", nil, map[string]any{}, nil, "verify the original store")
	s.applyError(r, d, nil)
	return d
}
func (s *Service) scheduler(ctx context.Context) {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			for _, row := range s.registry.Snapshot().Accounts {
				row := row
				go s.refreshIfDue(ctx, row)
			}
		}
	}
}
func (s *Service) refreshIfDue(ctx context.Context, row model.RegistryAccount) {
	s.mu.RLock()
	runtime := s.state[row.ID]
	s.mu.RUnlock()
	if runtime == nil {
		return
	}
	runtime.op.Lock()
	st, err := s.stores(row.Store)
	if err != nil {
		runtime.op.Unlock()
		return
	}
	raw, err := st.Read(ctx)
	if err != nil {
		runtime.op.Unlock()
		return
	}
	cred, d := s.adapters[row.Provider].ParseCredential(raw)
	runtime.op.Unlock()
	if d != nil {
		return
	}
	if cred.Expiry.IsZero() {
		s.setResult(row.ID, model.StatusDegraded, nil, teach.New(teach.CredentialExpiryUnknown, "Credential expiry cannot be derived.", "refresh", nil, map[string]any{"account_id": row.ID}, []model.RemedyCall{{Method: "POST", Path: "/api/v1/accounts/" + row.ID + "/refresh"}}))
		return
	}
	if cred.Expiry.Sub(s.now()) <= 10*time.Minute {
		_, _ = s.Refresh(ctx, row.ID)
	}
}
func (s *Service) availableProviders() []model.Provider {
	out := []model.Provider{}
	for _, p := range []model.Provider{model.ProviderCodex, model.ProviderClaude} {
		if a := s.adapters[p]; a != nil && a.CLIAvailable() {
			out = append(out, p)
		}
	}
	return out
}
func (s *Service) Health() map[string]any {
	providers := map[string]any{}
	for _, p := range []model.Provider{model.ProviderCodex, model.ProviderClaude} {
		counts := map[string]int{"total": 0, "ready": 0, "degraded": 0, "reauth_required": 0, "unknown": 0}
		for _, a := range s.List() {
			if a.Provider == p {
				counts["total"]++
				counts[string(a.Status)]++
			}
		}
		available := false
		if a := s.adapters[p]; a != nil {
			available = a.CLIAvailable()
		}
		providers[string(p)] = map[string]any{"cli_available": available, "accounts": counts}
	}
	status := "ready"
	for _, a := range s.List() {
		if a.Status != model.StatusReady {
			status = "degraded"
		}
	}
	return map[string]any{"status": status, "version": "0.1.0", "providers": providers, "links": map[string]string{"accounts": "/api/v1/accounts", "usage": "/api/v1/usage", "help": "/api/v1/help"}}
}

func validateAccountInput(p model.Provider, label string) *model.ErrorDetail {
	label = strings.TrimSpace(label)
	if !p.Valid() {
		return teach.New(teach.InvalidRequest, "Provider must be codex or claude.", "accounts", nil, map[string]any{"provider": p}, nil, "use codex or claude")
	}
	if !utf8.ValidString(label) || utf8.RuneCountInString(label) < 1 || utf8.RuneCountInString(label) > 80 {
		return teach.New(teach.InvalidRequest, "Label must contain 1 through 80 UTF-8 characters.", "accounts", nil, map[string]any{}, nil, "correct the label")
	}
	for _, r := range label {
		if unicode.IsControl(r) {
			return teach.New(teach.InvalidRequest, "Label cannot contain a control character.", "accounts", nil, map[string]any{}, nil, "correct the label")
		}
	}
	return nil
}
func newUUID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	s := hex.EncodeToString(b)
	return s[0:8] + "-" + s[8:12] + "-" + s[12:16] + "-" + s[16:20] + "-" + s[20:32]
}
