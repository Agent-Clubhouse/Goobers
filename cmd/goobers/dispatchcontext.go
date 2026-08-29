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
	"os"
	"path/filepath"

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
var errContextBlobPlaneUnavailable = errors.New("this pod has no blob endpoint to fetch upstream context artifacts from")

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
	pointers := podMaterializablePointers(env.ContextPointers)
	if len(pointers) == 0 {
		return nil
	}
	blobs := podBlobClient()
	if blobs == nil {
		return fmt.Errorf("%w: stage %s was handed %d artifact-backed context pointer(s), the first being %q (%s); set %s",
			errContextBlobPlaneUnavailable, env.TaskID, len(pointers), pointers[0].Name, pointers[0].Artifact.Digest, dispatcher.EnvBlobEndpoint)
	}
	// The fetch half of the data plane, unmodified and shared with the worker.
	// A store that is BROKEN (rather than merely incomplete) surfaces from here;
	// a blob that is merely absent does not, which is why the loop below exists.
	if err := workerhost.MaterializeContext(ctx, blobs, runsDir, pointers); err != nil {
		return fmt.Errorf("materialize context for stage %s: %w", env.TaskID, err)
	}
	for i := range pointers {
		ref := pointers[i].Artifact
		dest := filepath.Join(runsDir, filepath.FromSlash(ref.Path))
		if _, err := os.Stat(dest); err != nil {
			return fmt.Errorf("%w: pointer %q (digest %s, journal path %s) for stage %s was not found in %s",
				errContextBlobMissing, pointers[i].Name, ref.Digest, ref.Path, env.TaskID, blobs.Describe())
		}
		// Announced on stderr because the pod's stderr is the only place an
		// operator can see that this stage's inputs actually arrived: the pod
		// is disposed after surrender, and a resolved pointer leaves no journal
		// event of its own.
		pf(stderr, "context: materialized pointer %q (%s, %d bytes) from the blob plane at %s\n",
			pointers[i].Name, ref.Digest, ref.Size, ref.Path)
	}
	return nil
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
func podMaterializablePointers(pointers []apiv1.ContextPointer) []apiv1.ContextPointer {
	out := make([]apiv1.ContextPointer, 0, len(pointers))
	for i := range pointers {
		cp := pointers[i]
		if cp.Artifact == nil || cp.RunID != "" {
			continue
		}
		if cp.Artifact.Path == "" || cp.Artifact.Digest == "" {
			continue
		}
		out = append(out, cp)
	}
	return out
}
