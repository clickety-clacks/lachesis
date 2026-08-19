package fixture

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSanitizeUsageRejectsCredential(t *testing.T) {
	_, err := Sanitize([]byte(`{"access_token":"sentinel"}`), Usage)
	if err == nil {
		t.Fatal("expected credential rejection")
	}
}

func TestCommittedFixtures(t *testing.T) {
	root := filepath.Join("..", "..", "testdata", "fixtures")
	usageByProvider := map[string]int{"codex": 0, "claude": 0}
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil || entry.IsDir() || filepath.Ext(path) != ".json" {
			return err
		}
		var wrapper struct {
			Provider         string          `json:"provider"`
			Endpoint         string          `json:"endpoint"`
			CaptureDate      string          `json:"capture_date"`
			SanitizerVersion string          `json:"sanitizer_version"`
			RealResponse     bool            `json:"real_response"`
			Kind             Kind            `json:"kind"`
			Response         json.RawMessage `json:"response"`
		}
		b, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		if json.Unmarshal(b, &wrapper) != nil || wrapper.Provider == "" || wrapper.Endpoint == "" || wrapper.CaptureDate == "" || wrapper.SanitizerVersion == "" || !wrapper.RealResponse || len(wrapper.Response) == 0 {
			return os.ErrInvalid
		}
		if wrapper.Provider != "codex" && wrapper.Provider != "claude" {
			return os.ErrInvalid
		}
		if wrapper.Kind != Usage && wrapper.Kind != Token {
			return os.ErrInvalid
		}
		if err := Scan(wrapper.Response, wrapper.Kind); err != nil {
			return err
		}
		if wrapper.Kind == Usage {
			usageByProvider[wrapper.Provider]++
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	status := readProviderFixtureStatus(t)
	for provider, count := range usageByProvider {
		want := "available"
		if count == 0 {
			want = "unavailable"
		}
		if status.Usage[provider] != want {
			t.Errorf("%s fixture count=%d but evidence status=%q, want %q", provider, count, status.Usage[provider], want)
		}
	}
	t.Logf("sanitized-real usage evidence: codex=%s claude=%s; unavailable evidence does not certify provider compatibility", status.Usage["codex"], status.Usage["claude"])
}

type providerFixtureStatus struct {
	SchemaVersion int               `json:"schema_version"`
	Usage         map[string]string `json:"usage"`
	ReleaseEffect string            `json:"release_effect"`
}

func readProviderFixtureStatus(t *testing.T) providerFixtureStatus {
	t.Helper()
	path := filepath.Join("..", "..", "testdata", "evidence", "provider-fixtures.json")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var status providerFixtureStatus
	if json.Unmarshal(b, &status) != nil || status.SchemaVersion != 1 || status.Usage == nil || status.ReleaseEffect == "" {
		t.Fatalf("invalid provider fixture evidence status at %s", path)
	}
	return status
}
func TestSanitizeTokenReplacesDeclaredValues(t *testing.T) {
	out, err := Sanitize([]byte(`{"access_token":"private","refresh_token":"private","email":"person@example.test"}`), Token)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "private") || strings.Contains(string(out), "example.test") {
		t.Fatal("sensitive value survived")
	}
	if err = Scan(out, Token); err != nil {
		t.Fatal(err)
	}
}
