package claude

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestSanitizedRealUsageFixtures(t *testing.T) {
	paths, err := filepath.Glob(filepath.Join("..", "..", "..", "testdata", "fixtures", "*.json"))
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, path := range paths {
		var fixture struct {
			Provider     string          `json:"provider"`
			Kind         string          `json:"kind"`
			RealResponse bool            `json:"real_response"`
			Response     json.RawMessage `json:"response"`
		}
		b, err := os.ReadFile(path)
		if err != nil || json.Unmarshal(b, &fixture) != nil {
			t.Fatalf("read fixture %s: %v", path, err)
		}
		if fixture.Provider != "claude" || fixture.Kind != "usage" {
			continue
		}
		if !fixture.RealResponse {
			t.Fatalf("fixture %s is not an approved real-response capture", path)
		}
		sample, detail := normalize(fixture.Response, time.Unix(1, 0))
		if detail != nil || sample == nil || string(sample.Raw) != string(fixture.Response) {
			t.Fatalf("normalize fixture %s: sample=%#v detail=%#v", path, sample, detail)
		}
		if len(sample.Windows) == 0 || len(sample.Diagnostics) == 0 {
			t.Fatalf("fixture %s did not exercise valid-window retention with additional-window degradation: windows=%d diagnostics=%d", path, len(sample.Windows), len(sample.Diagnostics))
		}
		count++
	}
	if count == 0 {
		requireUnavailableFixtureStatus(t, "claude")
		t.Skip("approved sanitized-real Claude usage fixture is unavailable; provider compatibility remains a human release proof")
	}
}

func requireUnavailableFixtureStatus(t *testing.T, name string) {
	t.Helper()
	path := filepath.Join("..", "..", "..", "testdata", "evidence", "provider-fixtures.json")
	var status struct {
		Usage map[string]string `json:"usage"`
	}
	b, err := os.ReadFile(path)
	if err != nil || json.Unmarshal(b, &status) != nil || status.Usage[name] != "unavailable" {
		t.Fatalf("missing truthful unavailable fixture status for %s at %s", name, path)
	}
}
