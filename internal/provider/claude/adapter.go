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
	return model.StoreBinding{Kind: "keychain", Service: "Claude Code-credentials", Account: "default"}, nil
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
		Five   *bucket           `json:"five_hour"`
		Seven  *bucket           `json:"seven_day"`
		Sonnet *bucket           `json:"seven_day_sonnet"`
		Limits map[string]bucket `json:"limits"`
	}
	if json.Unmarshal(raw, &doc) != nil {
		return nil, detail(teach.UpstreamContractChanged, "Claude usage did not match the adapter contract.")
	}
	windows := []model.Window{}
	seen := map[string]bool{}
	if len(doc.Limits) > 0 {
		keys := make([]string, 0, len(doc.Limits))
		for k := range doc.Limits {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			w, ok := claudeWindow("limit:"+strings.ReplaceAll(k, "/", ":"), k, doc.Limits[k])
			if !ok {
				return nil, detail(teach.UpstreamContractChanged, "Claude reported an invalid utilization.")
			}
			windows = append(windows, w)
			seen[k] = true
		}
	} else {
		for _, x := range []struct {
			id, name string
			b        *bucket
		}{{"five_hour", "Five hour", doc.Five}, {"seven_day", "Seven day", doc.Seven}, {"seven_day_sonnet", "Seven day Sonnet", doc.Sonnet}} {
			if x.b != nil {
				w, ok := claudeWindow(x.id, x.name, *x.b)
				if !ok {
					return nil, detail(teach.UpstreamContractChanged, "Claude reported an invalid utilization.")
				}
				windows = append(windows, w)
			}
		}
	}
	if len(windows) == 0 {
		return nil, detail(teach.UpstreamContractChanged, "Claude usage contained no recognized limit window.")
	}
	return &model.UsageSample{Provider: model.ProviderClaude, ObservedAt: observed.UTC(), Windows: windows, Raw: append(json.RawMessage(nil), raw...)}, nil
}
func claudeWindow(id, name string, b bucket) (model.Window, bool) {
	if b.Utilization == nil {
		return model.Window{}, false
	}
	p := *b.Utilization * 100
	if p < 0 || p > 100 {
		return model.Window{}, false
	}
	return model.Window{ID: id, Name: name, UsedPercent: p, ResetsAt: b.ResetsAt}, true
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
