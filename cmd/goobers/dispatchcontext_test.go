package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/agentickit"
	"github.com/goobers/goobers/internal/credentials"
	"github.com/goobers/goobers/internal/dispatcher"
	"github.com/goobers/goobers/internal/harness"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/journal"
	harnesstest "github.com/goobers/goobers/test/testsupport/harness"
)

// seedBlobPlane publishes bytes to the fake plane under their journal artifact
// ref and returns the pointer a downstream stage would be handed for them.
//
// The ref is derived by journal.ArtifactRef — the same derivation the pod's own
// recordStageArtifacts uses and the same one the daemon's writer produces — so
// the fixture cannot agree with the code under test by accident.
func seedBlobPlane(t *testing.T, endpoint, name string, data []byte) apiv1.ContextPointer {
	t.Helper()
	ref, err := journal.ArtifactRef(data)
	if err != nil {
		t.Fatalf("derive artifact ref: %v", err)
	}
	client := &dispatcher.BlobClient{BaseURL: endpoint, Token: "pod-token"}
	if err := client.Put(context.Background(), ref.Digest, data); err != nil {
		t.Fatalf("seed blob plane: %v", err)
	}
	return apiv1.ContextPointer{
		Name: name,
		Artifact: &apiv1.ArtifactPointer{
			Path:      ref.Path,
			Digest:    ref.Digest,
			Size:      ref.Size,
			MediaType: "text/plain",
			Integrity: apiv1.IntegrityDerived,
		},
	}
}

// buildPodAgenticExecutorForTest builds the executor with the SAME wiring the
// pod uses — the pod's own recorder as the staging directory, and that same
// directory as the contextResolver's root — so a test that resolves a pointer
// through it is exercising the pod's seam rather than a rebuilt approximation.
func buildPodAgenticExecutorForTest(t *testing.T, runsDir string, act func(context.Context, harness.RunRequest) error) invoke.Goober {
	t.Helper()
	resolver, err := credentials.NewResolver(nil)
	if err != nil {
		t.Fatal(err)
	}
	registry := harness.NewRegistry()
	if err := registry.RegisterAs(string(apiv1.HarnessCopilot), &harnesstest.FakeAdapter{Act: act}); err != nil {
		t.Fatal(err)
	}
	scrubberRegistry, scrubber := journal.DefaultScrubber()
	exec, err := buildAgenticExecutor(agenticExecutorInput{
		GooberName:       "coder",
		Goobers:          map[string]apiv1.GooberSpec{"coder": {}},
		Instructions:     map[string]string{"coder": "instructions"},
		AdapterRegistry:  registry,
		Resolver:         resolver,
		SharedRegistry:   scrubberRegistry,
		RunsDir:          runsDir,
		ArtifactRecorder: podArtifactRecorder{stderr: os.Stderr, scrubber: scrubber, dir: runsDir},
		SecretRegistrar:  scrubberRegistry,
	})
	if err != nil {
		t.Fatalf("buildAgenticExecutor: %v", err)
	}
	return exec
}

// THE BUG, at the seam it crosses: an agentic stage in a pod whose envelope
// carries an upstream artifact pointer.
//
// The pod's contextResolver is rooted at a fresh MkdirTemp created moments
// earlier in this pod, so nothing an earlier stage produced is on this
// filesystem. harness.Executor.materializeContext resolves each pointer by
// READING that root and treats a failure as a hard executor error, so without a
// fetch step the stage cannot start at all — which makes every non-start agentic
// pod stage (production implement with contextFrom, a placed reviewer gate, the
// nomination triage stage) unrunnable on the pod substrate.
//
// The assertion is not "materialize returned nil": it is that the AGENT
// eventually read the upstream bytes out of its own workspace, which is the
// property the workflow author actually depends on.
func TestPodAgenticStageReadsAnUpstreamArtifactPointer(t *testing.T) {
	endpoint, _ := fakeBlobPlane(t)
	t.Setenv(dispatcher.EnvBlobEndpoint, endpoint)
	t.Setenv(dispatcher.EnvPodToken, "pod-token")
	// No journal plane: artifact recording is a no-op here, so this test is
	// about the READ side only.
	t.Setenv(dispatcher.EnvDaemonAPI, "")

	const upstream = "the finding the reviewer must actually read\n"
	pointer := seedBlobPlane(t, endpoint, "implement.artifact[0]", []byte(upstream))

	runsDir := t.TempDir()
	workspace := t.TempDir()
	env := apiv1.InvocationEnvelope{
		RunID:           "run-ctx",
		TaskID:          "review",
		Goober:          "coder",
		Workspace:       workspace,
		ContextPointers: []apiv1.ContextPointer{pointer},
	}

	// What the pod now does before invoking the harness.
	var podErr strings.Builder
	if err := materializePodContext(context.Background(), runsDir, env, &podErr); err != nil {
		t.Fatalf("materializePodContext: %v", err)
	}

	// The agent's own view: it reads its context out of the workspace, exactly
	// as a prompt instructs a real goober to.
	var seen string
	exec := buildPodAgenticExecutorForTest(t, runsDir, func(_ context.Context, req harness.RunRequest) error {
		data, err := os.ReadFile(filepath.Join(req.Workspace, ".goobers", "context", "00-implement.artifact_0_"))
		if err != nil {
			return err
		}
		seen = string(data)
		return harnesstest.WriteCompletion(req.Workspace, req.CompletionPath, apiv1.ResultEnvelope{
			Status:  apiv1.ResultSuccess,
			Summary: "read the upstream artifact",
		})
	})

	result, err := exec.Invoke(context.Background(), env)
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if result.Status != apiv1.ResultSuccess {
		t.Fatalf("status = %q (%+v), want success", result.Status, result.Error)
	}
	if seen != upstream {
		t.Fatalf("the agent read %q from its context directory, want the upstream artifact %q", seen, upstream)
	}
	if !strings.Contains(podErr.String(), pointer.Artifact.Digest) {
		t.Errorf("pod stderr does not name the materialized digest, so an operator cannot see the inputs arrived:\n%s", podErr.String())
	}
}

// A pointer whose blob the plane does not hold must fail CLOSED, and the
// refusal must name the pointer and the digest.
//
// workerhost.MaterializeContext fails SOFT on a missing blob by design — on a
// worker the local directory may already hold it. In a pod that directory was
// created empty seconds ago, so a missing blob is a blob this stage will never
// see. Reporting it as a generic materialize error (or letting the harness
// report a missing file under a content-addressed path) hands the operator a
// digest with no stage and no pointer name attached to it.
func TestPodContextMaterializationFailsClosedOnAnAbsentBlob(t *testing.T) {
	endpoint, _ := fakeBlobPlane(t)
	t.Setenv(dispatcher.EnvBlobEndpoint, endpoint)
	t.Setenv(dispatcher.EnvPodToken, "pod-token")

	// Derived but never published: the pointer names a digest the plane has
	// never been told about, which is exactly what an upstream pod stage that
	// journaled its artifact WITHOUT writing it through to the blob plane
	// produces (the other half of this issue).
	ref, err := journal.ArtifactRef([]byte("never published\n"))
	if err != nil {
		t.Fatal(err)
	}
	env := apiv1.InvocationEnvelope{
		TaskID: "review",
		ContextPointers: []apiv1.ContextPointer{{
			Name:     "implement.artifact[0]",
			Artifact: &apiv1.ArtifactPointer{Path: ref.Path, Digest: ref.Digest, Size: ref.Size},
		}},
	}

	gotErr := materializePodContext(context.Background(), t.TempDir(), env, &strings.Builder{})
	if gotErr == nil {
		t.Fatal("an absent upstream blob was accepted; the stage would run without an input its workflow declared")
	}
	if !errors.Is(gotErr, errContextBlobMissing) {
		t.Fatalf("error = %v, want one wrapping errContextBlobMissing", gotErr)
	}
	for _, want := range []string{"implement.artifact[0]", ref.Digest, "review"} {
		if !strings.Contains(gotErr.Error(), want) {
			t.Errorf("error %q does not name %q", gotErr.Error(), want)
		}
	}
}

// A pod handed artifact-backed pointers with no blob endpoint cannot fetch
// them. That is a deployment fault rather than a run fault, so it is a distinct
// named error — fixed in the dispatcher, not in the run.
func TestPodContextMaterializationRefusesWithoutABlobEndpoint(t *testing.T) {
	t.Setenv(dispatcher.EnvBlobEndpoint, "")
	ref, err := journal.ArtifactRef([]byte("x\n"))
	if err != nil {
		t.Fatal(err)
	}
	env := apiv1.InvocationEnvelope{
		TaskID: "review",
		ContextPointers: []apiv1.ContextPointer{{
			Name:     "implement.artifact[0]",
			Artifact: &apiv1.ArtifactPointer{Path: ref.Path, Digest: ref.Digest},
		}},
	}
	gotErr := materializePodContext(context.Background(), t.TempDir(), env, &strings.Builder{})
	if !errors.Is(gotErr, errContextBlobPlaneUnavailable) {
		t.Fatalf("error = %v, want one wrapping errContextBlobPlaneUnavailable", gotErr)
	}
	if !strings.Contains(gotErr.Error(), dispatcher.EnvBlobEndpoint) {
		t.Errorf("error %q does not name the variable that is missing", gotErr.Error())
	}
}

// THE WIRING, pinned at the call site rather than at the helper.
//
// A correct materializePodContext that nothing calls is the bug unchanged, so
// this drives runAgenticStage itself: it publishes a real kit whose envelope
// carries an unresolvable artifact pointer, and requires the stage to be refused
// FOR THAT REASON.
//
// The failure code is the whole assertion. ABLATED (call site removed, helper
// intact) this test is the only one in the file that fails, and it fails with
//
//	failure code = "agentic_executor_unavailable" (kit carries no spec for
//	goober "coder"), want context_materialize_failed
//
// — the pod walking straight past its missing context into executor
// construction, and blaming the runner image for an input the run never
// delivered. On a runner that DOES have the harness installed it goes further
// still and fails inside the harness, as an unreadable file under a
// content-addressed path with no pointer name attached.
func TestRunAgenticStageMaterializesContextBeforeBuildingTheExecutor(t *testing.T) {
	endpoint, _ := fakeBlobPlane(t)
	ref, err := journal.ArtifactRef([]byte("what implement decided\n"))
	if err != nil {
		t.Fatal(err)
	}
	kit := &agentickit.Kit{Envelope: apiv1.InvocationEnvelope{
		RunID:  "run-wiring",
		TaskID: "review",
		Goober: "coder",
		ContextPointers: []apiv1.ContextPointer{{
			Name:     "implement.artifact[0]",
			Artifact: &apiv1.ArtifactPointer{Path: ref.Path, Digest: ref.Digest, Size: ref.Size},
		}},
	}}
	data, digest, err := agentickit.Marshal(kit)
	if err != nil {
		t.Fatal(err)
	}
	// The kit IS published; the context artifact deliberately is not.
	if err := (&dispatcher.BlobClient{BaseURL: endpoint, Token: "pod-token"}).Put(context.Background(), digest, data); err != nil {
		t.Fatalf("publish kit: %v", err)
	}

	t.Setenv(dispatcher.EnvBlobEndpoint, endpoint)
	t.Setenv(dispatcher.EnvPodToken, "pod-token")
	t.Setenv(dispatcher.EnvAgenticKitDigest, digest)
	t.Setenv(dispatcher.EnvStageCapabilities, "")
	t.Setenv(dispatcher.EnvCheckoutCapability, "")
	t.Setenv(dispatcher.EnvStageWorkspace, string(apiv1.WorkspaceScratch))
	t.Setenv(dispatcher.EnvDaemonAPI, "")

	got := runAgenticStage(context.Background(), &strings.Builder{}, &strings.Builder{})
	if got.Status != apiv1.ResultFailure || got.Error == nil {
		t.Fatalf("envelope = %+v, want a failure naming the missing context", got)
	}
	if got.Error.Code != "context_materialize_failed" {
		t.Fatalf("failure code = %q (%s), want context_materialize_failed: the pod did not materialize its context before building the executor",
			got.Error.Code, got.Error.Message)
	}
	if !strings.Contains(got.Error.Message, ref.Digest) {
		t.Errorf("failure message %q does not name the digest that could not be fetched", got.Error.Message)
	}
}

// A stage with no artifact-backed pointers must not need a blob plane at all:
// every stage that has ever run on a pod is a start stage, and requiring an
// endpoint for them would break the substrate to fix the substrate.
func TestPodContextMaterializationIsANoOpWithoutArtifactPointers(t *testing.T) {
	t.Setenv(dispatcher.EnvBlobEndpoint, "")
	env := apiv1.InvocationEnvelope{
		TaskID: "implement",
		ContextPointers: []apiv1.ContextPointer{
			{Name: "issue", External: &apiv1.ExternalRef{Kind: "issue", URI: "https://example.test/1"}},
			// Cross-run pointers are the harness's to admit (journal:read) and
			// resolve against a different root; this layer must not fetch them.
			{Name: "other-run", RunID: "run-other", Artifact: &apiv1.ArtifactPointer{Path: "artifacts/aa/bb", Digest: "sha256:" + strings.Repeat("a", 64)}},
		},
	}
	if err := materializePodContext(context.Background(), t.TempDir(), env, &strings.Builder{}); err != nil {
		t.Fatalf("materializePodContext = %v, want nil for a stage with nothing to fetch", err)
	}
}

// HALF A, at the seam: the bytes a pod journals as an artifact must also land in
// the BLOB plane, under the exact digest the derived pointer names.
//
// Without this the pointer a pod surrenders names a digest the fleet store has
// never held: the artifact exists in the daemon's journal and nowhere the fetch
// side (workerhost.MaterializeContext, and now materializePodContext) can reach.
// A worker-executed stage has always written through (workerhost/artifacts.go);
// this is the pod closing the same loop.
func TestPodStageArtifactIsPublishedToTheBlobPlaneUnderItsDerivedDigest(t *testing.T) {
	journalPlane := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"applied":1}`))
	}))
	defer journalPlane.Close()
	endpoint, _ := fakeBlobPlane(t)

	t.Setenv(dispatcher.EnvDaemonAPI, journalPlane.URL)
	t.Setenv(dispatcher.EnvBlobEndpoint, endpoint)
	t.Setenv(dispatcher.EnvPodToken, "pod-token")
	t.Setenv(dispatcher.EnvRunID, "run-art")
	t.Setenv(dispatcher.EnvStage, "implement")
	t.Setenv(dispatcher.EnvAttempt, "1")

	data := []byte("candidate findings the next stage reads\n")
	var errOut strings.Builder
	pointers := recordStageArtifacts(context.Background(), &errOut, map[string][]byte{"findings.json": data})
	if len(pointers) != 1 {
		t.Fatalf("pointers = %+v, want exactly one", pointers)
	}
	// Read back through the SAME client the fetch side uses, so what is
	// asserted is "a later stage can get these bytes by this digest" rather
	// than "some map has a key".
	reader := &dispatcher.BlobClient{BaseURL: endpoint, Token: "pod-token"}
	stored, err := reader.Get(context.Background(), pointers[0].Digest)
	if err != nil {
		t.Fatalf("the blob plane holds nothing at %s (%v); the surrendered pointer names an artifact no other node can fetch", pointers[0].Digest, err)
	}
	if string(stored) != string(data) {
		t.Fatalf("blob at %s = %q, want the exact bytes the pointer was derived from %q", pointers[0].Digest, stored, data)
	}
	// The independent oracle: what the journal itself computes for these bytes.
	want, refErr := journal.ArtifactRef(data)
	if refErr != nil {
		t.Fatal(refErr)
	}
	if pointers[0].Digest != want.Digest {
		t.Fatalf("pointer digest = %s, want the journal's %s", pointers[0].Digest, want.Digest)
	}
}

// The write-through is BEST EFFORT: the stage has already produced its result,
// and a blob plane that is down must not convert a completed stage into a
// failure. It must still say so on stderr — a silent drop is how a later stage's
// fail-closed refusal becomes unexplainable.
func TestPodStageArtifactPutIsBestEffortAndNoisy(t *testing.T) {
	journalPlane := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"applied":1}`))
	}))
	defer journalPlane.Close()
	brokenBlobs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer brokenBlobs.Close()

	t.Setenv(dispatcher.EnvDaemonAPI, journalPlane.URL)
	t.Setenv(dispatcher.EnvBlobEndpoint, brokenBlobs.URL)
	t.Setenv(dispatcher.EnvPodToken, "pod-token")
	t.Setenv(dispatcher.EnvRunID, "run-art")
	t.Setenv(dispatcher.EnvStage, "implement")
	t.Setenv(dispatcher.EnvAttempt, "1")

	var errOut strings.Builder
	pointers := recordStageArtifacts(context.Background(), &errOut, map[string][]byte{"stdout.log": []byte("x\n")})
	if len(pointers) != 1 {
		t.Fatalf("a failed blob PUT dropped the journal pointer: %+v", pointers)
	}
	if !strings.Contains(errOut.String(), "blob plane") {
		t.Fatalf("stderr does not report the failed write-through:\n%s", errOut.String())
	}
}

// THE TWO HALVES, COUPLED — which is why they are one change. What one pod
// journals as an artifact must be readable by the NEXT pod, which shares no
// filesystem with it. Two staging roots stand in for two pods, exactly as
// dispatchdelta_test's two workspaces stand in for two checkouts.
func TestAPodProducedArtifactReachesTheNextPodStage(t *testing.T) {
	journalPlane := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"applied":1}`))
	}))
	defer journalPlane.Close()
	endpoint, _ := fakeBlobPlane(t)

	t.Setenv(dispatcher.EnvDaemonAPI, journalPlane.URL)
	t.Setenv(dispatcher.EnvBlobEndpoint, endpoint)
	t.Setenv(dispatcher.EnvPodToken, "pod-token")
	t.Setenv(dispatcher.EnvRunID, "run-coupled")
	t.Setenv(dispatcher.EnvStage, "implement")
	t.Setenv(dispatcher.EnvAttempt, "1")

	// --- pod A: produce an artifact ---------------------------------------
	produced := []byte("what implement decided\n")
	pointers := recordStageArtifacts(context.Background(), &strings.Builder{}, map[string][]byte{"decision.json": produced})
	if len(pointers) != 1 {
		t.Fatalf("pointers = %+v, want one", pointers)
	}

	// --- pod B: a DIFFERENT filesystem, holding nothing --------------------
	t.Setenv(dispatcher.EnvStage, "review")
	runsDir := t.TempDir()
	env := apiv1.InvocationEnvelope{
		TaskID: "review",
		ContextPointers: []apiv1.ContextPointer{{
			Name:     "implement.artifact[0]",
			Artifact: &pointers[0],
		}},
	}
	if _, err := env.ContextPointers[0].Artifact.Resolve(runsDir); err == nil {
		t.Fatal("pod B resolved the artifact before materializing it; the test proves nothing")
	}
	if err := materializePodContext(context.Background(), runsDir, env, &strings.Builder{}); err != nil {
		t.Fatalf("materializePodContext: %v", err)
	}
	// The harness's own read, against the root its contextResolver uses.
	got, err := env.ContextPointers[0].Artifact.Resolve(runsDir)
	if err != nil {
		t.Fatalf("harness-side Resolve after materialization: %v", err)
	}
	if string(got) != string(produced) {
		t.Fatalf("pod B read %q, want what pod A produced %q", got, produced)
	}
}
