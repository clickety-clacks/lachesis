package teach

import (
	"fmt"
	"net/http"

	"github.com/clickety-clacks/lachesis/internal/model"
)

const (
	InvalidRequest                  = "INVALID_REQUEST"
	AccountNotFound                 = "ACCOUNT_NOT_FOUND"
	JobNotFound                     = "JOB_NOT_FOUND"
	HelpTopicNotFound               = "HELP_TOPIC_NOT_FOUND"
	NoAccountsOnboarded             = "NO_ACCOUNTS_ONBOARDED"
	StoreAlreadyRegistered          = "STORE_ALREADY_REGISTERED"
	JobActive                       = "JOB_ACTIVE"
	JobCanceled                     = "JOB_CANCELED"
	JobProcessStopFailed            = "JOB_PROCESS_STOP_FAILED"
	LoginTimeout                    = "LOGIN_TIMEOUT"
	LoginListenerExited             = "LOGIN_LISTENER_EXITED"
	LoginURLUnavailable             = "LOGIN_URL_UNAVAILABLE"
	CredentialChanged               = "CREDENTIAL_CHANGED"
	CredentialStoreBusy             = "CREDENTIAL_STORE_BUSY"
	CredentialExpiryUnknown         = "CREDENTIAL_EXPIRY_UNKNOWN"
	CredentialMissing               = "CREDENTIAL_MISSING"
	CredentialRejected              = "CREDENTIAL_REJECTED"
	TokenScopeInsufficient          = "TOKEN_SCOPE_INSUFFICIENT"
	RefreshRejected                 = "REFRESH_REJECTED"
	RegistryCommitFailed            = "REGISTRY_COMMIT_FAILED"
	CredentialCleanupPending        = "CREDENTIAL_CLEANUP_PENDING"
	CredentialCommitFailed          = "CREDENTIAL_COMMIT_FAILED"
	KeychainAtomicCommitUnavailable = "KEYCHAIN_ATOMIC_COMMIT_UNAVAILABLE"
	KeychainSourceUnsupported       = "KEYCHAIN_SOURCE_UNSUPPORTED"
	UpstreamContractChanged         = "UPSTREAM_CONTRACT_CHANGED"
	CLIMissing                      = "CLI_MISSING"
	UpstreamUnavailable             = "UPSTREAM_UNAVAILABLE"
	UpstreamTimeout                 = "UPSTREAM_TIMEOUT"
)

var status = map[string]int{
	InvalidRequest: http.StatusBadRequest, AccountNotFound: http.StatusNotFound,
	JobNotFound: http.StatusNotFound, HelpTopicNotFound: http.StatusNotFound,
	NoAccountsOnboarded: http.StatusConflict, StoreAlreadyRegistered: http.StatusConflict,
	JobActive: http.StatusConflict, JobCanceled: http.StatusConflict,
	JobProcessStopFailed: http.StatusInternalServerError, LoginTimeout: http.StatusConflict,
	LoginListenerExited: http.StatusBadGateway, LoginURLUnavailable: http.StatusBadGateway, CredentialChanged: http.StatusConflict,
	CredentialStoreBusy: http.StatusConflict, CredentialExpiryUnknown: http.StatusConflict,
	CredentialMissing: http.StatusConflict, CredentialRejected: http.StatusConflict,
	TokenScopeInsufficient: http.StatusConflict, RefreshRejected: http.StatusConflict,
	RegistryCommitFailed:            http.StatusInternalServerError,
	CredentialCleanupPending:        http.StatusInternalServerError,
	CredentialCommitFailed:          http.StatusInternalServerError,
	KeychainAtomicCommitUnavailable: http.StatusInternalServerError,
	KeychainSourceUnsupported:       http.StatusBadRequest,
	UpstreamContractChanged:         http.StatusBadGateway, CLIMissing: http.StatusServiceUnavailable,
	UpstreamUnavailable: http.StatusServiceUnavailable, UpstreamTimeout: http.StatusServiceUnavailable,
}

func Status(code string) int {
	if v, ok := status[code]; ok {
		return v
	}
	return http.StatusInternalServerError
}

func New(code, message, help string, prerequisites []model.Prerequisite, state map[string]any, calls []model.RemedyCall, commands ...string) *model.ErrorDetail {
	if state == nil {
		state = map[string]any{}
	}
	if prerequisites == nil {
		prerequisites = []model.Prerequisite{}
	}
	if calls == nil {
		calls = []model.RemedyCall{}
	}
	if commands == nil {
		commands = []string{}
	}
	return &model.ErrorDetail{Code: code, Message: message, Prerequisites: prerequisites, State: state,
		Remedy: model.Remedy{Summary: remedySummary(code), Calls: calls, Commands: commands}, Help: "/api/v1/help/" + help}
}

func remedySummary(code string) string {
	switch code {
	case CredentialMissing, CredentialRejected, RefreshRejected, TokenScopeInsufficient:
		return "Re-onboard the account."
	case UpstreamUnavailable, UpstreamTimeout:
		return "Retry the exact call."
	case CLIMissing:
		return "Install the provider CLI, then retry."
	case KeychainSourceUnsupported:
		return "Onboard a file-backed account or adopt an explicit provider home."
	default:
		return "Follow one of the concrete next actions."
	}
}

func AccountMissing(id string, known []string) *model.ErrorDetail {
	return New(AccountNotFound, fmt.Sprintf("Account %s is not registered.", id), "accounts",
		[]model.Prerequisite{{Code: "ACCOUNT_EXISTS", Description: "Use a registered account ID.", Met: false}},
		map[string]any{"known_ids": known}, []model.RemedyCall{{Method: "GET", Path: "/api/v1/accounts"}})
}
