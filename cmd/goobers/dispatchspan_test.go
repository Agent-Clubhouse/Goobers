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
//
// The central test drives the REAL daemon blob plane (httpapi + podauth), not
// a stand-in: the plane is fail-closed on pod principal, so a span PUT that
// lost its bearer would 403 and the whole feature would no-op behind one
// stderr line in a pod log. Acceptance here is the same fact the far-side
// check reads on the cluster — the bytes are in the DAEMON's store.

import (
	"bytes"
	"context"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/agentickit"
	"github.com/goobers/goobers/internal/blobstore"
	"github.com/goobers/goobers/internal/dispatcher"
	"github.com/goobers/goobers/internal/harness"
	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/podauth"
	harnesstest "github.com/goobers/goobers/test/testsupport/harness"
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

// stampPod puts the plane's endpoint and the stage's bearer in the pod's
// environment exactly as the dispatcher stamps them, so the test builds its
// client through podBlobClient() — the production constructor — rather than
// by hand.
func (p *podBlobPlane) stampPod(t *testing.T, token string) {
	t.Helper()
	t.Setenv(dispatcher.EnvBlobEndpoint, p.server.URL)
	t.Setenv(dispatcher.EnvPodToken, token)
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
func spanRecorderFixture(t *testing.T, blobs *dispatcher.BlobClient, secret string) (podArtifactRecorder, *bytes.Buffer) {
	t.Helper()
	registry, scrubber := journal.DefaultScrubber()
	if secret != "" {
		registry.Register([]byte(secret))
	}
	stderr := &bytes.Buffer{}
	return podArtifactRecorder{stderr: stderr, scrubber: scrubber, dir: t.TempDir(), blobs: blobs}, stderr
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
	plane.stampPod(t, plane.token)

	rec, stderr := spanRecorderFixture(t, podBlobClient(), secret)
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
	plane.stampPod(t, "")

	client := podBlobClient()
	if client == nil {
		t.Fatal("a stamped endpoint produced no client")
	}
	if client.Token != "" {
		t.Fatalf("fixture error: client carries a token %q", client.Token)
	}
	rec, stderr := spanRecorderFixture(t, client, "")
	transcript := []byte(`{"event":"prompt"}`)

	ref, err := rec.RecordSpanWithSchema("implement", "copilot-cli.transcript", "", transcript)
	if err != nil {
		t.Fatalf("an unauthenticated blob PUT failed the span record: %v", err)
	}
	if _, err := plane.store.Get(context.Background(), ref.Digest); err == nil {
		t.Fatal("the blob plane accepted an unauthenticated span PUT; its pod-principal gate is not fail-closed")
	}
	if !strings.Contains(stderr.String(), "record span blob") {
		t.Fatalf("a refused span PUT was silent; stderr = %q", stderr.String())
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

	t.Setenv(dispatcher.EnvBlobEndpoint, server.URL)
	t.Setenv(dispatcher.EnvPodToken, "pod-token-fixture")
	rec, stderr := spanRecorderFixture(t, podBlobClient(), "")
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
	if !strings.Contains(stderr.String(), "record span blob") {
		t.Fatalf("a failed span PUT was silent; stderr = %q", stderr.String())
	}
}

// TestPodSpanRecorderGivesUpOnAHangingBlobPlane: the PUT sits on the stage's
// critical path, so it carries its own deadline rather than falling back to
// BlobClient's 60s default. A plane that hangs costs the stage the timeout,
// once, and then the same single stderr line.
func TestPodSpanRecorderGivesUpOnAHangingBlobPlane(t *testing.T) {
	release := make(chan struct{})
	plane := newBlobPlaneRecorder(http.StatusCreated)
	plane.block = release
	server := httptest.NewServer(http.HandlerFunc(plane.handler))
	t.Cleanup(server.Close)
	t.Cleanup(func() { close(release) })

	previous := spanBlobPutTimeout
	spanBlobPutTimeout = 100 * time.Millisecond
	t.Cleanup(func() { spanBlobPutTimeout = previous })

	t.Setenv(dispatcher.EnvBlobEndpoint, server.URL)
	t.Setenv(dispatcher.EnvPodToken, "pod-token-fixture")
	rec, stderr := spanRecorderFixture(t, podBlobClient(), "")

	start := time.Now()
	if _, err := rec.RecordSpanWithSchema("implement", "copilot-cli.transcript", "", []byte(`{"event":"prompt"}`)); err != nil {
		t.Fatalf("a hanging blob plane failed the span record: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 30*time.Second {
		t.Fatalf("the span PUT held the stage for %s; it must carry its own deadline", elapsed)
	}
	if !strings.Contains(stderr.String(), "record span blob") {
		t.Fatalf("a timed-out span PUT was silent; stderr = %q", stderr.String())
	}
}

// TestPodSpanRecorderWithoutABlobEndpointIsUnchanged: a pod with no
// GOOBERS_BLOB_ENDPOINT (the loopback / pre-blob-plane deployment shape, and
// every type-1 and type-2 posture) records exactly as it did before — no PUT
// attempted, no stderr noise, same ref.
func TestPodSpanRecorderWithoutABlobEndpointIsUnchanged(t *testing.T) {
	t.Setenv(dispatcher.EnvBlobEndpoint, "")
	rec, stderr := spanRecorderFixture(t, podBlobClient(), "")
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

// TestBuildPodAgenticExecutorGivesTheRecorderTheBlobClient is the pod-side
// CONSTRUCTION seam. RecordSpanWithSchema only PUTs when the recorder holds a
// client, and the one production site that gives it one is
// buildPodAgenticExecutor — the entry point every mode-3 agentic stage goes
// through. Nothing else in the repo exercises it, so without this test the
// whole pod half of #3805 is a field that can be deleted with a green suite.
func TestBuildPodAgenticExecutorGivesTheRecorderTheBlobClient(t *testing.T) {
	registry := harness.NewRegistry()
	if err := registry.RegisterAs(string(apiv1.HarnessCopilot), &harnesstest.FakeAdapter{
		AdapterName: string(apiv1.HarnessCopilot),
	}); err != nil {
		t.Fatal(err)
	}
	previousRegistry := podHarnessRegistry
	podHarnessRegistry = func(map[string]string, []string, map[string][]string, string, string, bool,
		func(context.Context) (string, error)) (*harness.Registry, error) {
		return registry, nil
	}
	t.Cleanup(func() { podHarnessRegistry = previousRegistry })

	var captured agenticExecutorInput
	previousBuilder := podExecutorBuilder
	podExecutorBuilder = func(input agenticExecutorInput) (invoke.Goober, error) {
		captured = input
		return nil, nil
	}
	t.Cleanup(func() { podExecutorBuilder = previousBuilder })

	kit := &agentickit.Kit{
		Envelope: apiv1.InvocationEnvelope{Goober: "implementer"},
		Goobers: map[string]apiv1.GooberSpec{
			"implementer": {Harness: apiv1.HarnessCopilot},
		},
	}

	t.Setenv(dispatcher.EnvBlobEndpoint, "https://goobers-api.goobers-system.svc.cluster.local:8080")
	t.Setenv(dispatcher.EnvPodToken, "pod-token-fixture")
	if _, err := buildPodAgenticExecutor(kit, io.Discard, nil); err != nil {
		t.Fatalf("buildPodAgenticExecutor: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(captured.RunsDir) })
	recorder, ok := captured.ArtifactRecorder.(podArtifactRecorder)
	if !ok {
		t.Fatalf("pod executor was built with a %T recorder", captured.ArtifactRecorder)
	}
	if recorder.blobs == nil {
		t.Fatal("the pod's span recorder was built with NO blob client: every transcript stays " +
			"inside the pod and the daemon's span source finds nothing to adopt")
	}
	if recorder.blobs.BaseURL != "https://goobers-api.goobers-system.svc.cluster.local:8080" ||
		recorder.blobs.Token != "pod-token-fixture" {
		t.Fatalf("recorder blob client = %+v, want the stamped endpoint and pod bearer", recorder.blobs)
	}

	// A pod with no blob endpoint keeps the pre-#3805 posture exactly: no
	// client, so no PUT is ever attempted.
	t.Setenv(dispatcher.EnvBlobEndpoint, "")
	if _, err := buildPodAgenticExecutor(kit, io.Discard, nil); err != nil {
		t.Fatalf("buildPodAgenticExecutor without a blob endpoint: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(captured.RunsDir) })
	recorder, ok = captured.ArtifactRecorder.(podArtifactRecorder)
	if !ok {
		t.Fatalf("pod executor was built with a %T recorder", captured.ArtifactRecorder)
	}
	if recorder.blobs != nil {
		t.Fatalf("an unstamped pod built a blob client anyway: %+v", recorder.blobs)
	}
}
