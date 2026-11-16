package core

import (
	"time"

	"github.com/clickety-clacks/lachesis/internal/model"
)

func (s *Service) usageSummary(row model.RegistryAccount, result model.UsageResult, generatedAt time.Time) model.UsageSummary {
	var previous *model.UsageHistorySample
	if result.Sample == nil && s.history != nil {
		if history, ok := s.history.Get(row.ID); ok && len(history.Samples) > 0 {
			last := history.Samples[len(history.Samples)-1]
			previous = &last
		}
	}
	if result.SummaryError != nil {
		result.Error = result.SummaryError
	}
	return buildUsageSummary(row, s.accountView(row), result, previous, generatedAt)
}

func buildUsageSummary(row model.RegistryAccount, account model.Account, result model.UsageResult, previous *model.UsageHistorySample, generatedAt time.Time) model.UsageSummary {
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

	var priorUsageRead bool
	var planObservedAt *time.Time
	var subscription *model.UsageSubscription
	if result.Sample != nil {
		priorUsageRead = true
		observedAt := result.Sample.ObservedAt.UTC()
		summary.UsageObservedAt = &observedAt
		ageSeconds := result.Sample.AgeSeconds
		summary.UsageAgeSeconds = &ageSeconds
		subscription = result.Sample.Subscription
		if subscription != nil && subscription.Plan != nil && row.Provider == model.ProviderClaude {
			plan := *subscription.Plan
			summary.Plan = &plan
			planObservedAt = subscription.ObservedAt
			if planObservedAt == nil {
				planObservedAt = &observedAt
			}
		} else if result.Sample.Plan != nil {
			plan := *result.Sample.Plan
			summary.Plan = &plan
			planObservedAt = &observedAt
		}
	} else if previous != nil {
		priorUsageRead = true
		if previous.Plan != nil {
			plan := *previous.Plan
			summary.Plan = &plan
			observedAt := previous.ObservedAt.UTC()
			planObservedAt = &observedAt
		}
	}

	if summary.Plan == nil {
		if subscription != nil && subscription.PlanUnknownReason != nil {
			summary.PlanUnknownReason = copyString(subscription.PlanUnknownReason)
		} else {
			summary.PlanUnknownReason = stringPointer(unknownReason(priorUsageRead, result.Error))
		}
	}

	sourceReason := subscriptionReadReason(result.Sample != nil, result.Error)
	if subscription != nil && subscription.ObservedAt != nil {
		observedAt := subscription.ObservedAt.UTC()
		summary.SubscriptionObservedAt = &observedAt
	} else if planObservedAt != nil {
		summary.SubscriptionObservedAt = planObservedAt
	}

	if subscription != nil && subscription.PeriodEndAt != nil {
		periodEnd := subscription.PeriodEndAt.UTC()
		summary.SubscriptionPeriodEndAt = &periodEnd
		kind := "unstated"
		if subscription.PeriodEndKind != nil {
			kind = *subscription.PeriodEndKind
		}
		summary.SubscriptionPeriodEndKind = stringPointer(kind)
	}

	summary.SubscriptionStatus, summary.SubscriptionStatusReason = subscriptionStatus(
		summary.Plan,
		subscription,
		generatedAt.UTC(),
		sourceReason,
	)

	if summary.SubscriptionPeriodEndAt == nil {
		if summary.SubscriptionStatus == "free" {
			summary.SubscriptionPeriodEndUnknownReason = nil
		} else if subscription != nil && subscription.PeriodEndUnknownReason != nil {
			summary.SubscriptionPeriodEndUnknownReason = copyString(subscription.PeriodEndUnknownReason)
		} else {
			summary.SubscriptionPeriodEndUnknownReason = stringPointer(sourceReason)
		}
	}
	return summary
}

func subscriptionStatus(plan *string, source *model.UsageSubscription, generatedAt time.Time, sourceReason string) (string, *string) {
	periodEnd := (*time.Time)(nil)
	statementAt := (*time.Time)(nil)
	directStatus := ""
	var directStatusReason *string
	if source != nil {
		periodEnd = source.PeriodEndAt
		statementAt = source.ObservedAt
		directStatus = source.Status
		directStatusReason = source.StatusReason
	}

	if directStatus == "lapsed" {
		return "lapsed", nil
	}
	if periodEnd != nil {
		if statementAt == nil || statementAt.IsZero() {
			reason := "unrecognized_value"
			return "unknown", &reason
		}
		if periodEnd.Before(*statementAt) {
			return "lapsed", nil
		}
		if periodEnd.Equal(*statementAt) {
			reason := "unrecognized_value"
			return "unknown", &reason
		}
		if periodEnd.Before(generatedAt) {
			reason := "period_end_passed"
			return "unknown", &reason
		}
		return "active", nil
	}

	switch directStatus {
	case "active":
		return "active", nil
	case "free":
		return "free", nil
	case "unknown", "":
	default:
		reason := "unrecognized_value"
		return "unknown", &reason
	}

	if plan != nil && *plan == "free" {
		return "free", nil
	}
	if directStatus == "unknown" && directStatusReason != nil {
		return "unknown", copyString(directStatusReason)
	}
	reason := sourceReason
	if source != nil && source.StatusReason != nil {
		reason = *source.StatusReason
	}
	return "unknown", &reason
}

func subscriptionReadReason(samplePresent bool, readError *model.ErrorDetail) string {
	if samplePresent {
		return "not_exposed_by_provider"
	}
	if readError != nil {
		return "read_failed"
	}
	return "not_yet_observed"
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

func copyString(value *string) *string {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}

func stringPointer(value string) *string { return &value }
