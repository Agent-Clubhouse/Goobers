package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/goobers/goobers/internal/apicontract"
	"github.com/goobers/goobers/internal/launchreceipt"
)

type forbiddenLaunchService struct{ t *testing.T }

func (s forbiddenLaunchService) Accept(context.Context, string, launchreceipt.Receipt) error {
	s.t.Fatal("unrelated principal reached receipt authority")
	return nil
}

func TestLaunchReceiptRejectsForgedPrincipalRoles(t *testing.T) {
	for _, issuer := range []string{"human", PodPrincipalIssuer, WorkerPrincipalIssuer, WorkerBlobPrincipalIssuer, CredentialGrantPrincipalIssuer} {
		for _, authorizer := range []Authorizer{AllowAll, RequireRoles()} {
			auth := &fakeAuthenticator{principal: &Principal{Subject: "controller", Issuer: issuer, Roles: []Role{RoleAdmin}, Scopes: []string{"*"}}}
			handler := writePlaneHandler(t, auth, authorizer, WithLaunchReceiptService(forbiddenLaunchService{t}))
			req := jsonRequest(http.MethodPost, apicontract.LaunchReceiptPath, `{}`)
			req.Header.Set("Authorization", "Bearer "+launchreceipt.TokenPrefix+"forged")
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, req)
			if response.Code != http.StatusForbidden {
				t.Fatalf("issuer %s status %d", issuer, response.Code)
			}
		}
	}
}

func TestLaunchReceiptHTTPRejectsLocalOnlyFacts(t *testing.T) {
	auth := &fakeAuthenticator{principal: &Principal{Subject: "controller", Issuer: LaunchGrantPrincipalIssuer}}
	handler := writePlaneHandler(t, auth, RequireRoles(), WithLaunchReceiptService(forbiddenLaunchService{t}))
	req := jsonRequest(http.MethodPost, apicontract.LaunchReceiptPath, `{"version":1,"local":{"integrity":"unverified"}}`)
	req.Header.Set("Authorization", "Bearer "+launchreceipt.TokenPrefix+"fixture")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, req)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("local receipt reached controller plane: %d %s", response.Code, response.Body.String())
	}
}
