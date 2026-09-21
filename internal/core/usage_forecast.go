package core

import (
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/clickety-clacks/lachesis/internal/model"
	"github.com/clickety-clacks/lachesis/internal/teach"
)

const (
	forecastShortLookback = 24 * time.Hour
	forecastLongLookback  = 48 * time.Hour
	forecastMinCoverage   = time.Hour
	forecastMaxGap        = 2 * time.Hour
	forecastStaleAfter    = 30 * time.Minute
)

type forecastPoint struct {
	at       time.Time
	used     float64
	resetsAt *time.Time
	duration *int64
}

type forecastSegment struct {
	points     []forecastPoint
	hadDecline bool
	resetKnown bool
	generation string
}

type forecastRate struct {
	rate     float64
	coverage time.Duration
	lookback time.Duration
	samples  int
	valid    bool
	zero     bool
	gapped   bool
	censored bool
}

// Forecast returns a forecast from persisted observations. It does not make a
// provider request, so asking for a forecast never refreshes credentials or
// changes cache state.
func (s *Service) Forecast(id string) (model.UsageForecast, *model.ErrorDetail) {
	row, ok := s.registry.Find(id)
	if !ok {
		return model.UsageForecast{}, teach.AccountMissing(id, s.KnownIDs())
	}
	history, found := s.history.Get(id)
	if !found {
		history = model.UsageHistory{AccountID: id, Provider: row.Provider}
	}
	forecast := model.UsageForecast{
		AccountID:          id,
		Provider:           row.Provider,
		GeneratedAt:        s.now().UTC(),
		Status:             "insufficient_history",
		HistorySampleCount: len(history.Samples),
		Windows:            []model.UsageForecastWindow{},
		Diagnostics:        []model.Diagnostic{},
	}
	forecast.HistoryError = s.historyErrorString()
	if plan := historyPlan(history); plan != nil {
		forecast.Plan = cloneString(plan)
	}
	if history.LatestObservedAt != nil {
		latest := history.LatestObservedAt.UTC()
		forecast.LatestObservedAt = &latest
	}
	if view, detail := s.Get(id); detail == nil {
		forecast.AccountStatus = view.Status
		if sample, _ := s.cache.Peek(id); sample != nil {
			age := sample.AgeSeconds
			forecast.CurrentSampleAgeSecs = &age
		}
	}
	if forecast.AccountStatus == model.StatusReauthRequired {
		forecast.Status = "reauth_required"
	} else if forecast.LatestObservedAt != nil && forecast.GeneratedAt.Sub(*forecast.LatestObservedAt) > forecastStaleAfter {
		forecast.Status = "stale"
	}
	if len(history.Samples) == 0 {
		return forecast, nil
	}

	latestSample := history.Samples[len(history.Samples)-1]
	planChanged := false
	for _, sample := range history.Samples[:len(history.Samples)-1] {
		if !samePlan(sample.Plan, latestSample.Plan) {
			planChanged = true
			break
		}
	}
	if planChanged {
		forecast.Diagnostics = append(forecast.Diagnostics, model.Diagnostic{Code: "USAGE_PLAN_CHANGED", Message: "Older observations use a different plan and were excluded from the rate."})
	}
	if latestSample.Plan != nil {
		forecast.Plan = cloneString(latestSample.Plan)
	}
	windowStale := false
	var earliest *time.Time
	var limiting model.UsageForecastWindow
	unknownWindow := false
	allZeroBurn := true
	for _, latestWindow := range latestSample.Windows {
		windowForecast := forecastWindow(history.Samples, latestSample, latestWindow, forecast.GeneratedAt)
		if windowForecast.Status != "zero_burn" {
			allZeroBurn = false
		}
		if windowForecast.Status == "stale" {
			windowStale = true
		}
		if planChanged {
			windowForecast.Diagnostics = append(windowForecast.Diagnostics, model.Diagnostic{Code: "USAGE_PLAN_CHANGED", Message: "Older observations use a different plan and were excluded from the rate."})
			if windowForecast.Status == "insufficient_history" {
				windowForecast.Status = "plan_changed"
			}
		}
		forecast.Windows = append(forecast.Windows, windowForecast)
		forecast.CoverageHours = math.Max(forecast.CoverageHours, windowForecast.Coverage48hHours)
		forecast.LookbackHours = math.Max(forecast.LookbackHours, windowForecast.Lookback48hHours)
		switch windowForecast.Status {
		case "steady":
			if windowForecast.ExhaustionAt != nil && (earliest == nil || windowForecast.ExhaustionAt.Before(*earliest)) {
				at := windowForecast.ExhaustionAt.UTC()
				earliest = &at
				limiting = windowForecast
			}
		case "exhausted":
			at := forecast.GeneratedAt
			if earliest == nil || at.Before(*earliest) {
				earliest = &at
				limiting = windowForecast
			}
		case "resets_before_exhaustion":
			unknownWindow = true
		case "zero_burn":
			// A measured idle window contributes capacity without imposing a
			// finite depletion time.
		default:
			unknownWindow = true
		}
		for _, diagnostic := range windowForecast.Diagnostics {
			if diagnostic.Code == "USAGE_CAPPED_DEMAND_CENSORED" || diagnostic.Code == "USAGE_UNKNOWN_RESET" {
				unknownWindow = true
			}
		}
		if windowForecast.Status == "steady" || windowForecast.Status == "exhausted" || windowForecast.Status == "resets_before_exhaustion" {
			if forecast.Status != "stale" && forecast.Status != "reauth_required" {
				forecast.Status = "estimated"
			}
		}
	}
	if len(forecast.Windows) == 0 {
		forecast.Status = "windows_missing"
	}
	if len(forecast.Windows) > 0 && allZeroBurn && forecast.AccountStatus != model.StatusReauthRequired && forecast.Status != "stale" {
		forecast.Status = "zero_burn"
	}
	if windowStale && forecast.AccountStatus != model.StatusReauthRequired {
		forecast.Status = "stale"
	}
	if earliest != nil {
		forecast.ExhaustionAt = earliest
		forecast.LimitingWindowID = limiting.ID
		forecast.LimitingWindowName = limiting.Name
		if unknownWindow && forecast.Status != "stale" && forecast.Status != "reauth_required" {
			forecast.Status = "estimated_qualified"
			forecast.Qualification = "At least one other usage window has no finite estimate."
		}
	}
	if forecast.HistoryError != "" && forecast.Status != "stale" && forecast.Status != "reauth_required" {
		forecast.Status = "history_persistence_error"
	}
	return forecast, nil
}

func forecastWindow(samples []model.UsageHistorySample, latestSample model.UsageHistorySample, latest model.Window, now time.Time) model.UsageForecastWindow {
	out := model.UsageForecastWindow{
		ID:               latest.ID,
		Name:             latest.Name,
		WindowSeconds:    cloneInt64(latest.WindowSeconds),
		UsedPercent:      latest.UsedPercent,
		RemainingPercent: math.Max(0, 100-latest.UsedPercent),
		ResetsAt:         cloneTime(latest.ResetsAt),
		Status:           "insufficient_history",
		Generation:       generationLabel(latest),
		Diagnostics:      []model.Diagnostic{},
	}
	points := matchingPoints(samples, latestSample, latest)
	if len(points) == 0 {
		return out
	}
	segments := splitForecastSegments(points)
	segment := segments[len(segments)-1]
	anchor := segment.points[len(segment.points)-1].at
	anyDecline := false
	for _, candidate := range segments {
		if candidate.hadDecline {
			anyDecline = true
			break
		}
	}
	unknownDecline := anyDecline && latest.ResetsAt == nil
	if anyDecline {
		out.Diagnostics = append(out.Diagnostics, model.Diagnostic{Code: "USAGE_RESET_OR_DECLINE", Message: "A decrease was observed, so rates do not cross that interval."})
		if unknownDecline {
			out.Diagnostics = append(out.Diagnostics, model.Diagnostic{Code: "USAGE_UNKNOWN_RESET", Message: "The decrease has no reset marker, so only post-decrease observations support a qualified rate."})
		}
	}
	rateSegments := segments
	if unknownDecline {
		rateSegments = []forecastSegment{segment}
	}
	short := rateForSegments(rateSegments, anchor, forecastShortLookback)
	long := rateForSegments(rateSegments, anchor, forecastLongLookback)
	populateRate(&out, short, long)
	if short.censored || long.censored {
		out.Diagnostics = append(out.Diagnostics, model.Diagnostic{Code: "USAGE_CAPPED_DEMAND_CENSORED", Message: "Flat observations at 100% are capped demand, not evidence of zero burn."})
	}
	if latest.ResetsAt != nil && !latest.ResetsAt.After(now) {
		out.Status = "stale"
		out.ExhaustionAt = nil
		return out
	}
	if latest.UsedPercent >= 100 {
		out.Status = "exhausted"
		return out
	}
	rateValue, lookback := selectedRate(short, long)
	if rateValue <= 0 {
		if unknownDecline {
			out.Status = "unknown_reset"
		} else if short.gapped || long.gapped {
			out.Status = "gapped_history"
		} else if short.zero || long.zero {
			out.Status = "zero_burn"
		}
		return out
	}
	out.Status = "steady"
	projectedHours, finite := depletionHours(out.RemainingPercent, rateValue)
	if !finite {
		out.Status = "estimate_out_of_range"
		out.Diagnostics = append(out.Diagnostics, model.Diagnostic{Code: "USAGE_ESTIMATE_OUT_OF_RANGE", Message: "The measured burn is too small for a representable exhaustion time."})
		return out
	}
	if latest.ResetsAt != nil && projectedHours >= latest.ResetsAt.Sub(anchor).Hours() {
		out.Status = "resets_before_exhaustion"
		out.ExhaustionAt = nil
		return out
	}
	exhaustion, bounded := boundedExhaustion(anchor, projectedHours)
	if !bounded {
		out.Status = "estimate_out_of_range"
		out.Diagnostics = append(out.Diagnostics, model.Diagnostic{Code: "USAGE_ESTIMATE_OUT_OF_RANGE", Message: "The measured burn is too small for a representable exhaustion time."})
		return out
	}
	out.ExhaustionAt = &exhaustion
	out.EstimateLookbackHours = int(lookback / time.Hour)
	return out
}

func matchingPoints(samples []model.UsageHistorySample, latestSample model.UsageHistorySample, latest model.Window) []forecastPoint {
	// A plan, duration, or missing-window change breaks the series. Find the
	// last break first so A -> B -> A cannot resurrect old A observations.
	start := 0
	for i, sample := range samples {
		if !samePlan(sample.Plan, latestSample.Plan) {
			start = i + 1
			continue
		}
		found := false
		for _, window := range sample.Windows {
			if window.ID != latest.ID {
				continue
			}
			found = true
			if !sameOptionalInt64(window.WindowSeconds, latest.WindowSeconds) {
				start = i + 1
			}
			break
		}
		if !found {
			start = i + 1
		}
	}
	points := make([]forecastPoint, 0, len(samples)-start)
	for _, sample := range samples[start:] {
		for _, window := range sample.Windows {
			if window.ID != latest.ID || !sameOptionalInt64(window.WindowSeconds, latest.WindowSeconds) {
				continue
			}
			points = append(points, forecastPoint{at: sample.ObservedAt, used: window.UsedPercent, resetsAt: cloneTime(window.ResetsAt), duration: cloneInt64(window.WindowSeconds)})
			break
		}
	}
	sort.SliceStable(points, func(i, j int) bool { return points[i].at.Before(points[j].at) })
	return points
}

func splitForecastSegments(points []forecastPoint) []forecastSegment {
	if len(points) == 0 {
		return nil
	}
	segments := []forecastSegment{{points: []forecastPoint{points[0]}, resetKnown: points[0].resetsAt != nil}}
	for _, point := range points[1:] {
		current := &segments[len(segments)-1]
		prior := current.points[len(current.points)-1]
		generationChanged := !sameResetPoint(prior, point)
		declined := point.used+0.000001 < prior.used
		if generationChanged || declined {
			current.hadDecline = current.hadDecline || declined
			segments = append(segments, forecastSegment{points: []forecastPoint{point}, resetKnown: point.resetsAt != nil})
			continue
		}
		current.points = append(current.points, point)
	}
	for i := range segments {
		segments[i].generation = fmt.Sprintf("%d", segments[i].points[0].at.Unix())
		if segments[i].resetKnown {
			segments[i].generation = fmt.Sprintf("%s:%d", segments[i].generation, segments[i].points[0].resetsAt.Unix())
		}
	}
	return segments
}

func sameResetPoint(a, b forecastPoint) bool {
	if a.resetsAt == nil || b.resetsAt == nil {
		return a.resetsAt == nil && b.resetsAt == nil
	}
	delta := a.resetsAt.Sub(*b.resetsAt)
	if delta < 0 {
		delta = -delta
	}
	tolerance := time.Minute
	if a.duration != nil && *a.duration > 0 {
		candidate := time.Duration(*a.duration) * time.Second / 100
		if candidate > tolerance {
			tolerance = candidate
		}
		if max := time.Duration(*a.duration) * time.Second / 4; tolerance > max {
			tolerance = max
		}
	}
	return delta <= tolerance
}

func rateForSegments(segments []forecastSegment, anchor time.Time, lookback time.Duration) forecastRate {
	rate := forecastRate{}
	start := anchor.Add(-lookback)
	var totalDelta float64
	var covered time.Duration
	var first, last time.Time
	for _, segment := range segments {
		points := make([]forecastPoint, 0, len(segment.points))
		for _, point := range segment.points {
			if !point.at.Before(start) && !point.at.After(anchor) {
				points = append(points, point)
			}
		}
		rate.samples += len(points)
		for i := 1; i < len(points); i++ {
			elapsed := points[i].at.Sub(points[i-1].at)
			if elapsed <= 0 {
				continue
			}
			if elapsed > forecastMaxGap {
				rate.gapped = true
				continue
			}
			if points[i-1].used >= 100 && points[i].used >= 100 {
				rate.censored = true
				continue
			}
			if first.IsZero() || points[i-1].at.Before(first) {
				first = points[i-1].at
			}
			if points[i].at.After(last) {
				last = points[i].at
			}
			covered += elapsed
			if points[i].used >= points[i-1].used {
				totalDelta += points[i].used - points[i-1].used
			}
		}
	}
	rate.coverage = covered
	if !first.IsZero() {
		rate.lookback = last.Sub(first)
	}
	if covered < forecastMinCoverage {
		return rate
	}
	if totalDelta <= 0 {
		rate.zero = true
		return rate
	}
	rate.rate = totalDelta / covered.Hours()
	rate.valid = rate.rate > 0 && !math.IsNaN(rate.rate) && !math.IsInf(rate.rate, 0)
	return rate
}

func populateRate(out *model.UsageForecastWindow, short, long forecastRate) {
	out.Coverage24hHours = short.coverage.Hours()
	out.Coverage48hHours = long.coverage.Hours()
	out.Lookback24hHours = short.lookback.Hours()
	out.Lookback48hHours = long.lookback.Hours()
	out.SampleCount24h = short.samples
	out.SampleCount48h = long.samples
	if short.valid {
		rate := short.rate
		out.Rate24hPercentPerHour = &rate
	}
	if long.valid {
		rate := long.rate
		out.Rate48hPercentPerHour = &rate
		out.RatePercentPerHour = &rate
	} else if short.valid {
		rate := short.rate
		out.RatePercentPerHour = &rate
	}
}

func selectedRate(short, long forecastRate) (float64, time.Duration) {
	if long.valid {
		return long.rate, forecastLongLookback
	}
	if short.valid {
		return short.rate, forecastShortLookback
	}
	return 0, 0
}

func depletionHours(remaining, rate float64) (float64, bool) {
	if remaining < 0 || rate <= 0 || math.IsNaN(remaining) || math.IsNaN(rate) || math.IsInf(remaining, 0) || math.IsInf(rate, 0) {
		return 0, false
	}
	hours := remaining / rate
	if hours < 0 || math.IsNaN(hours) || math.IsInf(hours, 0) {
		return 0, false
	}
	return hours, true
}

func boundedExhaustion(anchor time.Time, hours float64) (time.Time, bool) {
	if hours < 0 || math.IsNaN(hours) || math.IsInf(hours, 0) {
		return time.Time{}, false
	}
	const maxDuration = time.Duration(1<<63 - 1)
	nanos := hours * float64(time.Hour)
	if math.IsNaN(nanos) || math.IsInf(nanos, 0) || nanos >= float64(maxDuration) {
		return time.Time{}, false
	}
	return anchor.Add(time.Duration(nanos)), true
}

func generationLabel(window model.Window) string {
	if window.ResetsAt != nil {
		return fmt.Sprintf("reset:%d", window.ResetsAt.Unix())
	}
	return "unknown-reset"
}

func cloneTime(value *time.Time) *time.Time {
	if value == nil {
		return nil
	}
	out := value.UTC()
	return &out
}

func cloneInt64(value *int64) *int64 {
	if value == nil {
		return nil
	}
	out := *value
	return &out
}

func historyPlan(h model.UsageHistory) *string {
	if len(h.Samples) == 0 {
		return nil
	}
	return h.Samples[len(h.Samples)-1].Plan
}
