package httpapi

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/goobers/goobers/internal/apicontract"
	"github.com/goobers/goobers/internal/journal"
)

func TestWorkflowParentCredentialsRequireSeparateOwner(t *testing.T) {
	for _, installed := range []bool{false, true} {
		t.Run(map[bool]string{false: "missing", true: "installed"}[installed], func(t *testing.T) {
			ordinary, child := &fakeCredentialService{}, &fakeCredentialService{}
			auth := &fakeAuthenticator{principal: &Principal{Subject: podPrincipalSubject("run-1"), Issuer: WorkflowParentPrincipalIssuer, WorkflowParent: &WorkflowParentPrincipal{ContractDigest: journal.Digest([]byte("contract"))}}}
			opts := []HandlerOption{WithCredentialService(ordinary)}
			if installed {
				opts = append(opts, WithWorkflowParentCredentialService(child))
			}
			handler := writePlaneHandler(t, auth, RequireRoles(), opts...)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, jsonRequest(http.MethodPost, apicontract.CredentialResolvePath, `{"runId":"run-1","stage":"check"}`))
			want := http.StatusForbidden
			if installed {
				want = http.StatusOK
			}
			if response.Code != want || len(ordinary.requests) != 0 || response.Header().Get("Cache-Control") != "private, no-store" {
				t.Fatalf("credential owner selection: code=%d ordinary=%d headers=%v", response.Code, len(ordinary.requests), response.Header())
			}
			if installed && len(child.requests) != 1 {
				t.Fatal("installed child owner not invoked")
			}
		})
	}
}

func TestWorkflowParentCredentialOwnerCannotAdmitAnotherRun(t *testing.T) {
	child := &fakeCredentialService{}
	auth := &fakeAuthenticator{principal: &Principal{Subject: podPrincipalSubject("run-1"), Issuer: WorkflowParentPrincipalIssuer, WorkflowParent: &WorkflowParentPrincipal{ContractDigest: journal.Digest(nil)}}}
	handler := writePlaneHandler(t, auth, RequireRoles(), WithWorkflowParentCredentialService(child))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, jsonRequest(http.MethodPost, apicontract.CredentialResolvePath, `{"runId":"run-2","stage":"check"}`))
	if response.Code != http.StatusForbidden || len(child.requests) != 0 {
		t.Fatal("cross-run request reached credential owner", response.Code)
	}
}
