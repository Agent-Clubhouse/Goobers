package main

// dispatchspan_test.go covers the pod half of #3805: the stage pod's scrubbed
// transcript reaches the blob plane under the SAME digest the journal artifact
// ref carries, so the daemon's span source has something to fetch.
//
// Without those bytes in the store the daemon-side wiring is a no-op that looks
// like a fix — the recorded failure merely changes from "no span source
// configured" to "blobstore: blob not found", under the same span_unavailable
// code, with the same missing transcript. MEASURED on the cluster before the
// change: run d3edc8f3804eae63bc39115aeb6cd542's transcript digest
// sha256:2715ad13… is in runs/<id>/artifacts/ and absent from
// /var/lib/goobers/blobstore/.
//
// THE PUT ITSELF IS NOT SPAN-SPECIFIC, and after #3823 there is exactly one of
// them: recordStageArtifacts write-throughs every content-addressed stream a
// pod produces (putStageArtifactBlob), and a span reaches it through
// RecordArtifact like any other artifact. What these tests pin is that the SPAN
// path actually arrives there — a recorder that stopped routing spans through
// RecordArtifact, or a write-through that stopped covering the "spans/" names,
// would restore the original defect with the whole suite green.
//
// The central test drives the REAL daemon blob plane (httpapi + podauth), not a
// stand-in: the plane is fail-closed on pod principal, so a PUT that lost its
// bearer would 403 and the feature would silently no-op behind one stderr line
// in a pod log. Acceptance here is the same fact the far-side check reads on the
// cluster — the bytes are in the DAEMON's store.

import (
	"bytes"
	"context"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/blobstore"
	"github.com/goobers/goobers/internal/dispatcher"
	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/podauth"
)

// podBlobPlane is the daemon's REAL blob plane: podauth in front of a
// deny-all human fallback, httpapi.RequireRoles as the authorizer, and a
// directory-backed blobstore.Store behind httpapi.WithBlobService — the same
// construction cmd/goobers/up.go performs. store is the daemon side of the
// seam: what lands in it is what the span source can later adopt.
type podBlobPlane struct {
	server *httptest.Server
	store  blobstore.Store
	token  string
}

func newPodBlobPlane(t *testing.T, runID string) *podBlobPlane {
	t.Helper()
	store, err := blobstore.NewDir(t.TempDir())
	if err != nil {
		t.Fatalf("blobstore.NewDir: %v", err)
	}
	registry := podauth.NewRegistry()
	authenticator, err := podauth.NewAuthenticator(registry, httpapi.DenyAllAuthenticator{})
	if err != nil {
		t.Fatalf("podauth.NewAuthenticator: %v", err)
	}
	handler, err := httpapi.NewHandler(&telemetryParityReader{}, httpapi.RequireRoles(), log.New(io.Discard, "", 0),
		httpapi.WithAuthenticator(authenticator),
		httpapi.WithBlobService(store),
	)
	if err != nil {
		t.Fatalf("httpapi.NewHandler: %v", err)
	}
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	token, err := registry.Mint(runID, 0)
	if err != nil {
		t.Fatalf("mint pod token: %v", err)
	}
	return &podBlobPlane{server: server, store: store, token: token}
}

// stampPodSpanEnv puts a stage pod's whole environment in place: the journal
// plane recordStageArtifacts emits through, the blob endpoint it write-throughs
// to, and the stage identity both are keyed by. The recorder is then built with
// nothing hand-wired — the client comes from podBlobClient(), the production
// constructor, exactly as it does in a pod.
func stampPodSpanEnv(t *testing.T, journalURL, blobURL, token, runID string) {
	t.Helper()
	t.Setenv(dispatcher.EnvDaemonAPI, journalURL)
	t.Setenv(dispatcher.EnvBlobEndpoint, blobURL)
	t.Setenv(dispatcher.EnvPodToken, token)
	t.Setenv(dispatcher.EnvRunID, runID)
	t.Setenv(dispatcher.EnvStage, "implement")
	t.Setenv(dispatcher.EnvAttempt, "1")
}

// acceptingJournalPlane is the journal half of a stage pod's environment: it
// must be up for recordStageArtifacts to emit at all, and a test about the BLOB
// half should never fail because of it.
func acceptingJournalPlane(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"applied":1}`))
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// blobPlaneRecorder is a stand-in for the daemon's blob plane used only where
// the REAL plane cannot express the case: a refusal, and a hang. It records
// every PUT body and its Authorization header.
type blobPlaneRecorder struct {
	mu     sync.Mutex
	puts   map[string][]byte
	auth   map[string]string
	status int
	block  <-chan struct{}
}

func newBlobPlaneRecorder(status int) *blobPlaneRecorder {
	return &blobPlaneRecorder{puts: map[string][]byte{}, auth: map[string]string{}, status: status}
}

func (b *blobPlaneRecorder) handler(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodPut || !strings.HasPrefix(req.URL.Path, dispatcher.BlobPathPrefix) {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	if b.block != nil {
		// A plane that HANGS rather than refusing: hold the request until the
		// client's own deadline gives up (or the test tears down).
		select {
		case <-b.block:
		case <-req.Context().Done():
		}
		return
	}
	body, err := io.ReadAll(req.Body)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	digest := strings.TrimPrefix(req.URL.Path, dispatcher.BlobPathPrefix)
	b.mu.Lock()
	b.puts[digest] = body
	b.auth[digest] = req.Header.Get("Authorization")
	b.mu.Unlock()
	w.WriteHeader(b.status)
}

func (b *blobPlaneRecorder) authorization(digest string) string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.auth[digest]
}

// spanRecorderFixture builds a pod recorder with a real DefaultScrubber (the
// Chain(Registry, Pattern) a stage pod actually runs) holding one registered
// secret, so a test also pins that what is PUT is the SCRUBBED byte slice.
func spanRecorderFixture(t *testing.T, secret string) (podArtifactRecorder, *bytes.Buffer) {
	t.Helper()
	registry, scrubber := journal.DefaultScrubber()
	if secret != "" {
		registry.Register([]byte(secret))
	}
	stderr := &bytes.Buffer{}
	return podArtifactRecorder{stderr: stderr, scrubber: scrubber, dir: t.TempDir()}, stderr
}

// TestPodSpanRecorderPutsExactlyTheBytesTheRefAddresses is the pod→daemon
// seam, end to end through the real plane. The digest in the returned Ref is
// what the engine workflow threads onto its pointer-only span op and what the
// daemon fetches from its blob store; the plane re-hashes the body before it
// stores anything, and blobstore.Dir.Get re-verifies on the way out, so a PUT
// of anything but the bytes the ref was derived from is a permanently
// unavailable span rather than an error at record time.
//
// Reading the assertion the way the far side does: the transcript is in the
// DAEMON's store, under the ref's digest, scrubbed, having been accepted by
// the pod-principal gate.
func TestPodSpanRecorderPutsExactlyTheBytesTheRefAddresses(t *testing.T) {
	const secret = "ghp-not-a-real-token-fixture"
	plane := newPodBlobPlane(t, "run-span-1")
	stampPodSpanEnv(t, acceptingJournalPlane(t), plane.server.URL, plane.token, "run-span-1")

	rec, stderr := spanRecorderFixture(t, secret)
	transcript := []byte(`{"event":"prompt","auth":"` + secret + `","body":"implement the thing"}`)

	ref, err := rec.RecordSpanWithSchema("implement", "copilot-cli.transcript",
		"goobers.dev/telemetry/genai-event/v1", transcript)
	if err != nil {
		t.Fatalf("RecordSpanWithSchema: %v", err)
	}
	if stderr.Len() != 0 {
		t.Fatalf("a successful span PUT wrote to stderr: %q", stderr.String())
	}

	body, err := plane.store.Get(context.Background(), ref.Digest)
	if err != nil {
		t.Fatalf("the daemon's blob store does not hold the span under %s: %v — "+
			"the daemon would record span_unavailable with a blob-not-found message", ref.Digest, err)
	}
	// The load-bearing property: content address and content agree, so the
	// daemon's own digest re-verification accepts these bytes.
	if got := journal.Digest(body); got != ref.Digest {
		t.Fatalf("stored body hashes to %s but is filed under %s", got, ref.Digest)
	}
	if bytes.Contains(body, []byte(secret)) {
		t.Fatal("the stored blob carries a registered secret; the blob plane was handed unscrubbed bytes")
	}
	if !bytes.Contains(body, []byte("implement the thing")) {
		t.Fatalf("the stored blob is not the transcript: %q", body)
	}
}

// TestPodSpanRecorderNeedsThePodBearer pins the half a green suite would
// otherwise never notice. The blob plane is fail-closed on pod principal
// (internal/httpapi/blobplane.go requireBlobPodPrincipal), so a regression
// that drops Token from podBlobClient() turns EVERY span PUT into a refusal
// whose entire visible cost is one line in a pod log — the feature silently
// no-ops and the daemon still reports span_unavailable.
func TestPodSpanRecorderNeedsThePodBearer(t *testing.T) {
	plane := newPodBlobPlane(t, "run-span-2")
	stampPodSpanEnv(t, acceptingJournalPlane(t), plane.server.URL, "", "run-span-2")

	client := podBlobClient()
	if client == nil {
		t.Fatal("a stamped endpoint produced no client")
	}
	if client.Token != "" {
		t.Fatalf("fixture error: client carries a token %q", client.Token)
	}
	rec, stderr := spanRecorderFixture(t, "")
	transcript := []byte(`{"event":"prompt"}`)

	ref, err := rec.RecordSpanWithSchema("implement", "copilot-cli.transcript", "", transcript)
	if err != nil {
		t.Fatalf("an unauthenticated blob PUT failed the span record: %v", err)
	}
	if _, err := plane.store.Get(context.Background(), ref.Digest); err == nil {
		t.Fatal("the blob plane accepted an unauthenticated span PUT; its pod-principal gate is not fail-closed")
	}
	if !strings.Contains(stderr.String(), "blob plane") ||
		!strings.Contains(stderr.String(), "spans/copilot-cli.transcript") {
		t.Fatalf("a refused span PUT did not name the span and the plane; stderr = %q", stderr.String())
	}
}

// TestPodSpanRecorderSurvivesABlobPlaneFailure: the stage has already
// produced its work, so a telemetry store that refuses must cost one stderr
// line and nothing else — the same posture recordStageArtifacts and
// workerhost.StagingArtifacts take. The journal artifact ref is still
// returned, so the transcript is still recorded through the journal plane.
// It also pins the bearer on the wire: the pod's stage-scoped token, the
// credential the plane's gate is checking.
func TestPodSpanRecorderSurvivesABlobPlaneFailure(t *testing.T) {
	plane := newBlobPlaneRecorder(http.StatusInternalServerError)
	server := httptest.NewServer(http.HandlerFunc(plane.handler))
	defer server.Close()

	stampPodSpanEnv(t, acceptingJournalPlane(t), server.URL, "pod-token-fixture", "run-span-3")
	rec, stderr := spanRecorderFixture(t, "")
	transcript := []byte(`{"event":"prompt"}`)

	ref, err := rec.RecordSpanWithSchema("implement", "copilot-cli.transcript", "", transcript)
	if err != nil {
		t.Fatalf("a refused blob PUT failed the span record: %v", err)
	}
	if ref.Digest != journal.Digest(transcript) {
		t.Fatalf("ref digest = %q, want %q", ref.Digest, journal.Digest(transcript))
	}
	if got := plane.authorization(ref.Digest); got != "Bearer pod-token-fixture" {
		t.Fatalf("span PUT Authorization = %q, want the pod's stage-scoped bearer", got)
	}
	if !strings.Contains(stderr.String(), "blob plane") {
		t.Fatalf("a failed span PUT was silent; stderr = %q", stderr.String())
	}
}

// TestPodSpanRecorderGivesUpOnAHangingBlobPlane: the PUT sits on the stage's
// critical path — the harness has finished and the result is not returned until
// the recorder does — so it carries its own deadline rather than falling back
// to BlobClient's 60s default. A plane that hangs costs the stage
// blobWriteThroughBudget, once, and then the same single stderr line.
func TestPodSpanRecorderGivesUpOnAHangingBlobPlane(t *testing.T) {
	release := make(chan struct{})
	plane := newBlobPlaneRecorder(http.StatusCreated)
	plane.block = release
	server := httptest.NewServer(http.HandlerFunc(plane.handler))
	t.Cleanup(server.Close)
	t.Cleanup(func() { close(release) })

	previous := blobWriteThroughBudget
	blobWriteThroughBudget = 100 * time.Millisecond
	t.Cleanup(func() { blobWriteThroughBudget = previous })

	stampPodSpanEnv(t, acceptingJournalPlane(t), server.URL, "pod-token-fixture", "run-span-4")
	rec, stderr := spanRecorderFixture(t, "")

	start := time.Now()
	if _, err := rec.RecordSpanWithSchema("implement", "copilot-cli.transcript", "", []byte(`{"event":"prompt"}`)); err != nil {
		t.Fatalf("a hanging blob plane failed the span record: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 30*time.Second {
		t.Fatalf("the span PUT held the stage for %s; it must carry its own deadline", elapsed)
	}
	if !strings.Contains(stderr.String(), "blob plane") {
		t.Fatalf("a timed-out span PUT was silent; stderr = %q", stderr.String())
	}
}

// TestPodSpanRecorderWithoutABlobEndpointIsUnchanged: a pod with no
// GOOBERS_BLOB_ENDPOINT (the loopback / pre-blob-plane deployment shape, and
// every type-1 and type-2 posture) records exactly as it did before — no PUT
// attempted, no stderr noise, same ref.
func TestPodSpanRecorderWithoutABlobEndpointIsUnchanged(t *testing.T) {
	stampPodSpanEnv(t, acceptingJournalPlane(t), "", "pod-token-fixture", "run-span-5")
	rec, stderr := spanRecorderFixture(t, "")
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
// write-through's client comes from podBlobClient(), which reads the same
// GOOBERS_BLOB_ENDPOINT and GOOBERS_POD_TOKEN the dispatcher stamps on every
// stage pod — so a pod that HAS the endpoint gets an authenticated client and
// one that does not gets nil. Both halves matter: the plane is fail-closed on
// pod principal, so a client with the right address and no bearer is refused
// (401 from the authenticator; an authenticated non-pod principal gets 403
// blob_plane_requires_pod_principal).
func TestPodBlobClientFollowsTheStampedEndpoint(t *testing.T) {
	t.Setenv(dispatcher.EnvBlobEndpoint, "")
	t.Setenv(dispatcher.EnvPodToken, "pod-token-fixture")
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
	if client.Token != "pod-token-fixture" {
		t.Fatalf("client token = %q, want the stamped pod bearer; the blob plane refuses an "+
			"unauthenticated PUT and every span would silently 401", client.Token)
	}
}
