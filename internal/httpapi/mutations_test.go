package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/goobers/goobers/internal/apicontract"
	"github.com/goobers/goobers/internal/readservice"
)

func stagePath(action string) string {
	return apicontract.V1Prefix + "/runs/run-1/stages/review/" + action
}

type interventionCall struct {
	action string
	input  InterventionRequest
}

type fakeInterventions struct {
	calls  []interventionCall
	result InterventionResult
	err    error
}

func (f *fakeInterventions) call(action string, input InterventionRequest) (InterventionResult, error) {
	f.calls = append(f.calls, interventionCall{action: action, input: input})
	return f.result, f.err
}

func (f *fakeInterventions) Approve(_ context.Context, input InterventionRequest) (InterventionResult, error) {
	return f.call("approve", input)
}

func (f *fakeInterventions) Override(_ context.Context, input InterventionRequest) (InterventionResult, error) {
	return f.call("override", input)
}

func (f *fakeInterventions) RerunStage(_ context.Context, input InterventionRequest) (InterventionResult, error) {
	return f.call("rerun", input)
}

func newMutationRequest(method, action, body string) *http.Request {
	return httptest.NewRequest(method, stagePath(action), bytes.NewBufferString(body))
}

func TestMutationRoutesInvokeServiceThroughTier1Seam(t *testing.T) {
	service := &fakeInterventions{result: InterventionResult{Phase: "completed", State: "done"}}
	handler, err := NewHandler(
		&fakeReader{health: readservice.Health{Ready: true}},
		AllowAll,
		discardLogger(),
		WithInterventions(service),
	)
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		action string
		body   string
	}{
		{action: "approve", body: `{"actor":"local-user","decision":"pass"}`},
		{action: "override", body: `{"actor":"local-user","decision":"pass","rationale":"reviewed manually"}`},
		{action: "rerun", body: `{"actor":"local-user","instructionAddendum":"use the parser seam"}`},
	}
	for _, test := range tests {
		t.Run(test.action, func(t *testing.T) {
			service.calls = nil
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, newMutationRequest(http.MethodPost, test.action, test.body))
			if response.Code != http.StatusOK {
				t.Fatalf("status = %d, body = %s", response.Code, response.Body)
			}
			if len(service.calls) != 1 {
				t.Fatalf("calls = %+v", service.calls)
			}
			call := service.calls[0]
			if call.action != test.action || call.input.RunID != "run-1" ||
				call.input.Stage != "review" || call.input.Actor != "local-user" {
				t.Fatalf("call = %+v", call)
			}
			var result InterventionResult
			if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
				t.Fatal(err)
			}
			if result != service.result {
				t.Fatalf("result = %+v, want %+v", result, service.result)
			}
		})
	}
}

func TestMutationRoutesUseAuthenticatedPrincipalAsActor(t *testing.T) {
	service := &fakeInterventions{}
	authenticator := &fakeAuthenticator{principal: &Principal{Subject: "operator", Roles: []Role{RoleOperate}}}
	handler, err := NewHandler(
		&fakeReader{},
		RequireRoles(),
		discardLogger(),
		WithAuthenticator(authenticator),
		WithInterventions(service),
	)
	if err != nil {
		t.Fatal(err)
	}

	response := httptest.NewRecorder()
	handler.ServeHTTP(response, newMutationRequest(http.MethodPost, "approve", `{"actor":"spoofed","decision":"pass"}`))
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body)
	}
	if len(service.calls) != 1 || service.calls[0].input.Actor != "operator" {
		t.Fatalf("calls = %+v, want authenticated actor", service.calls)
	}
}

func TestMutationRoutesRequireOperateRole(t *testing.T) {
	service := &fakeInterventions{}
	authenticator := &fakeAuthenticator{principal: &Principal{Subject: "viewer", Roles: []Role{RoleView}}}
	handler, err := NewHandler(
		&fakeReader{},
		RequireRoles(),
		discardLogger(),
		WithAuthenticator(authenticator),
		WithInterventions(service),
	)
	if err != nil {
		t.Fatal(err)
	}

	response := httptest.NewRecorder()
	handler.ServeHTTP(response, newMutationRequest(http.MethodPost, "approve", `{"decision":"pass"}`))
	if response.Code != http.StatusForbidden {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body)
	}
	if len(service.calls) != 0 {
		t.Fatalf("unauthorized service calls = %+v", service.calls)
	}

	authenticator.principal = nil
	authenticator.err = errors.New("bad token")
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, newMutationRequest(http.MethodPost, "approve", `{"decision":"pass"}`))
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body)
	}
}

func TestMutationRoutesValidateRequestsAndSurfaceRefusals(t *testing.T) {
	service := &fakeInterventions{}
	handler, err := NewHandler(&fakeReader{}, AllowAll, discardLogger(), WithInterventions(service))
	if err != nil {
		t.Fatal(err)
	}

	for _, test := range []struct {
		name string
		body string
		code string
	}{
		{name: "missing body", body: "", code: "invalid_request"},
		{name: "unknown field", body: `{"actor":"local","unknown":true}`, code: "invalid_request"},
		{name: "missing actor", body: `{"decision":"pass"}`, code: "actor_required"},
	} {
		t.Run(test.name, func(t *testing.T) {
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, newMutationRequest(http.MethodPost, "approve", test.body))
			if response.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, body = %s", response.Code, response.Body)
			}
			var envelope ErrorEnvelope
			if err := json.NewDecoder(response.Body).Decode(&envelope); err != nil {
				t.Fatal(err)
			}
			if envelope.Error.Code != test.code {
				t.Fatalf("error = %+v", envelope.Error)
			}
		})
	}

	service.err = NewInterventionError(http.StatusConflict, "run_not_escalated", "run is not escalated", errors.New("internal detail"))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, newMutationRequest(http.MethodPost, "approve", `{"actor":"local","decision":"pass"}`))
	if response.Code != http.StatusConflict {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body)
	}
	var envelope ErrorEnvelope
	if err := json.NewDecoder(response.Body).Decode(&envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.Error.Code != "run_not_escalated" || envelope.Error.Message != "run is not escalated" {
		t.Fatalf("error = %+v", envelope.Error)
	}
}

func TestMutationRoutesUnavailableWithoutService(t *testing.T) {
	handler, err := NewHandler(&fakeReader{}, AllowAll, discardLogger())
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, newMutationRequest(http.MethodPost, "approve", `{"actor":"local"}`))
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body)
	}
}

func TestMutationRoutesRejectWrongMethod(t *testing.T) {
	handler, err := NewHandler(&fakeReader{}, AllowAll, discardLogger(), WithInterventions(&fakeInterventions{}))
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, newMutationRequest(http.MethodGet, "approve", `{"actor":"local"}`))
	if response.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body)
	}
}

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
		if found[id] != capability {
			t.Fatalf("action %q capability = %q, want %q", id, found[id], capability)
		}
	}
	if len(found) != len(want) {
		t.Fatalf("runtime mutations = %+v, want %+v", found, want)
	}
}
