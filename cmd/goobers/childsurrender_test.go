package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/childpod"
	"github.com/goobers/goobers/internal/dispatcher"
	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/journal"
)

// Uses the real signed identity, accepted source and host writer reservation
// from the credential/journal test. Cancellation has already fenced new work.
func testCancelledChildSurrenderCustody(t *testing.T, service *daemonCredentialService, auth httpapi.Authenticator, token string, contract childpod.Contract, digest string, custody *childInvocationBlobs) {
	t.Helper()
	store, err := dispatcher.NewSurrenderDir(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	opts := []httpapi.HandlerOption{httpapi.WithAuthenticator(auth), httpapi.WithSurrenderService(store)}
	makeHandler := func(options ...httpapi.HandlerOption) http.Handler {
		h, err := httpapi.NewHandler(&telemetryParityReader{}, httpapi.RequireRoles(), log.New(io.Discard, "", 0), options...)
		if err != nil {
			t.Fatal(err)
		}
		return h
	}
	ordinaryOnly := makeHandler(opts...)
	opts = append(opts, httpapi.WithGeneratedChildSurrenderService(childSurrenderPlane{store: store, service: service}))
	handler := makeHandler(opts...)
	raw, err := json.Marshal(childpod.Output{Version: 1, ContractDigest: digest})
	if err != nil {
		t.Fatal(err)
	}
	outputDigest := journal.Digest(raw)
	attemptBlobs := childpod.ChildAttemptBlobs{Store: custody.ScopedBlobs, ContractDigest: digest}
	if err = attemptBlobs.Put(t.Context(), outputDigest, raw); err != nil {
		t.Fatal(err)
	}
	valid := dispatcher.SurrenderedResult{RecoveryAcknowledged: true, ChildWorkspaceDigest: outputDigest, Result: apiv1.ResultEnvelope{Status: apiv1.ResultSuccess}}
	request := func(h http.Handler, attempt int, out dispatcher.SurrenderedResult) *httptest.ResponseRecorder {
		data, err := json.Marshal(out)
		if err != nil {
			t.Fatal(err)
		}
		path := fmt.Sprintf("/api/v1/runs/%s/stages/%s/attempts/%d/surrender", contract.Identity.RunID, contract.Stage, attempt)
		r := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(data))
		r.Header.Set("Authorization", "Bearer "+token)
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	if out := request(ordinaryOnly, contract.PodAttempt, valid); out.Code != http.StatusForbidden {
		t.Fatal("child reached ordinary surrender store", out.Code, out.Body)
	}
	if out := request(handler, contract.PodAttempt+1, valid); out.Code != http.StatusForbidden {
		t.Fatal("foreign physical attempt surrendered", out.Code, out.Body)
	}
	for _, change := range []func(*dispatcher.SurrenderedResult){
		func(r *dispatcher.SurrenderedResult) { r.RecoveryAcknowledged = false },
		func(r *dispatcher.SurrenderedResult) { r.WorkspaceDelta = journal.Digest(nil) },
		func(r *dispatcher.SurrenderedResult) { r.Result.Integrity = apiv1.IntegrityTrusted },
		func(r *dispatcher.SurrenderedResult) { r.ChildWorkspaceDigest = journal.Digest([]byte("foreign")) },
		func(r *dispatcher.SurrenderedResult) { r.Verdict = &apiv1.Verdict{Decision: apiv1.VerdictPass} },
	} {
		invalid := valid
		change(&invalid)
		if out := request(handler, contract.PodAttempt, invalid); out.Code != http.StatusBadRequest {
			t.Fatal("invalid surrender must fail without retrying", out.Code, out.Body)
		}
		if found, err := store.Has(t.Context(), contract.Identity.RunID, contract.Stage, contract.PodAttempt); err != nil || found {
			t.Fatal("invalid result persisted", err)
		}
	}
	for range 2 {
		out := request(handler, contract.PodAttempt, valid)
		if out.Code != http.StatusOK || out.Header().Get("Cache-Control") != "private, no-store" {
			t.Fatal("cancelled writer could not surrender", out.Code, out.Body)
		}
	}
	if found, err := store.Has(t.Context(), contract.Identity.RunID, contract.Stage, contract.PodAttempt); err != nil || !found {
		t.Fatal("worker cannot recover exact result", err)
	}
	if err = custody.record(childPodWriterJoined); err != nil {
		t.Fatal(err)
	}
	if out := request(handler, contract.PodAttempt, valid); out.Code != http.StatusForbidden {
		t.Fatal("joined worker retained write authority", out.Code, out.Body)
	}
}
