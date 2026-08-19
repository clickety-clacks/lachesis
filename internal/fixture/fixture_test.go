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
		return Scan(wrapper.Response, wrapper.Kind)
	})
	if err != nil {
		t.Fatal(err)
	}
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
