package codex

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
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

const usageURL = "https://chatgpt.com/backend-api/wham/usage"
const tokenURL = "https://auth.openai.com/oauth/token"
const clientID = "app_EMoamEEZ73f0CkXaXp7hrann"

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
func (*Adapter) Name() model.Provider { return model.ProviderCodex }
func (*Adapter) CLIAvailable() bool   { _, err := exec.LookPath("codex"); return err == nil }
func (*Adapter) DefaultBinding() (model.StoreBinding, *model.ErrorDetail) {
	h, err := os.UserHomeDir()
	if err != nil {
		return model.StoreBinding{}, detail(teach.CredentialMissing, "The default Codex home cannot be resolved.")
	}
	home := filepath.Join(h, ".codex")
	return model.StoreBinding{Kind: "file", Home: home, CredentialPath: filepath.Join(home, "auth.json")}, nil
}
func (*Adapter) ManagedBinding(home string) model.StoreBinding {
	return model.StoreBinding{Kind: "file", Home: home, CredentialPath: filepath.Join(home, "auth.json")}
}

func (a *Adapter) ParseCredential(raw []byte) (provider.Credential, *model.ErrorDetail) {
	var doc struct {
		Tokens struct {
			Access  string `json:"access_token"`
			Refresh string `json:"refresh_token"`
			ID      string `json:"id_token"`
			Account string `json:"account_id"`
		} `json:"tokens"`
	}
	if json.Unmarshal(raw, &doc) != nil {
		return provider.Credential{}, detail(teach.CredentialRejected, "The Codex credential is not valid JSON.")
	}
	if doc.Tokens.Access == "" || doc.Tokens.Refresh == "" || doc.Tokens.Account == "" {
		return provider.Credential{}, detail(teach.CredentialRejected, "The Codex credential lacks required OAuth fields.")
	}
	exp, ok := jwtExpiry(doc.Tokens.Access)
	if !ok {
		return provider.Credential{}, detail(teach.CredentialExpiryUnknown, "The Codex access token has no usable expiry claim.")
	}
	return provider.Credential{Raw: append([]byte(nil), raw...), AccessToken: doc.Tokens.Access, RefreshToken: doc.Tokens.Refresh, IDToken: doc.Tokens.ID, AccountID: doc.Tokens.Account, Expiry: exp}, nil
}

func (a *Adapter) Usage(ctx context.Context, c provider.Credential) (*model.UsageSample, *model.ErrorDetail) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, usageURL, nil)
	req.Header.Set("Authorization", "Bearer "+c.AccessToken)
	req.Header.Set("chatgpt-account-id", c.AccountID)
	resp, err := a.Client.Do(req)
	if err != nil {
		return nil, upstream(err)
	}
	defer resp.Body.Close()
	var raw json.RawMessage
	if err = json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return nil, detail(teach.UpstreamContractChanged, "Codex returned a non-JSON usage response.")
	}
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return nil, detail(teach.CredentialRejected, "Codex rejected the credential.")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, detail(teach.UpstreamUnavailable, fmt.Sprintf("Codex usage returned HTTP %d.", resp.StatusCode))
	}
	if !provider.RawUsageSafe(raw) {
		return nil, detail(teach.UpstreamContractChanged, "Codex usage contained a credential-bearing field.")
	}
	return normalize(raw, a.Now())
}

func (a *Adapter) Refresh(ctx context.Context, c provider.Credential) ([]byte, *model.ErrorDetail) {
	form := url.Values{"grant_type": {"refresh_token"}, "refresh_token": {c.RefreshToken}, "client_id": {clientID}}
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, tokenURL, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := a.Client.Do(req)
	if err != nil {
		return nil, upstream(err)
	}
	defer resp.Body.Close()
	var result struct {
		Access  string `json:"access_token"`
		Refresh string `json:"refresh_token"`
		ID      string `json:"id_token"`
		Expires int64  `json:"expires_in"`
	}
	if json.NewDecoder(resp.Body).Decode(&result) != nil {
		return nil, detail(teach.UpstreamContractChanged, "Codex returned a malformed token response.")
	}
	if resp.StatusCode == http.StatusBadRequest || resp.StatusCode == http.StatusUnauthorized {
		return nil, detail(teach.RefreshRejected, "Codex rejected the refresh token.")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, detail(teach.UpstreamUnavailable, fmt.Sprintf("Codex refresh returned HTTP %d.", resp.StatusCode))
	}
	if result.Access == "" || result.Refresh == "" || result.Expires <= 0 {
		return nil, detail(teach.UpstreamContractChanged, "Codex refresh omitted a required token field.")
	}
	var doc map[string]any
	if json.Unmarshal(c.Raw, &doc) != nil {
		return nil, detail(teach.CredentialRejected, "The original Codex credential is malformed.")
	}
	tokens, _ := doc["tokens"].(map[string]any)
	if tokens == nil {
		return nil, detail(teach.CredentialRejected, "The original Codex token object is missing.")
	}
	tokens["access_token"] = result.Access
	tokens["refresh_token"] = result.Refresh
	if result.ID != "" {
		tokens["id_token"] = result.ID
	}
	doc["last_refresh"] = a.Now().UTC().Format(time.RFC3339)
	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return nil, detail(teach.UpstreamContractChanged, "The refreshed Codex credential cannot be encoded.")
	}
	return append(out, '\n'), nil
}

func (*Adapter) StartLogin(ctx context.Context, home string) (provider.LoginProcess, *model.ErrorDetail) {
	p, err := provider.StartCommand(ctx, "codex", []string{"login"}, append(os.Environ(), "CODEX_HOME="+home))
	if err != nil {
		return nil, detail(teach.CLIMissing, "Codex login could not start.")
	}
	return p, nil
}

func normalize(raw json.RawMessage, observed time.Time) (*model.UsageSample, *model.ErrorDetail) {
	var doc struct {
		Plan *string `json:"plan_type"`
		Rate struct {
			Primary   window `json:"primary_window"`
			Secondary window `json:"secondary_window"`
		} `json:"rate_limit"`
		Additional []struct {
			Name  string `json:"name"`
			Limit window `json:"rate_limit"`
		} `json:"additional_rate_limits"`
	}
	if json.Unmarshal(raw, &doc) != nil {
		return nil, detail(teach.UpstreamContractChanged, "Codex usage did not match the adapter contract.")
	}
	windows := []model.Window{}
	if w, ok := toWindow("primary", "Primary", doc.Rate.Primary, observed); ok {
		windows = append(windows, w)
	}
	if w, ok := toWindow("secondary", "Secondary", doc.Rate.Secondary, observed); ok {
		windows = append(windows, w)
	}
	sort.SliceStable(doc.Additional, func(i, j int) bool { return doc.Additional[i].Name < doc.Additional[j].Name })
	for _, x := range doc.Additional {
		if w, ok := toWindow("additional:"+slug(x.Name), x.Name, x.Limit, observed); ok {
			windows = append(windows, w)
		}
	}
	if len(windows) == 0 {
		return nil, detail(teach.UpstreamContractChanged, "Codex usage contained no recognized limit window.")
	}
	return &model.UsageSample{Provider: model.ProviderCodex, Plan: doc.Plan, ObservedAt: observed.UTC(), Windows: windows, Raw: append(json.RawMessage(nil), raw...)}, nil
}

type window struct {
	Used       *float64 `json:"used_percent"`
	ResetAt    *int64   `json:"reset_at"`
	ResetAfter *int64   `json:"reset_after_seconds"`
	Seconds    *int64   `json:"limit_window_seconds"`
}

func toWindow(id, name string, w window, observed time.Time) (model.Window, bool) {
	if w.Used == nil {
		return model.Window{}, false
	}
	if *w.Used < 0 || *w.Used > 100 {
		return model.Window{}, false
	}
	var reset *time.Time
	if w.ResetAt != nil {
		t := time.Unix(*w.ResetAt, 0).UTC()
		reset = &t
	} else if w.ResetAfter != nil {
		t := observed.Add(time.Duration(*w.ResetAfter) * time.Second).UTC()
		reset = &t
	}
	return model.Window{ID: id, Name: name, UsedPercent: *w.Used, ResetsAt: reset, WindowSeconds: w.Seconds}, true
}
func slug(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	var b strings.Builder
	dash := false
	for _, r := range s {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			b.WriteRune(r)
			dash = false
		} else if !dash && b.Len() > 0 {
			b.WriteByte('-')
			dash = true
		}
	}
	return strings.TrimRight(b.String(), "-")
}
func jwtExpiry(token string) (time.Time, bool) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return time.Time{}, false
	}
	b, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return time.Time{}, false
	}
	var claims struct {
		Exp int64 `json:"exp"`
	}
	if json.Unmarshal(b, &claims) != nil || claims.Exp <= 0 {
		return time.Time{}, false
	}
	return time.Unix(claims.Exp, 0).UTC(), true
}
func detail(code, msg string) *model.ErrorDetail {
	return teach.New(code, msg, "usage", nil, nil, nil, "retry the exact call")
}
func upstream(err error) *model.ErrorDetail {
	if os.IsTimeout(err) || strings.Contains(strings.ToLower(err.Error()), "deadline") {
		return detail(teach.UpstreamTimeout, "The Codex request timed out.")
	}
	return detail(teach.UpstreamUnavailable, "Codex is unavailable.")
}
