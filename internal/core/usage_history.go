package core

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/clickety-clacks/lachesis/internal/model"
)

const (
	usageHistoryVersion       = 1
	usageHistoryRetention     = 7 * 24 * time.Hour
	usageHistoryMaxPerAccount = 2048
	usageHistoryBucket        = 5 * time.Minute
)

var errUsageHistoryUnchanged = errors.New("usage history observation already recorded")

// UsageHistoryStore keeps only the provider-neutral values needed for a
// forecast. It deliberately never receives UsageSample.Raw.
type UsageHistoryStore struct {
	mu   sync.RWMutex
	path string
	data usageHistoryDocument
	now  func() time.Time
}

type usageHistoryDocument struct {
	Version  int                            `json:"version"`
	Accounts map[string]usageHistoryAccount `json:"accounts"`
}

type usageHistoryAccount struct {
	Provider model.Provider             `json:"provider"`
	Samples  []model.UsageHistorySample `json:"samples"`
}

func openUsageHistory(stateDir string) (*UsageHistoryStore, error) {
	path := filepath.Join(stateDir, "usage-history.json")
	h := &UsageHistoryStore{
		path: path,
		data: usageHistoryDocument{Version: usageHistoryVersion, Accounts: map[string]usageHistoryAccount{}},
		now:  time.Now,
	}
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return h, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, &h.data); err != nil {
		return nil, fmt.Errorf("malformed usage history: %w", err)
	}
	if h.data.Version != usageHistoryVersion || h.data.Accounts == nil {
		return nil, errors.New("malformed usage history: expected version 1 and accounts object")
	}
	changed := false
	for id, account := range h.data.Accounts {
		if !account.Provider.Valid() {
			return nil, fmt.Errorf("malformed usage history: account %s has invalid provider", id)
		}
		if len(account.Samples) > usageHistoryMaxPerAccount {
			account.Samples = append([]model.UsageHistorySample(nil), account.Samples[len(account.Samples)-usageHistoryMaxPerAccount:]...)
			h.data.Accounts[id] = account
			changed = true
		}
		before := len(account.Samples)
		account.Samples = pruneHistory(account.Samples, h.now().UTC())
		account.Samples = compactHistorySamples(account.Samples)
		sortHistorySamples(account.Samples)
		if len(account.Samples) != before {
			h.data.Accounts[id] = account
			changed = true
		}
	}
	if changed {
		if err := h.writeDocument(h.data); err != nil {
			return nil, err
		}
	}
	return h, nil
}

func (h *UsageHistoryStore) SetClockForTests(now func() time.Time) { h.now = now }

func (h *UsageHistoryStore) Append(accountID string, sample model.UsageSample) error {
	if accountID == "" || !sample.Provider.Valid() || sample.ObservedAt.IsZero() {
		return errors.New("usage history sample is missing account, provider, or observation time")
	}
	normalized := normalizeHistorySample(accountID, sample)
	if normalized == nil || len(normalized.Windows) == 0 {
		return errors.New("usage history sample has no normalized windows")
	}

	h.mu.Lock()
	defer h.mu.Unlock()
	next := cloneHistoryDocument(h.data)
	account, exists := next.Accounts[accountID]
	if exists && account.Provider != normalized.Provider {
		// The UUID is the identity boundary. A provider change must not carry
		// rates from the previous identity into the new one.
		account = usageHistoryAccount{Provider: normalized.Provider}
	}
	if !exists {
		account.Provider = normalized.Provider
	}
	if len(account.Samples) > 0 {
		last := account.Samples[len(account.Samples)-1]
		if !normalized.ObservedAt.After(last.ObservedAt) {
			return errUsageHistoryUnchanged
		}
	}
	account.Samples = appendCompactedSample(account.Samples, *normalized)
	account.Samples = pruneHistory(account.Samples, h.now().UTC())
	account.Samples = compactHistorySamples(account.Samples)
	if len(account.Samples) > usageHistoryMaxPerAccount {
		account.Samples = account.Samples[len(account.Samples)-usageHistoryMaxPerAccount:]
	}
	next.Accounts[accountID] = account
	if err := h.writeDocument(next); err != nil {
		return err
	}
	h.data = next
	return nil
}

func (h *UsageHistoryStore) Get(accountID string) (model.UsageHistory, bool) {
	h.mu.RLock()
	account, ok := h.data.Accounts[accountID]
	if ok {
		account.Samples = cloneHistorySamples(account.Samples)
	}
	h.mu.RUnlock()
	if !ok {
		return model.UsageHistory{}, false
	}
	account.Samples = compactHistorySamples(pruneHistory(account.Samples, h.now().UTC()))
	result := model.UsageHistory{AccountID: accountID, Provider: account.Provider, Samples: account.Samples}
	if len(result.Samples) > 0 {
		first := result.Samples[0].ObservedAt
		last := result.Samples[len(result.Samples)-1].ObservedAt
		result.EarliestObservedAt = &first
		result.LatestObservedAt = &last
	}
	return result, true
}

func (h *UsageHistoryStore) Delete(accountID string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, ok := h.data.Accounts[accountID]; !ok {
		return errUsageHistoryUnchanged
	}
	next := cloneHistoryDocument(h.data)
	delete(next.Accounts, accountID)
	if err := h.writeDocument(next); err != nil {
		return err
	}
	h.data = next
	return nil
}

func (h *UsageHistoryStore) Reset(accountID string) error { return h.Delete(accountID) }

func (h *UsageHistoryStore) writeDocument(document usageHistoryDocument) error {
	b, err := json.MarshalIndent(document, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	tmp, err := os.CreateTemp(filepath.Dir(h.path), ".usage-history-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err = tmp.Chmod(0600); err == nil {
		_, err = tmp.Write(b)
	}
	if err == nil {
		err = tmp.Sync()
	}
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if err = os.Rename(tmpName, h.path); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(h.path))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

func cloneHistoryDocument(document usageHistoryDocument) usageHistoryDocument {
	out := usageHistoryDocument{Version: document.Version, Accounts: make(map[string]usageHistoryAccount, len(document.Accounts))}
	for id, account := range document.Accounts {
		out.Accounts[id] = usageHistoryAccount{Provider: account.Provider, Samples: cloneHistorySamples(account.Samples)}
	}
	return out
}

func pruneHistory(samples []model.UsageHistorySample, now time.Time) []model.UsageHistorySample {
	cutoff := now.Add(-usageHistoryRetention)
	first := 0
	for first < len(samples) && samples[first].ObservedAt.Before(cutoff) {
		first++
	}
	if first == 0 {
		return samples
	}
	return append([]model.UsageHistorySample(nil), samples[first:]...)
}

func normalizeHistorySample(accountID string, sample model.UsageSample) *model.UsageHistorySample {
	if sample.ObservedAt.IsZero() {
		return nil
	}
	plan := cloneString(sample.Plan)
	if plan != nil {
		trimmed := strings.TrimSpace(*plan)
		if trimmed == "" {
			plan = nil
		} else {
			*plan = trimmed
		}
	}
	out := &model.UsageHistorySample{
		AccountID:  accountID,
		Provider:   sample.Provider,
		Plan:       plan,
		ObservedAt: sample.ObservedAt.UTC(),
		Windows:    make([]model.Window, 0, len(sample.Windows)),
	}
	for _, window := range sample.Windows {
		if window.ID == "" || math.IsNaN(window.UsedPercent) || math.IsInf(window.UsedPercent, 0) || window.UsedPercent < 0 || window.UsedPercent > 100 {
			continue
		}
		copyWindow := cloneWindow(window)
		if copyWindow.WindowSeconds != nil && *copyWindow.WindowSeconds <= 0 {
			copyWindow.WindowSeconds = nil
		}
		out.Windows = append(out.Windows, copyWindow)
	}
	return out
}

func cloneHistorySamples(samples []model.UsageHistorySample) []model.UsageHistorySample {
	out := make([]model.UsageHistorySample, len(samples))
	for i, sample := range samples {
		out[i] = sample
		out[i].Plan = cloneString(sample.Plan)
		out[i].Windows = make([]model.Window, len(sample.Windows))
		for j, window := range sample.Windows {
			out[i].Windows[j] = cloneWindow(window)
		}
	}
	return out
}

func cloneWindow(window model.Window) model.Window {
	out := window
	if window.ResetsAt != nil {
		reset := window.ResetsAt.UTC()
		out.ResetsAt = &reset
	}
	if window.WindowSeconds != nil {
		seconds := *window.WindowSeconds
		out.WindowSeconds = &seconds
	}
	return out
}

func cloneString(value *string) *string {
	if value == nil {
		return nil
	}
	out := *value
	return &out
}

func sortHistorySamples(samples []model.UsageHistorySample) {
	sort.SliceStable(samples, func(i, j int) bool { return samples[i].ObservedAt.Before(samples[j].ObservedAt) })
}

func sameHistoryBucket(a, b model.UsageHistorySample) bool {
	return a.ObservedAt.UnixNano()/int64(usageHistoryBucket) == b.ObservedAt.UnixNano()/int64(usageHistoryBucket)
}

func sameHistoryGeneration(a, b model.UsageHistorySample) bool {
	if !samePlan(a.Plan, b.Plan) || len(a.Windows) != len(b.Windows) {
		return false
	}
	for _, left := range a.Windows {
		found := false
		for _, right := range b.Windows {
			if left.ID != right.ID || !sameOptionalInt64(left.WindowSeconds, right.WindowSeconds) || !sameResetGeneration(left, right) {
				continue
			}
			found = true
			break
		}
		if !found {
			return false
		}
	}
	return true
}

func samePlan(a, b *string) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

func sameOptionalInt64(a, b *int64) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

func sameResetGeneration(a, b model.Window) bool {
	if a.ResetsAt == nil || b.ResetsAt == nil {
		return a.ResetsAt == nil && b.ResetsAt == nil
	}
	delta := a.ResetsAt.Sub(*b.ResetsAt)
	if delta < 0 {
		delta = -delta
	}
	tolerance := time.Minute
	if a.WindowSeconds != nil && *a.WindowSeconds > 0 {
		candidate := time.Duration(*a.WindowSeconds) * time.Second / 100
		if candidate > tolerance {
			tolerance = candidate
		}
		if max := time.Duration(*a.WindowSeconds) * time.Second / 4; tolerance > max {
			tolerance = max
		}
	}
	return delta <= tolerance
}

func compactHistorySamples(samples []model.UsageHistorySample) []model.UsageHistorySample {
	if len(samples) < 2 {
		return samples
	}
	sortHistorySamples(samples)
	out := make([]model.UsageHistorySample, 0, len(samples))
	for _, sample := range samples {
		out = appendCompactedSample(out, sample)
	}
	return out
}

func appendCompactedSample(samples []model.UsageHistorySample, sample model.UsageHistorySample) []model.UsageHistorySample {
	if len(samples) == 0 {
		return append(samples, sample)
	}
	last := samples[len(samples)-1]
	if !sameHistoryBucket(last, sample) || !sameHistoryGeneration(last, sample) {
		return append(samples, sample)
	}
	bucketStart := len(samples) - 1
	for bucketStart > 0 && sameHistoryBucket(samples[bucketStart-1], sample) && sameHistoryGeneration(samples[bucketStart-1], sample) {
		bucketStart--
	}
	bucketDecline := false
	for i := bucketStart + 1; i < len(samples); i++ {
		if historySampleHasDecline(samples[i-1], samples[i]) {
			bucketDecline = true
			break
		}
	}
	if !bucketDecline && !historySampleHasDecline(last, sample) {
		samples[len(samples)-1] = sample
		return samples
	}
	if len(samples)-bucketStart < 3 {
		return append(samples, sample)
	}
	// Keep the first point, the boundary before the decline, and the newest
	// point. This preserves the discontinuity without allowing high-frequency
	// reads to consume the retention cap.
	samples[len(samples)-1] = sample
	return samples
}

func historySampleHasDecline(previous, current model.UsageHistorySample) bool {
	for _, left := range previous.Windows {
		for _, right := range current.Windows {
			if left.ID == right.ID && sameOptionalInt64(left.WindowSeconds, right.WindowSeconds) && right.UsedPercent+0.000001 < left.UsedPercent {
				return true
			}
		}
	}
	return false
}
