package core

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"reflect"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/clickety-clacks/lachesis/internal/model"
	"github.com/clickety-clacks/lachesis/internal/provider"
	"github.com/clickety-clacks/lachesis/internal/teach"
)

type cacheFetchResult struct {
	sample *model.UsageSample
	detail *model.ErrorDetail
	live   bool
}

type waitObservedContext struct {
	context.Context
	waiting chan struct{}
	once    sync.Once
}

type barrierWriter struct {
	buffer  bytes.Buffer
	started chan struct{}
	release <-chan struct{}
	once    sync.Once
}

func (w *barrierWriter) Write(data []byte) (int, error) {
	w.once.Do(func() {
		close(w.started)
		<-w.release
	})
	return w.buffer.Write(data)
}

func (c *waitObservedContext) Done() <-chan struct{} {
	c.once.Do(func() { close(c.waiting) })
	return c.Context.Done()
}

func TestCacheSupersedingMutationWinsAndWakesWaiterOnce(t *testing.T) {
	reset := time.Unix(2_000, 0).UTC()
	for _, mutation := range []string{"install", "clear"} {
		t.Run(mutation, func(t *testing.T) {
			cache := NewCache()
			cache.now = func() time.Time { return time.Unix(2_100, 0).UTC() }
			old := generationSample("account", 23, reset, time.Unix(1_900, 0).UTC(), "old")
			installed := generationSample("account", 100, reset, time.Unix(2_050, 0).UTC(), "installed")
			replacement := generationSample("account", 71, reset, time.Unix(2_075, 0).UTC(), "replacement")

			ownerStarted := make(chan struct{})
			releaseOwner := make(chan struct{})
			ownerResult := make(chan cacheFetchResult, 1)
			var ownerCalls atomic.Int32
			go func() {
				sample, detail, live := cache.Fetch(context.Background(), "account", func(context.Context) (*model.UsageSample, *model.ErrorDetail) {
					ownerCalls.Add(1)
					close(ownerStarted)
					<-releaseOwner
					return old, nil
				})
				ownerResult <- cacheFetchResult{sample: sample, detail: detail, live: live}
			}()
			<-ownerStarted

			waitBase, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			waitContext := &waitObservedContext{Context: waitBase, waiting: make(chan struct{})}
			waiterResult := make(chan cacheFetchResult, 2)
			var waiterCalls atomic.Int32
			go func() {
				sample, detail, live := cache.Fetch(waitContext, "account", func(context.Context) (*model.UsageSample, *model.ErrorDetail) {
					waiterCalls.Add(1)
					return replacement, nil
				})
				waiterResult <- cacheFetchResult{sample: sample, detail: detail, live: live}
			}()
			<-waitContext.waiting

			want := installed
			if mutation == "install" {
				cache.Install("account", *installed)
			} else {
				cache.Clear("account")
				want = replacement
			}
			waited := receiveCacheResult(t, waiterResult)
			assertGeneration(t, waited.sample, want)
			if waited.detail != nil || waited.live != (mutation == "clear") {
				t.Fatalf("waited = %#v", waited)
			}

			close(releaseOwner)
			owner := receiveCacheResult(t, ownerResult)
			assertGeneration(t, owner.sample, want)
			if owner.detail != nil || owner.live {
				t.Fatalf("owner = %#v", owner)
			}
			later, detail := cache.Peek("account")
			assertGeneration(t, later, want)
			if detail != nil || ownerCalls.Load() != 1 {
				t.Fatalf("detail = %#v, owner calls = %d", detail, ownerCalls.Load())
			}
			wantWaiterCalls := int32(0)
			if mutation == "clear" {
				wantWaiterCalls = 1
			}
			if waiterCalls.Load() != wantWaiterCalls {
				t.Fatalf("waiter calls = %d, want %d", waiterCalls.Load(), wantWaiterCalls)
			}
			select {
			case duplicate := <-waiterResult:
				t.Fatalf("waiter returned twice: %#v", duplicate)
			default:
			}
		})
	}
}

func TestCacheFailedReadPreservesStaleGeneration(t *testing.T) {
	cache := NewCache()
	reset := time.Unix(2_000, 0).UTC()
	old := generationSample("account", 23, reset, time.Unix(1_900, 0).UTC(), "old")
	cache.Install("account", *old)
	detail := teach.New(teach.UpstreamUnavailable, "Synthetic read failed.", "usage", nil, nil, nil, "retry the exact call")

	got, gotDetail, live := cache.Fetch(context.Background(), "account", func(context.Context) (*model.UsageSample, *model.ErrorDetail) {
		return nil, detail
	})
	assertGeneration(t, got, old)
	if gotDetail != detail || !live {
		t.Fatalf("detail = %#v, live = %t", gotDetail, live)
	}

	peeked, peekDetail := cache.Peek("account")
	assertGeneration(t, peeked, old)
	if peekDetail != detail {
		t.Fatalf("peek detail = %#v", peekDetail)
	}

	newer := generationSample("account", 100, reset, time.Unix(2_050, 0).UTC(), "new")
	got, gotDetail, live = cache.Fetch(context.Background(), "account", func(context.Context) (*model.UsageSample, *model.ErrorDetail) {
		return newer, nil
	})
	assertGeneration(t, got, newer)
	if gotDetail != nil || !live {
		t.Fatalf("detail = %#v, live = %t", gotDetail, live)
	}
}

func TestCacheOwnsInputAndOutputGenerationValues(t *testing.T) {
	cache := NewCache()
	observed := time.Unix(1_900, 0).UTC()
	reset := time.Unix(2_000, 0).UTC()
	cache.now = func() time.Time { return time.Unix(1_935, 0).UTC() }
	input := generationSample("account", 23, reset, observed, "owned")
	want := generationSample("account", 23, reset, observed, "owned")
	want.AgeSeconds = 35

	cache.Install("account", *input)
	mutateGeneration(input)
	first, detail := cache.Peek("account")
	if detail != nil || !reflect.DeepEqual(first, want) {
		t.Fatalf("first = %#v, detail = %#v, want %#v", first, detail, want)
	}

	mutateGeneration(first)
	second, detail := cache.Peek("account")
	if detail != nil || !reflect.DeepEqual(second, want) {
		t.Fatalf("second = %#v, detail = %#v, want %#v", second, detail, want)
	}
}

func TestDetachedGenerationSerializationCannotMixPublications(t *testing.T) {
	resetA := time.Unix(2_000, 0).UTC()
	resetB := time.Unix(3_000, 0).UTC()
	a := generationSample("account", 23, resetA, time.Unix(1_900, 0).UTC(), "a")
	b := generationSample("account", 100, resetB, time.Unix(2_900, 0).UTC(), "b")

	for _, test := range []struct {
		name  string
		first *model.UsageSample
		next  *model.UsageSample
	}{{"a then b", a, b}, {"b then a", b, a}} {
		t.Run(test.name, func(t *testing.T) {
			cache := NewCache()
			cache.now = func() time.Time { return time.Unix(4_000, 0).UTC() }
			cache.Install("account", *test.first)
			detached, detail := cache.Peek("account")
			if detail != nil {
				t.Fatal(detail)
			}
			releaseEncoding := make(chan struct{})
			writer := &barrierWriter{started: make(chan struct{}), release: releaseEncoding}
			encoded := make(chan error, 1)
			go func() {
				encoded <- json.NewEncoder(writer).Encode(model.UsageResult{AccountID: "account", Status: "cache", Sample: detached})
			}()
			<-writer.started
			cache.Install("account", *test.next)
			close(releaseEncoding)
			if err := <-encoded; err != nil {
				t.Fatal(err)
			}
			var decoded model.UsageResult
			if err := json.Unmarshal(writer.buffer.Bytes(), &decoded); err != nil {
				t.Fatal(err)
			}
			assertGeneration(t, decoded.Sample, test.first)
			if wantAge := int64(time.Unix(4_000, 0).Sub(test.first.ObservedAt) / time.Second); decoded.Sample.AgeSeconds != wantAge {
				t.Fatalf("age_seconds = %d, want %d", decoded.Sample.AgeSeconds, wantAge)
			}
		})
	}
}

func TestConcurrentAccountsKeepClaimsAndGenerationsIsolated(t *testing.T) {
	cache := NewCache()
	reset := time.Unix(2_000, 0).UTC()
	oldA := generationSample("account-a", 23, reset, time.Unix(1_900, 0).UTC(), "old-a")
	oldB := generationSample("account-b", 31, reset, time.Unix(1_900, 0).UTC(), "old-b")
	newA := generationSample("account-a", 100, reset, time.Unix(2_050, 0).UTC(), "new-a")
	newB := generationSample("account-b", 71, reset, time.Unix(2_050, 0).UTC(), "new-b")

	startA, startB := make(chan struct{}), make(chan struct{})
	releaseA, releaseB := make(chan struct{}), make(chan struct{})
	resultA, resultB := make(chan cacheFetchResult, 1), make(chan cacheFetchResult, 1)
	go func() {
		sample, detail, live := cache.Fetch(context.Background(), "account-a", func(context.Context) (*model.UsageSample, *model.ErrorDetail) {
			close(startA)
			<-releaseA
			return oldA, nil
		})
		resultA <- cacheFetchResult{sample: sample, detail: detail, live: live}
	}()
	go func() {
		sample, detail, live := cache.Fetch(context.Background(), "account-b", func(context.Context) (*model.UsageSample, *model.ErrorDetail) {
			close(startB)
			<-releaseB
			return oldB, nil
		})
		resultB <- cacheFetchResult{sample: sample, detail: detail, live: live}
	}()
	<-startA
	<-startB

	cache.Clear("account-b")
	cache.Install("account-a", *newA)
	cache.Install("account-b", *newB)
	close(releaseB)
	close(releaseA)

	gotA := receiveCacheResult(t, resultA)
	gotB := receiveCacheResult(t, resultB)
	assertGeneration(t, gotA.sample, newA)
	assertGeneration(t, gotB.sample, newB)
	if gotA.detail != nil || gotB.detail != nil || gotA.live || gotB.live {
		t.Fatalf("account-a = %#v, account-b = %#v", gotA, gotB)
	}
}

type generationAdapter struct {
	*fakeAdapter
	usage func(context.Context) (*model.UsageSample, *model.ErrorDetail)
}

func (a *generationAdapter) Usage(ctx context.Context, _ provider.Credential) (*model.UsageSample, *model.ErrorDetail) {
	if a.usage != nil {
		return a.usage(ctx)
	}
	return a.fakeAdapter.Usage(ctx, provider.Credential{})
}

func TestUsageKeepsAgeStaleSeparateFromFailedReadFallback(t *testing.T) {
	reset := time.Unix(2_000, 0).UTC()
	old := generationSample("", 23, reset, time.Unix(1_900, 0).UTC(), "old")
	adapter := &generationAdapter{fakeAdapter: &fakeAdapter{provider: model.ProviderClaude, credential: ".credentials.json", usageSample: old}}
	service, detail := OpenService(t.TempDir(), []provider.Adapter{adapter}, idleChecker{})
	if detail != nil {
		t.Fatal(detail)
	}
	defer service.Close()
	service.SetClockForTests(func() time.Time { return time.Unix(2_050, 0).UTC() })
	account := adoptGenerationAccount(t, service)
	old.AccountID = account.ID
	old.Label = account.Label

	refreshStarted := make(chan struct{})
	releaseRefresh := make(chan struct{})
	newer := generationSample("", 100, reset, time.Unix(2_025, 0).UTC(), "new")
	var refreshStartedOnce sync.Once
	adapter.usage = func(context.Context) (*model.UsageSample, *model.ErrorDetail) {
		refreshStartedOnce.Do(func() { close(refreshStarted) })
		<-releaseRefresh
		return newer, nil
	}
	ageStale, detail := service.Usage(context.Background(), account.ID, "background")
	if detail != nil || ageStale.Status != "stale" || ageStale.Error != nil {
		t.Fatalf("age-stale result = %#v, detail = %#v", ageStale, detail)
	}
	assertGeneration(t, ageStale.Sample, old)
	<-refreshStarted
	close(releaseRefresh)
	refreshed, detail := service.Usage(context.Background(), account.ID, "wait")
	if detail != nil || refreshed.Error != nil || refreshed.Sample == nil {
		t.Fatalf("refreshed result = %#v, detail = %#v", refreshed, detail)
	}
	assertGeneration(t, refreshed.Sample, newer)

	failure := teach.New(teach.UpstreamUnavailable, "Synthetic read failed.", "usage", nil, nil, nil, "retry the exact call")
	adapter.usage = func(context.Context) (*model.UsageSample, *model.ErrorDetail) { return nil, failure }
	failed, detail := service.Usage(context.Background(), account.ID, "wait")
	if detail != nil || failed.Status != "stale" || failed.Error != failure {
		t.Fatalf("failed-read result = %#v, detail = %#v", failed, detail)
	}
	assertGeneration(t, failed.Sample, newer)
}

func adoptGenerationAccount(t *testing.T, service *Service) model.Account {
	t.Helper()
	home := t.TempDir()
	path := home + "/.credentials.json"
	if err := os.WriteFile(path, []byte("synthetic"), 0600); err != nil {
		t.Fatal(err)
	}
	account, detail := service.Adopt(context.Background(), model.ProviderClaude, "test", model.StoreBinding{Kind: "file", Home: home, CredentialPath: path})
	if detail != nil {
		t.Fatal(detail)
	}
	return account
}

func generationSample(account string, used float64, reset, observed time.Time, sentinel string) *model.UsageSample {
	plan := "synthetic-" + sentinel
	windowSeconds := int64(300)
	return &model.UsageSample{
		AccountID:  account,
		Provider:   model.ProviderClaude,
		Label:      "label-" + sentinel,
		Plan:       &plan,
		ObservedAt: observed,
		Windows: []model.Window{{
			ID:            "five_hour",
			Name:          "Five hour",
			UsedPercent:   used,
			ResetsAt:      &reset,
			WindowSeconds: &windowSeconds,
		}},
		Diagnostics: []model.Diagnostic{{Code: "DIAGNOSTIC_" + sentinel, Message: "message-" + sentinel}},
		Raw:         json.RawMessage(`{"utilization":` + formatFloat(used) + `,"sentinel":"` + sentinel + `"}`),
	}
}

func formatFloat(value float64) string {
	return strconv.FormatFloat(value, 'f', -1, 64)
}

func mutateGeneration(sample *model.UsageSample) {
	sample.AccountID = "mutated"
	*sample.Plan = "mutated"
	sample.Windows[0].UsedPercent = -1
	mutatedReset := time.Unix(9_000, 0).UTC()
	*sample.Windows[0].ResetsAt = mutatedReset
	*sample.Windows[0].WindowSeconds = -1
	sample.Diagnostics[0].Code = "MUTATED"
	sample.Raw[0] = '['
}

func assertGeneration(t *testing.T, got, want *model.UsageSample) {
	t.Helper()
	if got == nil || want == nil {
		if got != want {
			t.Fatalf("generation = %#v, want %#v", got, want)
		}
		return
	}
	gotCopy, wantCopy := *got, *want
	gotCopy.AgeSeconds = 0
	wantCopy.AgeSeconds = 0
	if !reflect.DeepEqual(gotCopy, wantCopy) {
		t.Fatalf("generation = %#v, want %#v", got, want)
	}
}

func receiveCacheResult(t *testing.T, results <-chan cacheFetchResult) cacheFetchResult {
	t.Helper()
	select {
	case result := <-results:
		return result
	case <-time.After(2 * time.Second):
		t.Fatal("cache fetch did not return")
		return cacheFetchResult{}
	}
}
