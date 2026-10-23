package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/apicontract"
	"github.com/goobers/goobers/internal/bandit"
	"github.com/goobers/goobers/internal/bandit/conformance"
	"github.com/goobers/goobers/internal/readservice"
)

type fakeBanditPlane struct {
	calls int
	err   error
}

func (s *fakeBanditPlane) Observe(context.Context, bandit.ObservationRequest) (bandit.ObservationResponse, error) {
	s.calls++
	return bandit.ObservationResponse{}, s.err
}

func TestBanditPlaneRunScopeAndClosedParameters(t *testing.T) {
	record := conformance.Record("run-1", "build", 0, 1, 1)
	encoded, err := json.Marshal(bandit.ObservationRequest{Operation: "record", RunID: "run-1", Stage: "build", Record: &record})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, body string
		scopes     []string
		status     int
		err        error
	}{
		{name: "read", body: `{"operation":"read","runId":"run-1","stage":"build"}`, scopes: []string{ScopeJournal}, status: 200},
		{name: "record", body: string(encoded), status: 200},
		{name: "other run", body: `{"operation":"read","runId":"run-2","stage":"build"}`, status: 403},
		{name: "wrong scope", body: string(encoded), scopes: []string{ScopeClaims}, status: 403},
		{name: "unknown field", body: `{"operation":"read","runId":"run-1","stage":"build","owner_epoch":99}`, status: 400},
		{name: "unknown operation", body: `{"operation":"query","runId":"run-1","stage":"build"}`, status: 400},
		{name: "mismatched record", body: strings.Replace(string(encoded), `"runId":"run-1"`, `"runId":"run-2"`, 1), status: 400},
		{name: "conflict", body: string(encoded), status: 409, err: bandit.ErrObservationConflict},
		{name: "store fenced", body: string(encoded), status: 503, err: errors.New("stale lease")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			service := &fakeBanditPlane{err: tc.err}
			handler, err := NewHandler(&fakeReader{health: readservice.Health{Ready: true}}, RequireRoles(), discardLogger(),
				WithAuthenticator(&fakeAuthenticator{principal: &Principal{Issuer: PodPrincipalIssuer, Subject: "run:run-1", Scopes: tc.scopes}}), WithBanditService(service))
			if err != nil {
				t.Fatal(err)
			}
			request := httptest.NewRequest(http.MethodPost, apicontract.BanditObservationsPath, strings.NewReader(tc.body))
			request.Header.Set("Content-Type", "application/json")
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != tc.status {
				t.Fatalf("status=%d body=%s", response.Code, response.Body)
			}
			wantCalls := 0
			if tc.status == 200 || tc.err != nil {
				wantCalls = 1
			}
			if service.calls != wantCalls {
				t.Fatalf("store calls=%d want=%d", service.calls, wantCalls)
			}
		})
	}
}
