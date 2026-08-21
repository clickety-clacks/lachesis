package core

import (
	"encoding/json"
	"io"
	"os"
	"strings"
	"testing"
)

type fragmentedReader struct {
	reader *strings.Reader
	size   int
}

func (r *fragmentedReader) Read(p []byte) (int, error) {
	if len(p) > r.size {
		p = p[:r.size]
	}
	return r.reader.Read(p)
}

func TestScanCodexDeviceOutputPublishesCompleteStructuredFields(t *testing.T) {
	tests := []struct {
		name  string
		raw   string
		chunk int
		code  string
	}{
		{name: "together", raw: codexVerificationURL + " TEST-CODE\n", chunk: 256, code: "TEST-CODE"},
		{name: "separate lines", raw: codexVerificationURL + "\nTEST-CODE\n", chunk: 256, code: "TEST-CODE"},
		{name: "fragmented reads with ANSI", raw: "\x1b[94m" + codexVerificationURL + "\x1b[0m\n\x1b[94mTEST-CODE\x1b[0m\n", chunk: 1, code: "TEST-CODE"},
		{name: "numbered prose with variable code groups", raw: "1. \x1b[1;94m" + codexVerificationURL + "\x1b[0m\n2. \x1b[38;5;245mAB12-C345D\x1b[0m\n", chunk: 256, code: "AB12-C345D"},
		{name: "oversized line before prompt", raw: strings.Repeat("x", 128<<10) + "\n" + codexVerificationURL + "\nTEST-CODE\n", chunk: 256, code: "TEST-CODE"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reader := &fragmentedReader{reader: strings.NewReader(tt.raw), size: tt.chunk}
			published := 0
			result := scanCodexDeviceOutput(reader, func(url, code string) {
				published++
				if url != codexVerificationURL || code != tt.code {
					t.Fatalf("url = %q, code = %q", url, code)
				}
			})
			if !result.found || result.unavailable || result.expired || published != 1 {
				t.Fatalf("result = %#v, published = %d", result, published)
			}
		})
	}
}

func TestScanCodexDeviceOutputUsesSanitizedObservedStructure(t *testing.T) {
	raw, err := os.ReadFile("testdata/codex-device-auth-v0.149.0-structure.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Synthetic bool   `json:"synthetic"`
		Output    string `json:"output"`
		Code      string `json:"expected_code"`
	}
	if err := json.Unmarshal(raw, &fixture); err != nil || !fixture.Synthetic {
		t.Fatalf("fixture: synthetic=%t, err=%v", fixture.Synthetic, err)
	}
	published := 0
	result := scanCodexDeviceOutput(strings.NewReader(fixture.Output), func(url, code string) {
		published++
		if url != codexVerificationURL || code != fixture.Code {
			t.Fatalf("url = %q, code = %q", url, code)
		}
	})
	if !result.found || result.unavailable || result.expired || published != 1 {
		t.Fatalf("result = %#v, published = %d", result, published)
	}
}

func TestScanCodexDeviceOutputRejectsCallbackAndClassifiesFailures(t *testing.T) {
	tests := []struct {
		name        string
		raw         string
		unavailable bool
		expired     bool
	}{
		{name: "localhost callback", raw: "http://localhost:1455/auth/callback\nTEST-CODE\n"},
		{name: "https callback", raw: "https://auth.openai.com/deviceauth/callback\nTEST-CODE\n"},
		{name: "official URL without code", raw: codexVerificationURL + "\n"},
		{name: "disabled", raw: "Error logging in with device code: device code login is not enabled for this Codex server.\n", unavailable: true},
		{name: "workspace disabled", raw: "Please contact your workspace admin to enable device code authentication.\n", unavailable: true},
		{name: "request failed", raw: "Device code request failed.\n", unavailable: true},
		{name: "unexpected argument", raw: "error: unexpected argument '--device-auth' found\n", unavailable: true},
		{name: "unrecognized option", raw: "error: unrecognized option '--device-auth'\n", unavailable: true},
		{name: "expired", raw: "Error logging in with device code: device auth timed out after 15 minutes\n", expired: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			published := false
			result := scanCodexDeviceOutput(strings.NewReader(tt.raw), func(string, string) { published = true })
			if result.found || published || result.unavailable != tt.unavailable || result.expired != tt.expired {
				t.Fatalf("result = %#v, published = %t", result, published)
			}
		})
	}
}

func TestScanBrowserLoginOutputKeepsClaudeURLFlow(t *testing.T) {
	var got string
	result := scanBrowserLoginOutput(io.LimitReader(strings.NewReader("open https://example.invalid/login.\n"), 1024), func(url string) { got = url })
	if !result.found || got != "https://example.invalid/login" {
		t.Fatalf("result = %#v, url = %q", result, got)
	}
}
