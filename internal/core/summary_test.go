package core

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/clickety-clacks/lachesis/internal/model"
)

func TestUsageSummaryAlwaysSerializesAllFields(t *testing.T) {
	row := summaryTestRow()
	generatedAt := time.Date(2026, time.November, 15, 12, 0, 0, 0, time.UTC)
	summary := buildUsageSummary(row, model.Account{Status: model.StatusUnknown}, model.UsageResult{Status: "error"}, nil, generatedAt)

	encoded, err := json.Marshal(summary)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &fields); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"label", "provider", "account_state", "account_state_reason",
		"usage_freshness", "usage_freshness_reason", "usage_observed_at", "usage_age_seconds",
		"plan", "plan_unknown_reason", "subscription_status", "subscription_status_reason",
		"subscription_period_end_at", "subscription_period_end_kind", "subscription_period_end_unknown_reason",
		"subscription_observed_at",
	}
	if len(fields) != len(want) {
		t.Fatalf("summary fields = %d, want %d: %s", len(fields), len(want), encoded)
	}
	for _, key := range want {
		if _, ok := fields[key]; !ok {
			t.Errorf("summary field %q is missing: %s", key, encoded)
		}
	}
	for _, key := range []string{"usage_observed_at", "usage_age_seconds", "plan", "subscription_observed_at", "subscription_period_end_at", "subscription_period_end_kind"} {
		if string(fields[key]) != "null" {
			t.Errorf("%s = %s, want explicit null", key, fields[key])
		}
	}
	if summary.AccountState != model.StatusUnknown || pointerValue(summary.AccountStateReason) != "not_yet_checked" {
		t.Fatalf("unchecked account state = %q / %v", summary.AccountState, summary.AccountStateReason)
	}
	if summary.UsageFreshness != "none" || pointerValue(summary.UsageFreshnessReason) != "not_yet_observed" {
		t.Fatalf("unobserved usage = %q / %v", summary.UsageFreshness, summary.UsageFreshnessReason)
	}
	if pointerValue(summary.PlanUnknownReason) != "not_yet_observed" || pointerValue(summary.SubscriptionStatusReason) != "not_yet_observed" || pointerValue(summary.SubscriptionPeriodEndUnknownReason) != "not_yet_observed" {
		t.Fatalf("unobserved provider facts have wrong reasons: %#v", summary)
	}
}

func TestUsageSummaryFirstFailedReadUsesReadFailureReasons(t *testing.T) {
	failure := &model.ErrorDetail{Code: "CREDENTIAL_REJECTED"}
	account := model.Account{Status: model.StatusReauthRequired, LastError: failure}
	result := model.UsageResult{Status: "error", Error: failure}
	generatedAt := time.Date(2026, time.November, 15, 12, 0, 0, 0, time.UTC)
	summary := buildUsageSummary(summaryTestRow(), account, result, nil, generatedAt)

	if summary.AccountState != model.StatusReauthRequired || pointerValue(summary.AccountStateReason) != "CREDENTIAL_REJECTED" {
		t.Fatalf("account state = %q / %v", summary.AccountState, summary.AccountStateReason)
	}
	if summary.UsageFreshness != "none" || pointerValue(summary.UsageFreshnessReason) != "CREDENTIAL_REJECTED" {
		t.Fatalf("usage freshness = %q / %v", summary.UsageFreshness, summary.UsageFreshnessReason)
	}
	if pointerValue(summary.PlanUnknownReason) != "read_failed" || pointerValue(summary.SubscriptionStatusReason) != "read_failed" || pointerValue(summary.SubscriptionPeriodEndUnknownReason) != "read_failed" {
		t.Fatalf("first failed read reasons = %#v", summary)
	}
}

func TestUsageSummaryStaleReadSeparatesAccountStateFromFreshness(t *testing.T) {
	now := time.Date(2026, time.November, 15, 12, 0, 0, 0, time.UTC)
	observedAt := now.Add(-40 * time.Minute)
	age := int64(2400)
	plan := "pro"
	failure := &model.ErrorDetail{Code: "CREDENTIAL_REJECTED"}
	result := model.UsageResult{
		Status: "stale",
		Sample: &model.UsageSample{Plan: &plan, ObservedAt: observedAt, AgeSeconds: age},
		Error:  failure,
	}
	account := model.Account{Status: model.StatusReauthRequired, LastError: failure}
	summary := buildUsageSummary(summaryTestRow(), account, result, nil, now)

	if summary.AccountState != model.StatusReauthRequired || pointerValue(summary.AccountStateReason) != "CREDENTIAL_REJECTED" {
		t.Fatalf("account state = %q / %v", summary.AccountState, summary.AccountStateReason)
	}
	if summary.UsageFreshness != "stale" || pointerValue(summary.UsageFreshnessReason) != "CREDENTIAL_REJECTED" {
		t.Fatalf("usage freshness = %q / %v", summary.UsageFreshness, summary.UsageFreshnessReason)
	}
	if summary.UsageAgeSeconds == nil || *summary.UsageAgeSeconds != age || summary.UsageObservedAt == nil || !summary.UsageObservedAt.Equal(observedAt) {
		t.Fatalf("usage observation = %v / %v", summary.UsageObservedAt, summary.UsageAgeSeconds)
	}
	if summary.Plan == nil || *summary.Plan != plan || summary.PlanUnknownReason != nil {
		t.Fatalf("plan = %v / %v", summary.Plan, summary.PlanUnknownReason)
	}
	if summary.SubscriptionStatus != "unknown" || pointerValue(summary.SubscriptionStatusReason) != "not_exposed_by_provider" {
		t.Fatalf("paid validity was inferred from plan: %q / %v", summary.SubscriptionStatus, summary.SubscriptionStatusReason)
	}
	if result.Status != "stale" {
		t.Fatalf("existing result status changed to %q", result.Status)
	}
}

func TestUsageSummarySuccessfulOldSampleRemainsCurrentDuringRefresh(t *testing.T) {
	now := time.Date(2026, time.November, 15, 12, 0, 0, 0, time.UTC)
	observedAt := now.Add(-time.Minute)
	age := int64(60)
	result := model.UsageResult{
		Status: "stale",
		Sample: &model.UsageSample{ObservedAt: observedAt, AgeSeconds: age},
	}
	account := model.Account{Status: model.StatusReady}
	summary := buildUsageSummary(summaryTestRow(), account, result, nil, now)

	if summary.UsageFreshness != "current" || summary.UsageFreshnessReason != nil {
		t.Fatalf("freshness during pending refresh = %q / %v", summary.UsageFreshness, summary.UsageFreshnessReason)
	}
	if summary.UsageAgeSeconds == nil || *summary.UsageAgeSeconds != age || summary.UsageObservedAt == nil || !summary.UsageObservedAt.Equal(observedAt) {
		t.Fatalf("usage observation = %v / %v", summary.UsageObservedAt, summary.UsageAgeSeconds)
	}
	if result.Status != "stale" {
		t.Fatalf("existing result status changed to %q", result.Status)
	}
}

func TestUsageSummaryFreePlanHasNoPaidPeriod(t *testing.T) {
	observedAt := time.Date(2026, time.November, 15, 12, 0, 0, 0, time.UTC)
	plan := "free"
	result := model.UsageResult{Status: "live", Sample: &model.UsageSample{Plan: &plan, ObservedAt: observedAt}}
	summary := buildUsageSummary(summaryTestRow(), model.Account{Status: model.StatusReady}, result, nil, observedAt)

	if summary.SubscriptionStatus != "free" || summary.SubscriptionStatusReason != nil {
		t.Fatalf("free plan status = %q / %v", summary.SubscriptionStatus, summary.SubscriptionStatusReason)
	}
	if summary.SubscriptionPeriodEndAt != nil || summary.SubscriptionPeriodEndKind != nil || summary.SubscriptionPeriodEndUnknownReason != nil {
		t.Fatalf("free plan has a paid-period end: %#v", summary)
	}
	if summary.SubscriptionObservedAt == nil || !summary.SubscriptionObservedAt.Equal(observedAt) {
		t.Fatalf("subscription observation time = %v", summary.SubscriptionObservedAt)
	}
}

func TestUsageSummaryCodexActiveUntilUsesStatementAndGeneratedTime(t *testing.T) {
	statementAt := time.Date(2026, time.November, 15, 10, 0, 0, 0, time.UTC)
	generatedAt := statementAt.Add(2 * time.Hour)
	periodEnd := time.Date(2026, time.December, 3, 0, 0, 0, 0, time.UTC)
	plan := "pro"
	kind := "unstated"
	result := model.UsageResult{
		Status: "live",
		Sample: &model.UsageSample{
			Plan:       &plan,
			ObservedAt: generatedAt,
			Subscription: &model.UsageSubscription{
				PeriodEndAt:   &periodEnd,
				PeriodEndKind: &kind,
				ObservedAt:    &statementAt,
			},
		},
	}
	summary := buildUsageSummary(summaryTestRow(), model.Account{Status: model.StatusReady}, result, nil, generatedAt)

	if summary.Plan == nil || *summary.Plan != "pro" {
		t.Fatalf("Codex plan = %v, want the usage plan identifier verbatim", summary.Plan)
	}
	if summary.SubscriptionStatus != "active" || summary.SubscriptionStatusReason != nil {
		t.Fatalf("active-until summary status = %q / %v", summary.SubscriptionStatus, summary.SubscriptionStatusReason)
	}
	if summary.SubscriptionPeriodEndAt == nil || !summary.SubscriptionPeriodEndAt.Equal(periodEnd) || pointerValue(summary.SubscriptionPeriodEndKind) != "unstated" || summary.SubscriptionPeriodEndUnknownReason != nil {
		t.Fatalf("period end = %v / %v / %v", summary.SubscriptionPeriodEndAt, summary.SubscriptionPeriodEndKind, summary.SubscriptionPeriodEndUnknownReason)
	}
	if summary.SubscriptionObservedAt == nil || !summary.SubscriptionObservedAt.Equal(statementAt) {
		t.Fatalf("subscription observation = %v, want provider statement time %v", summary.SubscriptionObservedAt, statementAt)
	}
}

func TestUsageSummaryCodexActiveUntilLapsedAndPassedCases(t *testing.T) {
	generatedAt := time.Date(2026, time.December, 5, 12, 0, 0, 0, time.UTC)
	statementAt := time.Date(2026, time.November, 15, 12, 0, 0, 0, time.UTC)
	end := time.Date(2026, time.December, 3, 0, 0, 0, 0, time.UTC)
	kind := "unstated"
	plan := "pro"
	sample := model.UsageSample{
		Plan:       &plan,
		ObservedAt: generatedAt,
		Subscription: &model.UsageSubscription{
			PeriodEndAt:   &end,
			PeriodEndKind: &kind,
			ObservedAt:    &statementAt,
		},
	}
	summary := buildUsageSummary(summaryTestRow(), model.Account{Status: model.StatusReady}, model.UsageResult{Status: "live", Sample: &sample}, nil, generatedAt)
	if summary.SubscriptionStatus != "unknown" || pointerValue(summary.SubscriptionStatusReason) != "period_end_passed" {
		t.Fatalf("passed active-until summary = %q / %v", summary.SubscriptionStatus, summary.SubscriptionStatusReason)
	}
	if summary.SubscriptionPeriodEndAt == nil || !summary.SubscriptionPeriodEndAt.Equal(end) || pointerValue(summary.SubscriptionPeriodEndKind) != "unstated" {
		t.Fatalf("passed statement was discarded: %v / %v", summary.SubscriptionPeriodEndAt, summary.SubscriptionPeriodEndKind)
	}

	lapsedEnd := statementAt.Add(-time.Hour)
	sample.Subscription.PeriodEndAt = &lapsedEnd
	summary = buildUsageSummary(summaryTestRow(), model.Account{Status: model.StatusReady}, model.UsageResult{Status: "live", Sample: &sample}, nil, generatedAt)
	if summary.SubscriptionStatus != "lapsed" || summary.SubscriptionStatusReason != nil {
		t.Fatalf("lapsed-at-statement summary = %q / %v", summary.SubscriptionStatus, summary.SubscriptionStatusReason)
	}
}

func TestUsageSummaryClaudeUsesCredentialPlanAndProfileStatus(t *testing.T) {
	generatedAt := time.Date(2026, time.November, 15, 12, 0, 0, 0, time.UTC)
	observedAt := generatedAt.Add(-time.Minute)
	plan := "max"
	result := model.UsageResult{
		Status: "live",
		Sample: &model.UsageSample{
			Provider:   model.ProviderClaude,
			ObservedAt: generatedAt,
			Subscription: &model.UsageSubscription{
				Plan:                   &plan,
				Status:                 "active",
				PeriodEndUnknownReason: stringPointer("not_yet_observed"),
				ObservedAt:             &observedAt,
			},
		},
	}
	row := summaryTestRow()
	row.Provider = model.ProviderClaude
	summary := buildUsageSummary(row, model.Account{Status: model.StatusReady}, result, nil, generatedAt)

	if summary.Plan == nil || *summary.Plan != "max" || summary.PlanUnknownReason != nil {
		t.Fatalf("Claude plan = %v / %v, want credential identifier", summary.Plan, summary.PlanUnknownReason)
	}
	if summary.SubscriptionStatus != "active" || summary.SubscriptionStatusReason != nil {
		t.Fatalf("Claude profile status = %q / %v", summary.SubscriptionStatus, summary.SubscriptionStatusReason)
	}
	if summary.SubscriptionPeriodEndAt != nil || summary.SubscriptionPeriodEndUnknownReason == nil || *summary.SubscriptionPeriodEndUnknownReason != "not_yet_observed" {
		t.Fatalf("Claude period end = %v / %v", summary.SubscriptionPeriodEndAt, summary.SubscriptionPeriodEndUnknownReason)
	}
	if summary.SubscriptionObservedAt == nil || !summary.SubscriptionObservedAt.Equal(observedAt) {
		t.Fatalf("Claude subscription observation = %v, want %v", summary.SubscriptionObservedAt, observedAt)
	}
}

func TestUsageSummaryStaleAndPendingRefreshPreserveSubscriptionFacts(t *testing.T) {
	generatedAt := time.Date(2026, time.November, 15, 12, 0, 0, 0, time.UTC)
	statementAt := generatedAt.Add(-time.Hour)
	periodEnd := generatedAt.Add(24 * time.Hour)
	kind := "unstated"
	plan := "pro"
	facts := &model.UsageSubscription{PeriodEndAt: &periodEnd, PeriodEndKind: &kind, ObservedAt: &statementAt}
	sample := &model.UsageSample{Plan: &plan, ObservedAt: statementAt, AgeSeconds: 3600, Subscription: facts}
	failure := &model.ErrorDetail{Code: "UPSTREAM_UNAVAILABLE"}
	stale := buildUsageSummary(summaryTestRow(), model.Account{Status: model.StatusDegraded, LastError: failure}, model.UsageResult{Status: "stale", Sample: sample, Error: failure}, nil, generatedAt)
	if stale.UsageFreshness != "stale" || stale.SubscriptionStatus != "active" || stale.SubscriptionPeriodEndAt == nil || !stale.SubscriptionPeriodEndAt.Equal(periodEnd) {
		t.Fatalf("stale summary lost an earlier provider statement: %#v", stale)
	}
	pending := buildUsageSummary(summaryTestRow(), model.Account{Status: model.StatusReady}, model.UsageResult{Status: "stale", Sample: sample}, nil, generatedAt)
	if pending.UsageFreshness != "current" || pending.UsageFreshnessReason != nil || pending.SubscriptionStatus != "active" {
		t.Fatalf("pending refresh summary = %#v", pending)
	}
	if sample.Subscription.PeriodEndAt == nil || !sample.Subscription.PeriodEndAt.Equal(periodEnd) {
		t.Fatal("summary assembly mutated the cached provider statement")
	}
}

func TestBackgroundFreshCacheHitRetainsRecentFailure(t *testing.T) {
	now := time.Date(2026, time.November, 15, 12, 0, 0, 0, time.UTC)
	cache := NewCache()
	cache.now = func() time.Time { return now }
	cache.Install("", model.UsageSample{ObservedAt: now.Add(-20 * time.Second)})
	failure := &model.ErrorDetail{Code: "UPSTREAM_UNAVAILABLE"}
	_, _, started := cache.FetchForced(context.Background(), "", func(context.Context) (*model.UsageSample, *model.ErrorDetail) {
		return nil, failure
	})
	if !started {
		t.Fatal("forced failure check did not start")
	}

	service := &Service{cache: cache, now: func() time.Time { return now }}
	row := model.RegistryAccount{Label: "work", Provider: model.ProviderCodex}
	result := service.usageResult(context.Background(), row, "background")
	if result.Status != "cache" || result.Sample == nil || result.Error != failure {
		t.Fatalf("fresh cache hit lost the last failure or changed result status: %#v", result)
	}
	summary := buildUsageSummary(row, model.Account{Status: model.StatusDegraded, LastError: failure}, result, nil, now)
	if summary.UsageFreshness != "stale" || pointerValue(summary.UsageFreshnessReason) != failure.Code {
		t.Fatalf("fresh cache hit hid the latest completed failure: %q / %v", summary.UsageFreshness, summary.UsageFreshnessReason)
	}
}

func TestUsageSampleOmitsInternalSubscriptionFacts(t *testing.T) {
	plan := "max"
	encoded, err := json.Marshal(model.UsageSample{
		Provider:     model.ProviderClaude,
		Subscription: &model.UsageSubscription{Plan: &plan, Status: "active"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "subscription") || strings.Contains(string(encoded), "max") {
		t.Fatalf("internal subscription facts changed public sample JSON: %s", encoded)
	}
}

func summaryTestRow() model.RegistryAccount {
	return model.RegistryAccount{Label: "work", Provider: model.ProviderCodex}
}

func pointerValue(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}
