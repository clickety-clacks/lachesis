package model

import (
	"encoding/json"
	"time"
)

type Provider string

const (
	ProviderCodex  Provider = "codex"
	ProviderClaude Provider = "claude"
)

func (p Provider) Valid() bool { return p == ProviderCodex || p == ProviderClaude }

type AccountStatus string

const (
	StatusUnknown        AccountStatus = "unknown"
	StatusReady          AccountStatus = "ready"
	StatusDegraded       AccountStatus = "degraded"
	StatusReauthRequired AccountStatus = "reauth_required"
)

type MutationState string

const (
	MutationIdle         MutationState = "idle"
	MutationRefreshing   MutationState = "refreshing"
	MutationReOnboarding MutationState = "re_onboarding"
)

type StoreBinding struct {
	Kind           string `json:"kind"`
	Home           string `json:"home,omitempty"`
	CredentialPath string `json:"credential_path,omitempty"`
	Service        string `json:"service,omitempty"`
	Account        string `json:"account,omitempty"`
}

func (s StoreBinding) CanonicalKey() string {
	if s.Kind == "keychain" {
		return "keychain\x00" + s.Service + "\x00" + s.Account
	}
	return "file\x00" + s.CredentialPath
}

type RegistryAccount struct {
	ID       string       `json:"id"`
	Label    string       `json:"label"`
	Provider Provider     `json:"provider"`
	Store    StoreBinding `json:"store"`
}

type Registry struct {
	Version  int               `json:"version"`
	Accounts []RegistryAccount `json:"accounts"`
}

type Prerequisite struct {
	Code        string `json:"code"`
	Description string `json:"description"`
	Met         bool   `json:"met"`
}

type RemedyCall struct {
	Method string `json:"method"`
	Path   string `json:"path"`
	Body   any    `json:"body,omitempty"`
}

type Remedy struct {
	Summary  string       `json:"summary"`
	Calls    []RemedyCall `json:"calls"`
	Commands []string     `json:"commands"`
}

type ErrorDetail struct {
	Code              string         `json:"code"`
	Message           string         `json:"message"`
	Prerequisites     []Prerequisite `json:"prerequisites"`
	State             map[string]any `json:"state"`
	Remedy            Remedy         `json:"remedy"`
	Help              string         `json:"help"`
	RetryAt           *time.Time     `json:"retry_at,omitempty"`
	RetryAfterSeconds int64          `json:"retry_after_seconds,omitempty"`
}

func (e *ErrorDetail) Error() string { return e.Code + ": " + e.Message }

type ErrorEnvelope struct {
	Error     *ErrorDetail `json:"error"`
	RequestID string       `json:"request_id"`
}

type Account struct {
	ID            string            `json:"id"`
	Label         string            `json:"label"`
	Provider      Provider          `json:"provider"`
	StoreKind     string            `json:"store_kind"`
	Status        AccountStatus     `json:"status"`
	Working       bool              `json:"working"`
	MutationState MutationState     `json:"mutation_state"`
	LastCheckedAt *time.Time        `json:"last_checked_at"`
	LastError     *ErrorDetail      `json:"last_error"`
	Links         map[string]string `json:"links"`
}

type Window struct {
	ID            string     `json:"id"`
	Name          string     `json:"name"`
	UsedPercent   float64    `json:"used_percent"`
	ResetsAt      *time.Time `json:"resets_at"`
	WindowSeconds *int64     `json:"window_seconds"`
}

type Diagnostic struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type UsageSample struct {
	AccountID   string          `json:"account_id"`
	Provider    Provider        `json:"provider"`
	Label       string          `json:"label"`
	Plan        *string         `json:"plan"`
	ObservedAt  time.Time       `json:"observed_at"`
	AgeSeconds  int64           `json:"age_seconds"`
	Windows     []Window        `json:"windows"`
	Diagnostics []Diagnostic    `json:"diagnostics"`
	Raw         json.RawMessage `json:"raw"`
}

// UsageHistorySample is the persisted, provider-neutral subset of a usage
// read. It intentionally has no raw provider payload.
type UsageHistorySample struct {
	AccountID  string    `json:"account_id"`
	Provider   Provider  `json:"provider"`
	Plan       *string   `json:"plan,omitempty"`
	ObservedAt time.Time `json:"observed_at"`
	Windows    []Window  `json:"windows"`
}

type UsageHistory struct {
	AccountID          string               `json:"account_id"`
	Provider           Provider             `json:"provider"`
	Samples            []UsageHistorySample `json:"samples"`
	EarliestObservedAt *time.Time           `json:"earliest_observed_at,omitempty"`
	LatestObservedAt   *time.Time           `json:"latest_observed_at,omitempty"`
}

type UsageForecastWindow struct {
	ID                    string       `json:"id"`
	Name                  string       `json:"name"`
	WindowSeconds         *int64       `json:"window_seconds,omitempty"`
	UsedPercent           float64      `json:"used_percent"`
	RemainingPercent      float64      `json:"remaining_percent"`
	ResetsAt              *time.Time   `json:"resets_at,omitempty"`
	Status                string       `json:"status"`
	RatePercentPerHour    *float64     `json:"rate_percent_per_hour,omitempty"`
	Rate24hPercentPerHour *float64     `json:"rate_24h_percent_per_hour,omitempty"`
	Rate48hPercentPerHour *float64     `json:"rate_48h_percent_per_hour,omitempty"`
	Coverage24hHours      float64      `json:"coverage_24h_hours"`
	Coverage48hHours      float64      `json:"coverage_48h_hours"`
	Lookback24hHours      float64      `json:"lookback_24h_hours"`
	Lookback48hHours      float64      `json:"lookback_48h_hours"`
	SampleCount24h        int          `json:"sample_count_24h"`
	SampleCount48h        int          `json:"sample_count_48h"`
	EstimateLookbackHours int          `json:"estimate_lookback_hours,omitempty"`
	ExhaustionAt          *time.Time   `json:"exhaustion_at,omitempty"`
	Generation            string       `json:"generation"`
	Diagnostics           []Diagnostic `json:"diagnostics,omitempty"`
}

type UsageForecast struct {
	AccountID            string                `json:"account_id"`
	Provider             Provider              `json:"provider"`
	Plan                 *string               `json:"plan,omitempty"`
	GeneratedAt          time.Time             `json:"generated_at"`
	Status               string                `json:"status"`
	AccountStatus        AccountStatus         `json:"account_status"`
	HistorySampleCount   int                   `json:"history_sample_count"`
	CoverageHours        float64               `json:"coverage_hours"`
	LookbackHours        float64               `json:"lookback_hours"`
	LatestObservedAt     *time.Time            `json:"latest_observed_at,omitempty"`
	CurrentSampleAgeSecs *int64                `json:"current_sample_age_seconds,omitempty"`
	ExhaustionAt         *time.Time            `json:"exhaustion_at,omitempty"`
	LimitingWindowID     string                `json:"limiting_window_id,omitempty"`
	LimitingWindowName   string                `json:"limiting_window_name,omitempty"`
	Qualification        string                `json:"qualification,omitempty"`
	HistoryError         string                `json:"history_error,omitempty"`
	Windows              []UsageForecastWindow `json:"windows"`
	Diagnostics          []Diagnostic          `json:"diagnostics,omitempty"`
}

type UsageForecastMember struct {
	AccountID             string     `json:"account_id"`
	Label                 string     `json:"label"`
	Status                string     `json:"status"`
	ObservedAt            *time.Time `json:"observed_at,omitempty"`
	RemainingPercent      float64    `json:"remaining_percent"`
	ResetsAt              *time.Time `json:"resets_at,omitempty"`
	ExhaustionAt          *time.Time `json:"exhaustion_at,omitempty"`
	RatePercentPerHour    *float64   `json:"rate_percent_per_hour,omitempty"`
	Rate24hPercentPerHour *float64   `json:"rate_24h_percent_per_hour,omitempty"`
	Rate48hPercentPerHour *float64   `json:"rate_48h_percent_per_hour,omitempty"`
	CoverageHours         float64    `json:"coverage_hours"`
	SampleCount           int        `json:"sample_count"`
}

type UsageForecastPool struct {
	Key                                     string                `json:"key"`
	Provider                                Provider              `json:"provider"`
	Plan                                    *string               `json:"plan,omitempty"`
	PlanKnown                               bool                  `json:"plan_known"`
	WindowID                                string                `json:"window_id"`
	WindowName                              string                `json:"window_name"`
	WindowSeconds                           *int64                `json:"window_seconds,omitempty"`
	Compatibility                           string                `json:"compatibility"`
	Comparable                              bool                  `json:"comparable"`
	Members                                 []UsageForecastMember `json:"members"`
	RemainingPercentPoints                  float64               `json:"remaining_percent_points"`
	EffectiveRemainingPercentPoints         float64               `json:"effective_remaining_percent_points"`
	CurrentWorkloadRatePercentPointsPerHour *float64              `json:"current_workload_rate_percent_points_per_hour,omitempty"`
	Rate24hPercentPointsPerHour             *float64              `json:"rate_24h_percent_points_per_hour,omitempty"`
	Rate48hPercentPointsPerHour             *float64              `json:"rate_48h_percent_points_per_hour,omitempty"`
	CoverageHours                           float64               `json:"coverage_hours"`
	LookbackHours                           float64               `json:"lookback_hours"`
	MeasuredMemberCount                     int                   `json:"measured_member_count"`
	CapacityMemberCount                     int                   `json:"capacity_member_count"`
	MissingRateMemberCount                  int                   `json:"missing_rate_member_count"`
	Status                                  string                `json:"status"`
	ExhaustionAt                            *time.Time            `json:"exhaustion_at,omitempty"`
	Diagnostics                             []Diagnostic          `json:"diagnostics,omitempty"`
}

type AggregateForecast struct {
	GeneratedAt time.Time           `json:"generated_at"`
	Status      string              `json:"status"`
	Accounts    []UsageForecast     `json:"accounts"`
	Pools       []UsageForecastPool `json:"pools"`
	Assumptions []string            `json:"assumptions"`
	Diagnostics []Diagnostic        `json:"diagnostics,omitempty"`
}

type UsageResult struct {
	AccountID string       `json:"account_id"`
	Status    string       `json:"status"`
	Sample    *UsageSample `json:"sample"`
	Error     *ErrorDetail `json:"error"`
}

type AggregateUsage struct {
	GeneratedAt time.Time      `json:"generated_at"`
	Results     []UsageResult  `json:"results"`
	Counts      map[string]int `json:"counts"`
}

type Job struct {
	ID               string       `json:"id"`
	Kind             string       `json:"kind"`
	Provider         Provider     `json:"provider"`
	AccountID        *string      `json:"account_id"`
	State            string       `json:"state"`
	AuthorizationURL *string      `json:"authorization_url"`
	VerificationURL  *string      `json:"verification_url"`
	UserCode         *string      `json:"user_code"`
	CreatedAt        time.Time    `json:"created_at"`
	UpdatedAt        time.Time    `json:"updated_at"`
	ResultAccount    *Account     `json:"result_account"`
	Error            *ErrorDetail `json:"error"`
}
