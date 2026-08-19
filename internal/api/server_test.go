package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/clickety-clacks/lachesis/internal/core"
	"github.com/clickety-clacks/lachesis/internal/model"
	"github.com/clickety-clacks/lachesis/internal/provider"
	"github.com/clickety-clacks/lachesis/internal/provider/claude"
)

type checker struct{}

func (checker) Busy(_ context.Context, _ model.Provider) (bool, error) { return false, nil }
func TestEmptyUsageTeaches(t *testing.T) {
	svc, d := core.OpenService(t.TempDir(), nil, checker{})
	if d != nil {
		t.Fatal(d)
	}
	defer svc.Close()
	rr := httptest.NewRecorder()
	New(svc).Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/v1/usage", nil))
	if rr.Code != http.StatusConflict {
		t.Fatalf("status %d", rr.Code)
	}
	var env model.ErrorEnvelope
	if json.Unmarshal(rr.Body.Bytes(), &env) != nil || env.Error.Code != "NO_ACCOUNTS_ONBOARDED" || len(env.Error.Remedy.Calls) != 2 {
		t.Fatalf("body %s", rr.Body.String())
	}
}

func TestKeychainAdoptionReturnsStructuralFileOnlyRemedy(t *testing.T) {
	svc, d := core.OpenService(t.TempDir(), []provider.Adapter{claude.New(nil)}, checker{})
	if d != nil {
		t.Fatal(d)
	}
	defer svc.Close()
	body := `{"provider":"claude","label":"work","source":{"kind":"keychain","service":"legacy","account":"default"}}`
	rr := httptest.NewRecorder()
	New(svc).Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/api/v1/accounts/adopt", strings.NewReader(body)))
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status %d: %s", rr.Code, rr.Body.String())
	}
	var env model.ErrorEnvelope
	if json.Unmarshal(rr.Body.Bytes(), &env) != nil || env.Error == nil || env.Error.Code != "KEYCHAIN_SOURCE_UNSUPPORTED" {
		t.Fatalf("body %s", rr.Body.String())
	}
	if len(env.Error.Remedy.Calls) != 2 || env.Error.Remedy.Calls[0].Path != "/api/v1/accounts" || env.Error.Remedy.Calls[1].Path != "/api/v1/accounts/adopt" {
		t.Fatalf("remedy %#v", env.Error.Remedy)
	}
}
