package codex

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/clickety-clacks/lachesis/internal/teach"
)

func TestNormalizeRejectsAnyOutOfRangeWindow(t *testing.T) {
	raw := json.RawMessage(`{"rate_limit":{"primary_window":{"used_percent":101},"secondary_window":{"used_percent":10}}}`)
	if _, detail := normalize(raw, time.Unix(1, 0)); detail == nil || detail.Code != teach.UpstreamContractChanged {
		t.Fatalf("detail = %#v", detail)
	}
}

func TestNormalizeRejectsMalformedAdditionalWindow(t *testing.T) {
	raw := json.RawMessage(`{"rate_limit":{"primary_window":{"used_percent":10}},"additional_rate_limits":[{"name":"extra","rate_limit":{}}]}`)
	if _, detail := normalize(raw, time.Unix(1, 0)); detail == nil || detail.Code != teach.UpstreamContractChanged {
		t.Fatalf("detail = %#v", detail)
	}
}

func TestNormalizePreservesValidRawResponse(t *testing.T) {
	raw := json.RawMessage(`{"plan_type":"fixed","rate_limit":{"primary_window":{"used_percent":10,"reset_after_seconds":60}},"additional_rate_limits":[]}`)
	sample, detail := normalize(raw, time.Unix(1, 0))
	if detail != nil {
		t.Fatal(detail)
	}
	if len(sample.Windows) != 1 || sample.Windows[0].UsedPercent != 10 || string(sample.Raw) != string(raw) {
		t.Fatalf("sample = %#v", sample)
	}
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
	if len(first.Windows) != 4 || strings.Join(got, ",") != strings.Join(want, ",") || strings.Join(again, ",") != strings.Join(want, ",") || string(first.Raw) != string(raw) {
		t.Fatalf("first = %#v, second = %#v", first, second)
	}
}

func TestNormalizeRejectsDuplicateNamedAdditionalWindows(t *testing.T) {
	raw := json.RawMessage(`{"rate_limit":{"primary_window":{"used_percent":10}},"additional_rate_limits":[{"name":"Extra Limit","rate_limit":{"used_percent":20}},{"name":"extra-limit","rate_limit":{"used_percent":30}}]}`)
	if sample, detail := normalize(raw, time.Unix(1, 0)); sample != nil || detail == nil || detail.Code != teach.UpstreamContractChanged {
		t.Fatalf("sample = %#v, detail = %#v", sample, detail)
	}
}
