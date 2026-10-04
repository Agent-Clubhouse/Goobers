package main

import (
	"bytes"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/blobstore"
	"github.com/goobers/goobers/internal/dispatcher"
	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/podauth"
)

func TestChildBlobHTTPUsesAuthenticatedLineageAndNeverSharedFallback(t *testing.T) {
	f := actualChildLaunchFixture(t)
	id := publishInterruptedChild(t, f)
	service := &daemonCredentialService{layout: f.launcher.layout, childQueue: f.service.queue}
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
	token, err := registry.MintChildPod(id.RunID, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	handler, err := httpapi.NewHandler(&telemetryParityReader{}, httpapi.RequireRoles(), log.New(io.Discard, "", 0), httpapi.WithAuthenticator(auth), httpapi.WithBlobService(service.childBlobPlane(base)))
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
	unknownToken, err := registry.MintChildPod(strings.Repeat("b", 32), time.Hour)
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
	digest := journal.Digest(owned)
	if out := request(http.MethodPut, digest, owned); out.Code != http.StatusNoContent {
		t.Fatalf("scoped put: %d %s", out.Code, out.Body)
	}
	if found, err := base.Has(t.Context(), digest); err != nil || found {
		t.Fatal("child write escaped queue custody", err)
	}
	if out := request(http.MethodGet, digest, nil); out.Code != http.StatusOK || !bytes.Equal(out.Body.Bytes(), owned) || out.Header().Get("Cache-Control") != "private, no-store" {
		t.Fatalf("scoped get: %d %s cache=%q", out.Code, out.Body, out.Header().Get("Cache-Control"))
	}
	if err := f.service.queue.FenceChildParent(t.Context(), f.submission.Child.Identity.ChildParent, "operator", time.Now()); err != nil {
		t.Fatal(err)
	}
	// A cancelling pod may still surrender its own teardown evidence.
	if out := request(http.MethodPut, digest, owned); out.Code != http.StatusNoContent {
		t.Fatalf("cancelled pod lost custody: %d %s", out.Code, out.Body)
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
