package main

// dispatchreview.go is the reviewer-evidence half of an agentic gate evaluated
// in a pod (decision 001 ruling 7, the #301 property restored on the pod path).
//
// On the local runner an agentic reviewer gate is handed a RUNNER-PRODUCED
// unified diff of the run branch against its base (internal/runner/run.go
// recordReviewerDiff): the implementer's model cannot correctly report
// digested artifact pointers, and its true deliverable is the committed
// branch, so the runner computes the diff itself, journals it as
// <gate>/reviewer-diff.patch and hands the reviewer a "<gate>.diff" context
// pointer to it. The reviewer judges the real change with the same
// content-addressed integrity as any other evidence.
//
// A review pod owes the same property, and it is the pod — this binary, never
// the model — that produces it: after the checkout has applied the subject's
// workspace delta (dispatchcheckout.go), the diff is computed from the actual
// commits, recorded through the journal plane, staged where the harness's own
// context resolver reads, and appended to the envelope. The far-side evidence
// for decision 001 names exactly this artifact: `review/reviewer-diff.patch`
// containing the commit a pod-side implement stage made.

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/dispatcher"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/runner"
)

// reviewerDiffArtifact is the artifact name the diff is journaled under; the
// journal plane prefixes it with the stage, so it lands as
// <gate>/reviewer-diff.patch — the local runner's exact spelling.
const reviewerDiffArtifact = "reviewer-diff.patch"

// recordPodReviewerDiff computes `git diff <base>...HEAD` in the reviewer's
// checkout, journals it as <gate>/reviewer-diff.patch through the journal
// plane, stages the bytes under runsDir at the pointer's own journal path (so
// the harness's ArtifactPointer.Resolve finds and re-verifies them exactly as
// it does for a materialized upstream artifact), and returns the "<gate>.diff"
// context pointer for the reviewer's envelope.
//
// Returns (nil, nil) when there is nothing to attach — the stage has no repo
// workspace (scratch), or the branch carries no change against base (a
// repo-readonly reviewer detached AT base, or a subject that committed
// nothing) — matching recordReviewerDiff's two nil cases. Any failure to
// PRODUCE the evidence is an error, never a silent absence: the runner fails
// the gate on a diff error for the same reason.
//
// creds are every credential this pod minted, the checkout's included: a
// commit could have captured one, and the patch is journaled, so all of them
// are registered with the scrubber before the bytes leave this function.
func recordPodReviewerDiff(ctx context.Context, workspace, runsDir, stage string, creds []dispatcher.MintedCredential, stderr io.Writer) (*apiv1.ContextPointer, error) {
	mode := apiv1.WorkspaceMode(strings.TrimSpace(os.Getenv(dispatcher.EnvStageWorkspace)))
	if !mode.IsRepoBacked() {
		return nil, nil
	}
	if stage == "" {
		return nil, fmt.Errorf("reviewer diff: this pod carries no %s, so the evidence artifact cannot be named", dispatcher.EnvStage)
	}
	base := stageBaseBranch()
	baseRef, err := resolveBaseRef(workspace, base)
	if err != nil {
		return nil, fmt.Errorf("reviewer diff: %w", err)
	}
	// The same three-dot form worktree.Diff runs for the local runner: the
	// change since the merge-base with base, which is what a reviewer judges.
	cmd := workspaceGitCommand(workspace, "diff", baseRef+"...HEAD")
	cmd.Stderr = stderr
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	diff, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("reviewer diff: git diff %s...HEAD: %w", baseRef, err)
	}
	if len(diff) == 0 {
		pf(stderr, "reviewer diff: %s...HEAD is empty; no diff evidence attached\n", baseRef)
		return nil, nil
	}

	// The raw diff's address, taken BEFORE the scrub below: it is what the
	// redaction annotation correlates the reviewer's evidence back to (#3135).
	rawDigest, rawBytes := journal.Digest(diff), len(diff)

	// Scrub ONCE, here, and hand the recorder a nil scrubber: the bytes staged
	// for the harness below and the bytes the journal plane stores must be
	// the SAME bytes the pointer's digest names, and a second scrub between
	// them would let the two drift.
	registry, scrubber := journal.DefaultScrubber()
	for _, c := range creds {
		registry.Register([]byte(c.Value))
	}
	diff = scrubber.Scrub(diff)

	recorder := podArtifactRecorder{stderr: stderr, dir: runsDir}
	ref, err := recorder.RecordArtifact(reviewerDiffArtifact, diff)
	if err != nil {
		return nil, fmt.Errorf("reviewer diff: record artifact: %w", err)
	}
	recordPodReviewerDiffRedaction(recorder, stage, ref, rawDigest, rawBytes, stderr)
	// Stage the bytes where the harness will look. ref.Path is derived by
	// journal.ArtifactRef from the digest (a fan-out path under artifacts/),
	// never from input, so this join cannot escape runsDir.
	dest := filepath.Join(runsDir, filepath.FromSlash(ref.Path))
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return nil, fmt.Errorf("reviewer diff: stage evidence for the harness: %w", err)
	}
	if err := os.WriteFile(dest, diff, 0o644); err != nil {
		return nil, fmt.Errorf("reviewer diff: stage evidence for the harness: %w", err)
	}
	pf(stderr, "reviewer diff: recorded %s/%s (%s, %d bytes) from %s...HEAD\n", stage, reviewerDiffArtifact, ref.Digest, len(diff), baseRef)

	artifact := apiv1.ArtifactPointer{
		Path: ref.Path, Digest: ref.Digest, Size: ref.Size,
		MediaType: "text/x-diff", Integrity: ref.Integrity,
	}
	return &apiv1.ContextPointer{Name: stage + ".diff", Integrity: ref.Integrity, Artifact: &artifact}, nil
}

// recordPodReviewerDiffRedaction is the pod half of the local runner's
// recordReviewerDiffRedaction (#3135): when the scrub above transformed the
// bytes a review gate is about to read, the RUN journal must say so and carry
// both digests, or a reviewer finding about redacted content cannot be told
// apart from one about the branch's authoritative raw diff.
//
// It goes through the recorder's run-scoped Append — the journal plane's emit
// route, the pod's analogue of the runner's own executionJournal — rather than
// through the instance-scoped stageAnnotator seam, so the annotation lands in
// the SAME log, under the same run and stage, as the one the local runner
// writes. The event itself is built by runner.ReviewerDiffRedactionEvent so
// the two paths cannot drift.
//
// Best effort with a loud stderr line, the same posture the pod's other
// journal emits carry: the evidence is already recorded and staged at this
// point, and a plane round trip that fails must not turn a producible review
// into a failed stage. A no-op when the evidence is byte-identical to the raw
// diff, which keeps the annotation's presence meaningful.
func recordPodReviewerDiffRedaction(recorder podArtifactRecorder, stage string, ref journal.Ref, rawDigest string, rawBytes int, stderr io.Writer) {
	if ref.Digest == rawDigest {
		return
	}
	pf(stderr, "reviewer diff: redacted before review: raw %s (%d bytes) -> evidence %s (%d bytes)\n", rawDigest, rawBytes, ref.Digest, ref.Size)
	if err := recorder.Append(runner.ReviewerDiffRedactionEvent(stage, ref, rawDigest, rawBytes)); err != nil {
		pf(stderr, "reviewer diff: could not journal the redaction annotation: %v\n", err)
	}
}
