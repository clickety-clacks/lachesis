package codex

import (
	"encoding/json"
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
