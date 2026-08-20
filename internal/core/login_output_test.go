package core

import (
	"io"
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
	}{
		{name: "together", raw: codexVerificationURL + " TEST-CODE\n", chunk: 256},
		{name: "separate lines", raw: codexVerificationURL + "\nTEST-CODE\n", chunk: 256},
		{name: "fragmented reads with ANSI", raw: "\x1b[94m" + codexVerificationURL + "\x1b[0m\n\x1b[94mTEST-CODE\x1b[0m\n", chunk: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reader := &fragmentedReader{reader: strings.NewReader(tt.raw), size: tt.chunk}
			published := 0
			result := scanCodexDeviceOutput(reader, func(url, code string) {
				published++
				if url != codexVerificationURL || code != "TEST-CODE" {
					t.Fatalf("url = %q, code = %q", url, code)
				}
			})
			if !result.found || result.unavailable || result.expired || published != 1 {
				t.Fatalf("result = %#v, published = %d", result, published)
			}
		})
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
		{name: "disabled", raw: "Error logging in with device code: device code login is not enabled for this Codex server.\n", unavailable: true},
		{name: "unsupported flag", raw: "error: unexpected argument '--device-auth' found\n", unavailable: true},
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
