//go:build cloudintegration

package objstore

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/apicontract"
	cloudauth "github.com/goobers/goobers/internal/cloud/podauth"
	"github.com/goobers/goobers/internal/httpapi"
	shared "github.com/goobers/goobers/internal/podauth"
	"github.com/goobers/goobers/internal/readservice"
)

type noRevocations struct{}

func (noRevocations) Revoke(context.Context, string, time.Time) error     { return nil }
func (noRevocations) IsRevoked(context.Context, string) (bool, error)     { return false, nil }
func (noRevocations) CheckGrant(context.Context, string, time.Time) error { return nil }

// blobPlane is control's blob plane composition over a real object store, on
// a standby: the recovery gate is closed, as on a replica without the lease
// (#8188).
type blobPlane struct {
	url  string
	auth *cloudauth.Auth
}

func newBlobPlane(t *testing.T, store *Azure) blobPlane {
	t.Helper()
	path := filepath.Join(t.TempDir(), "key")
	raw, err := json.Marshal(map[string]shared.SigningKey{"current": {ID: "v1", Key: bytes.Repeat([]byte("k"), 32)}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	auth, err := cloudauth.New(cloudauth.Config{Mode: "signed", KeyFile: path, Revocations: noRevocations{}, Fallback: httpapi.DenyAllAuthenticator{}})
	if err != nil {
		t.Fatal(err)
	}
	handler, err := httpapi.NewHandler(struct{ readservice.Reader }{}, httpapi.RequireRoles(), log.New(io.Discard, "", 0),
		httpapi.WithAuthenticator(auth), httpapi.WithBlobService(store),
		httpapi.WithRecoveryGate(func() bool { return false }), httpapi.WithLeaseFreeBlobPlane())
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return blobPlane{url: server.URL, auth: auth}
}

func (p blobPlane) do(t *testing.T, method, digest, bearer string, body []byte) (int, []byte) {
	t.Helper()
	request, err := http.NewRequestWithContext(t.Context(), method, p.url+strings.Replace(apicontract.BlobDigestPath, "{digest}", digest, 1), bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if bearer != "" {
		request.Header.Set("Authorization", "Bearer "+bearer)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	data, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	return response.StatusCode, data
}

func (p blobPlane) worker(t *testing.T) string {
	t.Helper()
	token, err := p.auth.PodKeys.MintWorkerBlob("goobers-worker-0", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	return token
}

func (p blobPlane) pod(t *testing.T) string {
	t.Helper()
	token, err := p.auth.PodKeys.MintScoped("run-1", time.Minute, shared.ScopeBlob)
	if err != nil {
		t.Fatal(err)
	}
	return token
}

// Through control's blob plane on a standby: a worker PUTs and GETs by
// digest, a pod reads a blob control put, a mismatched body is refused, an
// absent digest is a 404, and only pod and worker-blob bearers get in.
func TestBlobPlaneServesTheObjectStoreOnAStandby(t *testing.T) {
	_, _, store := testMetadata(t, "goobers-blob-plane")
	plane := newBlobPlane(t, store)

	payload := []byte("a stage artifact")
	digest := digestFor(payload)
	if status, body := plane.do(t, http.MethodPut, digest, plane.worker(t), payload); status != http.StatusNoContent {
		t.Fatalf("worker PUT = %d %s", status, body)
	}
	if status, body := plane.do(t, http.MethodGet, digest, plane.worker(t), nil); status != http.StatusOK || !bytes.Equal(body, payload) {
		t.Fatalf("worker GET = %d %q", status, body)
	}
	if has, err := store.Has(t.Context(), digest); err != nil || !has {
		t.Fatalf("a worker PUT is not in control's object store: %v %v", has, err)
	}

	written := []byte("a child's sealed result receipt control wrote")
	if err := store.Put(t.Context(), digestFor(written), written); err != nil {
		t.Fatal(err)
	}
	if status, body := plane.do(t, http.MethodGet, digestFor(written), plane.pod(t), nil); status != http.StatusOK || !bytes.Equal(body, written) {
		t.Fatalf("pod GET of a control-written blob = %d %q", status, body)
	}

	if status, _ := plane.do(t, http.MethodPut, digest, plane.worker(t), []byte("other bytes")); status != http.StatusBadRequest {
		t.Fatalf("PUT with a body that does not hash to its digest = %d, want 400", status)
	}
	if status, _ := plane.do(t, http.MethodGet, digestFor([]byte("never stored")), plane.worker(t), nil); status != http.StatusNotFound {
		t.Fatalf("GET of an absent digest = %d, want 404", status)
	}

	configDigest, err := plane.auth.PodKeys.MintWorkerConfigDigest("goobers-worker-0", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	childAdmission, err := plane.auth.PodKeys.MintWorkerChildAdmission("goobers-worker-0", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	surrender, err := plane.auth.PodKeys.MintWorkerSurrender("goobers-worker-0", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	unscopedPod, err := plane.auth.PodKeys.MintScoped("run-1", time.Minute, shared.ScopeJournal)
	if err != nil {
		t.Fatal(err)
	}
	for name, bearer := range map[string]string{
		"no bearer": "", "unsigned": "not-a-token", "worker config digest": configDigest,
		"worker child admission": childAdmission, "worker surrender": surrender, "pod without the blob scope": unscopedPod,
	} {
		for _, method := range []string{http.MethodGet, http.MethodPut} {
			if status, _ := plane.do(t, method, digest, bearer, payload); status != http.StatusUnauthorized && status != http.StatusForbidden {
				t.Fatalf("%s %s = %d, want refused", name, method, status)
			}
		}
	}
}

// A standby's PUT of a blob that a collector is about to delete takes the
// per-digest storage lease and advances its ETag, so the collector re-observes
// instead of deleting: a stale replica's write is never lost to GC. The same
// candidate without the PUT is deleted, which shows the sweep would have.
func TestStandbyBlobPutIsNotLostToGC(t *testing.T) {
	metadata, pool, store := testMetadata(t, "goobers-blob-plane-gc")
	plane := newBlobPlane(t, store)
	candidate := func(content string) string {
		t.Helper()
		archive := []byte(content)
		digest := digestFor(archive)
		if err := metadata.RecordGeneration(t.Context(), "instance", digest, archive); err != nil {
			t.Fatal(err)
		}
		if err := metadata.PruneGeneration(t.Context(), "instance", digest); err != nil {
			t.Fatal(err)
		}
		ageCandidate(t, pool, digest)
		return digest
	}
	rewritten, untouched := candidate("rewritten through the plane"), candidate("left alone")
	manager, handle := gcLease(t, pool)
	if _, err := metadata.SweepGC(t.Context(), manager, handle, time.Hour); err != nil { // first observation
		t.Fatal(err)
	}
	if status, body := plane.do(t, http.MethodPut, rewritten, plane.worker(t), []byte("rewritten through the plane")); status != http.StatusNoContent {
		t.Fatalf("standby PUT = %d %s", status, body)
	}
	ageObservation(t, pool, rewritten)
	ageObservation(t, pool, untouched)
	deleted, err := metadata.SweepGC(t.Context(), manager, handle, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if has, err := store.Has(t.Context(), untouched); err != nil || has || deleted != 1 {
		t.Fatalf("baseline: the untouched candidate survived (present %v, deleted %d, %v); the sweep could not have deleted", has, deleted, err)
	}
	if has, err := store.Has(t.Context(), rewritten); err != nil || !has {
		t.Fatalf("a standby's PUT was lost to GC: present %v (%v)", has, err)
	}
}

// A blob a pod PUT through the plane and the journal later references
// survives a sweep, whatever candidate row exists for it: the plane writes the
// same object layout every census reference names (#8188).
func TestPlaneWrittenBlobReferencedByTheJournalSurvivesGC(t *testing.T) {
	metadata, pool, store := testMetadata(t, "goobers-blob-plane-census")
	plane := newBlobPlane(t, store)
	payload := []byte("a pod's stage output")
	digest := digestFor(payload)
	if status, body := plane.do(t, http.MethodPut, digest, plane.pod(t), payload); status != http.StatusNoContent {
		t.Fatalf("pod PUT = %d %s", status, body)
	}
	insertJournalArtifact(t, pool, digest)
	if _, err := pool.Exec(t.Context(), `INSERT INTO control.blob_gc_candidate (digest, unreferenced_at) VALUES ($1, clock_timestamp()-interval '40 days')`, digest); err != nil {
		t.Fatal(err)
	}
	manager, handle := gcLease(t, pool)
	for range 2 {
		if deleted, err := metadata.SweepGC(t.Context(), manager, handle, time.Hour); err != nil || deleted != 0 {
			t.Fatalf("sweep = %d, %v", deleted, err)
		}
		ageObservation(t, pool, digest)
	}
	if has, err := store.Has(t.Context(), digest); err != nil || !has {
		t.Fatalf("a journal-referenced plane blob was collected: present %v (%v)", has, err)
	}
	var candidates int
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM control.blob_gc_candidate WHERE digest=$1`, digest).Scan(&candidates); err != nil || candidates != 0 {
		t.Fatalf("referenced candidate rows = %d (%v), want the census to drop it", candidates, err)
	}
}
