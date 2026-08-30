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
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/dispatcher"
	"github.com/goobers/goobers/internal/journal"
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

	// Scrub ONCE, here, and hand the recorder a nil scrubber: the bytes staged
	// for the harness below and the bytes the journal plane stores must be
	// the SAME bytes the pointer's digest names, and a second scrub between
	// them would let the two drift.
	registry, scrubber := journal.DefaultScrubber()
	for _, c := range creds {
		registry.Register([]byte(c.Value))
	}
	source := diff
	diff = scrubber.Scrub(diff)

	recorder := podArtifactRecorder{stderr: stderr, dir: runsDir}
	ref, err := recorder.RecordArtifact(reviewerDiffArtifact, diff)
	if err != nil {
		return nil, fmt.Errorf("reviewer diff: record artifact: %w", err)
	}
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
	// Record whether the evidence was transformed on its way to the agent, and
	// the digest of the pre-scrub bytes, so a reviewer finding about a redacted
	// region can be correlated with the authoritative diff of the commits
	// (#3135) instead of read as a defect in the branch.
	pf(stderr, "reviewer diff: recorded %s/%s (%s, %d bytes) from %s...HEAD; redacted=%t source=%s\n",
		stage, reviewerDiffArtifact, ref.Digest, len(diff), baseRef, !bytes.Equal(source, diff), journal.Digest(source))

	artifact := apiv1.ArtifactPointer{
		Path: ref.Path, Digest: ref.Digest, Size: ref.Size,
		MediaType: "text/x-diff", Integrity: ref.Integrity,
	}
	return &apiv1.ContextPointer{Name: stage + ".diff", Integrity: ref.Integrity, Artifact: &artifact}, nil
}
