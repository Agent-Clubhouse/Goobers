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

// Called after stage termination, before the exact physical writer joins.
func testParentSurrenderCustody(t *testing.T, service *daemonCredentialService, auth httpapi.Authenticator, token string, c childpod.Contract, digest string, blobs childpod.ParentBlobs) func() {
	t.Helper()
	store, err := dispatcher.NewSurrenderDir(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	opts := []httpapi.HandlerOption{httpapi.WithAuthenticator(auth), httpapi.WithSurrenderService(store)}
	makeHandler := func(options []httpapi.HandlerOption) http.Handler {
		t.Helper()
		h, err := httpapi.NewHandler(&telemetryParityReader{}, httpapi.RequireRoles(), log.New(io.Discard, "", 0), options...)
		if err != nil {
			t.Fatal(err)
		}
		return h
	}
	path := fmt.Sprintf("/api/v1/runs/%s/stages/%s/attempts/%d/surrender", c.Identity.RunID, c.Stage, c.PodAttempt)
	submit := func(h http.Handler, method, path string, out dispatcher.SurrenderedResult) *httptest.ResponseRecorder {
		t.Helper()
		raw, err := json.Marshal(out)
		if err != nil {
			t.Fatal(err)
		}
		r := httptest.NewRequest(method, path, bytes.NewReader(raw))
		r.Header.Set("Authorization", "Bearer "+token)
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	raw, err := json.Marshal(childpod.Output{Version: 1, ContractDigest: digest})
	if err != nil {
		t.Fatal(err)
	}
	outputDigest := journal.Digest(raw)
	scoped := childpod.ParentAttemptBlobs{Store: blobs, ContractDigest: digest}
	if err = scoped.Put(t.Context(), outputDigest, raw); err != nil {
		t.Fatal(err)
	}
	out := dispatcher.SurrenderedResult{Result: apiv1.ResultEnvelope{Status: apiv1.ResultSuccess}, ChildWorkspaceDigest: outputDigest, RecoveryAcknowledged: true}
	if w := submit(makeHandler(opts), http.MethodPost, path, out); w.Code != http.StatusForbidden {
		t.Fatal("parent fell through to ordinary surrender", w.Code, w.Body)
	}
	opts = append(opts, httpapi.WithWorkflowParentSurrenderService(parentSurrenderPlane{store: store, service: service}))
	handler := makeHandler(opts)
	for _, change := range []func(*dispatcher.SurrenderedResult){
		func(v *dispatcher.SurrenderedResult) { v.RecoveryAcknowledged = false },
		func(v *dispatcher.SurrenderedResult) { v.WorkspaceDelta = "foreign" },
		func(v *dispatcher.SurrenderedResult) { v.ChildWorkspaceDigest = journal.Digest([]byte("unknown")) },
		func(v *dispatcher.SurrenderedResult) { v.Result.Integrity = apiv1.IntegrityTrusted },
	} {
		invalid := out
		change(&invalid)
		if w := submit(handler, http.MethodPost, path, invalid); w.Code != http.StatusBadRequest {
			t.Fatal("parent forged result authority", w.Code, w.Body)
		}
	}
	other := fmt.Sprintf("/api/v1/runs/%s/stages/%s/attempts/%d/surrender", c.Identity.RunID, c.Stage, c.PodAttempt+1)
	if w := submit(handler, http.MethodPost, other, out); w.Code != http.StatusForbidden {
		t.Fatal("parent wrote another physical attempt", w.Code, w.Body)
	}
	if w := submit(handler, http.MethodGet, path, out); w.Code != http.StatusForbidden {
		t.Fatal("parent acquired worker result reads", w.Code, w.Body)
	}
	if w := submit(handler, http.MethodPost, path, out); w.Code != http.StatusOK || w.Header().Get("Cache-Control") != "private, no-store" {
		t.Fatal("late parent surrender unavailable", w.Code, w.Body)
	}
	stored, err := store.Get(t.Context(), c.Identity.RunID, c.Stage, c.PodAttempt)
	if err != nil || len(stored) == 0 {
		t.Fatal("result not retained", err)
	}
	return func() {
		t.Helper()
		if w := submit(handler, http.MethodPost, path, out); w.Code != http.StatusForbidden {
			t.Fatal("joined parent retained result writes", w.Code, w.Body)
		}
	}
}
