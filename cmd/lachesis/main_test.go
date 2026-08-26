package main

import (
	"debug/elf"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

func TestDevelopmentBuildInfoDefaults(t *testing.T) {
	if buildVersion != "development" || buildCommit != "unknown" {
		t.Fatalf("build info = %q, %q", buildVersion, buildCommit)
	}
}

func TestBuiltBinaryBuildInfoLdflags(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the exact-commit build gate runs on Gibson Linux")
	}
	const version = "test-version"
	const commit = "0123456789abcdef0123456789abcdef01234567"
	bin := filepath.Join(t.TempDir(), "lachesis")
	cmd := exec.Command("go", "build", "-ldflags", "-X main.buildVersion="+version+" -X main.buildCommit="+commit, "-o", bin, ".")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build stamped binary: %v\n%s", err, output)
	}
	if got := linkedString(t, bin, "main.buildVersion.str"); got != version {
		t.Fatalf("linked version = %q", got)
	}
	if got := linkedString(t, bin, "main.buildCommit.str"); got != commit {
		t.Fatalf("linked commit = %q", got)
	}
}

func linkedString(t *testing.T, path, name string) string {
	t.Helper()
	f, err := elf.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	symbols, err := f.Symbols()
	if err != nil {
		t.Fatal(err)
	}
	for _, symbol := range symbols {
		if symbol.Name != name {
			continue
		}
		for _, section := range f.Sections {
			if symbol.Value < section.Addr || symbol.Value+symbol.Size > section.Addr+section.Size {
				continue
			}
			data, err := section.Data()
			if err != nil {
				t.Fatal(err)
			}
			start := symbol.Value - section.Addr
			value := data[start : start+symbol.Size]
			if len(value) == 0 || value[len(value)-1] != 0 {
				t.Fatalf("symbol %q is not NUL-terminated", name)
			}
			return string(value[:len(value)-1])
		}
		t.Fatalf("symbol %q has no file section", name)
	}
	t.Fatalf("symbol %q not found", name)
	return ""
}
