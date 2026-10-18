package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/apicontract"
)

type fakeChildWorkflowService struct {
	operation, grant, run, key, source string
	calls                              int
	err                                error
}

func (s *fakeChildWorkflowService) record(op, grant, run, key string, source []byte) {
	s.operation, s.grant, s.run, s.key, s.source = op, grant, run, key, string(source)
	s.calls++
}

func (s *fakeChildWorkflowService) ValidateChildWorkflow(_ context.Context, grant, run string, source []byte) (apicontract.ChildWorkflowValidationResponse, error) {
	s.record("validate", grant, run, "", source)
	return apicontract.ChildWorkflowValidationResponse{Valid: true, Advisory: true, Diagnostics: []apicontract.ChildWorkflowDiagnostic{}}, s.err
}

func (s *fakeChildWorkflowService) StartChildWorkflow(_ context.Context, grant, run, key string, source []byte) (apicontract.ChildWorkflowResponse, error) {
	s.record("start", grant, run, key, source)
	return apicontract.ChildWorkflowResponse{State: "queued", InvocationKey: key}, s.err
}

func (s *fakeChildWorkflowService) ChildWorkflowStatus(_ context.Context, grant, run, key string) (apicontract.ChildWorkflowResponse, error) {
	s.record("status", grant, run, key, nil)
	return apicontract.ChildWorkflowResponse{State: "queued", InvocationKey: key}, s.err
}

const childTestGrant = ChildWorkflowGrantTokenPrefix + "payload.signature"

func childRequest(op, body string) *http.Request {
	request := jsonRequest(http.MethodPost, RunsPath+"/parent-1/child-workflows/"+op, body)
	request.Header.Set("Authorization", "Bearer "+childTestGrant)
	request.Header.Set(HeaderIdempotencyKey, "inspect-1")
	return request
}

func TestChildWorkflowLoopbackForwardsGrantToVerifyingService(t *testing.T) {
	for _, op := range []string{"validate", "start", "status"} {
		t.Run(op, func(t *testing.T) {
			service := &fakeChildWorkflowService{}
			handler := writePlaneHandler(t, nil, AllowAll, WithChildWorkflowService(service))
			body := `{"source":"kind: Workflow\n"}`
			if op == "status" {
				body = `{"invocationKey":"inspect-1"}`
			}
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, childRequest(op, body))
			want := http.StatusOK
			if op == "start" {
				want = http.StatusAccepted
			}
			if response.Code != want || service.calls != 1 || service.operation != op || service.grant != childTestGrant || service.run != "parent-1" {
				t.Fatalf("status=%d body=%s service=%+v", response.Code, response.Body, service)
			}
			if response.Header().Get("Cache-Control") != "no-store" {
				t.Fatal("missing no-store")
			}
			if op == "status" && service.source != "" {
				t.Fatal("status accepted source")
			}
			if op != "validate" && service.key != "inspect-1" {
				t.Fatal("lost invocation key")
			}
		})
	}
}

func TestChildWorkflowRejectsExistingPrincipals(t *testing.T) {
	for _, issuer := range []string{"", PodPrincipalIssuer, WorkerPrincipalIssuer, CredentialGrantPrincipalIssuer, "oidc"} {
		service := &fakeChildWorkflowService{}
		auth := &fakeAuthenticator{principal: &Principal{Subject: "operator", Issuer: issuer, Roles: []Role{RoleAdmin}}}
		handler := writePlaneHandler(t, auth, AllowAll, WithChildWorkflowService(service))
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, childRequest("start", `{"source":"x"}`))
		if response.Code != http.StatusForbidden || service.calls != 0 {
			t.Fatalf("issuer=%q status=%d calls=%d", issuer, response.Code, service.calls)
		}
	}
}

func TestChildWorkflowRequiresOwnAuthenticatedRun(t *testing.T) {
	principal := childWorkflowPrincipal()
	service := &fakeChildWorkflowService{}
	handler := writePlaneHandler(t, &fakeAuthenticator{principal: &principal}, RequireRoles(), WithChildWorkflowService(service))
	for _, run := range []string{"parent-1", "sibling"} {
		request := childRequest("validate", `{"source":"x"}`)
		request.URL.Path = RunsPath + "/" + run + "/child-workflows/validate"
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		want := http.StatusOK
		if run != "parent-1" {
			want = http.StatusForbidden
		}
		if response.Code != want {
			t.Fatalf("run=%s status=%d body=%s", run, response.Code, response.Body)
		}
	}
	if service.calls != 1 {
		t.Fatalf("calls=%d", service.calls)
	}
}

func TestChildWorkflowStrictBodiesAndLimits(t *testing.T) {
	for _, body := range []string{
		`{}`, `null`, `[]`, `{"Source":"x"}`, `{"source":null}`, `{"source":""}`, `{"source":1}`,
		`{"source":"x","source":"y"}`, `{"source":"x"}{"source":"y"}`,
		`{"source":"x","actor":"human"}`, `{"source":"x","origin":{}}`,
		`{"source":"x","policy":{}}`, `{"source":"x","invocationKey":"body-key"}`,
		`{"source":"x","grant":"forged"}`, `{"source":"x","runId":"sibling"}`,
		`{"source":"` + strings.Repeat("x", apicontract.MaxChildWorkflowSourceBytes+1) + `"}`,
		`{"source":"x"}` + strings.Repeat(" ", 6*apicontract.MaxChildWorkflowSourceBytes+128),
	} {
		service := &fakeChildWorkflowService{}
		handler := writePlaneHandler(t, nil, AllowAll, WithChildWorkflowService(service))
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, childRequest("start", body))
		if response.Code != http.StatusBadRequest || service.calls != 0 {
			t.Fatalf("body len=%d status=%d calls=%d", len(body), response.Code, service.calls)
		}
	}
	for _, body := range []string{`{"invocationKey":" key "}`, `{"invocationKey":"a\u0085b"}`, `{"invocationKey":"x","source":"x"}`, `{"invocationKey":"` + strings.Repeat("x", 257) + `"}`} {
		service := &fakeChildWorkflowService{}
		response := httptest.NewRecorder()
		writePlaneHandler(t, nil, AllowAll, WithChildWorkflowService(service)).ServeHTTP(response, childRequest("status", body))
		if response.Code != http.StatusBadRequest || service.calls != 0 {
			t.Fatalf("status body=%s code=%d", body, response.Code)
		}
	}
}

func TestChildWorkflowAcceptsDecodedLimitAndEscapes(t *testing.T) {
	service := &fakeChildWorkflowService{}
	handler := writePlaneHandler(t, nil, AllowAll, WithChildWorkflowService(service))
	for _, source := range []string{strings.Repeat("x", apicontract.MaxChildWorkflowSourceBytes), strings.Repeat(`\u0061`, apicontract.MaxChildWorkflowSourceBytes)} {
		request := childRequest("validate", `{"source":"`+source+`"}`)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusOK || len(service.source) != apicontract.MaxChildWorkflowSourceBytes {
			t.Fatalf("status=%d length=%d", response.Code, len(service.source))
		}
	}
	request := childRequest("start", `{"source":"x"}`)
	request.Header.Set(HeaderIdempotencyKey, strings.Repeat("k", 256))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusAccepted || len(service.key) != 256 {
		t.Fatalf("status=%d key length=%d", response.Code, len(service.key))
	}
}

func TestChildWorkflowRejectsBadTransportBeforeService(t *testing.T) {
	for _, change := range []func(*http.Request){
		func(r *http.Request) { r.Header.Del("Authorization") },
		func(r *http.Request) { r.Header.Set("Authorization", "Bearer goobers-grant.payload") },
		func(r *http.Request) { r.Header.Add("Authorization", "Bearer "+childTestGrant) },
		func(r *http.Request) { r.Header.Del(HeaderIdempotencyKey) },
		func(r *http.Request) { r.Header.Add(HeaderIdempotencyKey, "another") },
		func(r *http.Request) { r.Header.Set(HeaderIdempotencyKey, " key ") },
		func(r *http.Request) { r.Header.Set(HeaderIdempotencyKey, strings.Repeat("x", 257)) },
		func(r *http.Request) { r.Header.Set("Content-Type", "text/plain") },
		func(r *http.Request) { r.Header.Set("Origin", "https://evil.invalid") },
	} {
		service := &fakeChildWorkflowService{}
		request := childRequest("start", `{"source":"x"}`)
		change(request)
		response := httptest.NewRecorder()
		writePlaneHandler(t, nil, AllowAll, WithChildWorkflowService(service)).ServeHTTP(response, request)
		if response.Code < 400 || service.calls != 0 {
			t.Fatalf("status=%d calls=%d", response.Code, service.calls)
		}
	}
}

func TestChildWorkflowUnavailableAndServiceRefusals(t *testing.T) {
	for _, op := range []string{"validate", "start", "status"} {
		response := httptest.NewRecorder()
		writePlaneHandler(t, nil, AllowAll).ServeHTTP(response, childRequest(op, `{}`))
		if response.Code != http.StatusServiceUnavailable {
			t.Fatalf("%s status=%d", op, response.Code)
		}
	}
	for _, tc := range []struct {
		err    error
		status int
		code   string
	}{
		{&InterventionError{Status: 403, Code: "child_authority_changed", Message: "current attempt required"}, 403, "child_authority_changed"},
		{&InterventionError{Status: 409, Code: "child_conflict", Message: "different source for key"}, 409, "child_conflict"},
		{context.DeadlineExceeded, 503, ""},
		{errors.New("secret-internal-token"), 500, ""},
	} {
		service := &fakeChildWorkflowService{err: tc.err}
		response := httptest.NewRecorder()
		writePlaneHandler(t, nil, AllowAll, WithChildWorkflowService(service)).ServeHTTP(response, childRequest("start", `{"source":"x"}`))
		if response.Code != tc.status || strings.Contains(response.Body.String(), "secret-internal-token") {
			t.Fatalf("status=%d body=%s", response.Code, response.Body)
		}
		if tc.code != "" && errorCode(t, response) != tc.code {
			t.Fatal("lost service refusal")
		}
	}
}

func TestChildWorkflowDiscoveryAvailability(t *testing.T) {
	for _, id := range []apicontract.RouteID{apicontract.RouteChildWorkflowValidate, apicontract.RouteChildWorkflowStart, apicontract.RouteChildWorkflowStatus} {
		if available, _, _ := routeAvailability(id, handlerConfig{}); available {
			t.Fatalf("%s available without service", id)
		}
		if available, _, _ := routeAvailability(id, handlerConfig{childWorkflows: &fakeChildWorkflowService{}}); !available {
			t.Fatalf("%s unavailable with service", id)
		}
	}
	if err := WithChildWorkflowService(nil)(&handlerConfig{}); err == nil {
		t.Fatal("nil service accepted")
	}
}

func TestChildWorkflowNoSourceEcho(t *testing.T) {
	service := &fakeChildWorkflowService{}
	response := httptest.NewRecorder()
	writePlaneHandler(t, nil, AllowAll, WithChildWorkflowService(service)).ServeHTTP(response, childRequest("validate", `{"source":"private proposal"}`))
	var result apicontract.ChildWorkflowValidationResponse
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil || !result.Advisory {
		t.Fatalf("body=%s err=%v", response.Body, err)
	}
	if strings.Contains(response.Body.String(), "private proposal") {
		t.Fatal("source echoed")
	}
}
