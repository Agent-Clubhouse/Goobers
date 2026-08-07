package httpapi

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

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

type blockingInterventions struct {
	started chan context.Context
	release chan struct{}
	done    chan error
}

func (b *blockingInterventions) call(ctx context.Context) (InterventionResult, error) {
	b.started <- ctx
	select {
	case <-ctx.Done():
		return InterventionResult{}, ctx.Err()
	case <-b.release:
		return InterventionResult{Phase: "completed", JournalSeq: 7}, nil
	}
}

func (b *blockingInterventions) Approve(ctx context.Context, _ InterventionRequest) (InterventionResult, error) {
	return b.call(ctx)
}

func (b *blockingInterventions) Override(ctx context.Context, _ InterventionRequest) (InterventionResult, error) {
	return b.call(ctx)
}

func (b *blockingInterventions) RerunStage(ctx context.Context, _ InterventionRequest) (InterventionResult, error) {
	return b.call(ctx)
}

func (b *blockingInterventions) accept(admission, execution context.Context) (InterventionResult, error) {
	if err := admission.Err(); err != nil {
		return InterventionResult{}, err
	}
	go func() {
		_, err := b.call(execution)
		if b.done != nil {
			b.done <- err
		}
	}()
	return InterventionResult{Phase: "running", JournalSeq: 7}, nil
}

func (b *blockingInterventions) AcceptApprove(admission, execution context.Context, _ InterventionRequest) (InterventionResult, error) {
	return b.accept(admission, execution)
}

func (b *blockingInterventions) AcceptOverride(admission, execution context.Context, _ InterventionRequest) (InterventionResult, error) {
	return b.accept(admission, execution)
}

func (b *blockingInterventions) AcceptRerunStage(admission, execution context.Context, _ InterventionRequest) (InterventionResult, error) {
	return b.accept(admission, execution)
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

func (f *fakeInterventions) AcceptApprove(_, _ context.Context, input InterventionRequest) (InterventionResult, error) {
	return f.call("approve", input)
}

func (f *fakeInterventions) AcceptOverride(_, _ context.Context, input InterventionRequest) (InterventionResult, error) {
	return f.call("override", input)
}

func (f *fakeInterventions) AcceptRerunStage(_, _ context.Context, input InterventionRequest) (InterventionResult, error) {
	return f.call("rerun", input)
}

func newMutationRequest(method, action, body string) *http.Request {
	request := httptest.NewRequest(method, stagePath(action), bytes.NewBufferString(body))
	request.Host = "127.0.0.1:8080"
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set(HeaderIdempotencyKey, "mutation-test-key")
	return request
}

func TestMutationRoutesInvokeServiceThroughTier1Seam(t *testing.T) {
	service := &fakeInterventions{result: InterventionResult{Phase: "completed", State: "done", JournalSeq: 42}}
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
			if got := response.Header().Get(HeaderSourceApplied); got != "run-1:42" {
				t.Fatalf("%s = %q, want run-1:42", HeaderSourceApplied, got)
			}
			if call.input.IdempotencyKey != "mutation-test-key" {
				t.Fatalf("IdempotencyKey = %q", call.input.IdempotencyKey)
			}
		})
	}
}

func TestMutationRoutesUseAuthenticatedPrincipalAsActor(t *testing.T) {
	service := &fakeInterventions{result: InterventionResult{Phase: "completed", JournalSeq: 7}}
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

func TestMutationReturnsAfterHandoffAndContinuesUnderDaemonContext(t *testing.T) {
	lifecycle, stopDaemon := context.WithCancel(context.Background())
	defer stopDaemon()
	service := &blockingInterventions{
		started: make(chan context.Context, 1),
		release: make(chan struct{}),
		done:    make(chan error, 1),
	}

	handler, err := NewHandler(
		&fakeReader{},
		AllowAll,
		discardLogger(),
		WithInterventions(service),
		WithInterventionContext(lifecycle),
	)
	if err != nil {
		t.Fatal(err)
	}
	requestContext, disconnect := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer disconnect()
	request := newMutationRequest(http.MethodPost, "override", `{"actor":"operator","rationale":"reviewed"}`)
	request = request.WithContext(requestContext)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	executionContext := <-service.started
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body)
	}
	<-requestContext.Done()
	if err := executionContext.Err(); err != nil {
		t.Fatalf("execution context after response budget: %v", err)
	}
	close(service.release)
	if err := <-service.done; err != nil {
		t.Fatalf("accepted execution: %v", err)
	}
}

func TestMutationExecutionStopsAtRequestBudget(t *testing.T) {
	service := &blockingInterventions{
		started: make(chan context.Context, 1),
		release: make(chan struct{}),
	}
	handler := stageMutationHandler("override", service, context.Background(), discardLogger())
	request := newMutationRequest(http.MethodPost, "override", `{"actor":"operator","rationale":"reviewed"}`)
	ctx, cancel := context.WithDeadline(request.Context(), time.Now().Add(-time.Second))
	defer cancel()
	request = request.WithContext(ctx)
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)

	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body)
	}
	if code := errorCode(t, response); code != "request_budget_exceeded" {
		t.Fatalf("code = %q, want request_budget_exceeded", code)
	}
}

func TestMutationRoutesRequireOperateRole(t *testing.T) {
	service := &fakeInterventions{result: InterventionResult{Phase: "completed", JournalSeq: 7}}
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

func TestMutationRoutesRequireIdempotencyKey(t *testing.T) {
	service := &fakeInterventions{}
	handler, err := NewHandler(&fakeReader{}, AllowAll, discardLogger(), WithInterventions(service))
	if err != nil {
		t.Fatal(err)
	}
	request := newMutationRequest(http.MethodPost, "approve", `{"actor":"local","decision":"pass"}`)
	request.Header.Del(HeaderIdempotencyKey)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body)
	}
	if code := errorCode(t, response); code != CodeIdempotencyKeyRequired {
		t.Fatalf("code = %q, want %q", code, CodeIdempotencyKeyRequired)
	}
	if len(service.calls) != 0 {
		t.Fatalf("service calls = %+v, want none", service.calls)
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

func TestMutationRoutesRejectNonJSONAndCrossOriginRequests(t *testing.T) {
	service := &fakeInterventions{}
	handler, err := NewHandler(&fakeReader{}, AllowAll, discardLogger(), WithInterventions(service))
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name        string
		contentType string
		origin      string
		host        string
		wantStatus  int
		wantCode    string
	}{
		{
			name:        "browser simple body",
			contentType: "text/plain",
			wantStatus:  http.StatusUnsupportedMediaType,
			wantCode:    "unsupported_media_type",
		},
		{
			name:        "cross origin JSON",
			contentType: "application/json",
			origin:      "https://attacker.example",
			wantStatus:  http.StatusForbidden,
			wantCode:    "origin_forbidden",
		},
		{
			name:        "DNS rebinding origin",
			contentType: "application/json",
			origin:      "http://attacker.example:8080",
			host:        "attacker.example:8080",
			wantStatus:  http.StatusForbidden,
			wantCode:    "origin_forbidden",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := newMutationRequest(http.MethodPost, "approve", `{"actor":"local","decision":"pass"}`)
			request.Header.Set("Content-Type", test.contentType)
			if test.origin != "" {
				request.Header.Set("Origin", test.origin)
			}
			if test.host != "" {
				request.Host = test.host
			}
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != test.wantStatus {
				t.Fatalf("status = %d, body = %s", response.Code, response.Body)
			}
			var envelope ErrorEnvelope
			if err := json.NewDecoder(response.Body).Decode(&envelope); err != nil {
				t.Fatal(err)
			}
			if envelope.Error.Code != test.wantCode {
				t.Fatalf("error = %+v", envelope.Error)
			}
			if len(service.calls) != 0 {
				t.Fatalf("service calls = %+v, want none", service.calls)
			}
		})
	}
}

func TestMutationRoutesAllowSameLoopbackOrigin(t *testing.T) {
	for _, test := range []struct {
		name       string
		backendTLS bool
	}{
		{name: "direct HTTP daemon"},
		{name: "HTTP dashboard proxy to TLS daemon", backendTLS: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			service := &fakeInterventions{result: InterventionResult{Phase: "completed", JournalSeq: 7}}
			handler, err := NewHandler(&fakeReader{}, AllowAll, discardLogger(), WithInterventions(service))
			if err != nil {
				t.Fatal(err)
			}
			request := newMutationRequest(http.MethodPost, "approve", `{"actor":"local","decision":"pass"}`)
			request.Header.Set("Origin", "http://127.0.0.1:8080")
			if test.backendTLS {
				request.TLS = &tls.ConnectionState{}
			}
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != http.StatusOK {
				t.Fatalf("status = %d, body = %s", response.Code, response.Body)
			}
			if len(service.calls) != 1 {
				t.Fatalf("service calls = %+v, want one", service.calls)
			}
		})
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
