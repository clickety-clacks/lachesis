package claude

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/clickety-clacks/lachesis/internal/model"
	"github.com/clickety-clacks/lachesis/internal/provider"
	"github.com/clickety-clacks/lachesis/internal/teach"
)

const usageURL = "https://api.anthropic.com/api/oauth/usage"
const tokenURL = "https://console.anthropic.com/v1/oauth/token"
const clientID = "9d1c250a-e61b-44d9-88ed-5944d1962f5e"

type Adapter struct {
	Client *http.Client
	Now    func() time.Time
}

func New(client *http.Client) *Adapter {
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	return &Adapter{Client: client, Now: time.Now}
}
func (*Adapter) Name() model.Provider { return model.ProviderClaude }
func (*Adapter) CLIAvailable() bool   { _, err := exec.LookPath("claude"); return err == nil }
func (*Adapter) DefaultBinding() (model.StoreBinding, *model.ErrorDetail) {
	home := strings.TrimSpace(os.Getenv("CLAUDE_CONFIG_DIR"))
	if home == "" {
		return model.StoreBinding{}, detail(teach.KeychainSourceUnsupported, "The default Claude login is not an MVP file store.")
	}
	if !filepath.IsAbs(home) {
		return model.StoreBinding{}, detail(teach.InvalidRequest, "CLAUDE_CONFIG_DIR must name an absolute provider home.")
	}
	home = filepath.Clean(home)
	return model.StoreBinding{Kind: "file", Home: home, CredentialPath: filepath.Join(home, ".credentials.json")}, nil
}
func (*Adapter) ManagedBinding(home string) model.StoreBinding {
	return model.StoreBinding{Kind: "file", Home: home, CredentialPath: filepath.Join(home, ".credentials.json")}
}

type credentialDoc struct {
	OAuth struct {
		Access  string   `json:"accessToken"`
		Refresh string   `json:"refreshToken"`
		Expires int64    `json:"expiresAt"`
		Scopes  []string `json:"scopes"`
	} `json:"claudeAiOauth"`
}

func (a *Adapter) ParseCredential(raw []byte) (provider.Credential, *model.ErrorDetail) {
	var d credentialDoc
	if json.Unmarshal(raw, &d) != nil {
		return provider.Credential{}, detail(teach.CredentialRejected, "The Claude credential is not valid JSON.")
	}
	if d.OAuth.Access == "" || d.OAuth.Refresh == "" || d.OAuth.Expires <= 0 {
		return provider.Credential{}, detail(teach.CredentialRejected, "The Claude credential lacks required OAuth fields.")
	}
	if !contains(d.OAuth.Scopes, "user:profile") {
		return provider.Credential{}, teach.New(teach.TokenScopeInsufficient, "The Claude credential lacks the user:profile scope.", "re-onboard", nil, map[string]any{"required_scopes": []string{"user:profile"}, "present_scopes": d.OAuth.Scopes}, nil, "POST /api/v1/accounts/{id}/re-onboard")
	}
	return provider.Credential{Raw: append([]byte(nil), raw...), AccessToken: d.OAuth.Access, RefreshToken: d.OAuth.Refresh, Expiry: time.UnixMilli(d.OAuth.Expires).UTC(), Scopes: append([]string(nil), d.OAuth.Scopes...)}, nil
}

func (a *Adapter) Usage(ctx context.Context, c provider.Credential) (*model.UsageSample, *model.ErrorDetail) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, usageURL, nil)
	req.Header.Set("Authorization", "Bearer "+c.AccessToken)
	req.Header.Set("anthropic-beta", "oauth-2025-04-20")
	resp, err := a.Client.Do(req)
	if err != nil {
		return nil, upstream(err)
	}
	defer resp.Body.Close()
	var raw json.RawMessage
	if json.NewDecoder(resp.Body).Decode(&raw) != nil {
		return nil, detail(teach.UpstreamContractChanged, "Claude returned a non-JSON usage response.")
	}
	if resp.StatusCode == 401 || resp.StatusCode == 403 {
		return nil, detail(teach.CredentialRejected, "Claude rejected the credential.")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, detail(teach.UpstreamUnavailable, fmt.Sprintf("Claude usage returned HTTP %d.", resp.StatusCode))
	}
	if !provider.RawUsageSafe(raw) {
		return nil, detail(teach.UpstreamContractChanged, "Claude usage contained a credential-bearing field.")
	}
	return normalize(raw, a.Now())
}

func (a *Adapter) Refresh(ctx context.Context, c provider.Credential) ([]byte, *model.ErrorDetail) {
	body, _ := json.Marshal(map[string]any{"grant_type": "refresh_token", "refresh_token": c.RefreshToken, "client_id": clientID})
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, tokenURL, strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	resp, err := a.Client.Do(req)
	if err != nil {
		return nil, upstream(err)
	}
	defer resp.Body.Close()
	var result struct {
		Access  string `json:"access_token"`
		Refresh string `json:"refresh_token"`
		Expires int64  `json:"expires_in"`
		Scope   string `json:"scope"`
	}
	if json.NewDecoder(resp.Body).Decode(&result) != nil {
		return nil, detail(teach.UpstreamContractChanged, "Claude returned a malformed token response.")
	}
	if resp.StatusCode == 400 || resp.StatusCode == 401 {
		return nil, detail(teach.RefreshRejected, "Claude rejected the refresh token.")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, detail(teach.UpstreamUnavailable, fmt.Sprintf("Claude refresh returned HTTP %d.", resp.StatusCode))
	}
	if result.Access == "" || result.Refresh == "" || result.Expires <= 0 {
		return nil, detail(teach.UpstreamContractChanged, "Claude refresh omitted a required token field.")
	}
	resultScopes := strings.Fields(result.Scope)
	if !contains(resultScopes, "user:profile") {
		return nil, detail(teach.TokenScopeInsufficient, "The refreshed Claude credential lacks user:profile.")
	}
	var doc credentialDoc
	if json.Unmarshal(c.Raw, &doc) != nil {
		return nil, detail(teach.CredentialRejected, "The original Claude credential is malformed.")
	}
	doc.OAuth.Access = result.Access
	doc.OAuth.Refresh = result.Refresh
	doc.OAuth.Expires = a.Now().Add(time.Duration(result.Expires) * time.Second).UnixMilli()
	doc.OAuth.Scopes = resultScopes
	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return nil, detail(teach.UpstreamContractChanged, "The refreshed Claude credential cannot be encoded.")
	}
	return append(out, '\n'), nil
}
func (*Adapter) StartLogin(ctx context.Context, home string) (provider.LoginProcess, *model.ErrorDetail) {
	p, err := provider.StartCommand(ctx, "claude", []string{"auth", "login", "--claudeai"}, append(os.Environ(), "CLAUDE_CONFIG_DIR="+home))
	if err != nil {
		return nil, detail(teach.CLIMissing, "Claude login could not start.")
	}
	return p, nil
}

type bucket struct {
	Utilization *float64   `json:"utilization"`
	ResetsAt    *time.Time `json:"resets_at"`
}

func normalize(raw json.RawMessage, observed time.Time) (*model.UsageSample, *model.ErrorDetail) {
	var doc struct {
		Five   json.RawMessage `json:"five_hour"`
		Seven  json.RawMessage `json:"seven_day"`
		Sonnet json.RawMessage `json:"seven_day_sonnet"`
		Limits json.RawMessage `json:"limits"`
	}
	if !jsonObject(raw) || json.Unmarshal(raw, &doc) != nil {
		return nil, detail(teach.UpstreamContractChanged, "Claude usage did not match the adapter contract.")
	}
	windows := []model.Window{}
	diagnostics := []model.Diagnostic{}
	seen := map[string]bool{}
	for _, candidate := range []struct {
		id, name string
		raw      json.RawMessage
	}{{"five_hour", "Five hour", doc.Five}, {"seven_day", "Seven day", doc.Seven}, {"seven_day_sonnet", "Seven day Sonnet", doc.Sonnet}} {
		if len(candidate.raw) == 0 {
			continue
		}
		if w, ok := decodeBucket(candidate.id, candidate.name, candidate.raw); ok {
			windows = append(windows, w)
			seen[candidate.id] = true
		} else {
			diagnostics = append(diagnostics, claudeWindowDiagnostic())
		}
	}
	if len(doc.Limits) > 0 {
		switch {
		case jsonObject(doc.Limits):
			var limits map[string]json.RawMessage
			if json.Unmarshal(doc.Limits, &limits) != nil {
				diagnostics = append(diagnostics, claudeWindowDiagnostic())
				break
			}
			keys := make([]string, 0, len(limits))
			for key := range limits {
				keys = append(keys, key)
			}
			sort.Strings(keys)
			for _, key := range keys {
				id := "limit:" + strings.ReplaceAll(key, "/", ":")
				w, ok := decodeBucket(id, key, limits[key])
				if !ok || seen[id] {
					diagnostics = append(diagnostics, claudeWindowDiagnostic())
					continue
				}
				windows = append(windows, w)
				seen[id] = true
			}
		case jsonArray(doc.Limits):
			var limits []json.RawMessage
			if json.Unmarshal(doc.Limits, &limits) != nil {
				diagnostics = append(diagnostics, claudeWindowDiagnostic())
				break
			}
			for range limits {
				diagnostics = append(diagnostics, claudeWindowDiagnostic())
			}
		default:
			diagnostics = append(diagnostics, claudeWindowDiagnostic())
		}
	}
	if len(windows) == 0 {
		return nil, noValidWindowDetail()
	}
	return &model.UsageSample{Provider: model.ProviderClaude, ObservedAt: observed.UTC(), Windows: windows, Diagnostics: diagnostics, Raw: append(json.RawMessage(nil), raw...)}, nil
}
func claudeWindow(id, name string, b bucket) (model.Window, bool) {
	if b.Utilization == nil {
		return model.Window{}, false
	}
	p := *b.Utilization
	if p < 0 || p > 100 {
		return model.Window{}, false
	}
	if p <= 1 {
		p *= 100
	}
	return model.Window{ID: id, Name: name, UsedPercent: p, ResetsAt: b.ResetsAt}, true
}
func decodeBucket(id, name string, raw json.RawMessage) (model.Window, bool) {
	var decoded bucket
	if !jsonObject(raw) || json.Unmarshal(raw, &decoded) != nil {
		return model.Window{}, false
	}
	return claudeWindow(id, name, decoded)
}
func jsonObject(raw json.RawMessage) bool {
	trimmed := strings.TrimSpace(string(raw))
	return len(trimmed) > 0 && trimmed[0] == '{'
}
func jsonArray(raw json.RawMessage) bool {
	trimmed := strings.TrimSpace(string(raw))
	return len(trimmed) > 0 && trimmed[0] == '['
}
func claudeWindowDiagnostic() model.Diagnostic {
	return model.Diagnostic{Code: "CLAUDE_USAGE_WINDOW_OMITTED", Message: "Claude omitted an invalid or unrecognized usage window."}
}
func noValidWindowDetail() *model.ErrorDetail {
	d := teach.New(
		teach.UpstreamContractChanged,
		"Claude usage contained no valid recognized limit window.",
		"usage",
		[]model.Prerequisite{{Code: "VALID_RECOGNIZED_WINDOW", Description: "The provider response contains at least one valid recognized usage window.", Met: false}},
		map[string]any{"provider": model.ProviderClaude},
		nil,
		"retry the exact call",
	)
	d.Remedy.Summary = "Retry the exact call. If the error repeats, update Lachesis before trusting provider usage."
	return d
}
func contains(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}
func detail(code, msg string) *model.ErrorDetail {
	return teach.New(code, msg, "usage", nil, nil, nil, "retry the exact call")
}
func upstream(err error) *model.ErrorDetail {
	if os.IsTimeout(err) || strings.Contains(strings.ToLower(err.Error()), "deadline") {
		return detail(teach.UpstreamTimeout, "The Claude request timed out.")
	}
	return detail(teach.UpstreamUnavailable, "Claude is unavailable.")
}
