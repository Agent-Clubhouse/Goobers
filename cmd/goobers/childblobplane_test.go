package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
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

func TestChildBlobHTTPUsesAuthenticatedLineageAndNeverSharedFallback(t *testing.T) {
	f := actualChildLaunchFixture(t)
	id := publishInterruptedChild(t, f)
	service := &daemonCredentialService{layout: f.launcher.layout, childQueue: f.service.queue, childCredentials: f.launcher.credentialCeiling}
	base, err := blobstore.NewDir(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	foreign := []byte("another run's private payload")
	foreignDigest := journal.Digest(foreign)
	if err = base.Put(t.Context(), foreignDigest, foreign); err != nil {
		t.Fatal(err)
	}
	registry, err := podauth.NewSignedKey([]byte(strings.Repeat("k", 32)))
	if err != nil {
		t.Fatal(err)
	}
	auth, err := podauth.NewAuthenticator(registry, httpapi.DenyAllAuthenticator{})
	if err != nil {
		t.Fatal(err)
	}
	digest, writer := publishChildPodContract(t, f, id, "check", 1)
	defer func() { _ = writer.Close() }()
	token, err := registry.MintChildPod(id.RunID, digest, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	handler, err := httpapi.NewHandler(&telemetryParityReader{}, httpapi.RequireRoles(), log.New(io.Discard, "", 0), httpapi.WithAuthenticator(auth), httpapi.WithBlobService(service.childBlobPlane(base)), httpapi.WithCredentialService(childAttemptCredentialProbe{service: service, id: id}))
	if err != nil {
		t.Fatal(err)
	}
	request := func(method, digest string, data []byte) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, dispatcher.BlobPathPrefix+digest, bytes.NewReader(data))
		r.Header.Set("Authorization", "Bearer "+token)
		out := httptest.NewRecorder()
		handler.ServeHTTP(out, r)
		return out
	}
	if out := request(http.MethodGet, foreignDigest, nil); out.Code != http.StatusNotFound {
		t.Fatalf("shared fallback: %d %s", out.Code, out.Body)
	}
	unknownToken, err := registry.MintChildPod(strings.Repeat("b", 32), journal.Digest(nil), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	childToken := token
	token = unknownToken
	if out := request(http.MethodGet, foreignDigest, nil); out.Code == http.StatusOK {
		t.Fatal("signed child without any custody reached shared blobs")
	}
	token = childToken
	owned := []byte("bounded child output")
	digest = journal.Digest(owned)
	if out := request(http.MethodPut, digest, owned); out.Code != http.StatusNoContent {
		t.Fatalf("scoped put: %d %s", out.Code, out.Body)
	}
	if found, err := base.Has(t.Context(), digest); err != nil || found {
		t.Fatal("child write escaped queue custody", err)
	}
	if out := request(http.MethodGet, digest, nil); out.Code != http.StatusOK || !bytes.Equal(out.Body.Bytes(), owned) || out.Header().Get("Cache-Control") != "private, no-store" {
		t.Fatalf("scoped get: %d %s cache=%q", out.Code, out.Body, out.Header().Get("Cache-Control"))
	}

	for _, stage := range []string{"check", "sibling"} {
		r := httptest.NewRequest(http.MethodPost, "/api/v1/credentials/resolve", strings.NewReader(`{"runId":"`+id.RunID+`","stage":"`+stage+`"}`))
		r.Header.Set("Authorization", "Bearer "+token)
		r.Header.Set("Content-Type", "application/json")
		out := httptest.NewRecorder()
		handler.ServeHTTP(out, r)
		if (out.Code == http.StatusOK) != (stage == "check") {
			t.Fatalf("credential contract stage=%s status=%d body=%s", stage, out.Code, out.Body)
		}
	}
	if err := f.service.queue.FenceChildParent(t.Context(), f.submission.Child.Identity.ChildParent, "operator", time.Now()); err != nil {
		t.Fatal(err)
	}
	// A cancelling pod may still surrender its own teardown evidence.
	if out := request(http.MethodPut, digest, owned); out.Code != http.StatusNoContent {
		t.Fatalf("cancelled pod lost custody: %d %s", out.Code, out.Body)
	}

	if err = writer.Append(journal.Event{Type: journal.EventStageFinished, Stage: "check", Attempt: 1, Status: "success"}); err != nil {
		t.Fatal(err)
	}
	if out := request(http.MethodGet, digest, nil); out.Code == http.StatusOK {
		t.Fatal("settled physical attempt retained pod read authority")
	}
	dir, err := service.layout.FindRunDir(id.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(dir, dir+"-missing"); err != nil {
		t.Fatal(err)
	}
	if out := request(http.MethodGet, foreignDigest, nil); out.Code == http.StatusOK {
		t.Fatal("missing child journal fell through to shared store")
	}
}

func publishChildPodContract(t *testing.T, f *actualChildFixture, id journal.RunIdentity, stage string, attempt int) (string, *journal.Run) {
	t.Helper()
	dir, err := f.launcher.layout.FindRunDir(id.RunID)
	if err != nil {
		t.Fatal(err)
	}
	writer, _, err := journal.Recover(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err = writer.Append(journal.Event{Type: journal.EventStageStarted, Stage: stage, Attempt: attempt}); err != nil {
		t.Fatal(err)
	}
	reader, err := journal.OpenReadOnly(dir)
	if err != nil {
		t.Fatal(err)
	}
	event, err := childPodStarted(reader, stage, attempt, false)
	if err != nil {
		t.Fatal(err)
	}
	c := childpod.Contract{Version: 1, Identity: id, Stage: stage, Attempt: attempt, PodAttempt: int(event.Seq), StartedAt: event.Time, Ceiling: credentials.NewChildCeiling(false, []string{"agent:model"}, nil)}
	raw, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	digest := journal.Digest(raw)
	blobs := childpod.ScopedBlobs{Queue: f.service.queue, Identity: f.submission.Child.Identity}
	if err = blobs.Put(t.Context(), digest, raw); err != nil {
		t.Fatal(err)
	}
	if err = blobs.BindContract(t.Context(), digest); err != nil {
		t.Fatal(err)
	}
	return digest, writer
}

type childAttemptCredentialProbe struct {
	service *daemonCredentialService
	id      journal.RunIdentity
}

func (p childAttemptCredentialProbe) Resolve(ctx context.Context, request httpapi.CredentialResolveRequest) (httpapi.CredentialResolveResponse, error) {
	ctx, lease, err := p.service.applyChildCredentialCeiling(ctx, pinnedStage{identity: p.id}, request.Stage)
	if err != nil {
		return httpapi.CredentialResolveResponse{}, err
	}
	defer lease.release()
	return httpapi.CredentialResolveResponse{RunID: request.RunID, Stage: request.Stage}, lease.finish(ctx)
}
