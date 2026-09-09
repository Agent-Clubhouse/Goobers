package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/goobers/goobers/internal/apicontract"
)

type fakeTriggerStatusService struct {
	fakeTriggerService
	statusRequest TriggerStatusRequest
}

func (s *fakeTriggerStatusService) TriggerStatus(_ context.Context, request TriggerStatusRequest) (TriggerStatusResponse, error) {
	s.statusRequest = request
	return TriggerStatusResponse{AcceptanceID: request.AcceptanceID, State: "accepted"}, nil
}

func TestTriggerStatusRouteUsesAuthenticatedActor(t *testing.T) {
	s := &fakeTriggerStatusService{}
	handler := writePlaneHandler(t, &fakeAuthenticator{principal: &Principal{Subject: "operator", Roles: []Role{RoleOperate}}}, RequireRoles(), WithTriggerService(s))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, apicontract.TriggerIngestPath+"/trigger-1?actor=another", nil))
	if response.Code != http.StatusOK || s.statusRequest.Actor != "operator" || s.statusRequest.AcceptanceID != "trigger-1" {
		t.Fatalf("response=%d request=%+v body=%s", response.Code, s.statusRequest, response.Body)
	}
	if response.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("status may be cached across identities")
	}
}

func TestTriggerStatusRouteStampsPodIdentity(t *testing.T) {
	s := &fakeTriggerStatusService{}
	principal := &Principal{Subject: "run:run-1", Issuer: PodPrincipalIssuer, Scopes: []string{ScopeState}}
	handler := writePlaneHandler(t, &fakeAuthenticator{principal: principal}, RequireRoles(), WithTriggerService(s))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, apicontract.TriggerIngestPath+"/trigger-1?podRunId=run-2", nil))
	if response.Code != http.StatusOK || !s.statusRequest.PodScoped || s.statusRequest.PodRunID != "run-1" || s.statusRequest.Actor != "run:run-1" {
		t.Fatalf("response=%d request=%+v body=%s", response.Code, s.statusRequest, response.Body)
	}
}

func TestTriggerStatusUnavailableCannotBeCached(t *testing.T) {
	handler := writePlaneHandler(t, nil, AllowAll)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, apicontract.TriggerIngestPath+"/trigger-1", nil))
	if response.Code != http.StatusServiceUnavailable || response.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("status=%d cache=%q", response.Code, response.Header().Get("Cache-Control"))
	}
}
