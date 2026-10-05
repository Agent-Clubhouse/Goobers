package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/goobers/goobers/internal/apicontract"
)

func childWorkflowPrincipal() Principal {
	return Principal{Subject: "run:parent-1", Issuer: ChildWorkflowPrincipalIssuer,
		Roles: []Role{RoleAdmin}, Scopes: knownPodScopes(),
		ChildWorkflow: &ChildWorkflowPrincipal{GrantID: "grant-1", Gaggle: "web", RunID: "parent-1",
			StageOccurrence: "branch/stage/visit-2", AttemptID: "attempt-3", ConfigDigest: "config", PolicyDigest: "policy"}}
}

func childAuthorized(principal Principal, method, path string) bool {
	request := httptest.NewRequest(method, path, nil)
	request = request.WithContext(context.WithValue(request.Context(), principalContextKey{}, principal))
	return RequireRoles().Authorize(request) == nil
}

func TestChildWorkflowGrantHasNoAmbientAuthority(t *testing.T) {
	paths := []string{HealthPath, RunsPath, EventsPath, "/api/v1/runs/parent-1/operator-messages", "/api/v1/unknown"}
	for _, route := range apicontract.V1Routes() {
		paths = append(paths, route.Path)
	}
	for _, path := range paths {
		for _, method := range []string{http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete} {
			if childAuthorized(childWorkflowPrincipal(), method, path) {
				t.Fatalf("child grant admitted %s %s despite plane confinement", method, path)
			}
		}
	}
}

func TestChildWorkflowGrantOnlyAdmitsExactOwnOperations(t *testing.T) {
	for _, operation := range []string{"validate", "start", "status", "await", "resolve"} {
		path := "/api/v1/runs/parent-1/child-workflows/" + operation
		if !childAuthorized(childWorkflowPrincipal(), http.MethodPost, path) {
			t.Fatalf("own operation refused: %s", operation)
		}
		for _, method := range []string{http.MethodGet, http.MethodHead, http.MethodPut, http.MethodPatch, http.MethodDelete} {
			if childAuthorized(childWorkflowPrincipal(), method, path) {
				t.Fatalf("wrong method admitted: %s %s", method, path)
			}
		}
		for _, wrong := range []string{path + "/", path + "/other", "/api/v1/runs/sibling-2/child-workflows/" + operation,
			"/api/v1/runs/parent-1/child-workflows/../child-workflows/" + operation} {
			if childAuthorized(childWorkflowPrincipal(), http.MethodPost, wrong) {
				t.Fatalf("wrong path admitted: %s", wrong)
			}
		}
	}
	for _, operation := range []string{"", "cancel", "admin", "start-again"} {
		if childAuthorized(childWorkflowPrincipal(), http.MethodPost, "/api/v1/runs/parent-1/child-workflows/"+operation) {
			t.Fatalf("unknown operation admitted: %s", operation)
		}
	}
}

func TestChildWorkflowGrantRequiresCompleteOrigin(t *testing.T) {
	for _, change := range []func(*Principal){
		func(p *Principal) { p.ChildWorkflow = nil },
		func(p *Principal) { p.Subject = "run:sibling-2" },
		func(p *Principal) { p.ChildWorkflow.GrantID = "" },
		func(p *Principal) { p.ChildWorkflow.Gaggle = "" },
		func(p *Principal) { p.ChildWorkflow.RunID = "" },
		func(p *Principal) { p.ChildWorkflow.StageOccurrence = "" },
		func(p *Principal) { p.ChildWorkflow.AttemptID = "" },
		func(p *Principal) { p.ChildWorkflow.ConfigDigest = "" },
		func(p *Principal) { p.ChildWorkflow.PolicyDigest = "" },
	} {
		principal := childWorkflowPrincipal()
		change(&principal)
		if childAuthorized(principal, http.MethodPost, "/api/v1/runs/parent-1/child-workflows/start") {
			t.Fatal("incomplete origin admitted")
		}
	}
}
