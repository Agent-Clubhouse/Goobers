package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/goobers/goobers/internal/livejournal"
)

type forbiddenControllerJournal struct{ t *testing.T }

func (s forbiddenControllerJournal) Emit(context.Context, livejournal.EmitRequest) (livejournal.EmitResponse, error) {
	s.t.Fatal("invalid controller request reached ordinary emission")
	return livejournal.EmitResponse{}, nil
}
func (s forbiddenControllerJournal) EmitController(context.Context, livejournal.EmitRequest) (livejournal.EmitResponse, error) {
	s.t.Fatal("unrelated principal reached controller emission")
	return livejournal.EmitResponse{}, nil
}

type fixtureControllerVerifier struct{ run, digest string }

func (v fixtureControllerVerifier) VerifyControllerJournal(string) (string, string, error) {
	return v.run, v.digest, nil
}

func TestControllerJournalRejectsForgedPrincipalRoles(t *testing.T) {
	batch := livejournal.EmitRequest{RunID: "run-1", Gaggle: "web", Ops: []livejournal.Op{{Kind: livejournal.OpAppend, Key: "start"}}}
	digest, err := livejournal.ControllerJournalDigest(batch)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(batch)
	if err != nil {
		t.Fatal(err)
	}
	for _, issuer := range []string{"human", PodPrincipalIssuer, WorkerPrincipalIssuer, WorkerBlobPrincipalIssuer, CredentialGrantPrincipalIssuer, LaunchGrantPrincipalIssuer} {
		for _, authorizer := range []Authorizer{AllowAll, RequireRoles()} {
			auth := &fakeAuthenticator{principal: &Principal{Subject: "run:run-1", Issuer: issuer, Roles: []Role{RoleAdmin}, Scopes: []string{"*"}}}
			h := writePlaneHandler(t, auth, authorizer, WithJournalService(forbiddenControllerJournal{t}), WithControllerJournalVerifier(fixtureControllerVerifier{batch.RunID, digest}))
			r := jsonRequest(http.MethodPost, "/api/v1/runs/run-1/journal/emit", string(raw))
			r.Header.Set("Authorization", "Bearer "+livejournal.ControllerJournalTokenPrefix+"fixture")
			out := httptest.NewRecorder()
			h.ServeHTTP(out, r)
			if out.Code != http.StatusForbidden {
				t.Fatalf("issuer %s status %d", issuer, out.Code)
			}
		}
	}
}
