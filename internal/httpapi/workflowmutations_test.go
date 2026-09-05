package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/apicontract"
	"github.com/goobers/goobers/internal/readservice"
)

// fakeWorkflowMutations records SetWorkflowEnabled inputs and returns a
// configurable result/error so PUT-handler behavior can be pinned without a
// real reloader or on-disk workflow YAML.
type fakeWorkflowMutations struct {
	calls  []WorkflowEnabledRequest
	result WorkflowEnabledResult
	err    error
}

func (f *fakeWorkflowMutations) SetWorkflowEnabled(_ context.Context, input WorkflowEnabledRequest) (WorkflowEnabledResult, error) {
	f.calls = append(f.calls, input)
	return f.result, f.err
}

func workflowEnabledPath(gaggle, workflow string) string {
	return apicontract.V1Prefix + "/gaggles/" + gaggle + "/workflows/" + workflow + "/enabled"
}

func newWorkflowEnabledRequest(body string) *http.Request {
	request := httptest.NewRequest(http.MethodPut, workflowEnabledPath("web", "implement"), bytes.NewBufferString(body))
	request.Host = "127.0.0.1:8080"
	request.Header.Set("Content-Type", "application/json")
	return request
}

func TestWorkflowEnabledRouteUnavailableWithoutService(t *testing.T) {
	// Even without WithWorkflowMutations, registerWorkflowMutationRoutes runs
	// so the surface is discoverable; the handler must fail closed with the
	// documented 503/workflow_mutations_unavailable envelope.
	handler, err := NewHandler(&fakeReader{}, AllowAll, discardLogger())
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, newWorkflowEnabledRequest(`{"enabled":true}`))
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body)
	}
	if code := errorCode(t, response); code != "workflow_mutations_unavailable" {
		t.Fatalf("code = %q, want workflow_mutations_unavailable", code)
	}
}

func TestWorkflowEnabledRouteRejectsNonJSONMediaType(t *testing.T) {
	service := &fakeWorkflowMutations{}
	handler, err := NewHandler(&fakeReader{}, AllowAll, discardLogger(), WithWorkflowMutations(service))
	if err != nil {
		t.Fatal(err)
	}
	request := newWorkflowEnabledRequest(`{"enabled":true}`)
	request.Header.Set("Content-Type", "text/plain")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body)
	}
	if code := errorCode(t, response); code != "unsupported_media_type" {
		t.Fatalf("code = %q, want unsupported_media_type", code)
	}
	if len(service.calls) != 0 {
		t.Fatalf("service was called for unsupported media type: %+v", service.calls)
	}
}

func TestWorkflowEnabledRouteRejectsCrossOriginRequests(t *testing.T) {
	service := &fakeWorkflowMutations{}
	handler, err := NewHandler(&fakeReader{}, AllowAll, discardLogger(), WithWorkflowMutations(service))
	if err != nil {
		t.Fatal(err)
	}
	request := newWorkflowEnabledRequest(`{"enabled":true}`)
	request.Header.Set("Origin", "https://attacker.example")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusForbidden {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body)
	}
	if code := errorCode(t, response); code != "origin_forbidden" {
		t.Fatalf("code = %q, want origin_forbidden", code)
	}
	if len(service.calls) != 0 {
		t.Fatalf("service was called on cross-origin: %+v", service.calls)
	}
}

func TestWorkflowEnabledRouteValidatesRequestBody(t *testing.T) {
	service := &fakeWorkflowMutations{}
	handler, err := NewHandler(&fakeReader{}, AllowAll, discardLogger(), WithWorkflowMutations(service))
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name         string
		body         string
		wantContains string
	}{
		{name: "empty body", body: "", wantContains: "JSON request body is required"},
		{name: "unknown field", body: `{"enabled":true,"unknown":"x"}`, wantContains: "invalid JSON request body"},
		{name: "trailing JSON", body: `{"enabled":true}{"enabled":false}`, wantContains: "one JSON object"},
		{name: "trailing garbage", body: `{"enabled":true}garbage`, wantContains: "invalid JSON request body"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, newWorkflowEnabledRequest(test.body))
			if response.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, body = %s", response.Code, response.Body)
			}
			var envelope ErrorEnvelope
			if err := json.NewDecoder(response.Body).Decode(&envelope); err != nil {
				t.Fatal(err)
			}
			if envelope.Error.Code != "invalid_request" {
				t.Fatalf("code = %q, want invalid_request", envelope.Error.Code)
			}
			if !strings.Contains(envelope.Error.Message, test.wantContains) {
				t.Fatalf("message = %q, want to contain %q", envelope.Error.Message, test.wantContains)
			}
			if len(service.calls) != 0 {
				t.Fatalf("service was called on invalid body: %+v", service.calls)
			}
		})
	}
}

func TestWorkflowEnabledRouteRejectsOversizedBody(t *testing.T) {
	// The handler bounds the request body at 64 KiB via io.LimitReader before
	// JSON decoding, so a body larger than the limit truncates and yields an
	// invalid-JSON decode error (the trailing brace is cut off).
	service := &fakeWorkflowMutations{}
	handler, err := NewHandler(&fakeReader{}, AllowAll, discardLogger(), WithWorkflowMutations(service))
	if err != nil {
		t.Fatal(err)
	}
	prefix := `{"enabled":true,"padding":"`
	suffix := `"}`
	// 64 KiB of raw padding plus the wrapper puts the closing brace beyond
	// the limit, so the reader-under-limit stops mid-string and the decoder
	// reports invalid JSON.
	padding := strings.Repeat("x", 1<<16)
	body := prefix + padding + suffix
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, newWorkflowEnabledRequest(body))
	if response.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body)
	}
	if code := errorCode(t, response); code != "invalid_request" {
		t.Fatalf("code = %q, want invalid_request", code)
	}
	if len(service.calls) != 0 {
		t.Fatalf("service was called on oversized body: %+v", service.calls)
	}
}

func TestWorkflowEnabledRouteMapsInterventionError(t *testing.T) {
	tests := []struct {
		name       string
		serviceErr error
		wantStatus int
		wantCode   string
		wantMsg    string
	}{
		{
			name:       "well-formed intervention error",
			serviceErr: NewInterventionError(http.StatusNotFound, "workflow_not_found", "workflow was not found", errors.New("internal detail")),
			wantStatus: http.StatusNotFound,
			wantCode:   "workflow_not_found",
			wantMsg:    "workflow was not found",
		},
		{
			name:       "invalid status clamps to 500",
			serviceErr: NewInterventionError(200, "wrapped_ok", "should not be sent", nil),
			wantStatus: http.StatusInternalServerError,
			wantCode:   "wrapped_ok",
			wantMsg:    "should not be sent",
		},
		{
			name:       "empty code and message get safe defaults",
			serviceErr: NewInterventionError(http.StatusUnprocessableEntity, "", "", nil),
			wantStatus: http.StatusUnprocessableEntity,
			wantCode:   "workflow_mutation_failed",
			wantMsg:    "workflow config mutation failed",
		},
		{
			name:       "plain error surfaces as generic 500",
			serviceErr: errors.New("disk full"),
			wantStatus: http.StatusInternalServerError,
			wantCode:   "workflow_mutation_failed",
			wantMsg:    "workflow config mutation failed",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			service := &fakeWorkflowMutations{err: test.serviceErr}
			handler, err := NewHandler(&fakeReader{}, AllowAll, discardLogger(), WithWorkflowMutations(service))
			if err != nil {
				t.Fatal(err)
			}
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, newWorkflowEnabledRequest(`{"enabled":false}`))
			if response.Code != test.wantStatus {
				t.Fatalf("status = %d, want %d, body = %s", response.Code, test.wantStatus, response.Body)
			}
			var envelope ErrorEnvelope
			if err := json.NewDecoder(response.Body).Decode(&envelope); err != nil {
				t.Fatal(err)
			}
			if envelope.Error.Code != test.wantCode || envelope.Error.Message != test.wantMsg {
				t.Fatalf("envelope = %+v, want code=%q msg=%q", envelope.Error, test.wantCode, test.wantMsg)
			}
			if len(service.calls) != 1 {
				t.Fatalf("service calls = %d, want one", len(service.calls))
			}
		})
	}
}

func TestWorkflowEnabledRouteSuccessDispatchesToService(t *testing.T) {
	service := &fakeWorkflowMutations{
		result: WorkflowEnabledResult{Gaggle: "web", Workflow: "implement", Enabled: false},
	}
	handler, err := NewHandler(
		&fakeReader{health: readservice.Health{Ready: true}},
		AllowAll,
		discardLogger(),
		WithWorkflowMutations(service),
	)
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, newWorkflowEnabledRequest(`{"enabled":false}`))
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body)
	}
	if len(service.calls) != 1 {
		t.Fatalf("service calls = %d, want one", len(service.calls))
	}
	call := service.calls[0]
	if call.Gaggle != "web" || call.Workflow != "implement" || call.Enabled != false {
		t.Fatalf("call = %+v, want gaggle=web workflow=implement enabled=false", call)
	}
	var got WorkflowEnabledResult
	if err := json.NewDecoder(response.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got != service.result {
		t.Fatalf("result = %+v, want %+v", got, service.result)
	}
}

func TestWorkflowEnabledRouteRejectsWrongMethod(t *testing.T) {
	service := &fakeWorkflowMutations{}
	handler, err := NewHandler(&fakeReader{}, AllowAll, discardLogger(), WithWorkflowMutations(service))
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, workflowEnabledPath("web", "implement"), bytes.NewBufferString(`{"enabled":true}`))
	request.Host = "127.0.0.1:8080"
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body)
	}
	if len(service.calls) != 0 {
		t.Fatalf("service was called on wrong method: %+v", service.calls)
	}
}
