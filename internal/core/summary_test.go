package core

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/clickety-clacks/lachesis/internal/model"
)

func TestUsageSummaryAlwaysSerializesAllFields(t *testing.T) {
	row := summaryTestRow()
	summary := buildUsageSummary(row, model.Account{Status: model.StatusUnknown}, model.UsageResult{Status: "error"}, nil)

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
	summary := buildUsageSummary(summaryTestRow(), account, result, nil)

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
	summary := buildUsageSummary(summaryTestRow(), account, result, nil)

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
	summary := buildUsageSummary(summaryTestRow(), account, result, nil)

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
	summary := buildUsageSummary(summaryTestRow(), model.Account{Status: model.StatusReady}, result, nil)

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

func summaryTestRow() model.RegistryAccount {
	return model.RegistryAccount{Label: "work", Provider: model.ProviderCodex}
}

func pointerValue(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}
