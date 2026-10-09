package httpapi

import (
	"github.com/goobers/goobers/internal/apicontract"
	"github.com/goobers/goobers/internal/journal"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestWorkflowParentExecutionObserverNeverFallsBack(t *testing.T) {
	for _, installed := range []bool{false, true} {
		t.Run(map[bool]string{false: "missing", true: "installed"}[installed], func(t *testing.T) {
			ordinary, child := &fakeClaimService{}, &fakeClaimService{}
			auth := &fakeAuthenticator{principal: &Principal{Subject: podPrincipalSubject("child"), Issuer: WorkflowParentPrincipalIssuer, WorkflowParent: &WorkflowParentPrincipal{ContractDigest: journal.Digest(nil)}}}
			opts := []HandlerOption{WithClaimService(ordinary)}
			if installed {
				opts = append(opts, WithWorkflowParentExecutionObserver(child))
			}
			handler := writePlaneHandler(t, auth, RequireRoles(), opts...)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, jsonRequest(http.MethodPost, apicontract.ClaimListPath, `{"runId":"child","scope":"run","execution":true,"includeHistory":true}`))
			want := http.StatusForbidden
			if installed {
				want = http.StatusOK
			}
			if response.Code != want || response.Header().Get("Cache-Control") != "private, no-store" || len(ordinary.lists) != 0 {
				t.Fatal(response.Code, response.Body, ordinary)
			}
			if installed && len(child.lists) != 1 {
				t.Fatal("child owner not invoked")
			}
		})
	}
}

func TestWorkflowParentExecutionObserverRefusesGeneralClaimAccess(t *testing.T) {
	child := &fakeClaimService{}
	auth := &fakeAuthenticator{principal: &Principal{Subject: podPrincipalSubject("child"), Issuer: WorkflowParentPrincipalIssuer, WorkflowParent: &WorkflowParentPrincipal{ContractDigest: journal.Digest(nil)}}}
	handler := writePlaneHandler(t, auth, RequireRoles(), WithWorkflowParentExecutionObserver(child), WithClaimService(child))
	for _, body := range []string{
		`{"runId":"parent","scope":"run","execution":true,"includeHistory":true}`,
		`{"runId":"child","scope":"namespace","gaggle":"g","provider":"github","execution":true,"includeHistory":true}`,
		`{"runId":"child","scope":"run","includeHistory":true}`,
		`{"runId":"child","scope":"run","execution":true}`,
		`{"runId":"child","scope":"run","gaggle":"g","execution":true,"includeHistory":true}`,
	} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, jsonRequest(http.MethodPost, apicontract.ClaimListPath, body))
		if response.Code != http.StatusForbidden || len(child.lists) != 0 {
			t.Fatal(body, response.Code)
		}
	}
	for _, path := range []string{apicontract.ClaimAcquirePath, apicontract.ClaimRenewPath, apicontract.ClaimReleasePath, apicontract.ClaimRecoverPath} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, jsonRequest(http.MethodPost, path, `{"runId":"child"}`))
		if response.Code != http.StatusForbidden {
			t.Fatal(path, response.Code)
		}
	}
}
