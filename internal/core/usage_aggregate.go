package core

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/clickety-clacks/lachesis/internal/model"
)

// AggregateForecast combines current-workload forecasts only when the
// provider, plan, window identity, and window duration describe the same
// quota. It does not choose an account or route work.
func (s *Service) AggregateForecast() (model.AggregateForecast, *model.ErrorDetail) {
	rows := s.registry.Snapshot().Accounts
	out := model.AggregateForecast{
		GeneratedAt: s.now().UTC(),
		Status:      "unavailable",
		Accounts:    []model.UsageForecast{},
		Pools:       []model.UsageForecastPool{},
		Assumptions: []string{
			"Pool rates assume recent demand continues at the measured current-workload rate.",
			"Pool estimates assume work can be redistributed among accounts in the same comparable pool.",
			"Multiple windows can overlap or constrain the same account, so pooled ETA is optimistic redistribution arithmetic, not a routing guarantee.",
			"Providers, plans, window IDs, and window durations remain separate quotas.",
		},
		Diagnostics: []model.Diagnostic{},
	}
	if len(rows) == 0 {
		out.Diagnostics = append(out.Diagnostics, model.Diagnostic{Code: "NO_ACCOUNTS", Message: "No registered accounts have forecast capacity."})
		return out, nil
	}
	type poolIndex struct {
		index int
	}
	pools := map[string]poolIndex{}
	accountIncomplete := false
	providerSet := map[model.Provider]bool{}
	for _, row := range rows {
		forecast, detail := s.Forecast(row.ID)
		if detail != nil {
			accountIncomplete = true
			continue
		}
		out.Accounts = append(out.Accounts, forecast)
		providerSet[row.Provider] = true
		if forecast.Status == "stale" || forecast.Status == "reauth_required" || forecast.Status == "insufficient_history" || forecast.Status == "windows_missing" || forecast.Status == "history_persistence_error" || forecast.Status == "estimated_qualified" || forecast.AccountStatus != model.StatusReady {
			accountIncomplete = true
		}
		if len(forecast.Windows) == 0 {
			accountIncomplete = true
		}
		for _, window := range forecast.Windows {
			key, compatibility, comparable := forecastPoolKey(row.ID, forecast, window)
			idx, exists := pools[key]
			if !exists {
				pool := model.UsageForecastPool{
					Key:           key,
					Provider:      forecast.Provider,
					Plan:          cloneString(forecast.Plan),
					PlanKnown:     forecast.Plan != nil && strings.TrimSpace(*forecast.Plan) != "",
					WindowID:      window.ID,
					WindowName:    window.Name,
					WindowSeconds: cloneInt64(window.WindowSeconds),
					Compatibility: compatibility,
					Comparable:    comparable,
					Members:       []model.UsageForecastMember{},
					Status:        "estimated",
					Diagnostics:   []model.Diagnostic{},
				}
				out.Pools = append(out.Pools, pool)
				idx = poolIndex{index: len(out.Pools) - 1}
				pools[key] = idx
			}
			pool := &out.Pools[idx.index]
			if forecast.Status == "estimated_qualified" {
				pool.Diagnostics = append(pool.Diagnostics, model.Diagnostic{Code: "POOL_ACCOUNT_QUALIFIED", Message: "The account has another usage window without a finite estimate; this pool remains usable on its own."})
			}
			memberStatus := aggregateMemberStatus(forecast, window)
			pool.Members = append(pool.Members, forecastMember(forecast, row, window, memberStatus))
			pool.RemainingPercentPoints += window.RemainingPercent
			if pool.CapacityMemberCount == 0 {
				pool.CoverageHours = window.Coverage48hHours
				pool.LookbackHours = window.Lookback48hHours
			} else {
				pool.CoverageHours = minNonNegative(pool.CoverageHours, window.Coverage48hHours)
				pool.LookbackHours = minNonNegative(pool.LookbackHours, window.Lookback48hHours)
			}
			pool.CapacityMemberCount++
			if window.RatePercentPerHour != nil && memberStatus != "stale" && memberStatus != "reauth_required" && memberStatus != "history_persistence_error" && memberStatus != "account_degraded" && memberStatus != "estimated_qualified" {
				pool.MeasuredMemberCount++
				addPoolRate(pool, window)
			} else if window.Status == "zero_burn" && memberStatus == "zero_burn" {
				pool.MeasuredMemberCount++
			} else {
				pool.MissingRateMemberCount++
			}
			if memberStatus == "stale" || memberStatus == "unknown_reset" || memberStatus == "gapped_history" || memberStatus == "plan_changed" || memberStatus == "insufficient_history" || memberStatus == "estimate_out_of_range" || memberStatus == "history_persistence_error" || memberStatus == "account_degraded" || memberStatus == "estimated_qualified" {
				markPoolIncomplete(pool)
				accountIncomplete = true
			}
			if memberStatus == "reauth_required" {
				markPoolReauth(pool)
				accountIncomplete = true
			}
			if window.Status == "resets_before_exhaustion" {
				markPoolReset(pool)
			}
			if hasDiagnostic(window.Diagnostics, "USAGE_CAPPED_DEMAND_CENSORED") {
				pool.Diagnostics = append(pool.Diagnostics, model.Diagnostic{Code: "POOL_CAPPED_DEMAND_CENSORED", Message: "A member reached 100%; flat capped intervals do not measure demand."})
			}
		}
	}
	if len(providerSet) > 1 {
		out.Diagnostics = append(out.Diagnostics, model.Diagnostic{Code: "SEPARATE_PROVIDERS", Message: "Codex and Claude quotas are shown in separate pools and are never added together."})
	}
	comparablePool := false
	for i := range out.Pools {
		pool := &out.Pools[i]
		if pool.Comparable {
			comparablePool = true
		}
		finalizePool(pool, out.GeneratedAt)
		if pool.Status != "estimated" && pool.Status != "zero_burn" && pool.Status != "resets_before_exhaustion" {
			accountIncomplete = true
		}
	}
	sort.SliceStable(out.Accounts, func(i, j int) bool { return out.Accounts[i].AccountID < out.Accounts[j].AccountID })
	sort.SliceStable(out.Pools, func(i, j int) bool { return out.Pools[i].Key < out.Pools[j].Key })
	if len(out.Pools) == 0 || !comparablePool {
		out.Status = "unavailable"
		out.Diagnostics = append(out.Diagnostics, model.Diagnostic{Code: "NO_COMPARABLE_POOL", Message: "No pool has a known common plan and window duration. Account forecasts remain available separately."})
	} else if accountIncomplete {
		out.Status = "incomplete"
	} else {
		out.Status = "estimated"
	}
	return out, nil
}

func forecastPoolKey(accountID string, forecast model.UsageForecast, window model.UsageForecastWindow) (string, string, bool) {
	if forecast.Plan == nil || strings.TrimSpace(*forecast.Plan) == "" {
		return fmt.Sprintf("%s:unknown-plan:%s:%s", forecast.Provider, accountID, window.ID), "singleton_unknown_plan", false
	}
	if window.WindowSeconds == nil || *window.WindowSeconds <= 0 {
		return fmt.Sprintf("%s:%s:%s:unknown-duration:%s", forecast.Provider, *forecast.Plan, window.ID, accountID), "singleton_unknown_window_duration", false
	}
	return fmt.Sprintf("%s:%s:%s:%d", forecast.Provider, *forecast.Plan, window.ID, *window.WindowSeconds), "same_capacity", true
}

func aggregateMemberStatus(forecast model.UsageForecast, window model.UsageForecastWindow) string {
	if forecast.Status == "reauth_required" {
		return "reauth_required"
	}
	if forecast.Status == "stale" {
		return "stale"
	}
	if forecast.Status == "history_persistence_error" || forecast.HistoryError != "" {
		return "history_persistence_error"
	}
	if forecast.AccountStatus != model.StatusReady {
		return "account_degraded"
	}
	return window.Status
}

func forecastMember(forecast model.UsageForecast, row model.RegistryAccount, window model.UsageForecastWindow, status string) model.UsageForecastMember {
	return model.UsageForecastMember{
		AccountID:             row.ID,
		Label:                 row.Label,
		Status:                status,
		ObservedAt:            cloneTime(forecast.LatestObservedAt),
		RemainingPercent:      window.RemainingPercent,
		ResetsAt:              cloneTime(window.ResetsAt),
		ExhaustionAt:          cloneTime(window.ExhaustionAt),
		RatePercentPerHour:    cloneFloat(window.RatePercentPerHour),
		Rate24hPercentPerHour: cloneFloat(window.Rate24hPercentPerHour),
		Rate48hPercentPerHour: cloneFloat(window.Rate48hPercentPerHour),
		CoverageHours:         window.Coverage48hHours,
		SampleCount:           window.SampleCount48h,
	}
}

func hasDiagnostic(diagnostics []model.Diagnostic, code string) bool {
	for _, diagnostic := range diagnostics {
		if diagnostic.Code == code {
			return true
		}
	}
	return false
}

func markPoolIncomplete(pool *model.UsageForecastPool) {
	if pool.Status == "reauth_required" {
		return
	}
	pool.Status = "incomplete"
}

func markPoolReauth(pool *model.UsageForecastPool) {
	pool.Status = "reauth_required"
}

func markPoolReset(pool *model.UsageForecastPool) {
	if pool.Status == "estimated" || pool.Status == "resets_before_exhaustion" {
		pool.Status = "resets_before_exhaustion"
	}
}

func addPoolRate(pool *model.UsageForecastPool, window model.UsageForecastWindow) {
	pool.CurrentWorkloadRatePercentPointsPerHour = addFloat(pool.CurrentWorkloadRatePercentPointsPerHour, *window.RatePercentPerHour)
	if window.Rate24hPercentPerHour != nil {
		pool.Rate24hPercentPointsPerHour = addFloat(pool.Rate24hPercentPointsPerHour, *window.Rate24hPercentPerHour)
	}
	if window.Rate48hPercentPerHour != nil {
		pool.Rate48hPercentPointsPerHour = addFloat(pool.Rate48hPercentPointsPerHour, *window.Rate48hPercentPerHour)
	}
}

func addFloat(target *float64, value float64) *float64 {
	if target == nil {
		out := value
		return &out
	}
	*target += value
	return target
}

func minNonNegative(left, right float64) float64 {
	if right < left {
		return right
	}
	return left
}

func finalizePool(pool *model.UsageForecastPool, now time.Time) {
	if pool.MissingRateMemberCount > 0 {
		markPoolIncomplete(pool)
		pool.Diagnostics = append(pool.Diagnostics, model.Diagnostic{Code: "POOL_MISSING_BURN", Message: "At least one member has no finite recent burn estimate, so the pool ETA is qualified."})
		return
	}
	if pool.Status == "incomplete" || pool.Status == "reauth_required" {
		return
	}
	if pool.CurrentWorkloadRatePercentPointsPerHour == nil {
		pool.Status = "zero_burn"
		return
	}
	if *pool.CurrentWorkloadRatePercentPointsPerHour <= 0 {
		pool.Status = "zero_burn"
		return
	}
	effectiveRemaining := 0.0
	var earliestReset *time.Time
	for _, member := range pool.Members {
		remaining := member.RemainingPercent
		if member.RatePercentPerHour != nil && member.ObservedAt != nil {
			age := now.Sub(*member.ObservedAt).Hours()
			if age > 0 {
				remaining -= *member.RatePercentPerHour * age
			}
		}
		if remaining < 0 {
			remaining = 0
		}
		effectiveRemaining += remaining
		if member.ResetsAt != nil && member.ResetsAt.After(now) && (earliestReset == nil || member.ResetsAt.Before(*earliestReset)) {
			reset := member.ResetsAt.UTC()
			earliestReset = &reset
		}
	}
	pool.EffectiveRemainingPercentPoints = effectiveRemaining
	projectedHours, bounded := depletionHours(effectiveRemaining, *pool.CurrentWorkloadRatePercentPointsPerHour)
	if !bounded {
		pool.Status = "incomplete"
		pool.Diagnostics = append(pool.Diagnostics, model.Diagnostic{Code: "POOL_ESTIMATE_OUT_OF_RANGE", Message: "The pooled burn is too small for a representable exhaustion time."})
		return
	}
	if earliestReset != nil && projectedHours >= earliestReset.Sub(now).Hours() {
		pool.Status = "resets_before_exhaustion"
		pool.ExhaustionAt = nil
		pool.Diagnostics = append(pool.Diagnostics, model.Diagnostic{Code: "POOL_RESET_BEFORE_EXHAUSTION", Message: "The earliest member reset occurs before the pooled estimate."})
		return
	}
	if pool.Status == "resets_before_exhaustion" {
		pool.Status = "estimated"
	}
	if pool.Status == "incomplete" || pool.Status == "reauth_required" {
		return
	}
	exhaustion, bounded := boundedExhaustion(now, projectedHours)
	if !bounded {
		pool.Status = "incomplete"
		pool.Diagnostics = append(pool.Diagnostics, model.Diagnostic{Code: "POOL_ESTIMATE_OUT_OF_RANGE", Message: "The pooled burn is too small for a representable exhaustion time."})
		return
	}
	pool.ExhaustionAt = &exhaustion
	if strings.Contains(pool.Compatibility, "singleton") {
		pool.Diagnostics = append(pool.Diagnostics, model.Diagnostic{Code: "POOL_COMPATIBILITY_UNKNOWN", Message: "This account is shown alone because plan or window capacity is unknown."})
	}
}

func cloneFloat(value *float64) *float64 {
	if value == nil {
		return nil
	}
	out := *value
	return &out
}
