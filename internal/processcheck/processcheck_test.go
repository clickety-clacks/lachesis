package processcheck

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/clickety-clacks/lachesis/internal/model"
)

func TestCodexBusyUsesExactTargetHome(t *testing.T) {
	for _, test := range []struct {
		name     string
		metadata string
		busy     bool
	}{
		{name: "unrelated Tightbeam home", metadata: "CODEX_HOME=/srv/tightbeam/homes/adapter HOME=/srv/operator"},
		{name: "other account home", metadata: "CODEX_HOME=/srv/lachesis/providers/codex/other HOME=/srv/operator"},
		{name: "exact target home", metadata: "CODEX_HOME=/srv/lachesis/providers/codex/target HOME=/srv/operator", busy: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			checker := PS{goos: "linux", run: syntheticPS("101 /usr/local/bin/codex\n104 claude\n", map[string]string{"101": test.metadata}, nil)}
			busy, err := checker.Busy(context.Background(), Target{Provider: model.ProviderCodex, Home: "/srv/lachesis/providers/codex/target"})
			if err != nil || busy != test.busy {
				t.Fatalf("busy = %v, err = %v", busy, err)
			}
		})
	}
}

func TestCodexUnrelatedProcessesDoNotBlock(t *testing.T) {
	checker := PS{goos: "linux", run: syntheticPS(
		"201 codex\n202 codex\n203 codex\n",
		map[string]string{
			"201": "CODEX_HOME=/srv/tightbeam/homes/adapter HOME=/srv/operator",
			"202": "CODEX_HOME=/srv/lachesis/providers/codex/other HOME=/srv/operator",
		},
		map[string]error{"203": errors.New("synthetic metadata refusal")},
	)}
	busy, err := checker.Busy(context.Background(), Target{Provider: model.ProviderCodex, Home: "/srv/lachesis/providers/codex/target"})
	if err != nil || busy {
		t.Fatalf("busy = %v, err = %v", busy, err)
	}
}

func TestCodexDefaultHomeUsesProcessHome(t *testing.T) {
	checker := PS{goos: "linux", run: syntheticPS("301 codex\n", map[string]string{"301": "PATH=/bin HOME=/srv/operator"}, nil)}
	busy, err := checker.Busy(context.Background(), Target{Provider: model.ProviderCodex, Home: "/srv/operator/.codex"})
	if err != nil || !busy {
		t.Fatalf("busy = %v, err = %v", busy, err)
	}
}

func TestCodexHomeUsesFinalEnvironmentAssignment(t *testing.T) {
	home, ok := codexHomeFromMetadata("/usr/bin/codex prompt CODEX_HOME=/argument/path PATH=/bin CODEX_HOME=/srv/target home HOME=/srv/operator")
	if !ok || home != "/srv/target home" {
		t.Fatalf("home = %q, ok = %v", home, ok)
	}
}

func TestProcessListFailureRemainsAnError(t *testing.T) {
	want := errors.New("synthetic process-list refusal")
	checker := PS{run: func(context.Context, string, ...string) ([]byte, error) { return nil, want }}
	busy, err := checker.Busy(context.Background(), Target{Provider: model.ProviderCodex, Home: "/srv/lachesis/providers/codex/target"})
	if busy || !errors.Is(err, want) {
		t.Fatalf("busy = %v, err = %v", busy, err)
	}
}

func TestClaudeBusyBehaviorRemainsProviderWide(t *testing.T) {
	calls := 0
	checker := PS{run: func(_ context.Context, _ string, args ...string) ([]byte, error) {
		calls++
		if !reflect.DeepEqual(args, []string{"-axo", "pid=,comm="}) {
			t.Fatalf("args = %#v", args)
		}
		return []byte("401 /usr/local/bin/claude\n"), nil
	}}
	busy, err := checker.Busy(context.Background(), Target{Provider: model.ProviderClaude, Home: "/unrelated/home"})
	if err != nil || !busy || calls != 1 {
		t.Fatalf("busy = %v, err = %v, calls = %d", busy, err, calls)
	}
}

func TestMetadataArgumentsArePlatformSpecific(t *testing.T) {
	if got := metadataArgs("darwin", "501"); !reflect.DeepEqual(got, []string{"-Eww", "-p", "501", "-o", "command="}) {
		t.Fatalf("darwin args = %#v", got)
	}
	if got := metadataArgs("linux", "501"); !reflect.DeepEqual(got, []string{"-ww", "-p", "501", "-o", "environ="}) {
		t.Fatalf("linux args = %#v", got)
	}
}

func syntheticPS(processes string, metadata map[string]string, failures map[string]error) commandRunner {
	return func(_ context.Context, _ string, args ...string) ([]byte, error) {
		if reflect.DeepEqual(args, []string{"-axo", "pid=,comm="}) {
			return []byte(processes), nil
		}
		if len(args) < 3 {
			return nil, errors.New("synthetic malformed ps call")
		}
		pid := args[2]
		if err := failures[pid]; err != nil {
			return nil, err
		}
		return []byte(metadata[pid]), nil
	}
}
