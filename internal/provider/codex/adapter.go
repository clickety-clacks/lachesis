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
		Plan       *string         `json:"plan_type"`
		Rate       json.RawMessage `json:"rate_limit"`
		Additional json.RawMessage `json:"additional_rate_limits"`
	}
	if !jsonObject(raw) || json.Unmarshal(raw, &doc) != nil {
		return nil, detail(teach.UpstreamContractChanged, "Codex usage did not match the adapter contract.")
	}
	windows := []model.Window{}
	diagnostics := []model.Diagnostic{}
	if len(doc.Rate) > 0 {
		var rate map[string]json.RawMessage
		if !jsonObject(doc.Rate) || json.Unmarshal(doc.Rate, &rate) != nil {
			diagnostics = append(diagnostics, codexWindowDiagnostic())
		} else {
			for _, candidate := range []struct {
				key, id, name string
			}{{"primary_window", "primary", "Primary"}, {"secondary_window", "secondary", "Secondary"}} {
				rawWindow, present := rate[candidate.key]
				if !present {
					continue
				}
				if w, ok := decodeWindow(candidate.id, candidate.name, rawWindow, observed); ok {
					windows = append(windows, w)
				} else {
					diagnostics = append(diagnostics, codexWindowDiagnostic())
				}
			}
		}
	}
	type positionedLimit struct {
		position int
		name     string
		id       string
		window   model.Window
		omitted  bool
	}
	additional := []positionedLimit{}
	if len(doc.Additional) > 0 {
		var entries []json.RawMessage
		if !jsonArray(doc.Additional) || json.Unmarshal(doc.Additional, &entries) != nil {
			diagnostics = append(diagnostics, codexWindowDiagnostic())
		} else {
			additional = make([]positionedLimit, 0, len(entries))
			for i, entry := range entries {
				candidate := positionedLimit{position: i + 1, omitted: true}
				var decoded struct {
					Name  string          `json:"name"`
					Limit json.RawMessage `json:"rate_limit"`
				}
				if jsonObject(entry) && json.Unmarshal(entry, &decoded) == nil && len(decoded.Limit) > 0 {
					candidate.name = decoded.Name
					slugged := slug(decoded.Name)
					candidate.id = "additional:" + slugged
					displayName := decoded.Name
					if slugged == "" {
						candidate.id = fmt.Sprintf("additional:unnamed:%d", candidate.position)
						displayName = fmt.Sprintf("Unnamed additional limit %d", candidate.position)
					}
					if w, ok := decodeWindow(candidate.id, displayName, decoded.Limit, observed); ok {
						candidate.window = w
						candidate.omitted = false
					}
				}
				additional = append(additional, candidate)
			}
		}
	}
	ordered := make([]*positionedLimit, 0, len(additional))
	for i := range additional {
		if !additional[i].omitted {
			ordered = append(ordered, &additional[i])
		}
	}
	sort.SliceStable(ordered, func(i, j int) bool { return ordered[i].name < ordered[j].name })
	seen := map[string]bool{}
	for _, candidate := range ordered {
		if seen[candidate.id] {
			candidate.omitted = true
			continue
		}
		seen[candidate.id] = true
		windows = append(windows, candidate.window)
	}
	for _, candidate := range additional {
		if candidate.omitted {
			diagnostics = append(diagnostics, codexWindowDiagnostic())
		}
	}
	if len(windows) == 0 {
		return nil, noValidWindowDetail()
	}
	return &model.UsageSample{Provider: model.ProviderCodex, Plan: doc.Plan, ObservedAt: observed.UTC(), Windows: windows, Diagnostics: diagnostics, Raw: append(json.RawMessage(nil), raw...)}, nil
}

type window struct {
	Used       *float64 `json:"used_percent"`
	ResetAt    *int64   `json:"reset_at"`
	ResetAfter *int64   `json:"reset_after_seconds"`
	Seconds    *int64   `json:"limit_window_seconds"`
}

func toWindow(id, name string, w window, observed time.Time) (model.Window, bool, *model.ErrorDetail) {
	if w.Used == nil {
		return model.Window{}, false, nil
	}
	if *w.Used < 0 || *w.Used > 100 {
		return model.Window{}, false, detail(teach.UpstreamContractChanged, "Codex reported a percentage outside 0 through 100.")
	}
	var reset *time.Time
	if w.ResetAt != nil {
		t := time.Unix(*w.ResetAt, 0).UTC()
		reset = &t
	} else if w.ResetAfter != nil {
		t := observed.Add(time.Duration(*w.ResetAfter) * time.Second).UTC()
		reset = &t
	}
	return model.Window{ID: id, Name: name, UsedPercent: *w.Used, ResetsAt: reset, WindowSeconds: w.Seconds}, true, nil
}
func decodeWindow(id, name string, raw json.RawMessage, observed time.Time) (model.Window, bool) {
	var decoded window
	if !jsonObject(raw) || json.Unmarshal(raw, &decoded) != nil {
		return model.Window{}, false
	}
	w, ok, d := toWindow(id, name, decoded, observed)
	return w, ok && d == nil
}
func jsonObject(raw json.RawMessage) bool {
	trimmed := strings.TrimSpace(string(raw))
	return len(trimmed) > 0 && trimmed[0] == '{'
}
func jsonArray(raw json.RawMessage) bool {
	trimmed := strings.TrimSpace(string(raw))
	return len(trimmed) > 0 && trimmed[0] == '['
}
func codexWindowDiagnostic() model.Diagnostic {
	return model.Diagnostic{Code: "CODEX_USAGE_WINDOW_OMITTED", Message: "Codex omitted an invalid or unrecognized usage window."}
}
func noValidWindowDetail() *model.ErrorDetail {
	d := teach.New(
		teach.UpstreamContractChanged,
		"Codex usage contained no valid recognized limit window.",
		"usage",
		[]model.Prerequisite{{Code: "VALID_RECOGNIZED_WINDOW", Description: "The provider response contains at least one valid recognized usage window.", Met: false}},
		map[string]any{"provider": model.ProviderCodex},
		nil,
		"retry the exact call",
	)
	d.Remedy.Summary = "Retry the exact call. If the error repeats, update Lachesis before trusting provider usage."
	return d
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
