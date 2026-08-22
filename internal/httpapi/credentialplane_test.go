package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/apicontract"
)

type fakeCredentialService struct {
	response CredentialResolveResponse
	err      error
	requests []CredentialResolveRequest
}

func (f *fakeCredentialService) Resolve(_ context.Context, request CredentialResolveRequest) (CredentialResolveResponse, error) {
	f.requests = append(f.requests, request)
	return f.response, f.err
}

// TestCredentialResolveRouteIsInTheContract pins the credential plane's
// contract entry: POST, mutation-pooled, workflow-execution class, and the
// mint-bound budget rather than the 8s mutation budget (a cold GitHub App
// mint is bounded at 30s; see apicontract.CredentialResolveBudget).
func TestCredentialResolveRouteIsInTheContract(t *testing.T) {
	route, ok := apicontract.V1Route(apicontract.RouteCredentialResolve)
	if !ok {
		t.Fatal("credentialResolve route is not in the V1 contract")
	}
	if route.Method != http.MethodPost {
		t.Errorf("method = %s, want POST", route.Method)
	}
	if route.Cost != apicontract.CostMutation {
		t.Errorf("cost = %s, want %s", route.Cost, apicontract.CostMutation)
	}
	if route.ActionClass != apicontract.ActionWorkflowExecution {
		t.Errorf("action class = %s, want %s", route.ActionClass, apicontract.ActionWorkflowExecution)
	}
	if route.Budget != apicontract.CredentialResolveBudget {
		t.Errorf("budget = %s, want %s", route.Budget, apicontract.CredentialResolveBudget)
	}
}

func TestCredentialResolveDispatchesAndAnswers(t *testing.T) {
	expires := time.Now().UTC().Truncate(time.Second).Add(time.Hour)
	service := &fakeCredentialService{response: CredentialResolveResponse{
		RunID: "run-1",
		Stage: "implement",
		Credentials: []MintedCredential{
			{Capability: "repo:push", Value: "minted-value", ExpiresAt: &expires},
		},
	}}
	handler := writePlaneHandler(t, nil, AllowAll, WithCredentialService(service))

	response := httptest.NewRecorder()
	handler.ServeHTTP(response, jsonRequest(http.MethodPost, apicontract.CredentialResolvePath,
		`{"runId":"run-1","stage":"implement","capabilities":["repo:push"]}`))
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body)
	}
	if got := response.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store — the body carries live secret material", got)
	}
	var decoded CredentialResolveResponse
	if err := json.NewDecoder(response.Body).Decode(&decoded); err != nil {
		t.Fatal(err)
	}
	if len(decoded.Credentials) != 1 || decoded.Credentials[0].Value != "minted-value" ||
		decoded.Credentials[0].ExpiresAt == nil || !decoded.Credentials[0].ExpiresAt.Equal(expires) {
		t.Fatalf("decoded = %+v", decoded)
	}
	if len(service.requests) != 1 || service.requests[0].RunID != "run-1" ||
		service.requests[0].Stage != "implement" || len(service.requests[0].Capabilities) != 1 {
		t.Fatalf("service saw %+v", service.requests)
	}
}

func TestCredentialResolveValidatesBeforeTheService(t *testing.T) {
	service := &fakeCredentialService{}
	handler := writePlaneHandler(t, nil, AllowAll, WithCredentialService(service))

	tooMany := make([]string, MaxCredentialResolveCapabilities+1)
	for i := range tooMany {
		tooMany[i] = fmt.Sprintf("capability-%d", i)
	}
	tooManyJSON, err := json.Marshal(tooMany)
	if err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{
		"missing runId":       `{"stage":"implement"}`,
		"missing stage":       `{"runId":"run-1"}`,
		"blank stage":         `{"runId":"run-1","stage":"  "}`,
		"empty capability":    `{"runId":"run-1","stage":"implement","capabilities":[""]}`,
		"oversize capability": fmt.Sprintf(`{"runId":"run-1","stage":"implement","capabilities":[%q]}`, strings.Repeat("x", MaxCredentialCapabilityBytes+1)),
		"too many":            fmt.Sprintf(`{"runId":"run-1","stage":"implement","capabilities":%s}`, tooManyJSON),
		"unknown field":       `{"runId":"run-1","stage":"implement","surprise":true}`,
		"empty body":          ``,
	} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, jsonRequest(http.MethodPost, apicontract.CredentialResolvePath, body))
		if response.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400", name, response.Code)
		}
	}
	if len(service.requests) != 0 {
		t.Fatalf("invalid requests reached the credential service: %+v", service.requests)
	}

	// Nil service: the route exists (503), never a routing 404.
	nilHandler := writePlaneHandler(t, nil, AllowAll)
	response := httptest.NewRecorder()
	nilHandler.ServeHTTP(response, jsonRequest(http.MethodPost, apicontract.CredentialResolvePath, `{"runId":"r","stage":"s"}`))
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("nil-service status = %d, want 503", response.Code)
	}
}

// TestCredentialResolveServesPodsOnly proves the DS9 posture end to end: a
// pod principal resolves its OWN run's stage credentials and no other run's,
// and an authenticated human principal — even operate/admin — is refused: the
// plane is a secret-disclosure surface built for stage pods.
func TestCredentialResolveServesPodsOnly(t *testing.T) {
	service := &fakeCredentialService{response: CredentialResolveResponse{RunID: "run-1", Stage: "implement"}}
	authenticator := &fakeAuthenticator{principal: &Principal{Subject: "run:run-1", Issuer: PodPrincipalIssuer}}
	handler := writePlaneHandler(t, authenticator, RequireRoles(), WithCredentialService(service))

	// Its own run: allowed through the authorizer and the handler binding.
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, jsonRequest(http.MethodPost, apicontract.CredentialResolvePath,
		`{"runId":"run-1","stage":"implement"}`))
	if response.Code != http.StatusOK {
		t.Fatalf("pod resolve for its own run: status = %d, body = %s", response.Code, response.Body)
	}

	// Another run's stage: refused before the service.
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, jsonRequest(http.MethodPost, apicontract.CredentialResolvePath,
		`{"runId":"run-2","stage":"implement"}`))
	if response.Code != http.StatusForbidden {
		t.Fatalf("pod resolve for another run: status = %d, want 403", response.Code)
	}
	var envelope ErrorEnvelope
	if err := json.NewDecoder(response.Body).Decode(&envelope); err != nil || envelope.Error.Code != "run_mismatch" {
		t.Fatalf("error envelope = %+v, err = %v", envelope, err)
	}

	// Human principals are refused outright, whatever their role.
	for _, role := range []Role{RoleOperate, RoleAdmin} {
		authenticator.principal = &Principal{Subject: "human", Roles: []Role{role}}
		response = httptest.NewRecorder()
		handler.ServeHTTP(response, jsonRequest(http.MethodPost, apicontract.CredentialResolvePath,
			`{"runId":"run-1","stage":"implement"}`))
		if response.Code != http.StatusForbidden {
			t.Errorf("%s human resolve: status = %d, want 403", role, response.Code)
		}
	}
	if len(service.requests) != 1 {
		t.Fatalf("refused requests reached the credential service: %+v", service.requests)
	}
}

// TestCredentialResolveTypedRefusalsPassThrough proves a service-typed
// refusal (the undeclared-capability 403 naming the capability) reaches the
// caller intact rather than collapsing into a generic 500.
func TestCredentialResolveTypedRefusalsPassThrough(t *testing.T) {
	service := &fakeCredentialService{err: NewInterventionError(
		http.StatusForbidden, "capability_undeclared",
		`capability "repo:push" is not declared by stage "plan"; nothing materializes for an undeclared capability`, nil)}
	handler := writePlaneHandler(t, nil, AllowAll, WithCredentialService(service))

	response := httptest.NewRecorder()
	handler.ServeHTTP(response, jsonRequest(http.MethodPost, apicontract.CredentialResolvePath,
		`{"runId":"run-1","stage":"plan","capabilities":["repo:push"]}`))
	if response.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", response.Code)
	}
	var envelope ErrorEnvelope
	if err := json.NewDecoder(response.Body).Decode(&envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.Error.Code != "capability_undeclared" || !strings.Contains(envelope.Error.Message, `"repo:push"`) {
		t.Fatalf("error envelope = %+v; the deny must be typed and must name the capability", envelope)
	}
}
