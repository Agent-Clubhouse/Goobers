package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/goobers/goobers/internal/apicontract"
)

type verificationServiceFixture struct {
	fakeClaimService
	observations []ClaimVerificationRequest
}

func (f *verificationServiceFixture) RecordVerification(_ context.Context, request ClaimVerificationRequest) (ClaimResponse, error) {
	f.observations = append(f.observations, request)
	return ClaimResponse{Ok: true}, nil
}

func TestClaimVerificationRouteContainsAndAuthenticatesCaller(t *testing.T) {
	service := &verificationServiceFixture{}
	auth := &fakeAuthenticator{principal: &Principal{Subject: "run:observer", Issuer: PodPrincipalIssuer}}
	handler := writePlaneHandler(t, auth, RequireRoles(), WithClaimService(service))
	for _, tc := range []struct {
		name, body string
		want       int
	}{
		{"own identity", `{"runId":"observer","ownerRunId":"lease-owner","gaggle":"g","provider":"github","itemId":"7"}`, http.StatusOK},
		{"other identity", `{"runId":"other","ownerRunId":"lease-owner"}`, http.StatusForbidden},
		{"forged scope", `{"runId":"observer","PodScoped":false}`, http.StatusBadRequest},
		{"unknown field", `{"runId":"observer","unexpected":true}`, http.StatusBadRequest},
		{"trailing body", `{"runId":"observer"} {}`, http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, jsonRequest(http.MethodPost, apicontract.ClaimVerifyPath, tc.body))
			if response.Code != tc.want {
				t.Fatalf("status=%d want=%d: %s", response.Code, tc.want, response.Body)
			}
		})
	}
	if len(service.observations) != 1 || !service.observations[0].PodScoped || service.observations[0].OwnerRunID != "lease-owner" {
		t.Fatalf("containment not passed to service: %+v", service.observations)
	}
	auth.err, auth.principal = http.ErrNoCookie, nil
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, jsonRequest(http.MethodPost, apicontract.ClaimVerifyPath, `{"runId":"observer"}`))
	if response.Code != http.StatusUnauthorized || len(service.observations) != 1 {
		t.Fatalf("unauthenticated reporting reached service: %d", response.Code)
	}
}
