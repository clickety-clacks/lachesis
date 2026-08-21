//go:build !windows

package codex

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/clickety-clacks/lachesis/internal/teach"
)

func TestStartLoginUsesDeviceAuthorizationInIsolatedHome(t *testing.T) {
	bin := t.TempDir()
	script := filepath.Join(bin, "codex")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nprintf 'args=%s\\n' \"$*\"\nprintf 'home=%s\\n' \"$CODEX_HOME\"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	home := filepath.Join(t.TempDir(), "isolated-codex-home")
	process, detail := (&Adapter{}).StartLogin(context.Background(), home)
	if detail != nil {
		t.Fatal(detail)
	}
	waitDone := make(chan error, 1)
	go func() { waitDone <- process.Wait() }()
	raw, err := io.ReadAll(process.Output())
	if err != nil {
		t.Fatal(err)
	}
	if err = <-waitDone; err != nil {
		t.Fatal(err)
	}
	output := string(raw)
	if !strings.Contains(output, "args=login --device-auth\n") || !strings.Contains(output, "home="+home+"\n") {
		t.Fatalf("synthetic command output = %q", output)
	}
}

func TestStartLoginFailureUsesOnboardHelp(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	process, detail := (&Adapter{}).StartLogin(context.Background(), t.TempDir())
	if process != nil || detail == nil || detail.Code != teach.CLIMissing || detail.Message != "Codex device authorization could not start." || detail.Help != "/api/v1/help/onboard" || len(detail.Remedy.Commands) != 1 || detail.Remedy.Commands[0] != "retry the exact call" {
		t.Fatalf("process = %#v, detail = %#v", process, detail)
	}
}
