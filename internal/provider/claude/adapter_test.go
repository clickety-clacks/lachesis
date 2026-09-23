package claude

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/clickety-clacks/lachesis/internal/model"
	"github.com/clickety-clacks/lachesis/internal/provider"
	"github.com/clickety-clacks/lachesis/internal/teach"
)

func TestDefaultBindingUsesClaudeConfigDirFile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", home)
	binding, detail := New(nil).DefaultBinding()
	if detail != nil {
		t.Fatal(detail)
	}
	if binding.Kind != "file" || binding.Home != home || binding.CredentialPath != filepath.Join(home, ".credentials.json") {
		t.Fatalf("binding = %#v", binding)
	}
}

func TestDefaultBindingRejectsPersonalKeychainFallback(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	binding, detail := New(nil).DefaultBinding()
	if detail == nil || detail.Code != teach.KeychainSourceUnsupported || binding.Kind != "" {
		t.Fatalf("binding = %#v, detail = %#v", binding, detail)
	}
}

func TestDefaultBindingRejectsRelativeClaudeConfigDir(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", "relative-home")
	_, detail := New(nil).DefaultBinding()
	if detail == nil || detail.Code != teach.InvalidRequest {
		t.Fatalf("detail = %#v", detail)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func TestUsageRateLimitExposesRetryTimingWithoutUpstreamBody(t *testing.T) {
	now := time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)
	client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.String() != usageURL {
			t.Fatalf("request URL = %q", req.URL)
		}
		return &http.Response{
			StatusCode: http.StatusTooManyRequests,
			Header:     http.Header{"Retry-After": []string{"800"}},
			Body:       io.NopCloser(strings.NewReader(`{"message":"upstream-secret"}`)),
			Request:    req,
		}, nil
	})}
	adapter := New(client)
	adapter.Now = func() time.Time { return now }
	_, detail := adapter.Usage(context.Background(), provider.Credential{AccessToken: "credential-secret"})
	if detail == nil || detail.Code != teach.UpstreamRateLimited || detail.RetryAfterSeconds != 800 || detail.RetryAt == nil || !detail.RetryAt.Equal(now.Add(800*time.Second)) {
		t.Fatalf("detail = %#v", detail)
	}
	encoded, err := json.Marshal(detail)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "upstream-secret") || strings.Contains(string(encoded), "credential-secret") || !strings.Contains(string(encoded), `"retry_after_seconds":800`) || !strings.Contains(string(encoded), `"retry_at":"2026-09-23T12:13:20Z"`) {
		t.Fatalf("public error JSON = %s", encoded)
	}
}

func TestRefreshRateLimitPreservesRetryAfterWithoutUpstreamBody(t *testing.T) {
	now := time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)
	client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.String() != tokenURL {
			t.Fatalf("request URL = %q", req.URL)
		}
		return &http.Response{
			StatusCode: http.StatusTooManyRequests,
			Header:     http.Header{"Retry-After": []string{"Wed, 23 Sep 2026 12:20:00 GMT"}},
			Body:       io.NopCloser(strings.NewReader(`{"message":"upstream-secret"}`)),
			Request:    req,
		}, nil
	})}
	adapter := New(client)
	adapter.Now = func() time.Time { return now }
	_, detail := adapter.Refresh(context.Background(), provider.Credential{RefreshToken: "refresh-secret"})
	if detail == nil || detail.Code != teach.UpstreamRateLimited || detail.Help != "/api/v1/help/refresh" || detail.RetryAfterSeconds != 1200 || detail.RetryAt == nil || !detail.RetryAt.Equal(now.Add(20*time.Minute)) {
		t.Fatalf("detail = %#v", detail)
	}
	encoded, err := json.Marshal(detail)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "upstream-secret") || strings.Contains(string(encoded), "refresh-secret") {
		t.Fatalf("public error JSON leaked a secret: %s", encoded)
	}
}

func TestRetryAtFromHeader(t *testing.T) {
	now := time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name   string
		header string
		want   time.Time
	}{{
		name:   "delta seconds",
		header: "800",
		want:   now.Add(800 * time.Second),
	}, {
		name:   "http date",
		header: "Wed, 23 Sep 2026 12:20:00 GMT",
		want:   now.Add(20 * time.Minute),
	}, {
		name:   "invalid fallback",
		header: "not-a-delay",
		want:   now.Add(time.Minute),
	}, {
		name:   "zero minimum",
		header: "0",
		want:   now.Add(time.Second),
	}}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := retryAtFromHeader(tt.header, now); !got.Equal(tt.want) {
				t.Fatalf("retryAt = %s, want %s", got, tt.want)
			}
		})
	}
}

func TestNormalizeOmitsInvalidFixedBucketAndKeepsSibling(t *testing.T) {
	raw := json.RawMessage(`{"five_hour":{"utilization":101},"seven_day":{"utilization":0.5}}`)
	sample, detail := normalize(raw, time.Unix(1, 0))
	if detail != nil {
		t.Fatal(detail)
	}
	if len(sample.Windows) != 1 || sample.Windows[0].ID != "seven_day" || len(sample.Diagnostics) != 1 || string(sample.Raw) != string(raw) {
		t.Fatalf("windows=%#v diagnostics=%#v raw_preserved=%t", sample.Windows, sample.Diagnostics, string(sample.Raw) == string(raw))
	}
	assertClaudeDiagnostic(t, sample.Diagnostics[0])
}

func TestNormalizeAcceptsFractionalAndPercentScaleBuckets(t *testing.T) {
	raw := json.RawMessage(`{"five_hour":{"utilization":0.25},"seven_day":{"utilization":25},"seven_day_sonnet":{"utilization":1}}`)
	sample, detail := normalize(raw, time.Unix(1, 0))
	if detail != nil {
		t.Fatal(detail)
	}
	if len(sample.Windows) != 3 || sample.Windows[0].UsedPercent != 25 || sample.Windows[1].UsedPercent != 25 || sample.Windows[2].UsedPercent != 100 || len(sample.Diagnostics) != 0 || string(sample.Raw) != string(raw) {
		t.Fatalf("sample = %#v", sample)
	}
	if *sample.Windows[0].WindowSeconds != 5*60*60 || *sample.Windows[1].WindowSeconds != 7*24*60*60 || *sample.Windows[2].WindowSeconds != 7*24*60*60 {
		t.Fatalf("window durations = %#v", sample.Windows)
	}
}

func TestNormalizeTreatsNullFixedBucketsAsAbsent(t *testing.T) {
	raw := json.RawMessage(`{"five_hour":{"utilization":0.25},"seven_day":null,"seven_day_sonnet":null}`)
	sample, detail := normalize(raw, time.Unix(1, 0))
	if detail != nil {
		t.Fatal(detail)
	}
	if got := strings.Join(claudeWindowIDs(sample.Windows), ","); got != "five_hour" || len(sample.Diagnostics) != 0 || string(sample.Raw) != string(raw) {
		t.Fatalf("window_ids=%v diagnostics=%#v raw_preserved=%t", claudeWindowIDs(sample.Windows), sample.Diagnostics, string(sample.Raw) == string(raw))
	}
}

func TestNormalizeLeavesOpaqueTopLevelBucketRawOnly(t *testing.T) {
	raw := json.RawMessage(`{"five_hour":{"utilization":0.25},"nimbus_quill":{"utilization":0.5,"resets_at":null}}`)
	sample, detail := normalize(raw, time.Unix(1, 0))
	if detail != nil {
		t.Fatal(detail)
	}
	if got := strings.Join(claudeWindowIDs(sample.Windows), ","); got != "five_hour" || len(sample.Diagnostics) != 0 || string(sample.Raw) != string(raw) {
		t.Fatalf("window_ids=%v diagnostics=%#v raw_preserved=%t", claudeWindowIDs(sample.Windows), sample.Diagnostics, string(sample.Raw) == string(raw))
	}
}

func TestNormalizeCombinesFixedAndMapLimitsWithLocalDegradation(t *testing.T) {
	raw := json.RawMessage(`{"five_hour":{"utilization":0.25},"seven_day":{"utilization":"synthetic-sentinel"},"limits":{"alpha":{"utilization":0.5},"unknown":{"other":true}}}`)
	sample, detail := normalize(raw, time.Unix(1, 0))
	if detail != nil {
		t.Fatal(detail)
	}
	if got := strings.Join(claudeWindowIDs(sample.Windows), ","); got != "five_hour,limit:alpha" || len(sample.Diagnostics) != 2 || string(sample.Raw) != string(raw) {
		t.Fatalf("window_ids=%v diagnostic_count=%d raw_preserved=%t", claudeWindowIDs(sample.Windows), len(sample.Diagnostics), string(sample.Raw) == string(raw))
	}
	for _, diagnostic := range sample.Diagnostics {
		assertClaudeDiagnostic(t, diagnostic)
	}
}

func TestNormalizeDegradesEnumerableAndUnrecognizedLimitsContainers(t *testing.T) {
	tests := []struct {
		name  string
		raw   json.RawMessage
		count int
	}{{
		name:  "enumerable array",
		raw:   json.RawMessage(`{"five_hour":{"utilization":0.25},"limits":[{"utilization":0.5},{"shape":"synthetic-sentinel"}]}`),
		count: 2,
	}, {
		name:  "unrecognized scalar",
		raw:   json.RawMessage(`{"five_hour":{"utilization":0.25},"limits":"synthetic-sentinel"}`),
		count: 1,
	}}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sample, detail := normalize(tt.raw, time.Unix(1, 0))
			if detail != nil {
				t.Fatal(detail)
			}
			if len(sample.Windows) != 1 || sample.Windows[0].ID != "five_hour" || len(sample.Diagnostics) != tt.count || string(sample.Raw) != string(tt.raw) {
				t.Fatalf("window_ids=%v diagnostic_count=%d raw_preserved=%t", claudeWindowIDs(sample.Windows), len(sample.Diagnostics), string(sample.Raw) == string(tt.raw))
			}
		})
	}
}

func TestNormalizeOmitsLaterCollidingLimitID(t *testing.T) {
	raw := json.RawMessage(`{"limits":{"a/b":{"utilization":0.25},"a:b":{"utilization":0.5}}}`)
	first, detail := normalize(raw, time.Unix(1, 0))
	if detail != nil {
		t.Fatal(detail)
	}
	second, detail := normalize(raw, time.Unix(1, 0))
	if detail != nil {
		t.Fatal(detail)
	}
	if got := strings.Join(claudeWindowIDs(first.Windows), ","); got != "limit:a:b" || strings.Join(claudeWindowIDs(second.Windows), ",") != got || len(first.Diagnostics) != 1 || len(second.Diagnostics) != 1 {
		t.Fatalf("first windows=%v diagnostics=%d; second windows=%v diagnostics=%d", claudeWindowIDs(first.Windows), len(first.Diagnostics), claudeWindowIDs(second.Windows), len(second.Diagnostics))
	}
}

func TestNormalizeInvalidLimitDoesNotReserveCollidingID(t *testing.T) {
	raw := json.RawMessage(`{"limits":{"a/b":{},"a:b":{"utilization":0.5}}}`)
	sample, detail := normalize(raw, time.Unix(1, 0))
	if detail != nil {
		t.Fatal(detail)
	}
	if got := strings.Join(claudeWindowIDs(sample.Windows), ","); got != "limit:a:b" || len(sample.Diagnostics) != 1 {
		t.Fatalf("window_ids=%v diagnostic_count=%d", claudeWindowIDs(sample.Windows), len(sample.Diagnostics))
	}
}

func TestNormalizePreservesValidRawResponse(t *testing.T) {
	raw := json.RawMessage(`{"five_hour":{"utilization":0.25},"seven_day":{"utilization":0.5}}`)
	sample, detail := normalize(raw, time.Unix(1, 0))
	if detail != nil {
		t.Fatal(detail)
	}
	encoded, err := json.Marshal(sample)
	if err != nil {
		t.Fatal(err)
	}
	if len(sample.Windows) != 2 || sample.Windows[0].UsedPercent != 25 || len(sample.Diagnostics) != 0 || string(sample.Raw) != string(raw) || !strings.Contains(string(encoded), `"diagnostics":[]`) {
		t.Fatalf("sample = %#v", sample)
	}
}

func TestNormalizeFailsClosedWithoutValidRecognizedWindow(t *testing.T) {
	for _, raw := range []json.RawMessage{
		json.RawMessage(`{}`),
		json.RawMessage(`{"five_hour":{"utilization":101}}`),
		json.RawMessage(`{"limits":[{"utilization":0.5}]}`),
	} {
		sample, detail := normalize(raw, time.Unix(1, 0))
		if sample != nil {
			t.Fatal("zero-valid response returned a sample")
		}
		assertNoValidWindowDetail(t, detail)
	}
}

func TestDiagnosticJSONIsFixedAndPublicSafe(t *testing.T) {
	raw := json.RawMessage(`{"five_hour":{"utilization":0.25},"seven_day":{"utilization":"synthetic-sentinel"}}`)
	sample, detail := normalize(raw, time.Unix(1, 0))
	if detail != nil {
		t.Fatal(detail)
	}
	encoded, err := json.Marshal(sample.Diagnostics[0])
	if err != nil {
		t.Fatal(err)
	}
	if string(encoded) != `{"code":"CLAUDE_USAGE_WINDOW_OMITTED","message":"Claude omitted an invalid or unrecognized usage window."}` || strings.Contains(string(encoded), "synthetic-sentinel") {
		t.Fatalf("diagnostic JSON = %s", encoded)
	}
}

func TestNormalizeRejectsNonObjectTopLevel(t *testing.T) {
	for _, raw := range []json.RawMessage{json.RawMessage(`[]`), json.RawMessage(`null`), json.RawMessage(`"value"`)} {
		if sample, detail := normalize(raw, time.Unix(1, 0)); sample != nil || detail == nil || detail.Code != teach.UpstreamContractChanged {
			t.Fatalf("sample=%#v detail=%#v", sample, detail)
		}
	}
}

func assertClaudeDiagnostic(t *testing.T, diagnostic model.Diagnostic) {
	t.Helper()
	if diagnostic.Code != "CLAUDE_USAGE_WINDOW_OMITTED" || diagnostic.Message != "Claude omitted an invalid or unrecognized usage window." {
		t.Fatalf("diagnostic = %#v", diagnostic)
	}
}

func assertNoValidWindowDetail(t *testing.T, detail *model.ErrorDetail) {
	t.Helper()
	if detail == nil || detail.Code != teach.UpstreamContractChanged || detail.Message != "Claude usage contained no valid recognized limit window." || detail.Help != "/api/v1/help/usage" {
		t.Fatalf("detail = %#v", detail)
	}
	if len(detail.Prerequisites) != 1 || detail.Prerequisites[0] != (model.Prerequisite{Code: "VALID_RECOGNIZED_WINDOW", Description: "The provider response contains at least one valid recognized usage window.", Met: false}) {
		t.Fatalf("prerequisites = %#v", detail.Prerequisites)
	}
	if len(detail.State) != 1 || detail.State["provider"] != model.ProviderClaude || detail.Remedy.Summary != "Retry the exact call. If the error repeats, update Lachesis before trusting provider usage." || len(detail.Remedy.Calls) != 0 || len(detail.Remedy.Commands) != 1 || detail.Remedy.Commands[0] != "retry the exact call" {
		t.Fatalf("detail = %#v", detail)
	}
}

func claudeWindowIDs(windows []model.Window) []string {
	ids := make([]string, 0, len(windows))
	for _, window := range windows {
		ids = append(ids, window.ID)
	}
	return ids
}
