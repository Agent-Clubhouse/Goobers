package httpapi

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
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
	body, err := json.Marshal(map[string]string{
		"state": "diverged", "workerDigest": "sha256:old", "daemonDigest": "sha256:new",
	})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, apicontract.WorkerConfigDivergencePath, bytes.NewReader(body))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusAccepted {
		t.Fatalf("status = %d, body=%s", response.Code, response.Body.String())
	}
	message, _ := recorded.Runner["message"].(string)
	if recorded.Type != journal.EventWorkerConfigDivergence || recorded.Runner["worker"] != "worker:node-a" || recorded.Runner["state"] != "diverged" ||
		!strings.Contains(message, "sha256:old") || !strings.Contains(message, "sha256:new") || !strings.Contains(message, "DEPLOY") {
		t.Fatalf("recorded = %+v", recorded)
	}
}

func TestWorkerConfigDivergencePlaneRejectsNonWorkerPrincipals(t *testing.T) {
	principals := []Principal{
		{Subject: "operator", Roles: []Role{RoleOperate}},
		{Subject: "run:stage", Issuer: PodPrincipalIssuer},
	}
	for _, principal := range principals {
		t.Run(principal.Subject, func(t *testing.T) {
			handler, err := NewHandler(&fakeReader{}, RequireRoles(), discardLogger(),
				WithAuthenticator(&fakeAuthenticator{principal: &principal}),
				WithWorkerConfigDivergence(func(journal.Event) error { t.Fatal("unauthorized append"); return nil }))
			if err != nil {
				t.Fatal(err)
			}
			request := httptest.NewRequest(http.MethodPost, apicontract.WorkerConfigDivergencePath,
				bytes.NewBufferString(`{"state":"not-checked","reason":"offline"}`))
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != http.StatusForbidden {
				t.Fatalf("status = %d, body=%s", response.Code, response.Body.String())
			}
		})
	}
	t.Run("anonymous local HTTP", func(t *testing.T) {
		handler, err := NewHandler(&fakeReader{}, AllowAll, discardLogger(),
			WithWorkerConfigDivergence(func(journal.Event) error { t.Fatal("anonymous append"); return nil }))
		if err != nil {
			t.Fatal(err)
		}
		request := httptest.NewRequest(http.MethodPost, apicontract.WorkerConfigDivergencePath,
			bytes.NewBufferString(`{"state":"not-checked","reason":"offline"}`))
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusForbidden {
			t.Fatalf("status = %d, body=%s", response.Code, response.Body.String())
		}
	})
}

func TestWorkerConfigDivergencePlaneRejectsControlCharactersAndImpossibleShapes(t *testing.T) {
	handler, err := NewHandler(&fakeReader{}, RequireRoles(), discardLogger(),
		WithAuthenticator(&fakeAuthenticator{principal: &Principal{Subject: "worker:node-a", Issuer: WorkerPrincipalIssuer}}),
		WithWorkerConfigDivergence(func(journal.Event) error { t.Fatal("invalid append"); return nil }))
	if err != nil {
		t.Fatal(err)
	}
	for _, body := range []string{
		`{"state":"not-checked","reason":"offline","worker":"forged","message":"forged"}`,
		`{"state":"not-checked","reason":"offline\nforged status"}`,
		`{"state":"in-sync","workerDigest":"sha256:a","daemonDigest":"sha256:b"}`,
		`{"state":"diverged","workerDigest":"sha256:a","daemonDigest":"sha256:a"}`,
		`{"state":"not-active","reason":"forged"}`,
	} {
		request := httptest.NewRequest(http.MethodPost, apicontract.WorkerConfigDivergencePath, bytes.NewBufferString(body))
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusBadRequest {
			t.Fatalf("body=%s status=%d response=%s", body, response.Code, response.Body.String())
		}
	}
}
