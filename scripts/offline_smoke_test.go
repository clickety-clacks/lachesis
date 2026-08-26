//go:build unix

package scripts_test

import (
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func TestOfflineSmokeUsesFreshChildWhenFixedPortIsOccupied(t *testing.T) {
	listener, err := net.Listen("tcp4", "127.0.0.1:7843")
	if err == nil {
		defer listener.Close()
	} else if !errors.Is(err, syscall.EADDRINUSE) {
		t.Fatalf("occupy smoke port: %v", err)
	}

	testDir := t.TempDir()
	fixedPortMarker := filepath.Join(testDir, "fixed-port-called")
	fakeBin := filepath.Join(testDir, "bin")
	if err := os.Mkdir(fakeBin, 0o755); err != nil {
		t.Fatalf("create fake bin: %v", err)
	}
	realCurl, err := exec.LookPath("curl")
	if err != nil {
		t.Fatalf("find real curl: %v", err)
	}
	fakeCurl := filepath.Join(fakeBin, "curl")
	fakeCurlScript := "#!/bin/sh\nfor arg do\n  case \"$arg\" in\n    *127.0.0.1:7843*) : > \"$OFFLINE_SMOKE_FIXED_PORT_MARKER\" ;;\n  esac\ndone\nexec \"$OFFLINE_SMOKE_REAL_CURL\" \"$@\"\n"
	if err := os.WriteFile(fakeCurl, []byte(fakeCurlScript), 0o755); err != nil {
		t.Fatalf("write fake curl: %v", err)
	}

	cmd := exec.Command("./scripts/offline-smoke.sh")
	cmd.Dir = ".."
	cmd.Env = replaceEnv(os.Environ(), map[string]string{
		"OFFLINE_SMOKE_FIXED_PORT_MARKER": fixedPortMarker,
		"OFFLINE_SMOKE_REAL_CURL":         realCurl,
		"PATH":                            fakeBin + string(os.PathListSeparator) + os.Getenv("PATH"),
	})
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("offline smoke failed with occupied fixed port: %v; output: %s", err, output)
	}
	if _, err := os.Stat(fixedPortMarker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("offline smoke sent HTTP to the fixed-port listener")
	}
}

func replaceEnv(environ []string, replacements map[string]string) []string {
	result := make([]string, 0, len(environ)+len(replacements))
	for _, entry := range environ {
		key, _, _ := strings.Cut(entry, "=")
		if _, replaced := replacements[key]; !replaced {
			result = append(result, entry)
		}
	}
	for key, value := range replacements {
		result = append(result, key+"="+value)
	}
	return result
}
