package dispatcher

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/apicontract"
	"github.com/goobers/goobers/internal/blobstore"
)

// fakeBlobEndpoint is an in-memory daemon-fronted blob endpoint speaking the
// BlobPathPrefix digest contract, requiring the stage-scoped bearer.
func fakeBlobEndpoint(t *testing.T, token string) (*httptest.Server, *sync.Map) {
	t.Helper()
	var blobs sync.Map
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+token {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		digest, ok := strings.CutPrefix(r.URL.Path, BlobPathPrefix)
		if !ok || digest == "" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		switch r.Method {
		case http.MethodGet:
			if data, ok := blobs.Load(digest); ok {
				_, _ = w.Write(data.([]byte))
				return
			}
			w.WriteHeader(http.StatusNotFound)
		case http.MethodHead:
			if _, ok := blobs.Load(digest); ok {
				w.WriteHeader(http.StatusOK)
				return
			}
			w.WriteHeader(http.StatusNotFound)
		case http.MethodPut:
			data, err := io.ReadAll(r.Body)
			if err != nil {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			blobs.Store(digest, data)
			w.WriteHeader(http.StatusCreated)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	}))
	t.Cleanup(server.Close)
	return server, &blobs
}

// Decision 010: artifacts move by sha256 DIGEST over the network with a
// stage-scoped credential — the client is a blobstore.Store, so
// materialize/surrender plug it in unchanged.
func TestBlobClientDigestRoundTrip(t *testing.T) {
	server, _ := fakeBlobEndpoint(t, "stage-scoped-token")
	client := &BlobClient{BaseURL: server.URL, Token: "stage-scoped-token"}

	// The seam contract: BlobClient IS a blobstore.Store.
	var store blobstore.Store = client

	data := []byte("artifact-bytes")
	digest := fmt.Sprintf("sha256:%x", sha256.Sum256(data))

	if _, err := store.Get(context.Background(), digest); !errors.Is(err, blobstore.ErrNotFound) {
		t.Fatalf("missing digest: got %v, want blobstore.ErrNotFound (the fail-soft materialize contract)", err)
	}
	if has, err := store.Has(context.Background(), digest); err != nil || has {
		t.Fatalf("Has before Put = %v, %v", has, err)
	}
	if err := store.Put(context.Background(), digest, data); err != nil {
		t.Fatalf("Put: %v", err)
	}
	got, err := store.Get(context.Background(), digest)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if string(got) != string(data) {
		t.Fatalf("round-trip mismatch: %q", got)
	}
	if has, err := store.Has(context.Background(), digest); err != nil || !has {
		t.Fatalf("Has after Put = %v, %v", has, err)
	}
}

// The credential is not optional decoration: a wrong bearer is refused by the
// endpoint and surfaces as an error, never as silent success.
func TestBlobClientRequiresCredential(t *testing.T) {
	server, _ := fakeBlobEndpoint(t, "stage-scoped-token")
	client := &BlobClient{BaseURL: server.URL, Token: "wrong"}
	digest := fmt.Sprintf("sha256:%x", sha256.Sum256([]byte("x")))
	if err := client.Put(context.Background(), digest, []byte("x")); err == nil {
		t.Fatal("unauthenticated Put succeeded")
	}
}

// #4260: the pod's artifact write-through must survive the same transient
// network blips the surrender PUT does.
func TestBlobClientPutRetriesTransientFailureThenSucceeds(t *testing.T) {
	shrinkRetryDelays(t)
	var attempts atomic.Int32
	const failures = 2
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		if attempts.Add(1) <= failures {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		_, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusCreated)
	}))
	t.Cleanup(server.Close)

	digest := fmt.Sprintf("sha256:%x", sha256.Sum256([]byte("x")))
	client := &BlobClient{BaseURL: server.URL, RetryDeadline: time.Second}
	if err := client.Put(context.Background(), digest, []byte("x")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if got := attempts.Load(); got != failures+1 {
		t.Fatalf("server saw %d attempts, want %d", got, failures+1)
	}
}

func TestBlobClientPutHonoursRetryDeadline(t *testing.T) {
	shrinkRetryDelays(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	t.Cleanup(server.Close)

	digest := fmt.Sprintf("sha256:%x", sha256.Sum256([]byte("x")))
	client := &BlobClient{BaseURL: server.URL, RetryDeadline: 30 * time.Millisecond}
	start := time.Now()
	err := client.Put(context.Background(), digest, []byte("x"))
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("expected an error once the retry deadline elapsed")
	}
	if elapsed > time.Second {
		t.Fatalf("elapsed = %s, want close to the 30ms retry deadline", elapsed)
	}
}

// The blob plane is idempotent by content-address (Put's own doc comment):
// a lost ack on a write that already landed must not corrupt the stored
// bytes when the pod retries with the identical digest and data.
func TestBlobClientRetryAfterLostAckDoesNotCorrupt(t *testing.T) {
	shrinkRetryDelays(t)
	server, blobs := fakeBlobEndpoint(t, "stage-scoped-token")
	// fakeBlobEndpoint's PUT handler already stores-then-200s; wrap it so the
	// FIRST PUT's response is dropped after the store completes, exactly as
	// TestSurrenderPutClientRetryAfterLostAckDoesNotDoubleApply does for the
	// surrender plane.
	base := server.Config.Handler
	first := true
	var puts atomic.Int32
	server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut {
			base.ServeHTTP(w, r)
			return
		}
		puts.Add(1)
		rec := httptest.NewRecorder()
		base.ServeHTTP(rec, r)
		if first {
			first = false
			hijacker, ok := w.(http.Hijacker)
			if !ok {
				t.Fatal("test server does not support hijacking")
			}
			conn, _, herr := hijacker.Hijack()
			if herr != nil {
				t.Fatalf("hijack: %v", herr)
			}
			_ = conn.Close()
			return
		}
		w.WriteHeader(rec.Code)
		_, _ = w.Write(rec.Body.Bytes())
	})

	data := []byte("artifact-bytes")
	digest := fmt.Sprintf("sha256:%x", sha256.Sum256(data))
	client := &BlobClient{BaseURL: server.URL, Token: "stage-scoped-token", RetryDeadline: time.Second}
	if err := client.Put(context.Background(), digest, data); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if got := puts.Load(); got != 2 {
		t.Fatalf("server saw %d PUTs, want 2 (the lost-ack write, then the pod's retry)", got)
	}
	stored, ok := blobs.Load(digest)
	if !ok {
		t.Fatal("blob was not stored")
	}
	if string(stored.([]byte)) != string(data) {
		t.Fatalf("stored blob = %q, want %q", stored, data)
	}
}

func TestBlobClientDescribeIsCredentialFree(t *testing.T) {
	client := &BlobClient{BaseURL: "http://endpoint:7777/", Token: "secret-token"}
	if described := client.Describe(); strings.Contains(described, "secret-token") {
		t.Fatalf("Describe leaks the credential: %q", described)
	}
}

// The stage-scoped credential request: POSTs the wire shape to the write
// API's resolve route with the pod bearer, decodes minted values.
func TestResolveStageCredentials(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != apicontract.CredentialResolvePath {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if r.Header.Get("Authorization") != "Bearer goobers-pod.tok" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		var request StageCredentialRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil || request.RunID != "run-1" || request.Stage != "build" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"runId": request.RunID, "stage": request.Stage,
			"credentials": []map[string]string{{"capability": "repo:push", "value": "minted"}},
		})
	}))
	t.Cleanup(server.Close)

	minted, err := ResolveStageCredentials(context.Background(), server.Client(), server.URL, "goobers-pod.tok",
		StageCredentialRequest{RunID: "run-1", Stage: "build"})
	if err != nil {
		t.Fatalf("ResolveStageCredentials: %v", err)
	}
	if len(minted) != 1 || minted[0].Capability != "repo:push" || minted[0].Value != "minted" {
		t.Fatalf("minted = %+v", minted)
	}

	// A refused resolve (undeclared capability → 403) surfaces as an error
	// naming the plane's answer.
	refusing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	t.Cleanup(refusing.Close)
	if _, err := ResolveStageCredentials(context.Background(), refusing.Client(), refusing.URL, "tok",
		StageCredentialRequest{RunID: "run-1", Stage: "build"}); err == nil {
		t.Fatal("403 resolve did not error")
	}
}
