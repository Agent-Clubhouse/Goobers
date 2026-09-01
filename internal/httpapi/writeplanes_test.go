package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/apicontract"
	"github.com/goobers/goobers/internal/claimsclient"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/readservice"
)

type fakeClaimService struct {
	response        ClaimResponse
	listResponse    ClaimListResponse
	recoverResponse ClaimRecoverResponse
	err             error
	requests        []ClaimRequest
	lists           []ClaimListRequest
	recovers        []ClaimRecoverRequest
	ops             []string
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

func (f *fakeClaimService) List(_ context.Context, request ClaimListRequest) (ClaimListResponse, error) {
	f.ops = append(f.ops, "list")
	f.lists = append(f.lists, request)
	return f.listResponse, f.err
}

func (f *fakeClaimService) Recover(_ context.Context, request ClaimRecoverRequest) (ClaimRecoverResponse, error) {
	f.ops = append(f.ops, "recover")
	f.recovers = append(f.recovers, request)
	return f.recoverResponse, f.err
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
		apicontract.ClaimListPath,
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
		apicontract.RouteClaimList,
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

// TestClaimRoutesCapLeaseSeconds pins the LeaseSeconds ceiling at the route:
// an at-cap lease dispatches, while over-cap and duration-overflow-range
// values are refused as a 400 before any service sees them — a ten-year (or
// negative-overflowed) lease would defeat lease-based liveness, and the
// overflow used to surface as a 500 write_failed.
func TestClaimRoutesCapLeaseSeconds(t *testing.T) {
	claims := &fakeClaimService{response: ClaimResponse{Ok: true}}
	handler := writePlaneHandler(t, nil, AllowAll, WithClaimService(claims))

	atCap := fmt.Sprintf(`{"gaggle":"g","provider":"github","itemId":"42","runId":"run-1","leaseSeconds":%d}`, MaxClaimLeaseSeconds)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, jsonRequest(http.MethodPost, apicontract.ClaimAcquirePath, atCap))
	if response.Code != http.StatusOK {
		t.Fatalf("at-cap acquire status = %d, body = %s", response.Code, response.Body)
	}
	if len(claims.requests) != 1 || claims.requests[0].LeaseSeconds != MaxClaimLeaseSeconds {
		t.Fatalf("claims service saw %+v", claims.requests)
	}

	for _, lease := range []string{fmt.Sprintf("%d", MaxClaimLeaseSeconds+1), "10000000000"} {
		body := fmt.Sprintf(`{"gaggle":"g","provider":"github","itemId":"42","runId":"run-1","leaseSeconds":%s}`, lease)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, jsonRequest(http.MethodPost, apicontract.ClaimAcquirePath, body))
		if response.Code != http.StatusBadRequest {
			t.Errorf("leaseSeconds %s: status = %d, want 400", lease, response.Code)
		}
	}
	if len(claims.requests) != 1 {
		t.Fatalf("over-cap leases reached the claim service: %+v", claims.requests)
	}
}

// TestTriggerRouteCapsRequestIDLength pins the delivery-id byte cap: the
// dedupe record is bounded by entry count, not bytes, so requestId gets the
// same 256-byte/400 bound the webhook handler puts on delivery ids.
func TestTriggerRouteCapsRequestIDLength(t *testing.T) {
	triggers := &fakeTriggerService{response: TriggerResponse{RunID: "run-9"}}
	handler := writePlaneHandler(t, nil, AllowAll, WithTriggerService(triggers))

	atCap := strings.Repeat("d", MaxTriggerRequestIDBytes)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, jsonRequest(http.MethodPost, apicontract.TriggerIngestPath,
		fmt.Sprintf(`{"workflow":"impl","requestId":%q}`, atCap)))
	if response.Code != http.StatusOK {
		t.Fatalf("at-cap requestId status = %d, body = %s", response.Code, response.Body)
	}
	if len(triggers.requests) != 1 || triggers.requests[0].RequestID != atCap {
		t.Fatalf("trigger service saw %+v", triggers.requests)
	}

	over := strings.Repeat("d", MaxTriggerRequestIDBytes+1)
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, jsonRequest(http.MethodPost, apicontract.TriggerIngestPath,
		fmt.Sprintf(`{"workflow":"impl","requestId":%q}`, over)))
	if response.Code != http.StatusBadRequest {
		t.Fatalf("over-cap requestId status = %d, want 400", response.Code)
	}
	if len(triggers.requests) != 1 {
		t.Fatalf("over-cap requestId reached the trigger service")
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

// TestClaimListRouteValidatesAndContainsPods pins the claims plane's read
// (finding 002 C1): scope and key validation happen before the service; a
// pod principal may list only its own run and its listing is flagged
// PodScoped so the service confines it to the run's gaggle; a human
// principal's listing is not flagged.
func TestClaimListRouteValidatesAndContainsPods(t *testing.T) {
	claims := &fakeClaimService{listResponse: ClaimListResponse{Entries: []ClaimEntry{{ItemID: "42", RunID: "run-1"}}}}
	pod := &fakeAuthenticator{principal: &Principal{Subject: "run:run-1", Issuer: PodPrincipalIssuer}}
	handler := writePlaneHandler(t, pod, RequireRoles(), WithClaimService(claims))

	response := httptest.NewRecorder()
	handler.ServeHTTP(response, jsonRequest(http.MethodPost, apicontract.ClaimListPath, `{"runId":"run-1","scope":"run"}`))
	if response.Code != http.StatusOK {
		t.Fatalf("pod run listing: status = %d, body = %s", response.Code, response.Body)
	}
	var decoded ClaimListResponse
	if err := json.NewDecoder(response.Body).Decode(&decoded); err != nil || len(decoded.Entries) != 1 || decoded.Entries[0].ItemID != "42" {
		t.Fatalf("decoded = %+v, err = %v", decoded, err)
	}
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, jsonRequest(http.MethodPost, apicontract.ClaimListPath, `{"gaggle":"g","provider":"github","runId":"run-1","scope":"namespace","includeHistory":true}`))
	if response.Code != http.StatusOK {
		t.Fatalf("pod namespace listing: status = %d, body = %s", response.Code, response.Body)
	}
	if len(claims.lists) != 2 || !claims.lists[0].PodScoped || !claims.lists[1].PodScoped || !claims.lists[1].IncludeHistory {
		t.Fatalf("service saw %+v; want two PodScoped listings, the second with history", claims.lists)
	}

	response = httptest.NewRecorder()
	handler.ServeHTTP(response, jsonRequest(http.MethodPost, apicontract.ClaimListPath, `{"runId":"run-2","scope":"run"}`))
	if response.Code != http.StatusForbidden {
		t.Fatalf("pod listing another run: status = %d, want 403", response.Code)
	}

	before := len(claims.lists)
	for name, body := range map[string]string{
		"no run":                     `{"scope":"run"}`,
		"unknown scope":              `{"runId":"run-1","scope":"everything"}`,
		"namespace without gaggle":   `{"runId":"run-1","scope":"namespace","provider":"github"}`,
		"namespace without provider": `{"runId":"run-1","scope":"namespace","gaggle":"g"}`,
		"unknown field":              `{"runId":"run-1","scope":"run","podScoped":true}`,
	} {
		response = httptest.NewRecorder()
		handler.ServeHTTP(response, jsonRequest(http.MethodPost, apicontract.ClaimListPath, body))
		if response.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400", name, response.Code)
		}
	}
	if len(claims.lists) != before {
		t.Fatal("an invalid listing reached the service")
	}

	human := &fakeAuthenticator{principal: &Principal{Subject: "alice", Roles: []Role{RoleOperate}}}
	handler = writePlaneHandler(t, human, RequireRoles(), WithClaimService(claims))
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, jsonRequest(http.MethodPost, apicontract.ClaimListPath, `{"gaggle":"g","provider":"github","runId":"run-9","scope":"namespace"}`))
	if response.Code != http.StatusOK {
		t.Fatalf("human listing: status = %d, body = %s", response.Code, response.Body)
	}
	if last := claims.lists[len(claims.lists)-1]; last.PodScoped {
		t.Fatalf("a human principal's listing was flagged PodScoped: %+v", last)
	}
	// The pod planes are POST-only for pods; a GET on the list path is refused.
	handler = writePlaneHandler(t, pod, RequireRoles(), WithClaimService(claims))
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, apicontract.ClaimListPath, nil))
	if response.Code == http.StatusOK {
		t.Fatal("GET on the list path was admitted for a pod principal")
	}
}

// TestClaimRecoverRouteValidatesAndContainsPods is the route half of
// Goobers#4016. The stale-claim sweep is instance-wide by nature — it has no
// item and no namespace — so the ONLY thing the route can contain is the
// caller's identity, and it must: a pod principal may ask for a sweep as
// itself and nobody else, an unnamed run is a 400, an unavailable service is
// a 503 rather than a silent success, and GET is refused like every other pod
// plane.
func TestClaimRecoverRouteValidatesAndContainsPods(t *testing.T) {
	claims := &fakeClaimService{recoverResponse: ClaimRecoverResponse{Released: []ClaimEntry{{ItemID: "42", RunID: "crashed-run"}}}}
	pod := &fakeAuthenticator{principal: &Principal{Subject: "run:run-1", Issuer: PodPrincipalIssuer}}
	handler := writePlaneHandler(t, pod, RequireRoles(), WithClaimService(claims))

	response := httptest.NewRecorder()
	handler.ServeHTTP(response, jsonRequest(http.MethodPost, apicontract.ClaimRecoverPath, `{"runId":"run-1"}`))
	if response.Code != http.StatusOK {
		t.Fatalf("pod recovery: status = %d, body = %s", response.Code, response.Body)
	}
	var decoded ClaimRecoverResponse
	if err := json.NewDecoder(response.Body).Decode(&decoded); err != nil || len(decoded.Released) != 1 || decoded.Released[0].RunID != "crashed-run" {
		t.Fatalf("decoded = %+v, err = %v", decoded, err)
	}
	if len(claims.recovers) != 1 || !claims.recovers[0].PodScoped || claims.recovers[0].RunID != "run-1" {
		t.Fatalf("service saw %+v; want one PodScoped recovery for run-1", claims.recovers)
	}

	response = httptest.NewRecorder()
	handler.ServeHTTP(response, jsonRequest(http.MethodPost, apicontract.ClaimRecoverPath, `{"runId":"run-2"}`))
	if response.Code != http.StatusForbidden {
		t.Fatalf("pod recovering as another run: status = %d, want 403", response.Code)
	}

	before := len(claims.recovers)
	for name, body := range map[string]string{
		"no run":        `{}`,
		"blank run":     `{"runId":"   "}`,
		"unknown field": `{"runId":"run-1","podScoped":true}`,
	} {
		response = httptest.NewRecorder()
		handler.ServeHTTP(response, jsonRequest(http.MethodPost, apicontract.ClaimRecoverPath, body))
		if response.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400", name, response.Code)
		}
	}
	if len(claims.recovers) != before {
		t.Fatal("an invalid recovery request reached the service")
	}

	response = httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, apicontract.ClaimRecoverPath, nil))
	if response.Code == http.StatusOK {
		t.Fatal("GET on the recover path was admitted for a pod principal")
	}

	// No claims plane wired at all: an explicit refusal, never a 200 the
	// caller would read as "the sweep happened".
	bare := writePlaneHandler(t, pod, RequireRoles())
	response = httptest.NewRecorder()
	bare.ServeHTTP(response, jsonRequest(http.MethodPost, apicontract.ClaimRecoverPath, `{"runId":"run-1"}`))
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("recovery without a claims plane: status = %d, want 503", response.Code)
	}
}

// TestClaimReleaseWithoutItemIsReleaseAllForRun pins the release route's
// itemId-omitted shape: it dispatches as a release of every claim the run
// holds (namespace optional, but never half a namespace), stays contained
// to the pod's own run, and every other verb still requires an item.
func TestClaimReleaseWithoutItemIsReleaseAllForRun(t *testing.T) {
	claims := &fakeClaimService{response: ClaimResponse{Ok: true, Released: []ClaimEntry{{ItemID: "42", RunID: "run-1"}}}}
	pod := &fakeAuthenticator{principal: &Principal{Subject: "run:run-1", Issuer: PodPrincipalIssuer}}
	handler := writePlaneHandler(t, pod, RequireRoles(), WithClaimService(claims))

	for name, body := range map[string]string{
		"every namespace": `{"runId":"run-1"}`,
		"one namespace":   `{"gaggle":"g","provider":"github","runId":"run-1"}`,
	} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, jsonRequest(http.MethodPost, apicontract.ClaimReleasePath, body))
		if response.Code != http.StatusOK {
			t.Fatalf("%s: status = %d, body = %s", name, response.Code, response.Body)
		}
		var decoded ClaimResponse
		if err := json.NewDecoder(response.Body).Decode(&decoded); err != nil || !decoded.Ok || len(decoded.Released) != 1 {
			t.Fatalf("%s: decoded = %+v, err = %v", name, decoded, err)
		}
	}
	namespaced := 0
	for _, request := range claims.requests {
		if request.ItemID != "" || request.RunID != "run-1" {
			t.Fatalf("service saw %+v; want item-less release-all requests for run-1", claims.requests)
		}
		if request.Gaggle == "g" && request.Provider == "github" {
			namespaced++
		}
	}
	if len(claims.requests) != 2 || namespaced != 1 {
		t.Fatalf("service saw %+v; want one namespaced and one unnarrowed release-all", claims.requests)
	}

	before := len(claims.requests)
	for name, request := range map[string]*http.Request{
		"half a namespace":        jsonRequest(http.MethodPost, apicontract.ClaimReleasePath, `{"gaggle":"g","runId":"run-1"}`),
		"acquire without item":    jsonRequest(http.MethodPost, apicontract.ClaimAcquirePath, `{"gaggle":"g","provider":"github","runId":"run-1"}`),
		"renew without item":      jsonRequest(http.MethodPost, apicontract.ClaimRenewPath, `{"gaggle":"g","provider":"github","runId":"run-1"}`),
		"settle without item":     jsonRequest(http.MethodPost, apicontract.ClaimSettlePath, `{"gaggle":"g","provider":"github","runId":"run-1","outcome":"completed"}`),
		"release without any run": jsonRequest(http.MethodPost, apicontract.ClaimReleasePath, `{}`),
	} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400", name, response.Code)
		}
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, jsonRequest(http.MethodPost, apicontract.ClaimReleasePath, `{"runId":"run-2"}`))
	if response.Code != http.StatusForbidden {
		t.Errorf("release-all for another run: status = %d, want 403", response.Code)
	}
	if len(claims.requests) != before {
		t.Fatal("an invalid or foreign release reached the service")
	}
}

// TestClaimEntryWireMatchesTheLedger pins the restated wire types against
// their originals: httpapi.ClaimEntry round-trips a ledger entry field for
// field (the daemon converts one way, the stage client decodes the other),
// and the client's lease ceiling is the route's.
func TestClaimEntryWireMatchesTheLedger(t *testing.T) {
	released := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	original := localscheduler.ClaimEntry{
		ItemID: "42", Gaggle: "g", Provider: "github", ExternalID: "42", RunID: "run-1", Workflow: "implementation",
		ClaimedAt: released.Add(-time.Hour), ExpiresAt: released.Add(time.Hour), ReleasedAt: &released,
	}
	encoded, err := json.Marshal(original)
	if err != nil {
		t.Fatal(err)
	}
	var wire ClaimEntry
	if err := json.Unmarshal(encoded, &wire); err != nil {
		t.Fatal(err)
	}
	back, err := json.Marshal(wire)
	if err != nil {
		t.Fatal(err)
	}
	var decoded localscheduler.ClaimEntry
	if err := json.Unmarshal(back, &decoded); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(original, decoded) {
		t.Fatalf("ledger entry did not survive the wire type: %+v != %+v", decoded, original)
	}
	if MaxClaimLeaseSeconds != claimsclient.MaxLeaseSeconds {
		t.Fatalf("claimsclient.MaxLeaseSeconds = %d, route cap = %d; the client clamps to a ceiling the route no longer has", claimsclient.MaxLeaseSeconds, MaxClaimLeaseSeconds)
	}
}
