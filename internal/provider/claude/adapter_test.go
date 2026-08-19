package claude

import (
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

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

func TestNormalizeRejectsOutOfRangeUtilization(t *testing.T) {
	raw := json.RawMessage(`{"five_hour":{"utilization":1.01}}`)
	if _, detail := normalize(raw, time.Unix(1, 0)); detail == nil || detail.Code != teach.UpstreamContractChanged {
		t.Fatalf("detail = %#v", detail)
	}
}

func TestNormalizePreservesValidRawResponse(t *testing.T) {
	raw := json.RawMessage(`{"five_hour":{"utilization":0.25},"seven_day":{"utilization":0.5}}`)
	sample, detail := normalize(raw, time.Unix(1, 0))
	if detail != nil {
		t.Fatal(detail)
	}
	if len(sample.Windows) != 2 || sample.Windows[0].UsedPercent != 25 || string(sample.Raw) != string(raw) {
		t.Fatalf("sample = %#v", sample)
	}
}
