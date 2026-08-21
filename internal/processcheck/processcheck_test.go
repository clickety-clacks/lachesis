package processcheck

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/clickety-clacks/lachesis/internal/model"
)

func TestBusyUsesExactTargetProviderHome(t *testing.T) {
	providers := []struct {
		provider model.Provider
		homeKey  string
	}{
		{provider: model.ProviderCodex, homeKey: "CODEX_HOME"},
		{provider: model.ProviderClaude, homeKey: "CLAUDE_CONFIG_DIR"},
	}
	for _, providerTest := range providers {
		t.Run(string(providerTest.provider), func(t *testing.T) {
			targetHome := "/srv/lachesis/providers/" + string(providerTest.provider) + "/target"
			otherHome := "/srv/lachesis/providers/" + string(providerTest.provider) + "/other"
			for _, test := range []struct {
				name     string
				metadata string
				busy     bool
			}{
				{name: "unrelated Tightbeam home", metadata: providerTest.homeKey + "=/srv/tightbeam/homes/adapter HOME=/srv/operator"},
				{name: "other account home", metadata: providerTest.homeKey + "=" + otherHome + " HOME=/srv/operator"},
				{name: "exact target home", metadata: providerTest.homeKey + "=" + targetHome + " HOME=/srv/operator", busy: true},
			} {
				t.Run(test.name, func(t *testing.T) {
					processes := "101 /usr/local/bin/" + string(providerTest.provider) + "\n104 unrelated\n"
					checker := PS{goos: "linux", run: syntheticPS(processes, map[string]string{"101": test.metadata}, nil)}
					busy, err := checker.Busy(context.Background(), Target{Provider: providerTest.provider, Home: targetHome})
					if err != nil || busy != test.busy {
						t.Fatalf("busy = %v, err = %v", busy, err)
					}
				})
			}
		})
	}
}

func TestUnreadableProviderMetadataDoesNotBecomeHostGlobal(t *testing.T) {
	for _, providerName := range []model.Provider{model.ProviderCodex, model.ProviderClaude} {
		t.Run(string(providerName), func(t *testing.T) {
			checker := PS{goos: "linux", run: syntheticPS(
				"201 "+string(providerName)+"\n202 "+string(providerName)+"\n203 "+string(providerName)+"\n",
				map[string]string{
					"201": providerHomeKey(providerName) + "=/srv/tightbeam/homes/adapter HOME=/srv/operator",
					"202": providerHomeKey(providerName) + "=/srv/lachesis/providers/" + string(providerName) + "/other HOME=/srv/operator",
				},
				map[string]error{"203": errors.New("synthetic metadata refusal")},
			)}
			busy, err := checker.Busy(context.Background(), Target{Provider: providerName, Home: "/srv/lachesis/providers/" + string(providerName) + "/target"})
			if err != nil || busy {
				t.Fatalf("busy = %v, err = %v", busy, err)
			}
		})
	}
}

func TestCodexDefaultHomeUsesProcessHome(t *testing.T) {
	checker := PS{goos: "linux", run: syntheticPS("301 codex\n", map[string]string{"301": "PATH=/bin HOME=/srv/operator"}, nil)}
	busy, err := checker.Busy(context.Background(), Target{Provider: model.ProviderCodex, Home: "/srv/operator/.codex"})
	if err != nil || !busy {
		t.Fatalf("busy = %v, err = %v", busy, err)
	}
}

func TestProviderHomeUsesFinalEnvironmentAssignment(t *testing.T) {
	for _, test := range []struct {
		provider model.Provider
		metadata string
	}{
		{provider: model.ProviderCodex, metadata: "/usr/bin/codex prompt CODEX_HOME=/argument/path PATH=/bin CODEX_HOME=/srv/target home HOME=/srv/operator"},
		{provider: model.ProviderClaude, metadata: "/usr/bin/claude prompt CLAUDE_CONFIG_DIR=/argument/path PATH=/bin CLAUDE_CONFIG_DIR=/srv/target home HOME=/srv/operator"},
	} {
		home, ok := providerHomeFromMetadata(test.provider, test.metadata)
		if !ok || home != "/srv/target home" {
			t.Fatalf("%s home = %q, ok = %v", test.provider, home, ok)
		}
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

func TestMetadataArgumentsArePlatformSpecific(t *testing.T) {
	if got := metadataArgs("darwin", "501"); !reflect.DeepEqual(got, []string{"-Eww", "-p", "501", "-o", "command="}) {
		t.Fatalf("darwin args = %#v", got)
	}
	if got := metadataArgs("linux", "501"); !reflect.DeepEqual(got, []string{"-ww", "-p", "501", "-o", "environ="}) {
		t.Fatalf("linux args = %#v", got)
	}
}

func providerHomeKey(providerName model.Provider) string {
	if providerName == model.ProviderClaude {
		return "CLAUDE_CONFIG_DIR"
	}
	return "CODEX_HOME"
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
