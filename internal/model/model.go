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
	Code          string         `json:"code"`
	Message       string         `json:"message"`
	Prerequisites []Prerequisite `json:"prerequisites"`
	State         map[string]any `json:"state"`
	Remedy        Remedy         `json:"remedy"`
	Help          string         `json:"help"`
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
