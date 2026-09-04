package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// #3807: cancelling a live run over the API is what lets an operator stop one
// without sharing the daemon's filesystem, so the route must carry the same
// transport discipline as its sibling mutations and hand the daemon a request
// it can act on.

type fakeCancelService struct {
	result CancelRunResult
	err    error
	inputs []CancelRunRequest
}

func (f *fakeCancelService) Cancel(_ context.Context, input CancelRunRequest) (CancelRunResult, error) {
	f.inputs = append(f.inputs, input)
	return f.result, f.err
}

func TestCancelRouteRequiresKeyAndActor(t *testing.T) {
	cancels := &fakeCancelService{result: CancelRunResult{Code: CancelCodeAborted, Phase: "aborted"}}
	authenticator := &fakeAuthenticator{principal: &Principal{Subject: "operator", Roles: []Role{RoleOperate}}}
	handler := writePlaneHandler(t, authenticator, RequireRoles(), WithCancelService(cancels))
	path := "/api/v1/runs/run-1/cancel"

	response := httptest.NewRecorder()
	handler.ServeHTTP(response, jsonRequest(http.MethodPost, path, `{}`))
	if response.Code != http.StatusBadRequest {
		t.Fatalf("missing key status = %d, want 400", response.Code)
	}
	if len(cancels.inputs) != 0 {
		t.Fatalf("a keyless cancel reached the service")
	}

	request := jsonRequest(http.MethodPost, path, `{"gaggle":"g","workflow":"w"}`)
	request.Header.Set("Idempotency-Key", "key-1")
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("cancel status = %d, body = %s", response.Code, response.Body)
	}
	if len(cancels.inputs) != 1 {
		t.Fatalf("cancel service saw %d inputs", len(cancels.inputs))
	}
	input := cancels.inputs[0]
	if input.RunID != "run-1" || input.Actor != "operator" || input.Gaggle != "g" || input.Workflow != "w" {
		t.Fatalf("input = %+v", input)
	}
	var decoded CancelRunResult
	if err := json.NewDecoder(response.Body).Decode(&decoded); err != nil {
		t.Fatalf("decode cancel result: %v", err)
	}
	if decoded.Code != CancelCodeAborted || decoded.Phase != "aborted" {
		t.Fatalf("result = %+v", decoded)
	}
}

// TestCancelRouteReportsRefusalsAsResults pins the disposition contract the
// CLI depends on: "this daemon is not running that run" is a well-formed
// answer with a code, not a transport error.
func TestCancelRouteReportsRefusalsAsResults(t *testing.T) {
	cancels := &fakeCancelService{result: CancelRunResult{Code: CancelCodeNotRunning, Error: "run run-1 is not currently running under this daemon"}}
	handler := writePlaneHandler(t, nil, AllowAll, WithCancelService(cancels))

	request := jsonRequest(http.MethodPost, "/api/v1/runs/run-1/cancel", `{"actor":"cli"}`)
	request.Header.Set("Idempotency-Key", "key-1")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body)
	}
	var decoded CancelRunResult
	if err := json.NewDecoder(response.Body).Decode(&decoded); err != nil {
		t.Fatalf("decode cancel result: %v", err)
	}
	if decoded.Code != CancelCodeNotRunning || decoded.Error == "" {
		t.Fatalf("result = %+v", decoded)
	}
}

// TestCancelRouteRequiresAuthenticationOffLoopback keeps the route on the same
// authenticated posture as the rest of the write surface.
func TestCancelRouteRequiresAuthenticationOffLoopback(t *testing.T) {
	cancels := &fakeCancelService{}
	authenticator := &fakeAuthenticator{err: context.DeadlineExceeded}
	handler := writePlaneHandler(t, authenticator, RequireRoles(), WithCancelService(cancels))

	request := jsonRequest(http.MethodPost, "/api/v1/runs/run-1/cancel", `{}`)
	request.Header.Set("Idempotency-Key", "key-1")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", response.Code)
	}
	if len(cancels.inputs) != 0 {
		t.Fatalf("an unauthenticated cancel reached the service")
	}
}

// TestCancelRouteUnavailableWithoutService pins the degraded posture shared
// with the other planes: the surface exists, this deployment does not serve it.
func TestCancelRouteUnavailableWithoutService(t *testing.T) {
	handler := writePlaneHandler(t, nil, AllowAll)
	request := jsonRequest(http.MethodPost, "/api/v1/runs/run-1/cancel", `{}`)
	request.Header.Set("Idempotency-Key", "key-1")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", response.Code)
	}
}
