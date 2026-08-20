package fixture

import (
	"context"
	"encoding/json"
	"errors"
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

func TestSanitizeUsageReplacesProviderMaterial(t *testing.T) {
	raw := []byte(`{"plan_type":"private-plan","rate_limit":{"primary_window":{"used_percent":73.5,"reset_at":"2026-08-20T12:34:56Z","reset_after_seconds":999}},"additional_rate_limits":[{"name":"private-name","rate_limit":{"used_percent":44}}],"five_hour":{"utilization":0.73,"resets_at":"2026-08-20T12:34:56Z"},"limits":{"private/model":{"utilization":0.62}},"balance":123.45,"free_form":"private-string"}`)
	out, err := Sanitize(raw, Usage)
	if err != nil {
		t.Fatal(err)
	}
	for _, private := range []string{"private-plan", "private-name", "private/model", "private-string", "73.5", "0.73", "0.62", "123.45", "999", "2026-08-20"} {
		if strings.Contains(string(out), private) {
			t.Fatalf("provider material survived: %s", private)
		}
	}
	for _, structural := range []string{`"rate_limit"`, `"primary_window"`, `"additional_rate_limits"`, `"five_hour"`, `"limits"`, `"used_percent": 25`, `"utilization": 0.25`, sanitizedTimestamp} {
		if !strings.Contains(string(out), structural) {
			t.Fatalf("missing sanitized structure: %s", structural)
		}
	}
	if err := Scan(out, Usage); err != nil {
		t.Fatal(err)
	}
}

func TestScanUsageRejectsNoncanonicalMaterial(t *testing.T) {
	for _, raw := range [][]byte{
		[]byte(`{"plan_type":"private"}`),
		[]byte(`{"used_percent":73}`),
		[]byte(`{"limits":{"private":{"utilization":0.25}}}`),
	} {
		if err := Scan(raw, Usage); err == nil {
			t.Fatalf("expected unsafe usage rejection: %s", raw)
		}
	}
}

func TestCaptureUsageLifecycle(t *testing.T) {
	for _, provider := range []string{"codex", "claude"} {
		t.Run(provider+" success", func(t *testing.T) {
			workspace := t.TempDir()
			rawPath := filepath.Join(workspace, provider+".raw.json")
			out, err := CaptureUsage(context.Background(), workspace, provider, func(context.Context) ([]byte, error) {
				return []byte(`{"used_percent":73}`), nil
			})
			if err != nil || len(out) == 0 {
				t.Fatalf("capture: out=%d err=%v", len(out), err)
			}
			if _, err := os.Stat(rawPath); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("raw capture remains: %v", err)
			}
		})

		t.Run(provider+" sanitizer failure", func(t *testing.T) {
			workspace := t.TempDir()
			rawPath := filepath.Join(workspace, provider+".raw.json")
			if _, err := CaptureUsage(context.Background(), workspace, provider, func(context.Context) ([]byte, error) {
				return []byte(`{"access_token":"synthetic"}`), nil
			}); err == nil {
				t.Fatal("expected sanitizer failure")
			}
			if _, err := os.Stat(rawPath); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("raw capture remains: %v", err)
			}
		})

		t.Run(provider+" capture failure", func(t *testing.T) {
			workspace := t.TempDir()
			rawPath := filepath.Join(workspace, provider+".raw.json")
			if _, err := CaptureUsage(context.Background(), workspace, provider, func(context.Context) ([]byte, error) {
				return nil, errors.New("synthetic capture failure")
			}); err == nil {
				t.Fatal("expected capture failure")
			}
			if _, err := os.Stat(rawPath); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("raw capture remains: %v", err)
			}
		})

		t.Run(provider+" restart recovery", func(t *testing.T) {
			workspace := t.TempDir()
			rawPath := filepath.Join(workspace, provider+".raw.json")
			if err := os.WriteFile(rawPath, []byte("synthetic residual"), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := CaptureUsage(context.Background(), workspace, provider, func(context.Context) ([]byte, error) {
				if _, err := os.Stat(rawPath); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("residual not removed before capture: %v", err)
				}
				return []byte(`{"used_percent":73}`), nil
			}); err != nil {
				t.Fatal(err)
			}
		})

		t.Run(provider+" caught cancellation", func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			workspace := t.TempDir()
			rawPath := filepath.Join(workspace, provider+".raw.json")
			if _, err := CaptureUsage(ctx, workspace, provider, func(context.Context) ([]byte, error) {
				cancel()
				return []byte(`{"used_percent":73}`), nil
			}); !errors.Is(err, context.Canceled) {
				t.Fatalf("got %v", err)
			}
			if _, err := os.Stat(rawPath); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("raw capture remains: %v", err)
			}
		})
	}
}

func TestCaptureUsageCleanupFailureBlocksFixture(t *testing.T) {
	captured := false
	out, err := captureUsage(context.Background(), "synthetic.raw.json", func(context.Context) ([]byte, error) {
		captured = true
		return []byte(`{}`), nil
	}, captureUsageOps{
		remove: func(string) error { return errors.New("synthetic cleanup refusal") },
		write:  os.WriteFile,
	})
	if !errors.Is(err, ErrCaptureCleanup) || out != nil || captured {
		t.Fatalf("out=%v err=%v captured=%v", out, err, captured)
	}
}

func TestCaptureUsageFinalCleanupFailureBlocksFixture(t *testing.T) {
	removeCalls := 0
	out, err := captureUsage(context.Background(), "synthetic.raw.json", func(context.Context) ([]byte, error) {
		return []byte(`{"used_percent":73}`), nil
	}, captureUsageOps{
		remove: func(string) error {
			removeCalls++
			if removeCalls == 1 {
				return os.ErrNotExist
			}
			return errors.New("synthetic cleanup refusal")
		},
		write: func(string, []byte, os.FileMode) error { return nil },
	})
	if !errors.Is(err, ErrCaptureCleanup) || out != nil {
		t.Fatalf("out=%v err=%v", out, err)
	}
}

func TestCaptureUsageWriteFailureCleansResidual(t *testing.T) {
	removeCalls := 0
	out, err := captureUsage(context.Background(), "synthetic.raw.json", func(context.Context) ([]byte, error) {
		return []byte(`{"used_percent":73}`), nil
	}, captureUsageOps{
		remove: func(string) error {
			removeCalls++
			return os.ErrNotExist
		},
		write: func(string, []byte, os.FileMode) error {
			return errors.New("synthetic partial write failure")
		},
	})
	if err == nil || out != nil || removeCalls != 2 {
		t.Fatalf("out=%v err=%v removeCalls=%d", out, err, removeCalls)
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
