package core

import (
	"time"

	"github.com/clickety-clacks/lachesis/internal/model"
)

func (s *Service) usageSummary(row model.RegistryAccount, result model.UsageResult) model.UsageSummary {
	var previous *model.UsageHistorySample
	if result.Sample == nil && s.history != nil {
		if history, ok := s.history.Get(row.ID); ok && len(history.Samples) > 0 {
			last := history.Samples[len(history.Samples)-1]
			previous = &last
		}
	}
	return buildUsageSummary(row, s.accountView(row), result, previous)
}

func buildUsageSummary(row model.RegistryAccount, account model.Account, result model.UsageResult, previous *model.UsageHistorySample) model.UsageSummary {
	summary := model.UsageSummary{
		Label:              row.Label,
		Provider:           row.Provider,
		AccountState:       account.Status,
		UsageFreshness:     "none",
		SubscriptionStatus: "unknown",
	}

	if account.Status != model.StatusReady {
		if account.LastError != nil && account.LastError.Code != "" {
			summary.AccountStateReason = stringPointer(account.LastError.Code)
		} else {
			summary.AccountStateReason = stringPointer("not_yet_checked")
		}
	}

	switch {
	case result.Status == "error":
		summary.UsageFreshness = "none"
		if result.Error != nil && result.Error.Code != "" {
			summary.UsageFreshnessReason = stringPointer(result.Error.Code)
		} else {
			summary.UsageFreshnessReason = stringPointer("not_yet_observed")
		}
	case result.Sample != nil && result.Error != nil:
		summary.UsageFreshness = "stale"
		summary.UsageFreshnessReason = stringPointer(result.Error.Code)
	case result.Sample != nil:
		summary.UsageFreshness = "current"
	default:
		summary.UsageFreshness = "none"
		if result.Error != nil && result.Error.Code != "" {
			summary.UsageFreshnessReason = stringPointer(result.Error.Code)
		} else {
			summary.UsageFreshnessReason = stringPointer("not_yet_observed")
		}
	}

	var successfulRead bool
	var planObservedAt *time.Time
	if result.Sample != nil {
		successfulRead = true
		observedAt := result.Sample.ObservedAt.UTC()
		summary.UsageObservedAt = &observedAt
		ageSeconds := result.Sample.AgeSeconds
		summary.UsageAgeSeconds = &ageSeconds
		if result.Sample.Plan != nil {
			plan := *result.Sample.Plan
			summary.Plan = &plan
			planObservedAt = &observedAt
		}
	} else if previous != nil {
		successfulRead = true
		if previous.Plan != nil {
			plan := *previous.Plan
			summary.Plan = &plan
			observedAt := previous.ObservedAt.UTC()
			planObservedAt = &observedAt
		}
	}

	if summary.Plan == nil {
		summary.PlanUnknownReason = stringPointer(unknownReason(successfulRead, result.Error))
	}

	// Codex's existing sample carries its provider plan value. No current
	// adapter reads a provider statement for paid validity or period end. A
	// literal free plan is the only validity fact represented by an existing
	// provider statement.
	if summary.Plan != nil && *summary.Plan == "free" {
		summary.SubscriptionStatus = "free"
		summary.SubscriptionStatusReason = nil
		summary.SubscriptionPeriodEndUnknownReason = nil
		summary.SubscriptionObservedAt = planObservedAt
	} else {
		reason := unknownReason(successfulRead, result.Error)
		summary.SubscriptionStatusReason = stringPointer(reason)
		summary.SubscriptionPeriodEndUnknownReason = stringPointer(reason)
		summary.SubscriptionObservedAt = planObservedAt
	}
	return summary
}

func unknownReason(successfulRead bool, readError *model.ErrorDetail) string {
	if successfulRead {
		return "not_exposed_by_provider"
	}
	if readError != nil {
		return "read_failed"
	}
	return "not_yet_observed"
}

func stringPointer(value string) *string { return &value }
