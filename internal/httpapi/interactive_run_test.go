package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/apicontract"
)

type interactiveRunStub struct {
	calls     int
	principal Principal
	run, key  string
	command   apicontract.InteractiveRunCommand
}

func (s *interactiveRunStub) InspectInteractiveRun(_ context.Context, p Principal, run string) (apicontract.InteractiveRunView, error) {
	s.calls++
	s.principal = p
	s.run = run
	return apicontract.InteractiveRunView{RunID: run}, nil
}
func (s *interactiveRunStub) AcceptInteractiveRun(_ context.Context, _ context.Context, p Principal, run, key string, input apicontract.InteractiveRunCommand) (apicontract.InteractiveRunCommandResult, error) {
	s.calls++
	s.principal = p
	s.run = run
	s.key = key
	s.command = input
	return apicontract.InteractiveRunCommandResult{Status: "pending", RunID: run}, nil
}

func TestInteractiveRunRouteUsesHumanIdentityAndClosedBody(t *testing.T) {
	service := &interactiveRunStub{}
	auth := &fakeAuthenticator{principal: &Principal{Issuer: "https://identity.example", Subject: "alice", Roles: []Role{RoleOperate}}}
	handler, err := NewHandler(&fakeReader{}, AllowAll, discardLogger(), WithAuthenticator(auth), WithInteractiveRuns(service))
	if err != nil {
		t.Fatal(err)
	}
	valid := `{"kind":"approve","stage":"review","expectedSubjectSequence":7,"decision":"pass"}`
	for _, tc := range []struct {
		name, body, origin, key string
		status                  int
	}{
		{"valid", valid, "https://factory.example", "key", 202},
		{"actor override", `{"kind":"approve","actor":"admin"}`, "", "key", 400},
		{"gaggle override", `{"gaggle":"other"}`, "", "key", 400},
		{"missing key", valid, "", "", 400},
		{"cross origin", valid, "https://attacker.example", "key", 400},
		{"oversized", `{"guidance":"` + strings.Repeat("x", 401<<10) + `"}`, "", "key", 400},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := service.calls
			request := httptest.NewRequest(http.MethodPost, "https://factory.example/api/v1/runs/run-1/interactive-commands", strings.NewReader(tc.body))
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set(HeaderIdempotencyKey, tc.key)
			request.Header.Set("Origin", tc.origin)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != tc.status {
				t.Fatalf("status=%d body=%s", response.Code, response.Body)
			}
			if tc.status != 202 && service.calls != before {
				t.Fatal("invalid input reached service")
			}
		})
	}
	if service.principal.Subject != "alice" || service.run != "run-1" || service.command.ExpectedSubjectSequence != 7 || service.key != "key" {
		t.Fatalf("lost trusted context: %+v", service)
	}
	for _, p := range []*Principal{nil, {Subject: "alice", Issuer: PodPrincipalIssuer, Roles: []Role{RoleAdmin}}, {Subject: "alice", Issuer: ChildWorkflowPrincipalIssuer, Roles: []Role{RoleAdmin}}, {Subject: "alice", Issuer: "https://identity.example", Roles: []Role{RoleView}}} {
		auth.principal = p
		before := service.calls
		request := jsonRequest(http.MethodPost, "/api/v1/runs/run-1/interactive-commands", valid)
		request.Header.Set(HeaderIdempotencyKey, "key")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != 401 && response.Code != 403 {
			t.Fatalf("nonoperator accepted: %d", response.Code)
		}
		if before != service.calls {
			t.Fatal("nonoperator reached service")
		}
	}
}

func TestInteractiveRunRoutesUnavailableAndReadOnlyViewer(t *testing.T) {
	auth := &fakeAuthenticator{principal: &Principal{Issuer: "https://identity.example", Subject: "reader", Roles: []Role{RoleView}}}
	for _, available := range []bool{false, true} {
		opts := []HandlerOption{WithAuthenticator(auth)}
		if available {
			opts = append(opts, WithInteractiveRuns(&interactiveRunStub{}))
		}
		handler, err := NewHandler(&fakeReader{}, AllowAll, discardLogger(), opts...)
		if err != nil {
			t.Fatal(err)
		}
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/v1/runs/run-1/interactive", nil))
		want := 503
		if available {
			want = 200
		}
		if response.Code != want || response.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("response=%d %s", response.Code, response.Body)
		}
	}
}
