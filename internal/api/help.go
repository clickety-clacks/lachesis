package api

type helpTopic struct {
	Topic         string        `json:"topic"`
	Summary       string        `json:"summary"`
	Prerequisites []helpPrereq  `json:"prerequisites"`
	Examples      []helpExample `json:"examples"`
}
type helpPrereq struct {
	Code        string `json:"code"`
	Description string `json:"description"`
}
type helpExample struct {
	Description string `json:"description"`
	Method      string `json:"method"`
	Path        string `json:"path"`
	Body        any    `json:"body"`
}

var topics = map[string]helpTopic{
	"usage":      {"usage", "Read normalized and raw usage for registered accounts.", []helpPrereq{{"ACCOUNT_EXISTS", "Register at least one account."}}, []helpExample{{"Read all usage", "GET", "/api/v1/usage?refresh=background", nil}}},
	"accounts":   {"accounts", "List and deregister accounts without deleting credentials.", nil, []helpExample{{"List accounts", "GET", "/api/v1/accounts", nil}}},
	"adopt":      {"adopt", "Verify and register an existing file-backed provider store.", []helpPrereq{{"CREDENTIAL_EXISTS", "The provider CLI credential already exists in a file-backed home."}}, []helpExample{{"Adopt default Codex", "POST", "/api/v1/accounts/adopt", map[string]any{"provider": "codex", "label": "personal", "source": map[string]any{"kind": "default"}}}, {"Adopt a Claude home", "POST", "/api/v1/accounts/adopt", map[string]any{"provider": "claude", "label": "work", "source": map[string]any{"kind": "home", "path": "/absolute/provider/home"}}}}},
	"onboard":    {"onboard", "Start an isolated browser-login job. A listener that exits before writing a credential fails with LOGIN_LISTENER_EXITED; inspect or cancel the job through the jobs topic.", []helpPrereq{{"CLI_AVAILABLE", "Install the provider CLI."}}, []helpExample{{"Onboard Claude", "POST", "/api/v1/accounts", map[string]any{"provider": "claude", "label": "work"}}, {"Read job help", "GET", "/api/v1/help/jobs", nil}}},
	"jobs":       {"jobs", "Inspect or cancel a login job. JOB_PROCESS_STOP_FAILED and CREDENTIAL_CLEANUP_PENDING retain the active lock; retry cancellation after fixing the reported condition.", nil, []helpExample{{"Inspect job", "GET", "/api/v1/jobs/{id}", nil}, {"Cancel or retry cleanup", "POST", "/api/v1/jobs/{id}/cancel", nil}}},
	"verify":     {"verify", "Check current credential and usage without inference.", []helpPrereq{{"ACCOUNT_EXISTS", "Use a registered account ID."}}, []helpExample{{"Verify", "POST", "/api/v1/accounts/{id}/verify", nil}}},
	"re-onboard": {"re-onboard", "Replace a known account credential through browser login.", []helpPrereq{{"ACCOUNT_EXISTS", "Use a registered account ID."}}, []helpExample{{"Re-onboard", "POST", "/api/v1/accounts/{id}/re-onboard", nil}}},
	"refresh":    {"refresh", "Refresh, commit, and verify a credential.", nil, []helpExample{{"Refresh", "POST", "/api/v1/accounts/{id}/refresh", nil}}},
	"health":     {"health", "Inspect service, provider, and account health.", nil, []helpExample{{"Health", "GET", "/api/v1/health", nil}}},
}
