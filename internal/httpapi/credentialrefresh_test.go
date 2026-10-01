package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/goobers/goobers/internal/apicontract"
)

// fakeRefreshingCredentialService is a credential service that also serves
// the refresh route.
type fakeRefreshingCredentialService struct {
	fakeCredentialService
	grants  []string
	refresh []CredentialRefreshRequest
}

func (f *fakeRefreshingCredentialService) Refresh(_ context.Context, grant string, request CredentialRefreshRequest) (CredentialResolveResponse, error) {
	f.grants = append(f.grants, grant)
	f.refresh = append(f.refresh, request)
	return CredentialResolveResponse{RunID: "run-1", Stage: "push", Credentials: []MintedCredential{{Capability: request.Capability, Value: "fresh"}}}, nil
}

const testGrant = CredentialGrantTokenPrefix + "payload-payload-payload.mac-mac-mac-mac-mac"

func refreshRequest(bearer string) *http.Request {
	request := jsonRequest(http.MethodPost, apicontract.CredentialRefreshPath, `{"capability":"repo:push"}`)
	if bearer != "" {
		request.Header.Set("Authorization", "Bearer "+bearer)
	}
	return request
}

func TestCredentialRefreshRouteIsInTheContract(t *testing.T) {
	route, ok := apicontract.V1Route(apicontract.RouteCredentialRefresh)
	if !ok {
		t.Fatal("credentialRefresh route is not in the V1 contract")
	}
	if route.Method != http.MethodPost || route.Cost != apicontract.CostMutation ||
		route.ActionClass != apicontract.ActionWorkflowExecution || route.Budget != apicontract.CredentialResolveBudget {
		t.Fatalf("route = %+v", route)
	}
}

// TestCredentialRefreshServesTheLoopbackGrantHolder is the local-daemon
// posture: no authenticator, and the grant is the whole authorization. The
// handler hands the bearer to the service, which verifies it.
func TestCredentialRefreshServesTheLoopbackGrantHolder(t *testing.T) {
	service := &fakeRefreshingCredentialService{}
	handler := writePlaneHandler(t, nil, AllowAll, WithCredentialService(service))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, refreshRequest(testGrant))
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body)
	}
	if got := response.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", got)
	}
	var decoded CredentialResolveResponse
	if err := json.NewDecoder(response.Body).Decode(&decoded); err != nil {
		t.Fatal(err)
	}
	if len(service.grants) != 1 || service.grants[0] != testGrant || service.refresh[0].Capability != "repo:push" {
		t.Fatalf("service saw grants %v requests %+v", service.grants, service.refresh)
	}
}

// TestCredentialRefreshGivesExistingPrincipalsNothing pins that no principal
// that existed before this route — no bearer at all, a pod token, a human
// operator — can use it: only a grant bearer reaches the service.
func TestCredentialRefreshGivesExistingPrincipalsNothing(t *testing.T) {
	for name, tc := range map[string]struct {
		authenticator Authenticator
		authorizer    Authorizer
		bearer        string
	}{
		"anonymous loopback":  {nil, AllowAll, ""},
		"pod token loopback":  {nil, AllowAll, "goobers-pod.abcdef"},
		"pod principal":       {podAuthenticator(), AllowAll, testGrant},
		"pod principal roles": {podAuthenticator(), RequireRoles(), "goobers-pod.abcdef"},
		"admin operator":      {&fakeAuthenticator{principal: &Principal{Subject: "alice", Roles: []Role{RoleAdmin}}}, RequireRoles(), "jwt"},
		"worker":              {&fakeAuthenticator{principal: &Principal{Subject: "worker:w", Issuer: WorkerPrincipalIssuer}}, RequireRoles(), testGrant},
	} {
		t.Run(name, func(t *testing.T) {
			service := &fakeRefreshingCredentialService{}
			handler := writePlaneHandler(t, tc.authenticator, tc.authorizer, WithCredentialService(service))
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, refreshRequest(tc.bearer))
			if response.Code != http.StatusForbidden {
				t.Fatalf("status = %d, want 403; body = %s", response.Code, response.Body)
			}
			if len(service.refresh) != 0 {
				t.Fatalf("a non-grant principal reached the refresh service: %+v", service.refresh)
			}
		})
	}
}

// TestCredentialGrantPrincipalReachesOnlyTheRefreshRoute pins the authorizer
// half: under RequireRoles a grant principal is admitted to the refresh route
// and refused everywhere else, including the resolve route.
func TestCredentialGrantPrincipalReachesOnlyTheRefreshRoute(t *testing.T) {
	grantPrincipal := &fakeAuthenticator{principal: &Principal{Subject: "run:run-1", Issuer: CredentialGrantPrincipalIssuer}}
	service := &fakeRefreshingCredentialService{}
	handler := writePlaneHandler(t, grantPrincipal, RequireRoles(), WithCredentialService(service))

	response := httptest.NewRecorder()
	handler.ServeHTTP(response, refreshRequest(testGrant))
	if response.Code != http.StatusOK {
		t.Fatalf("refresh status = %d, body = %s", response.Code, response.Body)
	}
	for _, request := range []*http.Request{
		jsonRequest(http.MethodPost, apicontract.CredentialResolvePath, `{"runId":"run-1","stage":"push"}`),
		jsonRequest(http.MethodPost, apicontract.ClaimAcquirePath, `{}`),
		httptest.NewRequest(http.MethodGet, apicontract.V1Prefix+"/health", nil),
	} {
		request.Header.Set("Authorization", "Bearer "+testGrant)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusForbidden {
			t.Errorf("%s %s: status = %d, want 403", request.Method, request.URL.Path, response.Code)
		}
	}
	if len(service.requests) != 0 {
		t.Fatalf("a grant principal reached the resolve service: %+v", service.requests)
	}
}

func TestCredentialRefreshValidatesTheBody(t *testing.T) {
	service := &fakeRefreshingCredentialService{}
	handler := writePlaneHandler(t, nil, AllowAll, WithCredentialService(service))
	for name, body := range map[string]string{
		"empty capability": `{"capability":""}`,
		"unknown field":    `{"capability":"repo:push","runId":"other-run"}`,
		"empty body":       ``,
	} {
		request := jsonRequest(http.MethodPost, apicontract.CredentialRefreshPath, body)
		request.Header.Set("Authorization", "Bearer "+testGrant)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400", name, response.Code)
		}
	}
	if len(service.refresh) != 0 {
		t.Fatalf("invalid requests reached the service: %+v", service.refresh)
	}
}

// TestCredentialRefreshUnavailableWithoutARefreshingService: a resolve-only
// credential service answers 503, never a routing 404.
func TestCredentialRefreshUnavailableWithoutARefreshingService(t *testing.T) {
	handler := writePlaneHandler(t, nil, AllowAll, WithCredentialService(&fakeCredentialService{}))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, refreshRequest(testGrant))
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", response.Code)
	}
}
