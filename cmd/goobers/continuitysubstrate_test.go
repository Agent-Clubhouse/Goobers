package main

// continuitysubstrate_test.go is the CROSS-SUBSTRATE WORKSPACE CONTINUITY
// HARNESS (#4009), the in-repo replacement for the two config-level probe
// arms that could never run.
//
// # Why this exists
//
// #3803 ("a worker-side gate or stage cannot see commits made in a pod") and
// #3767 ("continuity should follow the declared repoFrom edge") are both
// statements about a MIXED-SUBSTRATE run: one stage in a pod, a later stage
// on self, or the reverse. `Goobernetes-Workflows`' pod-continuity-probe was
// authored to answer them and declared exactly that shape — three self-pinned
// stages and three pod-pinned ones in one lane.
//
// That lane is unreachable BY CONSTRUCTION, and the two rules that make it so
// are each correct:
//
//   - engineselection.go's selectEngineForEntry routes a lane to the engine
//     only when its pin set is non-empty and NO pin is Self, because a
//     Temporal worker cannot execute a stage the placement solve says must run
//     on the daemon's own host;
//   - placementrefusal.go's checkpoint 3 then solves the remaining
//     runner-driven lane against the daemon's SELF-ONLY substrate, where a
//     pod-shaped requirement is unsatisfiable by definition.
//
// So the probe never ran, and every expectation recorded against it was a
// prediction. `Goobernetes-Workflows` PR #13 removed the invalid arms. This
// file is where the assertion goes instead: next to the dispatcher and the
// worker provisioner, at the seam the claim is actually about.
//
// # What makes this a seam test rather than a simulation
//
// Both halves of the carrier already have unit coverage, and each half tests
// the other with a STAND-IN. cmd/goobers' dispatchdelta_test.go carries a
// commit between two pods. internal/workerhost's workspaces_continuity_test.go
// carries one into the mirror from a `podPublish` helper that calls
// workspacedelta.Create directly — not the pod's real adapter, which is the
// thing that resolves the base ref, gates on the declared workspace mode,
// decides "unchanged", and speaks to the blob plane over HTTP.
//
// Nothing composed the two REAL adapters, which is precisely the boundary
// #3803 names. Here they are composed:
//
//   - the pod half is publishWorkspaceDelta / applyWorkspaceDelta from
//     dispatchdelta.go — the code `goobers dispatch-exec` runs in the pod;
//   - the self/worker half is workerhost.WorktreeWorkspaces.Provision and the
//     engine.DeltaPublisher it returns — the code goobers-worker runs;
//   - between them is ONE blobstore.Store, reached by the worker directly and
//     by the pod over HTTP on the real dispatcher.BlobPathPrefix route, which
//     is the production topology (the daemon's blob plane fronts the same
//     store the worker plugs into, internal/httpapi/blobplane.go);
//   - underneath is a real git origin, a real mirror clone and real worktrees.
//
// # What it must fail on
//
// A harness for this claim is worthless unless it fails on the shapes the
// claim is about, so each direction carries an ABLATION (the seam removed,
// the original defect reproduced) and the refusal arms are asserted as
// refusals rather than as passes:
//
//   - a consumer handed no delta must see BASE — that is the #3803 defect,
//     and asserting it is what proves the positive arms are not passing for
//     some other reason (a shared filesystem, a pushed branch);
//   - the producing commits must NEVER be on the origin — an "unpushed diff"
//     is the whole premise (committing locally and pushing in a later stage is
//     the universal idiom), so a harness whose commits reached origin would be
//     measuring git, not the carrier;
//   - a digest the store does not hold, and bytes that do not hash to their
//     digest, must be loud refusals on BOTH substrates, never a base checkout
//     that reports success.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/blobstore"
	"github.com/goobers/goobers/internal/dispatcher"
	"github.com/goobers/goobers/internal/engine"
	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/internal/testgit"
	"github.com/goobers/goobers/internal/workerhost"
	"github.com/goobers/goobers/internal/workspacedelta"
	"github.com/goobers/goobers/internal/worktree"
	"github.com/goobers/goobers/providers"
	"github.com/goobers/goobers/test/testsupport/continuityevidence"
)

// The run identity every arm below shares. The run branch is DERIVED with
// providers.BranchNameIn — the same call the worker's provisioner and the
// pod's checkout each make — rather than spelled literally, so both
// substrates agree on the branch for the reason production does and not by
// coincidence.
const (
	substrateRunID    = "run-4009"
	substrateWorkflow = "continuity"
	substrateGaggle   = "e2e"
)

var substrateRunBranch = providers.BranchNameIn("", substrateWorkflow, substrateRunID)

// substrateFixture is the two-substrate world: one origin, one blob store
// reached two ways, one worker mirror.
type substrateFixture struct {
	t *testing.T
	// origin is the bare repository both substrates clone from. Nothing in
	// this harness pushes to it; that is an assertion, not an omission.
	origin string
	// store is the fleet-wide content-addressed store. The worker reads it
	// directly; the pod reaches the SAME store over the blob plane.
	store blobstore.Store
	// endpoint is the blob plane's base URL, what a pod's GOOBERS_BLOB_ENDPOINT
	// points at.
	endpoint string
	// worker is the production engine.WorkspaceProvisioner.
	worker *workerhost.WorktreeWorkspaces
	// workerLog captures the worker's apply/publish diagnostics, which are the
	// far-side record of a reconciliation decision.
	workerLog *strings.Builder
}

// newSubstrateFixture builds the world and points the pod adapter's
// environment at it. The environment is what dispatch-exec reads in a real
// pod, so setting it here is how the pod half is driven, not a shortcut
// around it.
func newSubstrateFixture(t *testing.T) *substrateFixture {
	t.Helper()
	origin := initBareOrigin(t)
	store, err := blobstore.NewDir(filepath.Join(t.TempDir(), "blobs"))
	if err != nil {
		t.Fatalf("blob store: %v", err)
	}
	manager, err := worktree.NewManager(filepath.Join(t.TempDir(), "workcopies"))
	if err != nil {
		t.Fatalf("worktree manager: %v", err)
	}
	log := &strings.Builder{}
	fixture := &substrateFixture{
		t:        t,
		origin:   origin,
		store:    store,
		endpoint: blobPlaneOverStore(t, store),
		worker: &workerhost.WorktreeWorkspaces{
			Manager:    manager,
			ScratchDir: filepath.Join(t.TempDir(), "scratch"),
			CloneURL:   func(apiv1.RepoRef) (string, error) { return origin, nil },
			Store:      store,
			Log:        log,
		},
		workerLog: log,
	}
	t.Setenv(dispatcher.EnvBlobEndpoint, fixture.endpoint)
	t.Setenv(dispatcher.EnvPodToken, "pod-token-4009")
	t.Setenv(dispatcher.EnvStageWorkspace, string(apiv1.WorkspaceRepo))
	t.Setenv(executor.BaseBranchEnvVar, "main")
	return fixture
}

// blobPlaneOverStore serves the REAL blob-plane route (dispatcher.BlobPathPrefix,
// digest-addressed GET/PUT) over the store the worker reads directly.
//
// It is the transport, not a second store: production's daemon plane
// (internal/httpapi/blobplane.go) fronts exactly this blobstore.Store, and the
// property under test is that bytes a pod PUTs are bytes the worker can read.
// The plane's pod-principal authentication is deliberately not reproduced —
// that is the credential plane's seam (DS9), asserted where it lives, and
// wiring it here would only obscure which failure a red run means.
func blobPlaneOverStore(t *testing.T, store blobstore.Store) string {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		digest, ok := strings.CutPrefix(r.URL.Path, dispatcher.BlobPathPrefix)
		if !ok || !blobstore.ValidDigest(digest) {
			http.Error(w, "invalid digest", http.StatusBadRequest)
			return
		}
		switch r.Method {
		case http.MethodPut:
			data, err := io.ReadAll(io.LimitReader(r.Body, 64<<20))
			if err != nil {
				http.Error(w, "read body", http.StatusBadRequest)
				return
			}
			if err := store.Put(r.Context(), digest, data); err != nil {
				http.Error(w, "put", http.StatusInternalServerError)
				return
			}
			w.WriteHeader(http.StatusCreated)
		case http.MethodGet, http.MethodHead:
			data, err := store.Get(r.Context(), digest)
			if errors.Is(err, blobstore.ErrNotFound) {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			if err != nil {
				http.Error(w, "get", http.StatusInternalServerError)
				return
			}
			w.Header().Set("Content-Type", "application/octet-stream")
			if r.Method == http.MethodHead {
				w.WriteHeader(http.StatusOK)
				return
			}
			_, _ = w.Write(data)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	}))
	t.Cleanup(server.Close)
	return server.URL
}

// podCheckout is a fresh stage pod: a clone of BASE ONLY on the run branch,
// with no access to any other pod's filesystem and nothing of the mirror's.
// A pod is disposed after each attempt (D1/D3), so every call is a new one.
func (f *substrateFixture) podCheckout(name string) string {
	f.t.Helper()
	dir := filepath.Join(f.t.TempDir(), "pod-"+name)
	runGitT(f.t, filepath.Dir(dir), "clone", "--quiet", "--branch", "main", f.origin, dir)
	runGitT(f.t, dir, "config", "user.name", "pod-"+name)
	runGitT(f.t, dir, "config", "user.email", "pod-"+name+"@example.invalid")
	runGitT(f.t, dir, "checkout", "--quiet", "-b", substrateRunBranch)
	return dir
}

// commitFile writes and commits one file, returning the new HEAD.
func (f *substrateFixture) commitFile(dir, name, body string) string {
	f.t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
		f.t.Fatalf("write %s: %v", name, err)
	}
	runGitT(f.t, dir, "add", name)
	runGitT(f.t, dir, "-c", "user.name=harness", "-c", "user.email=harness@example.invalid",
		"commit", "--quiet", "-m", "commit "+name)
	return strings.TrimSpace(runGitOutputT(f.t, dir, "rev-parse", "HEAD"))
}

// provision asks the production worker provisioner for a writable repo
// workspace for one stage attempt, threading delta exactly as the engine's
// continuity record does.
func (f *substrateFixture) provision(stage, delta string) engine.Workspace {
	f.t.Helper()
	ws, err := f.worker.Provision(context.Background(), f.request(stage, delta))
	if err != nil {
		f.t.Fatalf("worker Provision(stage=%s, delta=%q): %v", stage, delta, err)
	}
	f.t.Cleanup(func() { _ = ws.Remove(context.Background()) })
	return ws
}

// release tears a workspace down between arms. Production disposes each
// attempt's worktree before the next stage is provisioned, and git refuses a
// second worktree on the same branch, so an arm that provisions twice must
// release in between — the same ordering the engine's activity host keeps.
func (f *substrateFixture) release(ws engine.Workspace) {
	f.t.Helper()
	if err := ws.Remove(context.Background()); err != nil {
		f.t.Fatalf("remove workspace: %v", err)
	}
}

// gitTry runs an isolated git command and returns its output and status
// without failing the test, for probes whose NEGATIVE answer is the expected
// one (an object the origin has never heard of).
func gitTry(t *testing.T, dir string, args ...string) (string, error) {
	t.Helper()
	cmd := testgit.Command(args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

func (f *substrateFixture) request(stage, delta string) engine.WorkspaceRequest {
	// WorkspaceBranch is deliberately left empty: it is the pr-remediation
	// REBIND (#392), which makes the provisioner require and acquire an
	// existing remote branch. The shape under test is the ordinary run
	// branch, which the provisioner derives itself.
	return engine.WorkspaceRequest{
		RunID:          substrateRunID,
		Stage:          stage,
		Gaggle:         substrateGaggle,
		Workflow:       substrateWorkflow,
		RepoRef:        apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web", Branch: "main"},
		Mode:           apiv1.WorkspaceRepo,
		WorkspaceDelta: delta,
	}
}

// assertOriginUnaware is the UNPUSHED-DIFF assertion, and it is what makes
// every positive arm mean something. The carrier exists because the universal
// idiom is to commit in one stage and push in a later one (#3803); if these
// commits were on the origin, both substrates would find them by ordinary
// fetch and the harness would be measuring git rather than the seam. It is
// also the shape #3766 rejected on purpose: pushing the run branch would make
// commits visible before the gates that guard them run.
func (f *substrateFixture) assertOriginUnaware(sha string) {
	f.t.Helper()
	// --git-dir names the bare origin explicitly rather than relying on
	// discovery from the working directory, which git refuses outright under
	// safe.bareRepository=explicit.
	root := filepath.Dir(f.origin)
	if _, err := gitTry(f.t, root, "--git-dir="+f.origin, "cat-file", "-e", sha+"^{commit}"); err != nil {
		// The origin has never seen the object at all, which is the strongest
		// form of the property and the ordinary one here.
		return
	}
	refs, err := gitTry(f.t, root, "--git-dir="+f.origin, "for-each-ref", "--contains="+sha, "--format=%(refname)")
	if err != nil {
		f.t.Fatalf("probe origin refs containing %s: %v: %s", sha, err, refs)
	}
	if refs != "" {
		f.t.Fatalf("commit %s is reachable from origin refs %q; the harness is not testing an unpushed diff", sha, refs)
	}
}

// headOf reports a checkout's or worktree's HEAD commit.
func headOf(t *testing.T, dir string) string {
	t.Helper()
	return strings.TrimSpace(runGitOutputT(t, dir, "rev-parse", "HEAD"))
}

// hasFile reports whether a path exists in a working copy.
func hasFile(t *testing.T, dir, name string) bool {
	t.Helper()
	_, err := os.Stat(filepath.Join(dir, name))
	if err == nil {
		return true
	}
	if os.IsNotExist(err) {
		return false
	}
	t.Fatalf("stat %s: %v", name, err)
	return false
}

// TestCrossSubstrateWorkspaceContinuity is the harness. Every arm shares one
// evidence document so an acceptance reader gets a single artifact naming
// what was proven, in which direction, on which SHAs and digests.
func TestCrossSubstrateWorkspaceContinuity(t *testing.T) {
	evidence := continuityevidence.New(t, "cross-substrate-continuity", "#4009",
		"cmd/goobers dispatch-exec pod adapter (publishWorkspaceDelta/applyWorkspaceDelta)",
		"internal/workerhost.WorktreeWorkspaces (Provision/PublishDelta)",
		"internal/worktree managed mirror + real git worktrees",
		"internal/blobstore.Store fronted by the dispatcher blob-plane route",
	)

	t.Run("pod to self", func(t *testing.T) {
		podToSelf(t, evidence)
	})
	t.Run("self to pod", func(t *testing.T) {
		selfToPod(t, evidence)
	})
	t.Run("ablation: no delta reproduces the #3803 defect", func(t *testing.T) {
		ablateTheSeam(t, evidence)
	})
	t.Run("refusals", func(t *testing.T) {
		refusals(t, evidence)
	})
}

// podToSelf is #3803 direction 1 and the arm the removed
// `observe-on-self-mixed` probe stage was meant to carry: a stage pod commits
// and publishes; the worker provisions the NEXT stage's workspace and must
// hand it the pod's commit — including the diff a reviewer gate reads, which
// is where the defect was worst (a reviewer that reviews base returns a
// verdict on work it never saw).
func podToSelf(t *testing.T, evidence *continuityevidence.Recorder) {
	fixture := newSubstrateFixture(t)
	ctx := context.Background()

	pod := fixture.podCheckout("committer")
	tip := fixture.commitFile(pod, "from-pod.txt", "work done in a pod\n")
	fixture.assertOriginUnaware(tip)

	published, err := publishWorkspaceDelta(ctx, pod, io.Discard)
	if err != nil {
		t.Fatalf("pod publishWorkspaceDelta: %v", err)
	}
	if published.Digest == "" {
		t.Fatal("the pod published nothing for a branch carrying a commit; that is the bug, not the fix")
	}
	if published.Tip != tip {
		t.Fatalf("published tip = %s, want the pod's commit %s", published.Tip, tip)
	}
	// The bytes must be in the store the WORKER reads, not merely accepted by
	// the plane: one store reached two ways is the whole topology claim.
	if _, err := fixture.store.Get(ctx, published.Digest); err != nil {
		t.Fatalf("the pod's bundle is not in the store the worker reads: %v", err)
	}

	ws := fixture.provision("review", published.Digest)
	if got := headOf(t, ws.Path()); got != tip {
		t.Fatalf("worker workspace HEAD = %s, want the pod's commit %s (the stage would run against base)", got, tip)
	}
	if !hasFile(t, ws.Path(), "from-pod.txt") {
		t.Fatal("the pod's committed file did not reach the worker's worktree")
	}

	// The reviewer-diff half of #3803: a gate that cannot see the diff either
	// passes work it never read or rejects work that is not there.
	reader, ok := ws.(engine.DiffReader)
	if !ok {
		t.Fatal("the worker's repo workspace must implement engine.DiffReader; a gate has no other way to see the subject")
	}
	diff, err := reader.Diff(ctx, "main")
	if err != nil {
		t.Fatalf("reviewer diff: %v", err)
	}
	if !bytes.Contains(diff, []byte("from-pod.txt")) || !bytes.Contains(diff, []byte("work done in a pod")) {
		t.Fatalf("the reviewer diff does not contain the pod's hunk:\n%s", diff)
	}

	workerHead := headOf(t, ws.Path())
	fixture.release(ws)

	evidence.Record(continuityevidence.Assertion{
		Claim: "a self/worker-placed stage and an agentic gate provisioned after a pod stage " +
			"observe the pod's unpushed commit, and the reviewer diff carries the pod's hunk",
		Kind:      continuityevidence.KindProof,
		Direction: continuityevidence.DirectionPodToSelf,
		Refs:      []string{"#3803", "#4009", "#3815"},
		Facts: map[string]string{
			"podTip":            tip,
			"bundleDigest":      published.Digest,
			"bundleBase":        published.Base,
			"workerHead":        workerHead,
			"reviewerDiffBytes": fmt.Sprint(len(diff)),
			"originContains":    "no (unpushed)",
			"workerLog":         strings.TrimSpace(fixture.workerLog.String()),
		},
	})
}

// selfToPod is direction 2 and the arm the removed `observe-on-pod-mixed`
// probe stage was meant to carry: a self/worker-placed stage commits into the
// managed mirror and publishes; a fresh pod, which cloned base only and shares
// no filesystem with the worker, must continue from that commit.
func selfToPod(t *testing.T, evidence *continuityevidence.Recorder) {
	fixture := newSubstrateFixture(t)
	ctx := context.Background()

	ws := fixture.provision("implement-on-self", "")
	tip := fixture.commitFile(ws.Path(), "from-self.txt", "work done on the daemon's substrate\n")
	fixture.assertOriginUnaware(tip)

	publisher, ok := ws.(engine.DeltaPublisher)
	if !ok {
		t.Fatal("the worker's repo workspace must implement engine.DeltaPublisher; without it the self arm publishes nothing and every later pod sees base")
	}
	published, err := publisher.PublishDelta(ctx)
	if err != nil {
		t.Fatalf("worker PublishDelta: %v", err)
	}
	if published.Digest == "" {
		t.Fatalf("the self arm published nothing for a stage that committed (unchanged=%v)", published.Unchanged)
	}
	if published.Tip != tip {
		t.Fatalf("published tip = %s, want the self stage's commit %s", published.Tip, tip)
	}
	fixture.release(ws)

	pod := fixture.podCheckout("consumer")
	if hasFile(t, pod, "from-self.txt") {
		t.Fatal("the fresh pod already had the file before applying the delta; the arm proves nothing")
	}
	if err := applyWorkspaceDelta(ctx, pod, published.Digest, nil, io.Discard); err != nil {
		t.Fatalf("pod applyWorkspaceDelta: %v", err)
	}
	if got := headOf(t, pod); got != tip {
		t.Fatalf("pod HEAD = %s, want the self stage's commit %s", got, tip)
	}
	if !hasFile(t, pod, "from-self.txt") {
		t.Fatal("the self stage's committed file did not reach the pod")
	}

	// Round trip: the pod commits on top and hands it back to a later
	// self-placed stage. A one-way carrier would strand the second half of
	// every implement-on-pod / land-on-self lane.
	roundTrip := fixture.commitFile(pod, "from-pod-after-self.txt", "pod continued the self stage's work\n")
	fixture.assertOriginUnaware(roundTrip)
	back, err := publishWorkspaceDelta(ctx, pod, io.Discard)
	if err != nil {
		t.Fatalf("pod publishWorkspaceDelta after round trip: %v", err)
	}
	returned := fixture.provision("land-on-self", back.Digest)
	if got := headOf(t, returned.Path()); got != roundTrip {
		t.Fatalf("round-trip workspace HEAD = %s, want %s", got, roundTrip)
	}
	for _, name := range []string{"from-self.txt", "from-pod-after-self.txt"} {
		if !hasFile(t, returned.Path(), name) {
			t.Fatalf("%s is missing after the round trip; one direction of the carrier drops work", name)
		}
	}
	returnedHead := headOf(t, returned.Path())
	fixture.release(returned)

	evidence.Record(continuityevidence.Assertion{
		Claim: "a stage pod continues from what a self/worker-placed stage committed, " +
			"and a later self-placed stage continues from the pod's commit on top of it",
		Kind:      continuityevidence.KindProof,
		Direction: continuityevidence.DirectionSelfToPod,
		Refs:      []string{"#3803", "#4009", "#3815"},
		Facts: map[string]string{
			"selfTip":             tip,
			"selfBundleDigest":    published.Digest,
			"podHeadAfterApply":   headOf(t, pod),
			"roundTripTip":        roundTrip,
			"roundTripDigest":     back.Digest,
			"roundTripSelfHead":   returnedHead,
			"originContains":      "no (unpushed)",
			"workerLog":           strings.TrimSpace(fixture.workerLog.String()),
			"filesAfterRoundTrip": "from-self.txt, from-pod-after-self.txt",
		},
	})
}

// ablateTheSeam removes the carrier and asserts the ORIGINAL defect
// reproduces on both substrates.
//
// This is the arm that makes the two above mean something. Without it a green
// harness cannot distinguish "the bundle carried the commits" from "the
// receiving substrate had them anyway", and that is exactly the class of
// mistake that let unrunnable probe arms be filed as measurements.
func ablateTheSeam(t *testing.T, evidence *continuityevidence.Recorder) {
	fixture := newSubstrateFixture(t)
	ctx := context.Background()

	// Ablation A — pod committed, engine threads NO digest to the self arm.
	pod := fixture.podCheckout("committer")
	podTip := fixture.commitFile(pod, "from-pod.txt", "work done in a pod\n")
	if _, err := publishWorkspaceDelta(ctx, pod, io.Discard); err != nil {
		t.Fatalf("pod publishWorkspaceDelta: %v", err)
	}
	blind := fixture.provision("review", "")
	if hasFile(t, blind.Path(), "from-pod.txt") {
		t.Fatal("a worker workspace provisioned with no delta saw the pod's file; the harness cannot tell the carrier from a shared filesystem")
	}
	if got := headOf(t, blind.Path()); got == podTip {
		t.Fatal("a worker workspace provisioned with no delta was already at the pod's commit; the ablation proves nothing")
	}
	baseHead := headOf(t, blind.Path())
	fixture.release(blind)

	// Ablation B — self committed, pod never applies the digest.
	selfWS := fixture.provision("implement-on-self", "")
	selfTip := fixture.commitFile(selfWS.Path(), "from-self.txt", "work done on the daemon's substrate\n")
	publisher, ok := selfWS.(engine.DeltaPublisher)
	if !ok {
		t.Fatal("the worker's repo workspace must implement engine.DeltaPublisher")
	}
	if _, err := publisher.PublishDelta(ctx); err != nil {
		t.Fatalf("worker PublishDelta: %v", err)
	}
	fixture.release(selfWS)
	blindPod := fixture.podCheckout("unapplied")
	if hasFile(t, blindPod, "from-self.txt") {
		t.Fatal("a fresh pod saw the self stage's file without applying a delta; the harness cannot tell the carrier from a pushed branch")
	}
	if got := headOf(t, blindPod); got == selfTip {
		t.Fatal("a fresh pod was already at the self stage's commit; the ablation proves nothing")
	}

	evidence.Record(continuityevidence.Assertion{
		Claim: "with the delta not threaded, both substrates provision at BASE and the " +
			"consuming stage would run against work it cannot see — the #3803 defect, reproduced",
		Kind:      continuityevidence.KindAblation,
		Direction: continuityevidence.DirectionPodToSelf,
		Refs:      []string{"#3803", "#4009"},
		Facts: map[string]string{
			"podTip":                 podTip,
			"workerHeadWithoutDelta": baseHead,
			"workerSawPodFile":       "no",
			"selfTip":                selfTip,
			"podHeadWithoutDelta":    headOf(t, blindPod),
			"podSawSelfFile":         "no",
		},
	})
}

// refusals asserts every fail-closed arm the carrier owns. Each of these is a
// case where provisioning at base and reporting success would be a SILENT
// wrong result — a stage that believes it continues from its predecessor and
// does not — so "it errored" is the required behaviour, on both substrates.
func refusals(t *testing.T, evidence *continuityevidence.Recorder) {
	fixture := newSubstrateFixture(t)
	ctx := context.Background()
	const absent = "sha256:0000000000000000000000000000000000000000000000000000000000000000"

	// 1. A digest the shared store does not hold. The producing stage's
	//    bundle never reached the store this consumer reads: the commits are
	//    real and nothing else will carry them.
	if _, err := fixture.worker.Provision(ctx, fixture.request("review", absent)); err == nil ||
		!strings.Contains(err.Error(), "not in the blob store") {
		t.Fatalf("worker Provision with an absent digest = %v, want the missing-blob refusal", err)
	}
	workerMissing := "not in the blob store"
	podMissing := ""
	if err := applyWorkspaceDelta(ctx, fixture.podCheckout("missing"), absent, nil, io.Discard); err == nil {
		t.Fatal("pod applyWorkspaceDelta with an absent digest succeeded; the stage would run against base believing otherwise")
	} else {
		podMissing = err.Error()
	}

	// 2. Bytes that do not hash to their digest. Content addressing is the
	//    integrity check; a substituted delta means running a stage on top of
	//    commits nobody in this run made.
	pod := fixture.podCheckout("committer")
	tip := fixture.commitFile(pod, "from-pod.txt", "work done in a pod\n")
	published, err := publishWorkspaceDelta(ctx, pod, io.Discard)
	if err != nil {
		t.Fatalf("pod publishWorkspaceDelta: %v", err)
	}
	forged := workspacedelta.Digest([]byte("not a bundle"))
	if err := fixture.store.Put(ctx, forged, []byte("not a bundle")); err != nil {
		t.Fatalf("seed forged blob: %v", err)
	}
	// The store is content-addressed, so a substitution can only be staged by
	// serving the wrong bytes for a real digest. Point a second plane at a
	// store that does exactly that and confirm both halves refuse.
	swapped := &swappingStore{Store: fixture.store, swap: map[string][]byte{published.Digest: []byte("not a bundle")}}
	t.Setenv(dispatcher.EnvBlobEndpoint, blobPlaneOverStore(t, swapped))
	if err := applyWorkspaceDelta(ctx, fixture.podCheckout("substituted"), published.Digest, nil, io.Discard); err == nil ||
		!strings.Contains(err.Error(), "digest mismatch") {
		t.Fatalf("pod applyWorkspaceDelta with substituted bytes = %v, want the digest-mismatch refusal", err)
	}
	swappedWorker := &workerhost.WorktreeWorkspaces{
		Manager: fixture.worker.Manager, ScratchDir: fixture.worker.ScratchDir,
		CloneURL: fixture.worker.CloneURL, Store: swapped, Log: io.Discard,
	}
	if _, err := swappedWorker.Provision(ctx, fixture.request("review", published.Digest)); err == nil ||
		!strings.Contains(err.Error(), "digest mismatch") {
		t.Fatalf("worker Provision with substituted bytes = %v, want the digest-mismatch refusal", err)
	}

	// 3. No store at all. A worker with no blob store cannot fetch a delta it
	//    was handed, and must say so rather than provisioning base.
	storeless := &workerhost.WorktreeWorkspaces{
		Manager: fixture.worker.Manager, ScratchDir: fixture.worker.ScratchDir,
		CloneURL: fixture.worker.CloneURL, Log: io.Discard,
	}
	if _, err := storeless.Provision(ctx, fixture.request("review", published.Digest)); err == nil ||
		!strings.Contains(err.Error(), "no blob store") {
		t.Fatalf("worker Provision with a delta and no store = %v, want the no-store refusal", err)
	}
	// And a pod with no blob endpoint refuses to publish commits it cannot carry.
	t.Setenv(dispatcher.EnvBlobEndpoint, "")
	orphan := fixture.podCheckout("orphan")
	orphanTip := fixture.commitFile(orphan, "stranded.txt", "commits with nowhere to go\n")
	if _, err := publishWorkspaceDelta(ctx, orphan, io.Discard); err == nil ||
		!strings.Contains(err.Error(), "cannot reach the next stage") {
		t.Fatalf("pod publishWorkspaceDelta with no blob endpoint = %v, want the stranded-commits refusal", err)
	}

	evidence.Record(continuityevidence.Assertion{
		Claim: "a missing bundle, substituted bytes, and an absent blob plane are loud refusals " +
			"on BOTH substrates — never a base workspace that reports success",
		Kind:      continuityevidence.KindRefusal,
		Direction: continuityevidence.DirectionPodToSelf,
		Refs:      []string{"#3803", "#4009", "#3821"},
		Facts: map[string]string{
			"podTip":                  tip,
			"absentDigestWorker":      workerMissing,
			"absentDigestPod":         firstLine(podMissing),
			"substitutedBytesDigest":  published.Digest,
			"forgedDigest":            forged,
			"strandedCommitNoCarrier": orphanTip,
		},
	})
}

// swappingStore serves chosen digests with the wrong bytes, which is the only
// way to stage a substitution against a content-addressed store.
type swappingStore struct {
	blobstore.Store
	swap map[string][]byte
}

func (s *swappingStore) Get(ctx context.Context, digest string) ([]byte, error) {
	if data, ok := s.swap[digest]; ok {
		return data, nil
	}
	return s.Store.Get(ctx, digest)
}
