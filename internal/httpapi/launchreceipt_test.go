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
