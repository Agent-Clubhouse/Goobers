package main

// Pod-side context materialization for mode 3 (#3823, half B).
//
// THE PROBLEM. A stage's upstream inputs travel as apiv1.ContextPointer values
// on the invocation envelope: a name, a content address, and a journal-relative
// path. The harness resolves each one by READING A FILE — Executor.
// materializeContext calls ArtifactPointer.Resolve against the journal root its
// contextResolver is rooted at (internal/harness/context.go), and a pointer it
// cannot read is a hard executor error, deliberately: an upstream artifact this
// stage was PROMISED and cannot get is an integrity fault in the run, not a
// business failure.
//
// On a worker that read succeeds because something put the bytes there first —
// either the stage that produced them ran on this node, or workerhost.
// MaterializeContext fetched them from the fleet blob store at the top of the
// stage seam (cmd/goobers/workerwiring.go materialize).
//
// A POD HAD NO SUCH STEP. Its contextResolver is rooted at a fresh MkdirTemp
// created moments earlier in this pod (dispatchagentic.go), which by
// construction contains nothing at all. So every artifact-backed pointer failed
// to resolve and every non-start agentic pod stage failed — production implement
// with contextFrom, a placed reviewer gate reading the subject's artifacts, the
// nomination triage stage. This had not surfaced only because the one agentic
// stage that has run on a pod (impl-real-probe's implement-on-pod) is a START
// stage, whose envelope carries no upstream pointers at all.
//
// THE FIX is the worker's own fetch half, called from the pod over the blob
// plane it already reaches: workerhost.MaterializeContext with a
// dispatcher.BlobClient in place of a local store, filling the very directory
// the contextResolver is rooted at. The pod is a cache-cold node in the same
// distributed data plane the worker is a warm one in; nothing new is invented.
//
// NETWORK:NONE IS NOT A BLOCKER, and no dispatch-time refusal is added for it.
// The blob endpoint is a runner class's OWN DATA PATH, not a network grant:
// every rendered NetworkPolicy carries the blob-endpoint egress row, network:none
// classes INCLUDED (decision 012; internal/netpolrender/netpolrender.go
// classPolicy, "network:none INCLUDED — carries the DNS row and the
// blob-endpoint row"). A network:none pod already fetches its agentic kit and
// its workspace delta over exactly this path, so context is the third payload on
// a route that must work for the first two. A pod is also never told its own
// restrictions (no restriction env is stamped, dispatcher/podspec.go), so an
// in-pod refusal could not be written honestly even if one were wanted.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/dispatcher"
	"github.com/goobers/goobers/internal/workerhost"
)

// errContextBlobMissing is the fail-closed refusal for an upstream artifact the
// blob plane does not hold.
//
// NAMED, and never folded into the generic materialize error, because the two
// mean opposite things about where to look. MaterializeContext fails SOFT on a
// missing blob on purpose — on a worker the local directory may already hold it,
// so absence there is not yet a fault and the executor's own integrity check is
// left to be the thing that refuses. In a pod there is no such second chance:
// the staging directory was created empty seconds ago, so a blob the plane does
// not have is a blob this stage will never see, and the honest report names the
// POINTER and the DIGEST rather than letting the harness report a missing file
// under a content-addressed path no operator can trace back to a stage.
var errContextBlobMissing = errors.New("upstream context artifact is not in the blob plane")

// errContextBlobPlaneUnavailable is the fail-closed refusal for a pod that was
// handed artifact-backed pointers with no blob endpoint to fetch them from.
// Distinct from errContextBlobMissing: the plane being absent is a deployment
// fault, the blob being absent is a run fault, and they are fixed in different
// places.
//
// UNREACHABLE FROM THE ONE PRODUCTION CALLER TODAY, deliberately kept anyway:
// runAgenticStage is the only caller, and it cannot reach materializePodContext
// without an endpoint because fetchAgenticKit refuses first ("no blob endpoint
// is configured for this pod" -> agentic_kit_unavailable). So podBlobClient()
// is never nil at that call site and only the direct-helper test exercises this
// branch. It is the guard for the SECOND caller — a deterministic pod stage
// that starts consuming ContextPointers, or any caller that does not fetch a
// kit first — because the alternative for that caller is MaterializeContext's
// nil-store no-op silently returning success with an empty staging root.
var errContextBlobPlaneUnavailable = errors.New("this pod has no blob endpoint to fetch upstream context artifacts from")

// errContextPointerRefused is the fail-closed refusal for a context pointer
// this pod will not act on at all: a path that would escape the staging root
// (absolute, volume-bound, or ".." traversal), or one whose digest is not a
// real content address.
//
// SEPARATE FROM THE OTHER TWO because it is not a fault in the plane or in the
// run's data — it is a fault in the ENVELOPE, and the envelope reached this pod
// over the network from a producer this process did not author (an upstream
// stage's surrendered ResultEnvelope, decoded by a bare json.Unmarshal in
// dispatcher.ReadSurrenderedResult). Materialization WRITES FILES, so a pointer
// is untrusted input to a write primitive and gets the #120 treatment every
// other declared-path call site in this repo already gets. Refused before the
// fetch, so nothing is on disk to clean up.
var errContextPointerRefused = errors.New("upstream context pointer is not a contained journal path")

// errContextStagingUnreadable is the fail-closed refusal for a staging root
// this pod cannot inspect — a permission fault, an I/O error, a symlink loop.
//
// THE THIRD THING. errContextBlobPlaneUnavailable means the plane is absent and
// an operator fixes the deployment; errContextBlobMissing means the run's data
// is absent and an operator looks in the blob store. A local filesystem fault
// is neither, and reporting it as "not in the blob plane" sends the operator to
// a store that holds the blob perfectly well.
var errContextStagingUnreadable = errors.New("this pod cannot read its own context staging directory")

// materializePodContext fetches every artifact-backed context pointer on env
// into runsDir — the directory the executor's contextResolver is rooted at — so
// the harness's own resolve finds the bytes on the local filesystem exactly as
// it does on a worker.
//
// It fails CLOSED. Every failure mode here means this stage is about to run
// without an input its workflow declared it needs, and an agentic stage that
// runs without half its context does not fail: it produces a confident, wrong
// answer from the context it did get. That is the silent shape #3763 removed
// from the workspace and this removes from the envelope.
func materializePodContext(ctx context.Context, runsDir string, env apiv1.InvocationEnvelope, stderr io.Writer) error {
	// CONTAINMENT FIRST, before anything is fetched or joined: this function's
	// whole job is to write foreign-authored paths into a directory, so the
	// paths are validated before the write primitive ever sees them.
	pointers, err := podMaterializablePointers(env.ContextPointers)
	if err != nil {
		return fmt.Errorf("%w for stage %s", err, env.TaskID)
	}
	if len(pointers) == 0 {
		return nil
	}
	blobs := podBlobClient()
	if blobs == nil {
		return fmt.Errorf("%w: stage %s was handed %d artifact-backed context pointer(s), the first being %q (%s); set %s",
			errContextBlobPlaneUnavailable, env.TaskID, len(pointers), pointers[0].Name, pointers[0].Artifact.Digest, dispatcher.EnvBlobEndpoint)
	}
	// The fetch half of the data plane, shared with the worker. A store that is
	// BROKEN (rather than merely incomplete) surfaces from here; a blob that is
	// merely absent does not, which is why the loop below exists. It applies
	// the same containment refusal a second time, at the join itself — the two
	// checks are deliberate: this one names the pointer and the stage in a pod
	// diagnostic, that one guards the write for every caller including a future
	// one that forgets.
	if err := workerhost.MaterializeContext(ctx, blobs, runsDir, pointers); err != nil {
		return fmt.Errorf("materialize context for stage %s: %w", env.TaskID, err)
	}
	for i := range pointers {
		if err := verifyMaterializedPointer(runsDir, pointers[i], env.TaskID, blobs.Describe()); err != nil {
			return err
		}
		ref := pointers[i].Artifact
		// Announced on stderr because the pod's stderr is the only place an
		// operator can see that this stage's inputs actually arrived: the pod
		// is disposed after surrender, and a resolved pointer leaves no journal
		// event of its own.
		pf(stderr, "context: materialized pointer %q (%s, %d bytes) from the blob plane at %s\n",
			pointers[i].Name, ref.Digest, ref.Size, ref.Path)
	}
	return nil
}

// verifyMaterializedPointer confirms one pointer's bytes actually landed where
// the harness's own resolve will look for them, and reports WHICH KIND of
// failure it was when they did not.
//
// The lookup goes through apiv1.ResolveContainedPath — the same #120 primitive
// ArtifactPointer.Resolve uses — rather than a bare filepath.Join + os.Stat, so
// a staging root that acquired an escaping symlink between the fetch and this
// check is caught here instead of being confirmed as a successful
// materialization.
//
// THE THREE OUTCOMES ARE DISTINCT ON PURPOSE. "Absent" is a run fault an
// operator chases in the blob store; "escapes" is an envelope fault nobody
// chases in a store at all; a permission or I/O fault is a node fault. Folding
// the last two into errContextBlobMissing (which a bare os.Stat does, since it
// cannot tell EACCES from ENOENT) names the blob plane as the place to look for
// a problem the blob plane does not have.
func verifyMaterializedPointer(runsDir string, cp apiv1.ContextPointer, taskID, plane string) error {
	ref := cp.Artifact
	_, err := apiv1.ResolveContainedPath(runsDir, ref.Path)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, fs.ErrNotExist):
		return fmt.Errorf("%w: pointer %q (digest %s, journal path %s) for stage %s was not found in %s",
			errContextBlobMissing, cp.Name, ref.Digest, ref.Path, taskID, plane)
	case errors.Is(err, apiv1.ErrPathEscape), errors.Is(err, apiv1.ErrSymlinkEscape):
		return fmt.Errorf("%w: pointer %q (digest %s, journal path %s) for stage %s: %w",
			errContextPointerRefused, cp.Name, ref.Digest, ref.Path, taskID, err)
	default:
		return fmt.Errorf("%w: pointer %q (journal path %s) for stage %s: %w",
			errContextStagingUnreadable, cp.Name, ref.Path, taskID, err)
	}
}

// podMaterializablePointers selects the pointers this pod can and should fetch.
//
// External pointers carry a locator and no in-journal content, so there is
// nothing to fetch — the harness surfaces the URI directly and never resolves
// them.
//
// CROSS-RUN pointers (RunID set) are deliberately left alone. They resolve
// against a DIFFERENT root (filepath.Join(RunsDir(), RunID)), and reading
// another run's journal is gated on the journal:read capability, which the
// harness checks and this layer does not — fetching the bytes here would move
// that admission decision below the check that guards it. A cross-run pointer in
// a pod therefore still fails, and fails closed, with the harness's own
// ErrJournalReadRequired / resolve diagnostic. Widening that is #103/T3's
// business, not this fix's.
//
// AND IT REFUSES, rather than selects, a pointer whose path is not a contained
// journal path. Selection returning it would hand
// workerhost.MaterializeContext an attacker-influenceable destination for an
// os.WriteFile, and the verification loop that follows would then re-join the
// same escaped path and find the file exactly where it was written — an escape
// that reports success. What is refused here is refused BEFORE the fetch, so no
// byte of it reaches the disk; a skip would be quieter but would leave the
// stage running without an input it declared, which is the silent shape this
// whole file exists to remove.
func podMaterializablePointers(pointers []apiv1.ContextPointer) ([]apiv1.ContextPointer, error) {
	out := make([]apiv1.ContextPointer, 0, len(pointers))
	for i := range pointers {
		cp := pointers[i]
		if cp.Artifact == nil || cp.RunID != "" {
			continue
		}
		if cp.Artifact.Path == "" || cp.Artifact.Digest == "" {
			continue
		}
		// The contract's own structural check: contained relative path, real
		// sha256 digest, known integrity grade. Nothing here touches the disk.
		if err := cp.Artifact.Validate(); err != nil {
			return nil, fmt.Errorf("%w: pointer %q names %q (digest %s): %w",
				errContextPointerRefused, cp.Name, cp.Artifact.Path, cp.Artifact.Digest, err)
		}
		out = append(out, cp)
	}
	return out, nil
}
