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
	inflight chan struct{}
}

type Cache struct {
	mu      sync.Mutex
	entries map[string]*cacheEntry
	now     func() time.Time
}

func NewCache() *Cache { return &Cache{entries: map[string]*cacheEntry{}, now: time.Now} }

func (c *Cache) Clear(id string) {
	c.mu.Lock()
	if e := c.entries[id]; e != nil && e.inflight != nil {
		close(e.inflight)
		e.inflight = nil
	}
	delete(c.entries, id)
	c.mu.Unlock()
}

func (c *Cache) Install(id string, sample model.UsageSample) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e := c.entry(id)
	if e.inflight != nil {
		close(e.inflight)
		e.inflight = nil
	}
	copy := sample
	e.sample = &copy
	e.err = nil
}

func (c *Cache) RecordError(id string, detail *model.ErrorDetail) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entry(id).err = detail
}

func (c *Cache) Peek(id string) (*model.UsageSample, *model.ErrorDetail, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e := c.entry(id)
	return aged(e.sample, c.now()), e.err, e.inflight != nil
}

func (c *Cache) Fetch(ctx context.Context, id string, fetch func(context.Context) (*model.UsageSample, *model.ErrorDetail)) (*model.UsageSample, *model.ErrorDetail, bool) {
	c.mu.Lock()
	e := c.entry(id)
	if e.inflight != nil {
		done := e.inflight
		c.mu.Unlock()
		select {
		case <-done:
			s, err, _ := c.Peek(id)
			return s, err, false
		case <-ctx.Done():
			return nil, timeoutError(), false
		}
	}
	e.inflight = make(chan struct{})
	done := e.inflight
	c.mu.Unlock()
	s, detail := fetch(ctx)
	c.finish(id, done, s, detail)
	if s != nil {
		s = aged(s, c.now())
	}
	return s, detail, true
}

func (c *Cache) FetchBackground(ctx context.Context, id string, fetch func(context.Context) (*model.UsageSample, *model.ErrorDetail)) bool {
	c.mu.Lock()
	e := c.entry(id)
	if e.inflight != nil {
		c.mu.Unlock()
		return false
	}
	e.inflight = make(chan struct{})
	done := e.inflight
	c.mu.Unlock()
	go func() {
		s, detail := fetch(ctx)
		c.finish(id, done, s, detail)
	}()
	return true
}

func (c *Cache) finish(id string, done chan struct{}, s *model.UsageSample, detail *model.ErrorDetail) {
	c.mu.Lock()
	e := c.entry(id)
	if e.inflight != done {
		c.mu.Unlock()
		return
	}
	if s != nil {
		copy := *s
		e.sample = &copy
		e.err = nil
	} else {
		e.err = detail
	}
	e.inflight = nil
	close(done)
	c.mu.Unlock()
}

func (c *Cache) entry(id string) *cacheEntry {
	e := c.entries[id]
	if e == nil {
		e = &cacheEntry{}
		c.entries[id] = e
	}
	return e
}

func aged(s *model.UsageSample, now time.Time) *model.UsageSample {
	if s == nil {
		return nil
	}
	out := *s
	d := now.Sub(out.ObservedAt)
	if d < 0 {
		d = 0
	}
	out.AgeSeconds = int64(d / time.Second)
	return &out
}

func timeoutError() *model.ErrorDetail {
	return &model.ErrorDetail{Code: "UPSTREAM_TIMEOUT", Message: "The provider request did not finish before the wait deadline.", Prerequisites: []model.Prerequisite{}, State: map[string]any{}, Remedy: model.Remedy{Summary: "Retry the exact call.", Calls: []model.RemedyCall{}, Commands: []string{"retry the exact call"}}, Help: "/api/v1/help/usage"}
}
