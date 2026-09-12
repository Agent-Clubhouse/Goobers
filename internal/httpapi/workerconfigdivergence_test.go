package httpapi

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/goobers/goobers/internal/apicontract"
	"github.com/goobers/goobers/internal/journal"
)

func TestWorkerConfigDivergencePlaneStampsAuthenticatedWorker(t *testing.T) {
	var recorded journal.Event
	handler, err := NewHandler(&fakeReader{}, RequireRoles(), discardLogger(),
		WithAuthenticator(&fakeAuthenticator{principal: &Principal{Subject: "worker:node-a", Issuer: WorkerPrincipalIssuer}}),
		WithWorkerConfigDivergence(func(event journal.Event) error { recorded = event; return nil }))
	if err != nil {
		t.Fatal(err)
	}
	event := journal.Event{Type: journal.EventWorkerConfigDivergence, Runner: map[string]any{
		"worker": "forged", "state": "diverged", "message": "mismatch",
	}}
	body, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, apicontract.WorkerConfigDivergencePath, bytes.NewReader(body))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusAccepted {
		t.Fatalf("status = %d, body=%s", response.Code, response.Body.String())
	}
	if recorded.Type != journal.EventWorkerConfigDivergence || recorded.Runner["worker"] != "worker:node-a" || recorded.Runner["state"] != "diverged" {
		t.Fatalf("recorded = %+v", recorded)
	}
}
