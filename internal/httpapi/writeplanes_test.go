package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/apicontract"
	"github.com/goobers/goobers/internal/readservice"
)

type fakeClaimService struct {
	response ClaimResponse
	err      error
	requests []ClaimRequest
	ops      []string
}

func (f *fakeClaimService) call(op string, request ClaimRequest) (ClaimResponse, error) {
	f.ops = append(f.ops, op)
	f.requests = append(f.requests, request)
	return f.response, f.err
}

func (f *fakeClaimService) Acquire(_ context.Context, request ClaimRequest) (ClaimResponse, error) {
	return f.call("acquire", request)
}

func (f *fakeClaimService) Renew(_ context.Context, request ClaimRequest) (ClaimResponse, error) {
	return f.call("renew", request)
}

func (f *fakeClaimService) Release(_ context.Context, request ClaimRequest) (ClaimResponse, error) {
	return f.call("release", request)
}

func (f *fakeClaimService) Settle(_ context.Context, request ClaimRequest) (ClaimResponse, error) {
	return f.call("settle", request)
}

type fakeTriggerService struct {
	response TriggerResponse
	err      error
	requests []TriggerRequest
}

func (f *fakeTriggerService) Trigger(_ context.Context, request TriggerRequest) (TriggerResponse, error) {
	f.requests = append(f.requests, request)
	return f.response, f.err
}

type fakeEscalationService struct {
	result InterventionResult
	err    error
	inputs []EscalationResolutionRequest
}

func (f *fakeEscalationService) AcceptResolve(_, _ context.Context, input EscalationResolutionRequest) (InterventionResult, error) {
	f.inputs = append(f.inputs, input)
	return f.result, f.err
}

func writePlaneHandler(t *testing.T, authenticator Authenticator, authorizer Authorizer, opts ...HandlerOption) http.Handler {
	t.Helper()
	reader := &fakeReader{health: readservice.Health{Ready: true}}
	if authenticator != nil {
		opts = append(opts, WithAuthenticator(authenticator))
	}
	handler, err := NewHandler(reader, authorizer, discardLogger(), opts...)
	if err != nil {
		t.Fatal(err)
	}
	return handler
}

func jsonRequest(method, path, body string) *http.Request {
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	return request
}

func writePlanePaths() []string {
	return []string{
		apicontract.ClaimAcquirePath,
		apicontract.ClaimRenewPath,
		apicontract.ClaimReleasePath,
		apicontract.ClaimSettlePath,
		apicontract.TriggerIngestPath,
		"/api/v1/runs/run-1/escalation/resolve",
	}
}

// TestWritePlaneRoutesAreInTheContract pins the §7 write surface: every plane
// route exists, is a POST mutation, and carries the mutation budget — the
// contract discipline the routes joined.
func TestWritePlaneRoutesAreInTheContract(t *testing.T) {
	for _, id := range []apicontract.RouteID{
		apicontract.RouteClaimAcquire,
		apicontract.RouteClaimRenew,
		apicontract.RouteClaimRelease,
		apicontract.RouteClaimSettle,
		apicontract.RouteTriggerIngest,
		apicontract.RouteResolveEscalation,
	} {
		route, ok := apicontract.V1Route(id)
		if !ok {
			t.Fatalf("route %s is not in the V1 contract", id)
		}
		if route.Method != http.MethodPost {
			t.Errorf("route %s method = %s, want POST", id, route.Method)
		}
		if route.Cost != apicontract.CostMutation {
			t.Errorf("route %s cost = %s, want %s", id, route.Cost, apicontract.CostMutation)
		}
		if route.Budget != apicontract.MutationBudget {
			t.Errorf("route %s budget = %s, want %s", id, route.Budget, apicontract.MutationBudget)
		}
	}
}

// TestWritePlanesRequireAuthenticationOffLoopback proves the write routes ride
// the existing authenticated posture: with a real authenticator (the
// off-loopback requirement, #640) an unauthenticated request is 401, an
// authenticated principal without operate is 403, and nothing reaches the
// services.
func TestWritePlanesRequireAuthenticationOffLoopback(t *testing.T) {
	claims := &fakeClaimService{}
	triggers := &fakeTriggerService{}
	escalations := &fakeEscalationService{}
	authenticator := &fakeAuthenticator{err: context.DeadlineExceeded}
	handler := writePlaneHandler(t, authenticator, RequireRoles(),
		WithClaimService(claims), WithTriggerService(triggers), WithEscalationService(escalations))

	for _, path := range writePlanePaths() {
		// Unauthenticated: 401.
		authenticator.err = http.ErrNoCookie
		authenticator.principal = nil
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, jsonRequest(http.MethodPost, path, `{}`))
		if response.Code != http.StatusUnauthorized {
			t.Errorf("%s unauthenticated status = %d, want 401", path, response.Code)
		}

		// Authenticated view-only principal: 403 — writes need operate.
		authenticator.err = nil
		authenticator.principal = &Principal{Subject: "viewer", Roles: []Role{RoleView}}
		response = httptest.NewRecorder()
		handler.ServeHTTP(response, jsonRequest(http.MethodPost, path, `{}`))
		if response.Code != http.StatusForbidden {
			t.Errorf("%s viewer status = %d, want 403", path, response.Code)
		}
	}
	if len(claims.requests)+len(triggers.requests)+len(escalations.inputs) != 0 {
		t.Fatalf("refused requests reached a service: claims=%d triggers=%d escalations=%d",
			len(claims.requests), len(triggers.requests), len(escalations.inputs))
	}
}

// TestPodPrincipalIsConfinedToItsOwnClaims proves both halves of pod
// containment: the authorizer restricts pod principals to the claims plane,
// and the claims handlers bind them to their own run.
func TestPodPrincipalIsConfinedToItsOwnClaims(t *testing.T) {
	claims := &fakeClaimService{response: ClaimResponse{Ok: true}}
	triggers := &fakeTriggerService{}
	escalations := &fakeEscalationService{}
	authenticator := &fakeAuthenticator{principal: &Principal{Subject: "run:run-1", Issuer: PodPrincipalIssuer}}
	handler := writePlaneHandler(t, authenticator, RequireRoles(),
		WithClaimService(claims), WithTriggerService(triggers), WithEscalationService(escalations))

	body := `{"gaggle":"g","provider":"github","itemId":"42","runId":"run-1"}`
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, jsonRequest(http.MethodPost, apicontract.ClaimAcquirePath, body))
	if response.Code != http.StatusOK {
		t.Fatalf("pod acquire for its own run: status = %d, body = %s", response.Code, response.Body)
	}
	if len(claims.requests) != 1 || claims.requests[0].RunID != "run-1" {
		t.Fatalf("claims service saw %+v", claims.requests)
	}

	// Another run's claims are refused before the service.
	otherRun := `{"gaggle":"g","provider":"github","itemId":"42","runId":"run-2"}`
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, jsonRequest(http.MethodPost, apicontract.ClaimAcquirePath, otherRun))
	if response.Code != http.StatusForbidden {
		t.Fatalf("pod acquire for another run: status = %d, want 403", response.Code)
	}

	// Triggers, escalations, and reads are off-plane for pods entirely.
	for _, request := range []*http.Request{
		jsonRequest(http.MethodPost, apicontract.TriggerIngestPath, `{"workflow":"w"}`),
		jsonRequest(http.MethodPost, "/api/v1/runs/run-1/escalation/resolve", `{"resolution":"deny"}`),
		httptest.NewRequest(http.MethodGet, apicontract.HealthPath, nil),
	} {
		response = httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusForbidden {
			t.Errorf("pod %s %s: status = %d, want 403", request.Method, request.URL.Path, response.Code)
		}
	}
	if len(claims.requests) != 1 || len(triggers.requests)+len(escalations.inputs) != 0 {
		t.Fatalf("off-plane pod requests reached a service")
	}
}

func TestClaimRoutesValidateAndDispatch(t *testing.T) {
	expires := time.Now().UTC().Truncate(time.Second)
	claims := &fakeClaimService{response: ClaimResponse{Ok: true, ExpiresAt: &expires}}
	handler := writePlaneHandler(t, nil, AllowAll, WithClaimService(claims))

	// Each verb dispatches to its own service operation.
	body := `{"gaggle":"g","provider":"github","itemId":"42","runId":"run-1","workflow":"impl","leaseSeconds":60}`
	for _, route := range []struct{ path, op string }{
		{apicontract.ClaimAcquirePath, "acquire"},
		{apicontract.ClaimRenewPath, "renew"},
		{apicontract.ClaimReleasePath, "release"},
	} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, jsonRequest(http.MethodPost, route.path, body))
		if response.Code != http.StatusOK {
			t.Fatalf("%s status = %d, body = %s", route.path, response.Code, response.Body)
		}
	}
	settle := `{"gaggle":"g","provider":"github","itemId":"42","runId":"run-1","outcome":"completed"}`
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, jsonRequest(http.MethodPost, apicontract.ClaimSettlePath, settle))
	if response.Code != http.StatusOK {
		t.Fatalf("settle status = %d, body = %s", response.Code, response.Body)
	}
	if want := []string{"acquire", "renew", "release", "settle"}; strings.Join(claims.ops, ",") != strings.Join(want, ",") {
		t.Fatalf("ops = %v, want %v", claims.ops, want)
	}
	var decoded ClaimResponse
	if err := json.NewDecoder(response.Body).Decode(&decoded); err != nil || !decoded.Ok {
		t.Fatalf("settle response = %+v, err = %v", decoded, err)
	}

	// Validation failures never reach the service.
	before := len(claims.requests)
	for name, request := range map[string]*http.Request{
		"missing key fields":     jsonRequest(http.MethodPost, apicontract.ClaimAcquirePath, `{"itemId":"42"}`),
		"negative lease":         jsonRequest(http.MethodPost, apicontract.ClaimAcquirePath, `{"gaggle":"g","provider":"p","itemId":"42","runId":"r","leaseSeconds":-1}`),
		"outcome outside settle": jsonRequest(http.MethodPost, apicontract.ClaimAcquirePath, `{"gaggle":"g","provider":"p","itemId":"42","runId":"r","outcome":"completed"}`),
		"unknown field":          jsonRequest(http.MethodPost, apicontract.ClaimAcquirePath, `{"gaggle":"g","provider":"p","itemId":"42","runId":"r","surprise":true}`),
		"empty body":             jsonRequest(http.MethodPost, apicontract.ClaimAcquirePath, ``),
		"wrong content type":     httptest.NewRequest(http.MethodPost, apicontract.ClaimAcquirePath, strings.NewReader(`{}`)),
		"second JSON value":      jsonRequest(http.MethodPost, apicontract.ClaimAcquirePath, `{"gaggle":"g","provider":"p","itemId":"42","runId":"r"}{}`),
	} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusBadRequest && response.Code != http.StatusUnsupportedMediaType {
			t.Errorf("%s: status = %d, want 400/415", name, response.Code)
		}
	}
	if len(claims.requests) != before {
		t.Fatalf("invalid requests reached the claim service")
	}
}

func TestTriggerRouteValidatesAndDispatches(t *testing.T) {
	triggers := &fakeTriggerService{response: TriggerResponse{RunID: "run-9"}}
	handler := writePlaneHandler(t, nil, AllowAll, WithTriggerService(triggers))

	response := httptest.NewRecorder()
	handler.ServeHTTP(response, jsonRequest(http.MethodPost, apicontract.TriggerIngestPath, `{"gaggle":"g","workflow":"impl","requestId":"d-1"}`))
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body)
	}
	var decoded TriggerResponse
	if err := json.NewDecoder(response.Body).Decode(&decoded); err != nil || decoded.RunID != "run-9" {
		t.Fatalf("response = %+v, err = %v", decoded, err)
	}
	if len(triggers.requests) != 1 || triggers.requests[0].RequestID != "d-1" {
		t.Fatalf("trigger service saw %+v", triggers.requests)
	}

	response = httptest.NewRecorder()
	handler.ServeHTTP(response, jsonRequest(http.MethodPost, apicontract.TriggerIngestPath, `{"gaggle":"g"}`))
	if response.Code != http.StatusBadRequest {
		t.Fatalf("workflow-less trigger status = %d, want 400", response.Code)
	}
}

func TestEscalationRouteRequiresKeyActorAndResolution(t *testing.T) {
	escalations := &fakeEscalationService{result: InterventionResult{Phase: "escalated", JournalSeq: 7}}
	authenticator := &fakeAuthenticator{principal: &Principal{Subject: "operator", Roles: []Role{RoleOperate}}}
	handler := writePlaneHandler(t, authenticator, RequireRoles(), WithEscalationService(escalations))
	path := "/api/v1/runs/run-1/escalation/resolve"

	// Missing Idempotency-Key.
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, jsonRequest(http.MethodPost, path, `{"resolution":"deny","rationale":"no"}`))
	if response.Code != http.StatusBadRequest {
		t.Fatalf("missing key status = %d, want 400", response.Code)
	}

	// Unknown resolution.
	request := jsonRequest(http.MethodPost, path, `{"resolution":"escalate-more"}`)
	request.Header.Set("Idempotency-Key", "key-1")
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("bad resolution status = %d, want 400", response.Code)
	}
	if len(escalations.inputs) != 0 {
		t.Fatalf("invalid escalation requests reached the service")
	}

	// A valid deny: actor comes from the principal, key and run from the
	// transport, and the journal position is surfaced.
	request = jsonRequest(http.MethodPost, path, `{"resolution":"deny","rationale":"not shippable"}`)
	request.Header.Set("Idempotency-Key", "key-1")
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("deny status = %d, body = %s", response.Code, response.Body)
	}
	if len(escalations.inputs) != 1 {
		t.Fatalf("escalation service saw %d inputs", len(escalations.inputs))
	}
	input := escalations.inputs[0]
	if input.RunID != "run-1" || input.IdempotencyKey != "key-1" || input.Actor != "operator" || input.Resolution != EscalationResolutionDeny {
		t.Fatalf("input = %+v", input)
	}
	if got := response.Header().Get(HeaderSourceApplied); got != "run-1:7" {
		t.Fatalf("Source-Applied = %q", got)
	}
}

// TestWritePlanesUnavailableWithoutServices pins the degraded posture: a
// handler assembled without the plane services (the dashboard's standalone
// read-only construction) answers 503, not 404 — the surface exists, the
// deployment does not serve it.
func TestWritePlanesUnavailableWithoutServices(t *testing.T) {
	handler := writePlaneHandler(t, nil, AllowAll)
	for _, path := range writePlanePaths() {
		request := jsonRequest(http.MethodPost, path, `{}`)
		request.Header.Set("Idempotency-Key", "key-1")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusServiceUnavailable {
			t.Errorf("%s status = %d, want 503", path, response.Code)
		}
	}
}
