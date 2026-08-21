package processcheck

import (
	"context"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"

	"github.com/clickety-clacks/lachesis/internal/model"
)

type Target struct {
	Provider model.Provider
	Home     string
}

type Checker interface {
	Busy(context.Context, Target) (bool, error)
}

type commandRunner func(context.Context, string, ...string) ([]byte, error)

type PS struct {
	run  commandRunner
	goos string
}

func (p PS) Busy(ctx context.Context, target Target) (bool, error) {
	run := p.run
	if run == nil {
		run = runCommand
	}
	goos := p.goos
	if goos == "" {
		goos = runtime.GOOS
	}
	out, err := run(ctx, "ps", "-axo", "pid=,comm=")
	if err != nil {
		return false, err
	}
	want := string(target.Provider)
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 || commandBase(fields[1]) != want {
			continue
		}
		if target.Provider != model.ProviderCodex {
			return true, nil
		}
		metadata, metadataErr := run(ctx, "ps", metadataArgs(goos, fields[0])...)
		if metadataErr != nil {
			// The process may have exited between the process list and metadata read.
			// A per-process miss cannot prove a target match and must not widen to
			// every Codex home on the host.
			continue
		}
		home, ok := codexHomeFromMetadata(string(metadata))
		if ok && sameHome(home, target.Home) {
			return true, nil
		}
	}
	return false, nil
}

func runCommand(ctx context.Context, name string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, name, args...).Output()
}

func metadataArgs(goos, pid string) []string {
	if goos == "darwin" {
		return []string{"-Eww", "-p", pid, "-o", "command="}
	}
	return []string{"-ww", "-p", pid, "-o", "environ="}
}

func commandBase(command string) string {
	if i := strings.LastIndex(command, "/"); i >= 0 {
		return command[i+1:]
	}
	return command
}

var environmentEntry = regexp.MustCompile(`(?:^|\s)([A-Za-z_][A-Za-z0-9_]*)=`)

func codexHomeFromMetadata(metadata string) (string, bool) {
	matches := environmentEntry.FindAllStringSubmatchIndex(metadata, -1)
	var codexHome, userHome string
	for i, match := range matches {
		name := metadata[match[2]:match[3]]
		if name != "CODEX_HOME" && name != "HOME" {
			continue
		}
		end := len(metadata)
		if i+1 < len(matches) {
			end = matches[i+1][0]
		}
		value := strings.TrimSpace(metadata[match[1]:end])
		if name == "CODEX_HOME" {
			codexHome = value
		} else {
			userHome = value
		}
	}
	if codexHome != "" {
		return codexHome, true
	}
	if userHome != "" {
		return filepath.Join(userHome, ".codex"), true
	}
	return "", false
}

func sameHome(processHome, targetHome string) bool {
	return filepath.IsAbs(processHome) && filepath.IsAbs(targetHome) && filepath.Clean(processHome) == filepath.Clean(targetHome)
}
