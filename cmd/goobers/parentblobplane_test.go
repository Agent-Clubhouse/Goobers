package main

import (
	"bytes"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/blobstore"
	"github.com/goobers/goobers/internal/childpod"
	"github.com/goobers/goobers/internal/credentials"
	"github.com/goobers/goobers/internal/dispatcher"
	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/podauth"
)

func TestParentBlobHTTPPinsAttemptAndStopsAfterJoin(t *testing.T) {
	f := newPinnedChildFixture(t)
	run, env := configuredChildStage(t, f)
	reader, err := journal.OpenReadOnly(run.Dir())
	if err != nil {
		t.Fatal(err)
	}
	id, err := reader.Identity()
	if err != nil {
		t.Fatal(err)
	}
	started, err := childPodStarted(reader, "plan", 1, false, 0)
	if err != nil {
		t.Fatal(err)
	}
	c := childpod.Contract{Version: 1, Identity: id, ParentOrigin: env.ChildWorkflowOrigin, Stage: "plan", Attempt: 1, PodAttempt: int(started.Seq), StartedAt: started.Time, Ceiling: credentials.NewChildCeiling(false, nil, nil)}
	raw, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	digest := journal.Digest(raw)
	store := childpod.ParentBlobs{RunDir: run.Dir(), Identity: id}
	if err = store.Put(t.Context(), digest, raw); err != nil {
		t.Fatal(err)
	}
	if err = store.BindContract(t.Context(), digest); err != nil {
		t.Fatal(err)
	}
	mark := func(kind string) {
		t.Helper()
		if err := run.Append(journal.Event{Type: journal.EventRunnerAnnotation, Stage: "plan", Attempt: 1, Runner: map[string]any{"kind": kind, "contractDigest": digest}}); err != nil {
			t.Fatal(err)
		}
	}
	mark(childpod.ParentWriterStarted)
	base, err := blobstore.NewDir(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	foreign := []byte("shared secret from another run")
	foreignDigest := journal.Digest(foreign)
	if err = base.Put(t.Context(), foreignDigest, foreign); err != nil {
		t.Fatal(err)
	}
	keys, err := podauth.NewSignedKey([]byte(strings.Repeat("k", 32)))
	if err != nil {
		t.Fatal(err)
	}
	auth, err := podauth.NewAuthenticator(keys, httpapi.DenyAllAuthenticator{})
	if err != nil {
		t.Fatal(err)
	}
	token, err := keys.MintWorkflowParentPod(id.RunID, digest, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	service := &daemonCredentialService{layout: f.layout}
	handler, err := httpapi.NewHandler(&telemetryParityReader{}, httpapi.RequireRoles(), log.New(io.Discard, "", 0), httpapi.WithAuthenticator(auth), httpapi.WithBlobService(base), httpapi.WithWorkflowParentBlobService(service.childBlobPlane(base)))
	if err != nil {
		t.Fatal(err)
	}
	request := func(method, hash string, data []byte) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, dispatcher.BlobPathPrefix+hash, bytes.NewReader(data))
		r.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}
	if w := request(http.MethodGet, foreignDigest, nil); w.Code != http.StatusNotFound {
		t.Fatalf("shared fallback: %d %s", w.Code, w.Body)
	}
	own := []byte("exact attempt output")
	ownDigest := journal.Digest(own)
	if w := request(http.MethodPut, ownDigest, own); w.Code != http.StatusNoContent {
		t.Fatalf("put: %d %s", w.Code, w.Body)
	}
	if ok, err := base.Has(t.Context(), ownDigest); err != nil || ok {
		t.Fatal("parent write escaped", err)
	}
	if err := run.Append(journal.Event{Type: journal.EventStageFinished, Stage: "plan", Attempt: 1}); err != nil {
		t.Fatal(err)
	}
	if w := request(http.MethodGet, ownDigest, nil); w.Code != http.StatusOK || !bytes.Equal(w.Body.Bytes(), own) || w.Header().Get("Cache-Control") != "private, no-store" {
		t.Fatalf("late custody: %d %s", w.Code, w.Body)
	}
	assertSurrenderRevoked := testParentSurrenderCustody(t, service, auth, token, c, digest, store)
	mark(childpod.ParentWriterJoined)
	assertSurrenderRevoked()
	if w := request(http.MethodGet, ownDigest, nil); w.Code == http.StatusOK {
		t.Fatal("joined parent retained blob authority")
	}
	if w := request(http.MethodPut, ownDigest, own); w.Code == http.StatusNoContent {
		t.Fatal("joined parent retained write authority")
	}
}
