//go:build ignore

package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
)

type accountCounts struct {
	Total          int `json:"total"`
	Ready          int `json:"ready"`
	Degraded       int `json:"degraded"`
	ReauthRequired int `json:"reauth_required"`
	Unknown        int `json:"unknown"`
}

type providerHealth struct {
	CLIAvailable bool          `json:"cli_available"`
	Accounts     accountCounts `json:"accounts"`
}

type healthResponse struct {
	Status    string                    `json:"status"`
	Version   string                    `json:"version"`
	Providers map[string]providerHealth `json:"providers"`
	Links     map[string]string         `json:"links"`
}

type helpResponse struct {
	Topics []struct {
		Name    string `json:"name"`
		Summary string `json:"summary"`
		Path    string `json:"path"`
	} `json:"topics"`
}

type errorResponse struct {
	Error struct {
		Code          string           `json:"code"`
		Message       string           `json:"message"`
		Prerequisites []map[string]any `json:"prerequisites"`
		State         map[string]any   `json:"state"`
		Remedy        struct {
			Summary  string           `json:"summary"`
			Calls    []map[string]any `json:"calls"`
			Commands []string         `json:"commands"`
		} `json:"remedy"`
		Help string `json:"help"`
	} `json:"error"`
	RequestID string `json:"request_id"`
}

func main() {
	if len(os.Args) != 2 {
		fatalf("usage: smokecheck base-url")
	}
	base := strings.TrimRight(os.Args[1], "/")
	var health healthResponse
	healthRaw := get(base+"/api/v1/health", http.StatusOK, &health)
	if health.Status != "ready" || health.Version != "0.1.0" {
		fatalf("invalid health status or version")
	}
	if len(health.Providers) != 2 || health.Providers["codex"].Accounts.Total != 0 || health.Providers["claude"].Accounts.Total != 0 {
		fatalf("invalid empty-registry provider health")
	}
	wantLinks := map[string]string{"accounts": "/api/v1/accounts", "usage": "/api/v1/usage", "help": "/api/v1/help"}
	for key, want := range wantLinks {
		if health.Links[key] != want {
			fatalf("invalid health link %s", key)
		}
	}

	var help helpResponse
	helpRaw := get(base+"/api/v1/help", http.StatusOK, &help)
	names := make([]string, 0, len(help.Topics))
	for _, topic := range help.Topics {
		if topic.Name == "" || topic.Summary == "" || topic.Path != "/api/v1/help/"+topic.Name {
			fatalf("invalid help topic")
		}
		names = append(names, topic.Name)
	}
	sort.Strings(names)
	wantNames := []string{"accounts", "adopt", "health", "jobs", "onboard", "re-onboard", "refresh", "usage", "verify"}
	if fmt.Sprint(names) != fmt.Sprint(wantNames) {
		fatalf("invalid help topic set: %v", names)
	}

	var envelope errorResponse
	errorRaw := get(base+"/api/v1/usage", http.StatusConflict, &envelope)
	if envelope.Error.Code != "NO_ACCOUNTS_ONBOARDED" || envelope.Error.Message == "" || envelope.Error.Help != "/api/v1/help/usage" {
		fatalf("invalid empty-registry error")
	}
	if !strings.HasPrefix(envelope.RequestID, "req_") || envelope.Error.Prerequisites == nil || envelope.Error.State == nil || len(envelope.Error.Remedy.Calls) != 2 || envelope.Error.Remedy.Commands == nil {
		fatalf("invalid ErrorEnvelope structure")
	}
	if count, ok := envelope.Error.State["accounts_onboarded"].(float64); !ok || count != 0 {
		fatalf("invalid empty-registry state")
	}

	for _, raw := range [][]byte{healthRaw, helpRaw, errorRaw} {
		if strings.Contains(strings.ToLower(string(raw)), "smoke_secret_sentinel") {
			fatalf("response contains forbidden sentinel")
		}
		var value any
		if err := json.Unmarshal(raw, &value); err != nil {
			fatalf("decode response for credential scan: %v", err)
		}
		if key := forbiddenResponseKey(value); key != "" {
			fatalf("response contains forbidden credential field %q", key)
		}
	}
}

func forbiddenResponseKey(value any) string {
	forbidden := map[string]bool{"access_token": true, "refresh_token": true, "id_token": true, "authorization": true, "cookie": true}
	switch typed := value.(type) {
	case map[string]any:
		for key, child := range typed {
			if forbidden[strings.ToLower(key)] {
				return key
			}
			if found := forbiddenResponseKey(child); found != "" {
				return found
			}
		}
	case []any:
		for _, child := range typed {
			if found := forbiddenResponseKey(child); found != "" {
				return found
			}
		}
	}
	return ""
}

func get(url string, wantStatus int, dst any) []byte {
	response, err := http.Get(url)
	if err != nil {
		fatalf("GET %s: %v", url, err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		fatalf("read %s: %v", url, err)
	}
	if response.StatusCode != wantStatus || !strings.HasPrefix(response.Header.Get("Content-Type"), "application/json") {
		fatalf("GET %s returned %d %q", url, response.StatusCode, response.Header.Get("Content-Type"))
	}
	if err := json.Unmarshal(body, dst); err != nil {
		fatalf("decode %s: %v", url, err)
	}
	return body
}

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}
