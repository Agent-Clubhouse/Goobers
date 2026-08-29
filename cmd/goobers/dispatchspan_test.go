package main

// dispatchspan_test.go covers the pod half of #3805: the stage pod publishes
// its scrubbed transcript to the blob plane under the SAME digest the journal
// artifact ref carries, so the daemon's span source has something to fetch.
//
// Without this PUT the daemon-side wiring is a no-op that looks like a fix —
// the recorded failure merely changes from "no span source configured" to
// "blobstore: blob not found", under the same span_unavailable code, with the
// same missing transcript. MEASURED on the cluster before the change: run
// d3edc8f3804eae63bc39115aeb6cd542's transcript digest sha256:2715ad13… is in
// runs/<id>/artifacts/ and absent from /var/lib/goobers/blobstore/.

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/goobers/goobers/internal/dispatcher"
	"github.com/goobers/goobers/internal/journal"
)

// blobPlaneRecorder is a stand-in for the daemon's blob plane that records
// every PUT body by the digest it was addressed to, so a test can compare the
// bytes stored against the address they were stored under.
type blobPlaneRecorder struct {
	mu     sync.Mutex
	puts   map[string][]byte
	status int
}

func newBlobPlaneRecorder(status int) *blobPlaneRecorder {
	return &blobPlaneRecorder{puts: map[string][]byte{}, status: status}
}

func (b *blobPlaneRecorder) handler(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodPut || !strings.HasPrefix(req.URL.Path, dispatcher.BlobPathPrefix) {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	body, err := io.ReadAll(req.Body)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	b.mu.Lock()
	b.puts[strings.TrimPrefix(req.URL.Path, dispatcher.BlobPathPrefix)] = body
	b.mu.Unlock()
	w.WriteHeader(b.status)
}

func (b *blobPlaneRecorder) stored() map[string][]byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make(map[string][]byte, len(b.puts))
	for k, v := range b.puts {
		out[k] = v
	}
	return out
}

// spanRecorderFixture builds a pod recorder with a real DefaultScrubber (the
// Chain(Registry, Pattern) a stage pod actually runs) holding one registered
// secret, so the test also pins that what is PUT is the SCRUBBED byte slice.
func spanRecorderFixture(t *testing.T, endpoint, secret string) (podArtifactRecorder, *bytes.Buffer) {
	t.Helper()
	registry, scrubber := journal.DefaultScrubber()
	if secret != "" {
		registry.Register([]byte(secret))
	}
	stderr := &bytes.Buffer{}
	rec := podArtifactRecorder{stderr: stderr, scrubber: scrubber, dir: t.TempDir()}
	if endpoint != "" {
		rec.blobs = &dispatcher.BlobClient{BaseURL: endpoint}
	}
	return rec, stderr
}

// TestPodSpanRecorderPutsExactlyTheBytesTheRefAddresses is the pod→daemon
// seam. The digest in the returned Ref is what the engine workflow threads
// onto its pointer-only span op, and the daemon fetches THAT digest from the
// blob plane; blobstore.Dir.Get re-verifies the hash, so a PUT of anything
// but the bytes the ref was derived from is a permanently unavailable span
// rather than an error at record time.
func TestPodSpanRecorderPutsExactlyTheBytesTheRefAddresses(t *testing.T) {
	const secret = "ghp-not-a-real-token-fixture"
	plane := newBlobPlaneRecorder(http.StatusCreated)
	server := httptest.NewServer(http.HandlerFunc(plane.handler))
	defer server.Close()

	rec, stderr := spanRecorderFixture(t, server.URL, secret)
	transcript := []byte(`{"event":"prompt","auth":"` + secret + `","body":"implement the thing"}`)

	ref, err := rec.RecordSpanWithSchema("implement", "copilot-cli.transcript",
		"goobers.dev/telemetry/genai-event/v1", transcript)
	if err != nil {
		t.Fatalf("RecordSpanWithSchema: %v", err)
	}

	stored := plane.stored()
	if len(stored) != 1 {
		t.Fatalf("blob plane received %d PUTs, want exactly 1: %v", len(stored), stored)
	}
	body, ok := stored[ref.Digest]
	if !ok {
		t.Fatalf("span was not PUT under the digest the ref carries (%s); addresses seen: %v",
			ref.Digest, stored)
	}
	// The load-bearing assertion: content address and content agree, so the
	// daemon's digest re-verification will accept these bytes.
	if got := journal.Digest(body); got != ref.Digest {
		t.Fatalf("PUT body hashes to %s but was stored under %s — the daemon would reject it as not-found",
			got, ref.Digest)
	}
	if bytes.Contains(body, []byte(secret)) {
		t.Fatal("the PUT body carries a registered secret; the blob plane was handed unscrubbed bytes")
	}
	if !bytes.Contains(body, []byte("implement the thing")) {
		t.Fatalf("the PUT body is not the transcript: %q", body)
	}
	if stderr.Len() != 0 {
		t.Fatalf("a successful span PUT wrote to stderr: %q", stderr.String())
	}
}

// TestPodSpanRecorderSurvivesABlobPlaneFailure: the stage has already
// produced its work, so a telemetry store that refuses must cost one stderr
// line and nothing else — the same posture recordStageArtifacts and
// workerhost.StagingArtifacts take. The journal artifact ref is still
// returned, so the transcript is still recorded through the journal plane.
func TestPodSpanRecorderSurvivesABlobPlaneFailure(t *testing.T) {
	plane := newBlobPlaneRecorder(http.StatusInternalServerError)
	server := httptest.NewServer(http.HandlerFunc(plane.handler))
	defer server.Close()

	rec, stderr := spanRecorderFixture(t, server.URL, "")
	transcript := []byte(`{"event":"prompt"}`)

	ref, err := rec.RecordSpanWithSchema("implement", "copilot-cli.transcript", "", transcript)
	if err != nil {
		t.Fatalf("a refused blob PUT failed the span record: %v", err)
	}
	if ref.Digest != journal.Digest(transcript) {
		t.Fatalf("ref digest = %q, want %q", ref.Digest, journal.Digest(transcript))
	}
	if !strings.Contains(stderr.String(), "record span blob") {
		t.Fatalf("a failed span PUT was silent; stderr = %q", stderr.String())
	}
}

// TestPodSpanRecorderWithoutABlobEndpointIsUnchanged: a pod with no
// GOOBERS_BLOB_ENDPOINT (the loopback / pre-blob-plane deployment shape, and
// every type-1 and type-2 posture) records exactly as it did before — no PUT
// attempted, no stderr noise, same ref.
func TestPodSpanRecorderWithoutABlobEndpointIsUnchanged(t *testing.T) {
	rec, stderr := spanRecorderFixture(t, "", "")
	transcript := []byte(`{"event":"prompt"}`)

	ref, err := rec.RecordSpanWithSchema("implement", "copilot-cli.transcript", "", transcript)
	if err != nil {
		t.Fatalf("RecordSpanWithSchema without a blob endpoint: %v", err)
	}
	if ref.Digest != journal.Digest(transcript) {
		t.Fatalf("ref digest = %q, want %q", ref.Digest, journal.Digest(transcript))
	}
	if stderr.Len() != 0 {
		t.Fatalf("a pod with no blob endpoint wrote to stderr: %q", stderr.String())
	}
}

// TestPodBlobClientFollowsTheStampedEndpoint pins the construction half: the
// recorder's client comes from podBlobClient(), which reads the same
// GOOBERS_BLOB_ENDPOINT the dispatcher stamps on every stage pod — so a pod
// that HAS the endpoint gets a client and one that does not gets nil.
func TestPodBlobClientFollowsTheStampedEndpoint(t *testing.T) {
	t.Setenv(dispatcher.EnvBlobEndpoint, "")
	if client := podBlobClient(); client != nil {
		t.Fatalf("no stamped endpoint produced a client: %+v", client)
	}
	t.Setenv(dispatcher.EnvBlobEndpoint, "https://goobers-api.goobers-system.svc.cluster.local:8080")
	client := podBlobClient()
	if client == nil {
		t.Fatal("a stamped blob endpoint produced no client; the span PUT would silently never happen")
	}
	if client.BaseURL != "https://goobers-api.goobers-system.svc.cluster.local:8080" {
		t.Fatalf("client base URL = %q", client.BaseURL)
	}
}
