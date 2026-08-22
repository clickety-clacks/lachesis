package core

import (
	"context"
	"sync"
	"time"

	"github.com/clickety-clacks/lachesis/internal/model"
)

type cacheEntry struct {
	sample   *model.UsageSample
	err      *model.ErrorDetail
	inflight *cacheClaim
}

type cacheClaim struct{ done chan struct{} }

type cacheMutation uint8

const (
	cacheStart cacheMutation = iota
	cacheResume
	cacheFinish
	cacheInstall
	cacheClear
)

type cacheSnapshot struct {
	sample *model.UsageSample
	err    *model.ErrorDetail
}

type cacheMutationResult struct {
	snapshot cacheSnapshot
	inflight *cacheClaim
	applied  bool
	ready    bool
}

type Cache struct {
	mu      sync.Mutex
	entries map[string]*cacheEntry
	now     func() time.Time
}

func NewCache() *Cache { return &Cache{entries: map[string]*cacheEntry{}, now: time.Now} }

func (c *Cache) Clear(id string) { c.mutate(id, cacheClear, nil, nil, nil) }

func (c *Cache) Install(id string, sample model.UsageSample) {
	c.mutate(id, cacheInstall, nil, &sample, nil)
}

func (c *Cache) Peek(id string) (*model.UsageSample, *model.ErrorDetail) {
	c.mu.Lock()
	defer c.mu.Unlock()
	snapshot := c.snapshot(c.entries[id])
	return snapshot.sample, snapshot.err
}

func (c *Cache) Fetch(ctx context.Context, id string, fetch func(context.Context) (*model.UsageSample, *model.ErrorDetail)) (*model.UsageSample, *model.ErrorDetail, bool) {
	mutation := cacheStart
	for {
		claim := &cacheClaim{done: make(chan struct{})}
		result := c.mutate(id, mutation, claim, nil, nil)
		if result.ready {
			return result.snapshot.sample, result.snapshot.err, false
		}
		if !result.applied {
			select {
			case <-result.inflight.done:
				mutation = cacheResume
				continue
			case <-ctx.Done():
				return nil, timeoutError(), false
			}
		}
		sample, detail := fetch(ctx)
		finish := c.mutate(id, cacheFinish, claim, sample, detail)
		return finish.snapshot.sample, finish.snapshot.err, finish.applied
	}
}

// mutate is the only transition point for an account's sample, error, and fetch claim.
func (c *Cache) mutate(id string, mutation cacheMutation, claim *cacheClaim, sample *model.UsageSample, detail *model.ErrorDetail) cacheMutationResult {
	c.mu.Lock()
	defer c.mu.Unlock()

	e := c.entries[id]
	if mutation == cacheClear {
		if e != nil {
			delete(c.entries, id)
			if e.inflight != nil {
				close(e.inflight.done)
			}
		}
		return cacheMutationResult{applied: true}
	}
	if e == nil && mutation == cacheFinish {
		return cacheMutationResult{}
	}
	if e == nil {
		e = &cacheEntry{}
		c.entries[id] = e
	}

	switch mutation {
	case cacheStart:
		if e.inflight != nil {
			return cacheMutationResult{inflight: e.inflight}
		}
		e.inflight = claim
		return cacheMutationResult{inflight: claim, applied: true}
	case cacheResume:
		if e.inflight != nil {
			return cacheMutationResult{inflight: e.inflight}
		}
		if e.sample != nil || e.err != nil {
			return cacheMutationResult{snapshot: c.snapshot(e), ready: true}
		}
		e.inflight = claim
		return cacheMutationResult{inflight: claim, applied: true}
	case cacheFinish:
		if e.inflight != claim {
			return cacheMutationResult{snapshot: c.snapshot(e), inflight: e.inflight}
		}
		if sample != nil {
			e.sample = copyUsageSample(sample)
			e.err = nil
		} else {
			e.err = detail
		}
		e.inflight = nil
		close(claim.done)
		return cacheMutationResult{snapshot: c.snapshot(e), applied: true}
	case cacheInstall:
		retired := e.inflight
		e.sample = copyUsageSample(sample)
		e.err = nil
		e.inflight = nil
		if retired != nil {
			close(retired.done)
		}
		return cacheMutationResult{applied: true}
	default:
		panic("unknown cache mutation")
	}
}

func aged(s *model.UsageSample, now time.Time) *model.UsageSample {
	if s == nil {
		return nil
	}
	out := copyUsageSample(s)
	d := now.Sub(out.ObservedAt)
	if d < 0 {
		d = 0
	}
	out.AgeSeconds = int64(d / time.Second)
	return out
}

func (c *Cache) snapshot(e *cacheEntry) cacheSnapshot {
	if e == nil {
		return cacheSnapshot{}
	}
	var sample *model.UsageSample
	if e.sample != nil {
		sample = aged(e.sample, c.now())
	}
	return cacheSnapshot{sample: sample, err: e.err}
}

func copyUsageSample(sample *model.UsageSample) *model.UsageSample {
	if sample == nil {
		return nil
	}
	out := *sample
	if sample.Plan != nil {
		plan := *sample.Plan
		out.Plan = &plan
	}
	if sample.Windows != nil {
		out.Windows = make([]model.Window, len(sample.Windows))
		copy(out.Windows, sample.Windows)
		for i := range out.Windows {
			if sample.Windows[i].ResetsAt != nil {
				resetsAt := *sample.Windows[i].ResetsAt
				out.Windows[i].ResetsAt = &resetsAt
			}
			if sample.Windows[i].WindowSeconds != nil {
				windowSeconds := *sample.Windows[i].WindowSeconds
				out.Windows[i].WindowSeconds = &windowSeconds
			}
		}
	}
	if sample.Diagnostics != nil {
		out.Diagnostics = make([]model.Diagnostic, len(sample.Diagnostics))
		copy(out.Diagnostics, sample.Diagnostics)
	}
	if sample.Raw != nil {
		out.Raw = make([]byte, len(sample.Raw))
		copy(out.Raw, sample.Raw)
	}
	return &out
}

func timeoutError() *model.ErrorDetail {
	return &model.ErrorDetail{Code: "UPSTREAM_TIMEOUT", Message: "The provider request did not finish before the wait deadline.", Prerequisites: []model.Prerequisite{}, State: map[string]any{}, Remedy: model.Remedy{Summary: "Retry the exact call.", Calls: []model.RemedyCall{}, Commands: []string{"retry the exact call"}}, Help: "/api/v1/help/usage"}
}
