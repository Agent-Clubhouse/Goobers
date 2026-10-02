package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/apicontract"
)

type fakeOperatorMessageService struct {
	response OperatorMessageSubmissionResponse
	err      error
	requests []OperatorMessageSubmissionRequest
	records  []apiv1.OperatorMessageRecord
}

func (f *fakeOperatorMessageService) SubmitOperatorMessage(_ context.Context, req OperatorMessageSubmissionRequest) (OperatorMessageSubmissionResponse, error) {
	f.requests = append(f.requests, req)
	if f.err != nil {
		f.records = append(f.records, apiv1.OperatorMessageRecord{
			Request: apiv1.OperatorMessageRequest{
				Schema:         apiv1.OperatorMessageRequestSchema,
				RequestID:      req.IdempotencyKey,
				IdempotencyKey: req.IdempotencyKey,
				TargetAddress:  req.TargetAddress,
				PrincipalRef:   req.PrincipalRef,
				Purpose:        req.Purpose,
				Content:        req.Content,
				DeliveryMode:   req.DeliveryMode,
			},
			State: apiv1.OperatorMessageState(apiv1.OperatorMessageRejected),
			Outcome: &apiv1.OperatorMessageOutcome{
				Schema:         apiv1.OperatorMessageOutcomeSchema,
				RequestID:      req.IdempotencyKey,
				IdempotencyKey: req.IdempotencyKey,
				Status:         apiv1.OperatorMessageRejected,
				Code:           "not_authorized",
			},
		})
	}
	return f.response, f.err
}

func operatorMessageBody() string {
	return `{"gaggle":"web","targetAddress":"terminal:operator","purpose":"approval-required","content":{"text":"please review"},"deliveryMode":"terminal"}`
}

func TestOperatorMessageRouteStampsAuthenticatedPrincipal(t *testing.T) {
	now := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
	service := &fakeOperatorMessageService{response: OperatorMessageSubmissionResponse{
		Accepted: true,
		Record: apiv1.OperatorMessageRecord{
			Request: apiv1.OperatorMessageRequest{
				Schema: apiv1.OperatorMessageRequestSchema, RequestID: "key-1", IdempotencyKey: "key-1",
				TargetAddress: "terminal:operator", PrincipalRef: "issuer:operator", RequestedAt: now,
				Purpose: "approval-required", Content: apiv1.OperatorMessageContent{Text: "please review"}, DeliveryMode: "terminal",
			},
			State: apiv1.OperatorMessageAccepted,
		},
	}}
	authenticator := &fakeAuthenticator{principal: &Principal{Subject: "operator", Issuer: "issuer", Roles: []Role{RoleOperate}}}
	handler := writePlaneHandler(t, authenticator, RequireRoles(), WithOperatorMessageService(service))

	request := jsonRequest(http.MethodPost, "/api/v1/runs/run-1/operator-messages", operatorMessageBody())
	request.Header.Set(HeaderIdempotencyKey, "key-1")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusAccepted {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body)
	}
	if len(service.requests) != 1 {
		t.Fatalf("service requests = %d, want 1", len(service.requests))
	}
	input := service.requests[0]
	if input.RunID != "run-1" || input.IdempotencyKey != "key-1" || input.PrincipalRef != "issuer:operator" ||
		input.Principal.Subject != "operator" || input.Gaggle != "web" {
		t.Fatalf("input = %+v", input)
	}
}

func TestOperatorMessageRouteLetsServiceJournalAuthenticatedOperateDenials(t *testing.T) {
	for _, tc := range []struct {
		name         string
		principal    Principal
		wantRef      string
		wantResponse int
	}{
		{
			name:         "view role",
			principal:    Principal{Subject: "viewer", Issuer: "issuer", Roles: []Role{RoleView}},
			wantRef:      "issuer:viewer",
			wantResponse: http.StatusForbidden,
		},
		{
			name:         "no mapped role",
			principal:    Principal{Subject: "auditor", Issuer: "issuer"},
			wantRef:      "issuer:auditor",
			wantResponse: http.StatusForbidden,
		},
		{
			name:         "pod without journal scope",
			principal:    Principal{Subject: "run:run-1", Issuer: PodPrincipalIssuer, Scopes: []string{ScopeClaims}},
			wantRef:      "goobers/pod:run:run-1",
			wantResponse: http.StatusForbidden,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			service := &fakeOperatorMessageService{err: NewInterventionError(http.StatusForbidden, "not_authorized", "denied", nil)}
			authenticator := &fakeAuthenticator{principal: &tc.principal}
			handler := writePlaneHandler(t, authenticator, RequireRoles(), WithOperatorMessageService(service))

			request := jsonRequest(http.MethodPost, "/api/v1/runs/run-1/operator-messages", operatorMessageBody())
			request.Header.Set(HeaderIdempotencyKey, "key-1")
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != tc.wantResponse {
				t.Fatalf("status = %d, want %d: %s", response.Code, tc.wantResponse, response.Body)
			}
			if len(service.requests) != 1 || service.requests[0].PrincipalRef != tc.wantRef {
				t.Fatalf("denied request did not reach service with verified attribution: %+v", service.requests)
			}
			if len(service.records) != 1 || service.records[0].Request.PrincipalRef != tc.wantRef ||
				service.records[0].Outcome == nil || service.records[0].Outcome.Code != "not_authorized" {
				t.Fatalf("denial was not journaled with verified attribution: %+v", service.records)
			}
		})
	}
}

func TestOperatorMessageRouteRejectsAuthorityFieldsAsData(t *testing.T) {
	service := &fakeOperatorMessageService{}
	authenticator := &fakeAuthenticator{principal: &Principal{Subject: "operator", Roles: []Role{RoleOperate}}}
	handler := writePlaneHandler(t, authenticator, RequireRoles(), WithOperatorMessageService(service))
	for _, field := range []string{"capabilities", "credentials", "sandbox", "egress", "budget", "delegation"} {
		request := jsonRequest(http.MethodPost, "/api/v1/runs/run-1/operator-messages",
			`{"gaggle":"web","targetAddress":"terminal:operator","purpose":"approval-required","content":{"text":"please review"},"deliveryMode":"terminal",`+
				jsonField(field)+`:true}`)
		request.Header.Set(HeaderIdempotencyKey, "key-1")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusBadRequest {
			t.Fatalf("%s status = %d, want 400: %s", field, response.Code, response.Body)
		}
	}
	if len(service.requests) != 0 {
		t.Fatalf("authority-bearing bodies reached service: %+v", service.requests)
	}
}

func TestOperatorMessageRouteIsInContract(t *testing.T) {
	route, ok := apicontract.V1Route(apicontract.RouteOperatorMessageSubmit)
	if !ok {
		t.Fatal("operatorMessageSubmit is not in the V1 contract")
	}
	if route.Method != http.MethodPost || route.Path != apicontract.RunOperatorMessagesPath ||
		route.Cost != apicontract.CostMutation || route.Budget != apicontract.MutationBudget {
		t.Fatalf("route = %+v", route)
	}
}

func jsonField(name string) string {
	b, _ := json.Marshal(name)
	return string(b)
}
