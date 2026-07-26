package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/apicontract"
	"github.com/goobers/goobers/internal/readservice"
)

func stagePath(action string) string {
	return apicontract.V1Prefix + "/runs/run-1/stages/review/" + action
}

// TestMutationRoutesPassThroughTier1SeamUnauthenticated proves the tier-1
// default (#469's own acceptance criterion): with the null authenticator and
// AllowAll authorizer, a tier-2 mutation reaches its stub handler with no
// auth required at all — the seam neither blocks nor silently 404s it.
func TestMutationRoutesPassThroughTier1SeamUnauthenticated(t *testing.T) {
	handler, err := NewHandler(&fakeReader{health: readservice.Health{Ready: true}}, AllowAll, discardLogger())
	if err != nil {
		t.Fatal(err)
	}
	for _, action := range []string{"approve", "override", "rerun"} {
		t.Run(action, func(t *testing.T) {
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, httptest.NewRequest(http.MethodPost, stagePath(action), nil))
			if response.Code != http.StatusNotImplemented {
				t.Fatalf("status = %d, body = %s, want 501 (stub reachable, not 404/501-before-auth)", response.Code, response.Body)
			}
			var envelope ErrorEnvelope
			if err := json.NewDecoder(response.Body).Decode(&envelope); err != nil {
				t.Fatal(err)
			}
			if envelope.Error.Code != "not_implemented" || !strings.Contains(envelope.Error.Message, action) {
				t.Fatalf("error = %+v", envelope.Error)
			}
		})
	}
}

// TestMutationRoutesRequireOperateRole proves the seam is pluggable for a
// later auth tier: under RequireRoles(), a view-only principal is refused a
// mutation with 403, and an unauthenticated caller is refused with 401 —
// identical to how every existing read route behaves, since Router.Handle
// applies the same Authenticate-then-Authorize path to every route
// regardless of ActionClass.
func TestMutationRoutesRequireOperateRole(t *testing.T) {
	authenticator := &fakeAuthenticator{principal: &Principal{Subject: "viewer", Roles: []Role{RoleView}}}
	handler, err := NewHandler(&fakeReader{}, RequireRoles(), discardLogger(), WithAuthenticator(authenticator))
	if err != nil {
		t.Fatal(err)
	}

	// A view-only principal fails the operate floor RequireRoles() imposes on
	// every non-GET/HEAD method — 403, not a mutation-specific rule.
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodPost, stagePath("approve"), nil))
	if response.Code != http.StatusForbidden {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body)
	}

	// An operate principal passes authorization and reaches the stub.
	authenticator.principal = &Principal{Subject: "operator", Roles: []Role{RoleOperate}}
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodPost, stagePath("approve"), nil))
	if response.Code != http.StatusNotImplemented {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body)
	}

	// An unauthenticated caller is refused before authorization even runs.
	authenticator.principal = nil
	authenticator.err = errors.New("bad token")
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodPost, stagePath("approve"), nil))
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body)
	}
}

// TestMutationRoutesRejectWrongMethod proves a GET against a mutation route
// gets the structured 405, not a silent fall-through — the same contract
// every read route already gets from Router.Handle's method check.
func TestMutationRoutesRejectWrongMethod(t *testing.T) {
	handler, err := NewHandler(&fakeReader{}, AllowAll, discardLogger())
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, stagePath("approve"), nil))
	if response.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body)
	}
}

// TestMutationRoutesAreRuntimeMutationSurfaceActions proves the new routes
// are classified correctly in the API surface registry SurfaceActions()
// exposes for the future CLI/UI runtime-parity check (#466/#468 land those
// surfaces; this only proves the API side is already registered right).
func TestMutationRoutesAreRuntimeMutationSurfaceActions(t *testing.T) {
	want := map[apicontract.ActionID]apicontract.CapabilityID{
		"approveStage":  "approve",
		"overrideStage": "override",
		"rerunStage":    "rerun",
	}
	found := make(map[apicontract.ActionID]apicontract.CapabilityID, len(want))
	for _, action := range SurfaceActions() {
		if action.Class != apicontract.ActionRuntimeMutation {
			continue
		}
		found[action.ID] = action.Capability
	}
	for id, capability := range want {
		got, ok := found[id]
		if !ok {
			t.Fatalf("SurfaceActions() is missing runtime-mutation action %q", id)
		}
		if got != capability {
			t.Fatalf("action %q capability = %q, want %q", id, got, capability)
		}
	}
	if len(found) != len(want) {
		t.Fatalf("SurfaceActions() runtime mutations = %+v, want exactly %+v", found, want)
	}
}
