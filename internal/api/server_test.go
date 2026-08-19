package api

import (
	"context"
	"encoding/json"
	"github.com/clickety-clacks/lachesis/internal/core"
	"github.com/clickety-clacks/lachesis/internal/model"
	"net/http"
	"net/http/httptest"
	"testing"
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
