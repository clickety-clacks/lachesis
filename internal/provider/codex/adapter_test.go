package codex

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/clickety-clacks/lachesis/internal/model"
	"github.com/clickety-clacks/lachesis/internal/teach"
)

func TestNormalizeOmitsInvalidPrimaryAndKeepsSecondary(t *testing.T) {
	raw := json.RawMessage(`{"rate_limit":{"primary_window":{"used_percent":101},"secondary_window":{"used_percent":10}}}`)
	sample, detail := normalize(raw, time.Unix(1, 0))
	if detail != nil {
		t.Fatal(detail)
	}
	if len(sample.Windows) != 1 || sample.Windows[0].ID != "secondary" || len(sample.Diagnostics) != 1 || string(sample.Raw) != string(raw) {
		t.Fatalf("windows=%#v diagnostics=%#v raw_preserved=%t", sample.Windows, sample.Diagnostics, string(sample.Raw) == string(raw))
	}
	assertCodexDiagnostic(t, sample.Diagnostics[0])
}

func TestNormalizeOmitsMalformedAdditionalWindows(t *testing.T) {
	raw := json.RawMessage(`{"rate_limit":{"primary_window":{"used_percent":10}},"additional_rate_limits":[{"name":"missing","rate_limit":{}},{"name":"wrong","rate_limit":"synthetic-sentinel"},{"name":"valid","rate_limit":{"used_percent":20}}]}`)
	sample, detail := normalize(raw, time.Unix(1, 0))
	if detail != nil {
		t.Fatal(detail)
	}
	if len(sample.Windows) != 2 || sample.Windows[0].ID != "primary" || sample.Windows[1].ID != "additional:valid" || len(sample.Diagnostics) != 2 || string(sample.Raw) != string(raw) {
		t.Fatalf("window_ids=%v diagnostic_count=%d raw_preserved=%t", windowIDs(sample.Windows), len(sample.Diagnostics), string(sample.Raw) == string(raw))
	}
	for _, diagnostic := range sample.Diagnostics {
		assertCodexDiagnostic(t, diagnostic)
	}
}

func TestNormalizePreservesValidRawResponse(t *testing.T) {
	raw := json.RawMessage(`{"plan_type":"fixed","rate_limit":{"primary_window":{"used_percent":10,"reset_after_seconds":60}},"additional_rate_limits":[]}`)
	sample, detail := normalize(raw, time.Unix(1, 0))
	if detail != nil {
		t.Fatal(detail)
	}
	encoded, err := json.Marshal(sample)
	if err != nil {
		t.Fatal(err)
	}
	if len(sample.Windows) != 1 || sample.Windows[0].UsedPercent != 10 || len(sample.Diagnostics) != 0 || string(sample.Raw) != string(raw) || !strings.Contains(string(encoded), `"diagnostics":[]`) {
		t.Fatalf("sample = %#v", sample)
	}
}

func TestNormalizeCurrentNestedAdditionalWindows(t *testing.T) {
	raw := json.RawMessage(`{"rate_limit":{"primary_window":{"used_percent":10},"secondary_window":null},"additional_rate_limits":[{"limit_name":"Synthetic Feature","metered_feature":"synthetic","rate_limit":{"primary_window":{"used_percent":20,"reset_at":100,"limit_window_seconds":18000},"secondary_window":{"used_percent":30,"reset_at":200,"limit_window_seconds":604800}}}]}`)
	sample, detail := normalize(raw, time.Unix(1, 0))
	if detail != nil {
		t.Fatal(detail)
	}
	want := "primary,additional:synthetic-feature:primary,additional:synthetic-feature:secondary"
	if got := strings.Join(windowIDs(sample.Windows), ","); got != want || len(sample.Diagnostics) != 0 || string(sample.Raw) != string(raw) {
		t.Fatalf("window_ids=%s diagnostics=%#v raw_preserved=%t", got, sample.Diagnostics, string(sample.Raw) == string(raw))
	}
	if sample.Windows[1].Name != "Synthetic Feature Primary" || sample.Windows[2].Name != "Synthetic Feature Secondary" {
		t.Fatalf("window names = %q, %q", sample.Windows[1].Name, sample.Windows[2].Name)
	}
}

func TestNormalizeNestedAdditionalNullAndInvalidCandidates(t *testing.T) {
	raw := json.RawMessage(`{"rate_limit":{"primary_window":{"used_percent":10}},"additional_rate_limits":[{"limit_name":"Synthetic Feature","rate_limit":{"primary_window":null,"secondary_window":{"used_percent":101}}}]}`)
	sample, detail := normalize(raw, time.Unix(1, 0))
	if detail != nil {
		t.Fatal(detail)
	}
	if got := strings.Join(windowIDs(sample.Windows), ","); got != "primary" || len(sample.Diagnostics) != 1 {
		t.Fatalf("window_ids=%s diagnostics=%#v", got, sample.Diagnostics)
	}
	assertCodexDiagnostic(t, sample.Diagnostics[0])
}

func TestNormalizeKeepsUnnamedAdditionalWindowsStable(t *testing.T) {
	raw := json.RawMessage(`{"rate_limit":{"primary_window":{"used_percent":10}},"additional_rate_limits":[{"name":"unnamed 2","rate_limit":{"used_percent":20}},{"name":"  ","rate_limit":{"used_percent":30}},{"rate_limit":{"used_percent":40}}]}`)
	first, detail := normalize(raw, time.Unix(1, 0))
	if detail != nil {
		t.Fatal(detail)
	}
	second, detail := normalize(raw, time.Unix(1, 0))
	if detail != nil {
		t.Fatal(detail)
	}
	want := []string{"additional:unnamed:3|Unnamed additional limit 3", "additional:unnamed:2|Unnamed additional limit 2", "additional:unnamed-2|unnamed 2"}
	got := make([]string, 0, 3)
	again := make([]string, 0, 3)
	for _, window := range first.Windows[1:] {
		got = append(got, window.ID+"|"+window.Name)
	}
	for _, window := range second.Windows[1:] {
		again = append(again, window.ID+"|"+window.Name)
	}
	if len(first.Windows) != 4 || strings.Join(got, ",") != strings.Join(want, ",") || strings.Join(again, ",") != strings.Join(want, ",") || len(first.Diagnostics) != 0 || string(first.Raw) != string(raw) {
		t.Fatalf("first = %#v, second = %#v", first, second)
	}
}

func TestNormalizeOmitsLaterDuplicateNamedAdditionalWindow(t *testing.T) {
	raw := json.RawMessage(`{"rate_limit":{"primary_window":{"used_percent":10}},"additional_rate_limits":[{"name":"Extra Limit","rate_limit":{"used_percent":20}},{"name":"extra-limit","rate_limit":{"used_percent":30}}]}`)
	first, detail := normalize(raw, time.Unix(1, 0))
	if detail != nil {
		t.Fatal(detail)
	}
	second, detail := normalize(raw, time.Unix(1, 0))
	if detail != nil {
		t.Fatal(detail)
	}
	if got := strings.Join(windowIDs(first.Windows), ","); got != "primary,additional:extra-limit" || strings.Join(windowIDs(second.Windows), ",") != got || len(first.Diagnostics) != 1 || len(second.Diagnostics) != 1 {
		t.Fatalf("first windows=%v diagnostics=%d; second windows=%v diagnostics=%d", windowIDs(first.Windows), len(first.Diagnostics), windowIDs(second.Windows), len(second.Diagnostics))
	}
}

func TestNormalizeInvalidCandidatesDoNotReserveIDsOrRenumberUnnamedWindows(t *testing.T) {
	raw := json.RawMessage(`{"rate_limit":{"primary_window":{"used_percent":10}},"additional_rate_limits":[{"name":"duplicate","rate_limit":{}},{"name":"duplicate","rate_limit":{"used_percent":20}},{"rate_limit":{}},{"rate_limit":{"used_percent":30}}]}`)
	sample, detail := normalize(raw, time.Unix(1, 0))
	if detail != nil {
		t.Fatal(detail)
	}
	if got := strings.Join(windowIDs(sample.Windows), ","); got != "primary,additional:unnamed:4,additional:duplicate" || len(sample.Diagnostics) != 2 {
		t.Fatalf("window_ids=%v diagnostic_count=%d", windowIDs(sample.Windows), len(sample.Diagnostics))
	}
}

func TestNormalizeDegradesUnrecognizedContainers(t *testing.T) {
	tests := []struct {
		name string
		raw  json.RawMessage
		id   string
	}{{
		name: "rate limit",
		raw:  json.RawMessage(`{"rate_limit":"synthetic-sentinel","additional_rate_limits":[{"name":"valid","rate_limit":{"used_percent":20}}]}`),
		id:   "additional:valid",
	}, {
		name: "additional limits",
		raw:  json.RawMessage(`{"rate_limit":{"primary_window":{"used_percent":10}},"additional_rate_limits":{"shape":"synthetic-sentinel"}}`),
		id:   "primary",
	}}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sample, detail := normalize(tt.raw, time.Unix(1, 0))
			if detail != nil {
				t.Fatal(detail)
			}
			if len(sample.Windows) != 1 || sample.Windows[0].ID != tt.id || len(sample.Diagnostics) != 1 || string(sample.Raw) != string(tt.raw) {
				t.Fatalf("window_ids=%v diagnostic_count=%d raw_preserved=%t", windowIDs(sample.Windows), len(sample.Diagnostics), string(sample.Raw) == string(tt.raw))
			}
		})
	}
}

func TestNormalizeFailsClosedWithoutValidRecognizedWindow(t *testing.T) {
	for _, raw := range []json.RawMessage{
		json.RawMessage(`{}`),
		json.RawMessage(`{"rate_limit":{"primary_window":{"used_percent":101}}}`),
		json.RawMessage(`{"additional_rate_limits":[{"name":"invalid","rate_limit":{}}]}`),
	} {
		sample, detail := normalize(raw, time.Unix(1, 0))
		if sample != nil {
			t.Fatal("zero-valid response returned a sample")
		}
		assertNoValidWindowDetail(t, detail)
	}
}

func TestDiagnosticJSONIsFixedAndPublicSafe(t *testing.T) {
	raw := json.RawMessage(`{"rate_limit":{"primary_window":{"used_percent":10}},"additional_rate_limits":[{"name":"synthetic-sentinel","rate_limit":{}}]}`)
	sample, detail := normalize(raw, time.Unix(1, 0))
	if detail != nil {
		t.Fatal(detail)
	}
	encoded, err := json.Marshal(sample.Diagnostics[0])
	if err != nil {
		t.Fatal(err)
	}
	if string(encoded) != `{"code":"CODEX_USAGE_WINDOW_OMITTED","message":"Codex omitted an invalid or unrecognized usage window."}` || strings.Contains(string(encoded), "synthetic-sentinel") {
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

func assertCodexDiagnostic(t *testing.T, diagnostic model.Diagnostic) {
	t.Helper()
	if diagnostic.Code != "CODEX_USAGE_WINDOW_OMITTED" || diagnostic.Message != "Codex omitted an invalid or unrecognized usage window." {
		t.Fatalf("diagnostic = %#v", diagnostic)
	}
}

func assertNoValidWindowDetail(t *testing.T, detail *model.ErrorDetail) {
	t.Helper()
	if detail == nil || detail.Code != teach.UpstreamContractChanged || detail.Message != "Codex usage contained no valid recognized limit window." || detail.Help != "/api/v1/help/usage" {
		t.Fatalf("detail = %#v", detail)
	}
	if len(detail.Prerequisites) != 1 || detail.Prerequisites[0] != (model.Prerequisite{Code: "VALID_RECOGNIZED_WINDOW", Description: "The provider response contains at least one valid recognized usage window.", Met: false}) {
		t.Fatalf("prerequisites = %#v", detail.Prerequisites)
	}
	if len(detail.State) != 1 || detail.State["provider"] != model.ProviderCodex || detail.Remedy.Summary != "Retry the exact call. If the error repeats, update Lachesis before trusting provider usage." || len(detail.Remedy.Calls) != 0 || len(detail.Remedy.Commands) != 1 || detail.Remedy.Commands[0] != "retry the exact call" {
		t.Fatalf("detail = %#v", detail)
	}
}

func windowIDs(windows []model.Window) []string {
	ids := make([]string, 0, len(windows))
	for _, window := range windows {
		ids = append(ids, window.ID)
	}
	return ids
}
