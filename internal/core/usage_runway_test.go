package core

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/clickety-clacks/lachesis/internal/model"
	"github.com/clickety-clacks/lachesis/internal/provider"
)

func TestUsageHistoryPersistsNormalizedSamplesAndPurges(t *testing.T) {
	state := t.TempDir()
	now := time.Now().UTC().Truncate(time.Second)
	history, err := openUsageHistory(state)
	if err != nil {
		t.Fatal(err)
	}
	history.SetClockForTests(func() time.Time { return now })
	plan := "private"
	reset := now.Add(4 * time.Hour)
	seconds := int64(5 * 60 * 60)
	sample := model.UsageSample{
		AccountID:  "account-a",
		Provider:   model.ProviderCodex,
		Plan:       &plan,
		ObservedAt: now.Add(-time.Hour),
		Raw:        []byte(`{"credential":"must-not-persist"}`),
		Windows:    []model.Window{{ID: "primary", Name: "Primary", UsedPercent: 10, ResetsAt: &reset, WindowSeconds: &seconds}},
	}
	if err := history.Append(sample.AccountID, sample); err != nil {
		t.Fatal(err)
	}
	if err := history.Append(sample.AccountID, sample); err != errUsageHistoryUnchanged {
		t.Fatalf("duplicate error = %v, want %v", err, errUsageHistoryUnchanged)
	}
	reopened, err := openUsageHistory(state)
	if err != nil {
		t.Fatal(err)
	}
	reopened.SetClockForTests(func() time.Time { return now })
	got, ok := reopened.Get(sample.AccountID)
	if !ok || len(got.Samples) != 1 || len(got.Samples[0].Windows) != 1 {
		t.Fatalf("history = %#v, found = %v", got, ok)
	}
	b, err := os.ReadFile(filepath.Join(state, "usage-history.json"))
	if err != nil {
		t.Fatal(err)
	}
	if string(b) == "" || json.Valid(b) == false {
		t.Fatalf("history file is not valid JSON: %q", b)
	}
	for _, forbidden := range []string{"credential", "raw"} {
		if containsJSONText(b, forbidden) {
			t.Fatalf("history file contains provider payload field %q: %s", forbidden, b)
		}
	}
	if err := reopened.Delete(sample.AccountID); err != nil {
		t.Fatal(err)
	}
	if _, ok := reopened.Get(sample.AccountID); ok {
		t.Fatal("deleted account history remains in memory")
	}
	reopenedAgain, err := openUsageHistory(state)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := reopenedAgain.Get(sample.AccountID); ok {
		t.Fatal("deleted account history returned after restart")
	}
}

func TestUsageHistoryWriteFailureDoesNotCommitMemoryAndRetryPersists(t *testing.T) {
	state := t.TempDir()
	history, err := openUsageHistory(state)
	if err != nil {
		t.Fatal(err)
	}
	history.SetClockForTests(time.Now)
	sample := model.UsageSample{AccountID: "account-a", Provider: model.ProviderCodex, ObservedAt: time.Now().UTC(), Windows: []model.Window{{ID: "primary", UsedPercent: 10}}}
	originalPath := history.path
	history.path = filepath.Join(state, "missing", "usage-history.json")
	if err := history.Append(sample.AccountID, sample); err == nil {
		t.Fatal("write failure was not reported")
	}
	if _, ok := history.Get(sample.AccountID); ok {
		t.Fatal("failed append mutated in-memory history")
	}
	history.path = originalPath
	if err := history.Append(sample.AccountID, sample); err != nil {
		t.Fatal(err)
	}
	if _, ok := history.Get(sample.AccountID); !ok {
		t.Fatal("retry did not persist the observation")
	}
}

func TestUsageHistoryNoopPurgePreservesPersistenceError(t *testing.T) {
	service, detail := OpenService(t.TempDir(), nil, idleChecker{})
	if detail != nil {
		t.Fatal(detail)
	}
	now := time.Now().UTC().Truncate(time.Second)
	service.SetClockForTests(func() time.Time { return now })
	originalPath := service.history.path
	service.history.path = filepath.Join(filepath.Dir(originalPath), "missing", "usage-history.json")
	service.recordUsage("account-a", model.UsageSample{
		AccountID:  "account-a",
		Provider:   model.ProviderCodex,
		ObservedAt: now,
		Windows:    []model.Window{{ID: "primary", UsedPercent: 10}},
	})
	if service.historyErrorString() == "" {
		t.Fatal("history write failure was not retained")
	}
	if err := service.clearUsageHistory("account-a"); err != nil {
		t.Fatalf("purging absent history: %v", err)
	}
	if service.historyErrorString() == "" {
		t.Fatal("no-op purge cleared the persistence error")
	}
}

func TestUsageHistoryBucketsFrequentReadsWithoutLosingTwoDayCoverage(t *testing.T) {
	state := t.TempDir()
	now := time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)
	history, err := openUsageHistory(state)
	if err != nil {
		t.Fatal(err)
	}
	history.SetClockForTests(func() time.Time { return now })
	for i := 0; i <= 48*12; i++ {
		at := now.Add(-48*time.Hour + time.Duration(i)*5*time.Minute)
		used := float64(i) / 12
		if err := history.Append("account-a", model.UsageSample{AccountID: "account-a", Provider: model.ProviderClaude, ObservedAt: at, Windows: []model.Window{{ID: "five_hour", UsedPercent: used}}}); err != nil {
			t.Fatal(err)
		}
		if err := history.Append("account-a", model.UsageSample{AccountID: "account-a", Provider: model.ProviderClaude, ObservedAt: at.Add(time.Minute), Windows: []model.Window{{ID: "five_hour", UsedPercent: used + 0.01}}}); err != nil {
			t.Fatal(err)
		}
	}
	got, ok := history.Get("account-a")
	if !ok || len(got.Samples) < 400 {
		t.Fatalf("sample count = %d, want enough 48-hour coverage", len(got.Samples))
	}
	if got.EarliestObservedAt == nil || now.Sub(*got.EarliestObservedAt) < 47*time.Hour {
		t.Fatalf("earliest sample = %v, lost 48-hour baseline", got.EarliestObservedAt)
	}
}

func TestUsageHistoryPreservesDeclineBoundaryWithinBucket(t *testing.T) {
	state := t.TempDir()
	now := time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)
	history, err := openUsageHistory(state)
	if err != nil {
		t.Fatal(err)
	}
	history.SetClockForTests(func() time.Time { return now })
	for i, used := range []float64{20, 5, 30} {
		if err := history.Append("account-a", model.UsageSample{AccountID: "account-a", Provider: model.ProviderCodex, ObservedAt: now.Add(-time.Hour + time.Duration(i)*time.Minute), Windows: []model.Window{{ID: "primary", UsedPercent: used}}}); err != nil {
			t.Fatal(err)
		}
	}
	got, ok := history.Get("account-a")
	if !ok || len(got.Samples) != 3 {
		t.Fatalf("history samples = %#v, found = %v", got.Samples, ok)
	}
	if got.Samples[0].Windows[0].UsedPercent != 20 || got.Samples[1].Windows[0].UsedPercent != 5 || got.Samples[2].Windows[0].UsedPercent != 30 {
		t.Fatalf("decline boundary was compacted away: %#v", got.Samples)
	}
}

func TestForecastRejectsUnrepresentablyTinyBurn(t *testing.T) {
	for _, test := range []struct {
		name       string
		reset      bool
		wantStatus string
	}{
		{name: "without reset", wantStatus: "estimate_out_of_range"},
		{name: "bounded by reset", reset: true, wantStatus: "resets_before_exhaustion"},
	} {
		t.Run(test.name, func(t *testing.T) {
			service, account, now := forecastService(t)
			defer service.Close()
			var reset *time.Time
			if test.reset {
				resetAt := now.Add(24 * time.Hour)
				reset = &resetAt
			}
			appendForecastSample(t, service, account.ID, now.Add(-time.Hour), 10, reset, nil)
			appendForecastSample(t, service, account.ID, now, 10.00000001, reset, nil)
			forecast, detail := service.Forecast(account.ID)
			if detail != nil {
				t.Fatal(detail)
			}
			if len(forecast.Windows) != 1 || forecast.Windows[0].Status != test.wantStatus || forecast.Windows[0].ExhaustionAt != nil {
				t.Fatalf("tiny burn produced an invalid forecast: %#v", forecast)
			}
		})
	}
}

func TestForecastTreatsFlatHistoryPastResetAsStale(t *testing.T) {
	service, account, now := forecastService(t)
	defer service.Close()
	reset := now.Add(-30 * time.Second)
	appendForecastSample(t, service, account.ID, now.Add(-61*time.Minute), 10, &reset, nil)
	appendForecastSample(t, service, account.ID, now.Add(-time.Minute), 10, &reset, nil)
	forecast, detail := service.Forecast(account.ID)
	if detail != nil {
		t.Fatal(detail)
	}
	if len(forecast.Windows) != 1 || forecast.Windows[0].Status != "stale" || forecast.Windows[0].ExhaustionAt != nil {
		t.Fatalf("past reset was treated as usable zero burn: %#v", forecast)
	}
}

func TestForecastUsesLatestObservationAsExhaustionAnchor(t *testing.T) {
	service, account, now := forecastService(t)
	defer service.Close()
	seconds := int64(5 * 60 * 60)
	reset := now.Add(24 * time.Hour)
	for i, used := range []float64{10, 20, 30} {
		at := now.Add(-time.Duration(2-i) * time.Hour)
		appendForecastSample(t, service, account.ID, at, used, &reset, &seconds)
	}
	forecast, detail := service.Forecast(account.ID)
	if detail != nil || len(forecast.Windows) != 1 {
		t.Fatalf("forecast = %#v, detail = %#v", forecast, detail)
	}
	window := forecast.Windows[0]
	if window.Status != "steady" || window.RatePercentPerHour == nil || *window.RatePercentPerHour != 10 {
		t.Fatalf("window = %#v", window)
	}
	want := now.Add(7 * time.Hour)
	if window.ExhaustionAt == nil || !window.ExhaustionAt.Equal(want) {
		t.Fatalf("exhaustion = %v, want %v", window.ExhaustionAt, want)
	}
}

func TestForecastCappedDemandDoesNotDiluteRate(t *testing.T) {
	service, account, now := forecastService(t)
	defer service.Close()
	for i, used := range []float64{80, 100, 100, 100} {
		appendForecastSample(t, service, account.ID, now.Add(-time.Duration(3-i)*time.Hour), used, nil, nil)
	}
	forecast, detail := service.Forecast(account.ID)
	if detail != nil || len(forecast.Windows) != 1 {
		t.Fatalf("forecast = %#v, detail = %#v", forecast, detail)
	}
	window := forecast.Windows[0]
	if window.Status != "exhausted" || window.RatePercentPerHour == nil || *window.RatePercentPerHour != 20 {
		t.Fatalf("capped rate = %#v", window)
	}
	if !hasDiagnostic(window.Diagnostics, "USAGE_CAPPED_DEMAND_CENSORED") || forecast.Status != "estimated_qualified" {
		t.Fatalf("capped qualification missing: forecast = %#v, window = %#v", forecast, window)
	}
}

func TestForecastReportsResetBeforeExhaustionAndStalePastReset(t *testing.T) {
	service, account, now := forecastService(t)
	defer service.Close()
	seconds := int64(5 * 60 * 60)
	reset := now.Add(time.Hour)
	for i, used := range []float64{10, 20, 30} {
		at := now.Add(-time.Duration(2-i) * time.Hour)
		appendForecastSample(t, service, account.ID, at, used, &reset, &seconds)
	}
	forecast, detail := service.Forecast(account.ID)
	if detail != nil || forecast.Windows[0].Status != "resets_before_exhaustion" || forecast.Windows[0].ExhaustionAt != nil {
		t.Fatalf("reset forecast = %#v, detail = %#v", forecast, detail)
	}
	service.history.Append(account.ID, model.UsageSample{AccountID: account.ID, Provider: account.Provider, ObservedAt: now.Add(time.Minute), Windows: []model.Window{{ID: "primary", UsedPercent: 100, ResetsAt: ptrTime(now.Add(-time.Hour)), WindowSeconds: &seconds}}})
	forecast, detail = service.Forecast(account.ID)
	if detail != nil || forecast.Windows[0].Status != "stale" || forecast.Windows[0].ExhaustionAt != nil {
		t.Fatalf("stale forecast = %#v, detail = %#v", forecast, detail)
	}
}

func TestForecastPreservesReauthStatus(t *testing.T) {
	service, account, now := forecastService(t)
	defer service.Close()
	service.state[account.ID].mu.Lock()
	service.state[account.ID].status = model.StatusReauthRequired
	service.state[account.ID].mu.Unlock()
	appendForecastSample(t, service, account.ID, now.Add(-2*time.Hour), 10, nil, nil)
	appendForecastSample(t, service, account.ID, now.Add(-time.Hour), 20, nil, nil)
	forecast, detail := service.Forecast(account.ID)
	if detail != nil || forecast.Status != "reauth_required" || forecast.AccountStatus != model.StatusReauthRequired {
		t.Fatalf("reauth forecast = %#v, detail = %#v", forecast, detail)
	}
}

func TestReOnboardPurgeFailureLeavesOriginalCredential(t *testing.T) {
	adapter := &fakeAdapter{provider: model.ProviderCodex, loginValue: []byte("new")}
	service, detail := OpenService(t.TempDir(), []provider.Adapter{adapter}, idleChecker{})
	if detail != nil {
		t.Fatal(detail)
	}
	defer service.Close()
	home := t.TempDir()
	credential := filepath.Join(home, "auth.json")
	if err := os.WriteFile(credential, []byte("old"), 0600); err != nil {
		t.Fatal(err)
	}
	account, detail := service.Adopt(context.Background(), model.ProviderCodex, "re-onboard", model.StoreBinding{Kind: "file", Home: home, CredentialPath: credential})
	if detail != nil {
		t.Fatal(detail)
	}
	service.history.path = filepath.Join(service.stateDir, "missing", "usage-history.json")
	job, detail := service.Jobs().StartReOnboard(account.ID)
	if detail != nil {
		t.Fatal(detail)
	}
	job = waitForJob(t, service, job.ID)
	if job.State != "failed" || job.Error == nil || job.Error.Code != "REGISTRY_COMMIT_FAILED" {
		t.Fatalf("job = %#v", job)
	}
	got, err := os.ReadFile(credential)
	if err != nil || string(got) != "old" {
		t.Fatalf("credential after failed purge = %q, %v", got, err)
	}
}

func TestForecastReportsZeroBurnDeclineAndInsufficientSpan(t *testing.T) {
	service, account, now := forecastService(t)
	defer service.Close()
	appendForecastSample(t, service, account.ID, now.Add(-30*time.Minute), 10, nil, nil)
	appendForecastSample(t, service, account.ID, now, 10, nil, nil)
	forecast, _ := service.Forecast(account.ID)
	if forecast.Windows[0].Status != "insufficient_history" {
		t.Fatalf("short flat forecast = %#v", forecast.Windows[0])
	}
	service, account, now = forecastService(t)
	defer service.Close()
	appendForecastSample(t, service, account.ID, now.Add(-90*time.Minute), 10, nil, nil)
	appendForecastSample(t, service, account.ID, now.Add(-30*time.Minute), 10, nil, nil)
	forecast, _ = service.Forecast(account.ID)
	if forecast.Windows[0].Status != "zero_burn" || forecast.Windows[0].RatePercentPerHour != nil {
		t.Fatalf("flat forecast = %#v", forecast.Windows[0])
	}
	service, account, now = forecastService(t)
	defer service.Close()
	appendForecastSample(t, service, account.ID, now.Add(-2*time.Hour), 10, nil, nil)
	appendForecastSample(t, service, account.ID, now.Add(-time.Hour), 20, nil, nil)
	appendForecastSample(t, service, account.ID, now, 5, nil, nil)
	forecast, _ = service.Forecast(account.ID)
	if forecast.Windows[0].Status != "unknown_reset" {
		t.Fatalf("decline forecast = %#v", forecast.Windows[0])
	}
}

func TestForecastDoesNotBridgePlanOrMissingWindowSeries(t *testing.T) {
	service, account, now := forecastService(t)
	defer service.Close()
	planA := "plan-a"
	planB := "plan-b"
	appendForecastPlanSample(t, service, account.ID, now.Add(-4*time.Hour), 0, &planA, "primary", nil)
	appendForecastPlanSample(t, service, account.ID, now.Add(-3*time.Hour), 80, &planB, "primary", nil)
	appendForecastPlanSample(t, service, account.ID, now.Add(-2*time.Hour), 10, &planA, "primary", nil)
	appendForecastPlanSample(t, service, account.ID, now.Add(-time.Hour), 20, &planA, "primary", nil)
	appendForecastPlanSample(t, service, account.ID, now, 30, &planA, "primary", nil)
	forecast, detail := service.Forecast(account.ID)
	if detail != nil || forecast.Windows[0].RatePercentPerHour == nil || *forecast.Windows[0].RatePercentPerHour != 10 {
		t.Fatalf("plan series was bridged: %#v, detail = %#v", forecast, detail)
	}

	service, account, now = forecastService(t)
	defer service.Close()
	appendForecastPlanSample(t, service, account.ID, now.Add(-3*time.Hour), 0, nil, "primary", nil)
	appendForecastPlanSample(t, service, account.ID, now.Add(-2*time.Hour), 0, nil, "secondary", nil)
	appendForecastPlanSample(t, service, account.ID, now.Add(-time.Hour), 10, nil, "primary", nil)
	appendForecastPlanSample(t, service, account.ID, now, 20, nil, "primary", nil)
	forecast, detail = service.Forecast(account.ID)
	if detail != nil || forecast.Windows[0].RatePercentPerHour == nil || *forecast.Windows[0].RatePercentPerHour != 10 {
		t.Fatalf("missing window was bridged: %#v, detail = %#v", forecast, detail)
	}

	service, account, now = forecastService(t)
	defer service.Close()
	shortWindow := int64(5 * 60 * 60)
	longWindow := int64(7 * 24 * 60 * 60)
	appendForecastPlanSample(t, service, account.ID, now.Add(-3*time.Hour), 0, nil, "primary", &shortWindow)
	appendForecastPlanSample(t, service, account.ID, now.Add(-2*time.Hour), 90, nil, "primary", &longWindow)
	appendForecastPlanSample(t, service, account.ID, now.Add(-time.Hour), 10, nil, "primary", &shortWindow)
	appendForecastPlanSample(t, service, account.ID, now, 20, nil, "primary", &shortWindow)
	forecast, detail = service.Forecast(account.ID)
	if detail != nil || forecast.Windows[0].RatePercentPerHour == nil || *forecast.Windows[0].RatePercentPerHour != 10 {
		t.Fatalf("duration change was bridged: %#v, detail = %#v", forecast, detail)
	}
}

func TestAggregateForecastUsesEarliestPoolReset(t *testing.T) {
	service, accounts, now := aggregateForecastService(t)
	defer service.Close()
	seconds := int64(5 * 60 * 60)
	resetA := now.Add(2 * time.Hour)
	resetB := now.Add(100 * time.Hour)
	for i, account := range accounts {
		if i == 0 {
			appendForecastWindow(t, service, account.ID, now.Add(-time.Hour), 80, "private", &seconds, &resetA)
			appendForecastWindow(t, service, account.ID, now, 90, "private", &seconds, &resetA)
		} else {
			appendForecastWindow(t, service, account.ID, now.Add(-time.Hour), 9, "private", &seconds, &resetB)
			appendForecastWindow(t, service, account.ID, now, 10, "private", &seconds, &resetB)
		}
	}
	aggregate, detail := service.AggregateForecast()
	if detail != nil || len(aggregate.Pools) != 1 {
		t.Fatalf("aggregate = %#v, detail = %#v", aggregate, detail)
	}
	pool := aggregate.Pools[0]
	if pool.Status != "resets_before_exhaustion" || pool.ExhaustionAt != nil || pool.CurrentWorkloadRatePercentPointsPerHour == nil || *pool.CurrentWorkloadRatePercentPointsPerHour != 11 {
		t.Fatalf("pool reset bound = %#v", pool)
	}

	service, accounts, now = aggregateForecastService(t)
	defer service.Close()
	resetA = now.Add(10 * time.Hour)
	resetB = now.Add(100 * time.Hour)
	for i, account := range accounts {
		if i == 0 {
			appendForecastWindow(t, service, account.ID, now.Add(-time.Hour), 9, "private", &seconds, &resetA)
			appendForecastWindow(t, service, account.ID, now, 10, "private", &seconds, &resetA)
		} else {
			appendForecastWindow(t, service, account.ID, now.Add(-time.Hour), 80, "private", &seconds, &resetB)
			appendForecastWindow(t, service, account.ID, now, 90, "private", &seconds, &resetB)
		}
	}
	aggregate, detail = service.AggregateForecast()
	if detail != nil || len(aggregate.Pools) != 1 || aggregate.Pools[0].Status != "estimated" || aggregate.Pools[0].ExhaustionAt == nil {
		t.Fatalf("pool valid before earliest reset = %#v, detail = %#v", aggregate, detail)
	}
}

func TestAggregateForecastIncludesZeroBurnAndCappedQualification(t *testing.T) {
	service, accounts, now := aggregateForecastService(t)
	defer service.Close()
	seconds := int64(5 * 60 * 60)
	for i, account := range accounts {
		if i == 0 {
			appendForecastWindow(t, service, account.ID, now.Add(-time.Hour), 0, "private", &seconds, nil)
			appendForecastWindow(t, service, account.ID, now, 0, "private", &seconds, nil)
		} else {
			for j, used := range []float64{80, 100, 100, 100} {
				appendForecastWindow(t, service, account.ID, now.Add(-time.Duration(3-j)*time.Hour), used, "private", &seconds, nil)
			}
		}
	}
	aggregate, detail := service.AggregateForecast()
	if detail != nil || len(aggregate.Pools) != 1 {
		t.Fatalf("aggregate = %#v, detail = %#v", aggregate, detail)
	}
	pool := aggregate.Pools[0]
	if pool.MeasuredMemberCount != 2 || pool.MissingRateMemberCount != 0 || pool.Status != "estimated" || pool.ExhaustionAt == nil || !hasDiagnostic(pool.Diagnostics, "POOL_CAPPED_DEMAND_CENSORED") {
		t.Fatalf("zero burn or cap was mishandled: %#v", pool)
	}
}

func forecastService(t *testing.T) (*Service, model.Account, time.Time) {
	t.Helper()
	now := time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)
	adapter := &fakeAdapter{provider: model.ProviderCodex, usageSample: &model.UsageSample{Provider: model.ProviderCodex, ObservedAt: now, Windows: []model.Window{{ID: "primary", UsedPercent: 10}}}}
	service, detail := OpenService(t.TempDir(), []provider.Adapter{adapter}, idleChecker{})
	if detail != nil {
		t.Fatal(detail)
	}
	service.SetClockForTests(func() time.Time { return now })
	home := t.TempDir()
	credential := filepath.Join(home, "auth.json")
	if err := os.WriteFile(credential, []byte("credential"), 0600); err != nil {
		t.Fatal(err)
	}
	account, detail := service.Adopt(context.Background(), model.ProviderCodex, "forecast", model.StoreBinding{Kind: "file", Home: home, CredentialPath: credential})
	if detail != nil {
		t.Fatal(detail)
	}
	if err := service.history.Delete(account.ID); err != nil {
		t.Fatal(err)
	}
	return service, account, now
}

func aggregateForecastService(t *testing.T) (*Service, []model.Account, time.Time) {
	t.Helper()
	now := time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)
	adapter := &fakeAdapter{provider: model.ProviderCodex, usageSample: &model.UsageSample{Provider: model.ProviderCodex, ObservedAt: now, Windows: []model.Window{{ID: "primary", UsedPercent: 10}}}}
	service, detail := OpenService(t.TempDir(), []provider.Adapter{adapter}, idleChecker{})
	if detail != nil {
		t.Fatal(detail)
	}
	service.SetClockForTests(func() time.Time { return now })
	accounts := make([]model.Account, 0, 2)
	for _, label := range []string{"one", "two"} {
		home := t.TempDir()
		credential := filepath.Join(home, "auth.json")
		if err := os.WriteFile(credential, []byte("credential"), 0600); err != nil {
			t.Fatal(err)
		}
		account, detail := service.Adopt(context.Background(), model.ProviderCodex, label, model.StoreBinding{Kind: "file", Home: home, CredentialPath: credential})
		if detail != nil {
			t.Fatal(detail)
		}
		if err := service.history.Delete(account.ID); err != nil {
			t.Fatal(err)
		}
		accounts = append(accounts, account)
	}
	return service, accounts, now
}

func appendForecastSample(t *testing.T, service *Service, accountID string, at time.Time, used float64, reset *time.Time, seconds *int64) {
	t.Helper()
	if err := service.history.Append(accountID, model.UsageSample{AccountID: accountID, Provider: model.ProviderCodex, ObservedAt: at, Windows: []model.Window{{ID: "primary", UsedPercent: used, ResetsAt: reset, WindowSeconds: seconds}}}); err != nil && err != errUsageHistoryUnchanged {
		t.Fatal(err)
	}
}

func appendForecastPlanSample(t *testing.T, service *Service, accountID string, at time.Time, used float64, plan *string, windowID string, seconds *int64) {
	t.Helper()
	if err := service.history.Append(accountID, model.UsageSample{AccountID: accountID, Provider: model.ProviderCodex, Plan: plan, ObservedAt: at, Windows: []model.Window{{ID: windowID, UsedPercent: used, WindowSeconds: seconds}}}); err != nil && err != errUsageHistoryUnchanged {
		t.Fatal(err)
	}
}

func appendForecastWindow(t *testing.T, service *Service, accountID string, at time.Time, used float64, plan string, seconds *int64, reset *time.Time) {
	t.Helper()
	if err := service.history.Append(accountID, model.UsageSample{AccountID: accountID, Provider: model.ProviderCodex, Plan: &plan, ObservedAt: at, Windows: []model.Window{{ID: "primary", UsedPercent: used, WindowSeconds: seconds, ResetsAt: reset}}}); err != nil && err != errUsageHistoryUnchanged {
		t.Fatal(err)
	}
}

func ptrTime(value time.Time) *time.Time { return &value }

func containsJSONText(b []byte, value string) bool {
	return len(b) > 0 && stringContains(string(b), value)
}

func stringContains(value, term string) bool {
	for i := 0; i+len(term) <= len(value); i++ {
		if value[i:i+len(term)] == term {
			return true
		}
	}
	return false
}
