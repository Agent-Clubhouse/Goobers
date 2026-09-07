package main

import (
	"context"
	"io"
	"strings"
	"testing"

	"github.com/goobers/goobers/providers"
)

// TestPostMergeReTickQueuesWhenAMergeFreesPullRequests is #2492's regression
// pin.
//
// Clearing the label was never the slow part — the three unpark passes already
// remove goobers:blocked-on-sibling, goobers:merge-escalated and the demotion
// marker as soon as a merge resolves them. Nothing then looked at the pull
// requests they freed, so they waited for the next unrelated trigger. Live on
// 2026-08-05, PR #2474 was unblocked at 15:32Z when its cited sibling merged
// and sat untouched until a human merged it by hand at 01:16Z the next day —
// 9h44m, against a §8 acceptance bar of no starvation over an hour.
func TestPostMergeReTickQueuesWhenAMergeFreesPullRequests(t *testing.T) {
	root := initDemo(t)
	t.Setenv("GOOBERS_RUN_ID", "run-postmerge-2492")
	t.Setenv("GOOBERS_WORKFLOW", "merge-review")
	t.Setenv("GOOBERS_GAGGLE", "acme-web")

	var stdout, stderr strings.Builder
	requestPostMergeReTick(context.Background(), root, 3, &stdout, &stderr)

	schedulerDir := layoutFor(root).SchedulerDir()
	if got := countRequests(t, schedulerDir); got != 1 {
		t.Fatalf("pending trigger requests = %d, want 1: a merge that frees pull requests must "+
			"re-tick instead of waiting for the next unrelated trigger", got)
	}
	if !strings.Contains(stdout.String(), "3 pr(s) became eligible") {
		t.Fatalf("stdout = %q, want it to report how many pull requests the merge freed", stdout.String())
	}
	if stderr.String() != "" {
		t.Fatalf("stderr = %q, want none", stderr.String())
	}
}

// TestPostMergeReTickIsSilentWhenNothingWasFreed keeps the steady state quiet:
// most merges free nobody, and a re-tick per merge regardless would be a
// self-inflicted trigger storm on the lane this change exists to unblock.
func TestPostMergeReTickIsSilentWhenNothingWasFreed(t *testing.T) {
	root := initDemo(t)
	t.Setenv("GOOBERS_RUN_ID", "run-postmerge-2492")
	t.Setenv("GOOBERS_WORKFLOW", "merge-review")
	t.Setenv("GOOBERS_GAGGLE", "acme-web")

	var stdout, stderr strings.Builder
	requestPostMergeReTick(context.Background(), root, 0, &stdout, &stderr)

	if got := countRequests(t, layoutFor(root).SchedulerDir()); got != 0 {
		t.Fatalf("pending trigger requests = %d, want 0 when the merge freed nobody", got)
	}
	if stdout.String() != "" || stderr.String() != "" {
		t.Fatalf("stdout = %q, stderr = %q, want both empty", stdout.String(), stderr.String())
	}
}

// TestPostMergeReTickCollapsesRepeatsFromOneRun proves the re-tick inherits the
// #4326 idempotency key rather than adding a second unbounded producer of
// trigger files. That incident accumulated 1,177 request files over 59 hours
// from one resubmitting caller.
func TestPostMergeReTickCollapsesRepeatsFromOneRun(t *testing.T) {
	root := initDemo(t)
	t.Setenv("GOOBERS_RUN_ID", "run-postmerge-2492")
	t.Setenv("GOOBERS_WORKFLOW", "merge-review")
	t.Setenv("GOOBERS_GAGGLE", "acme-web")

	for i := 0; i < 5; i++ {
		requestPostMergeReTick(context.Background(), root, 1, io.Discard, io.Discard)
	}
	if got := countRequests(t, layoutFor(root).SchedulerDir()); got != 1 {
		t.Fatalf("pending trigger requests = %d after 5 post-merge runs of one source run, want 1", got)
	}
}

// TestPostMergeReTickFailureIsAWarning pins the degradation: the merge has
// already happened and the labels are already correct, so a re-tick that cannot
// be queued costs latency, not a merge. Losing the run context is the reachable
// version of that — a post-merge invoked outside a workflow stage.
func TestPostMergeReTickFailureIsAWarning(t *testing.T) {
	root := initDemo(t)
	t.Setenv("GOOBERS_RUN_ID", "")
	t.Setenv("GOOBERS_WORKFLOW", "")

	var stdout, stderr strings.Builder
	requestPostMergeReTick(context.Background(), root, 2, &stdout, &stderr)

	if !strings.Contains(stderr.String(), "skip post-merge re-tick") {
		t.Fatalf("stderr = %q, want a warning naming the skipped re-tick", stderr.String())
	}
	if got := countRequests(t, layoutFor(root).SchedulerDir()); got != 0 {
		t.Fatalf("pending trigger requests = %d, want 0 when the run context is unavailable", got)
	}
}

// TestPostMergeQueuesReTickAfterUnparkingASibling covers the wiring rather than
// the helper: a real post-merge run whose merge resolves a parked sibling must
// both unpark it and queue the re-tick. Unit-testing requestPostMergeReTick
// alone would pass with the call site deleted.
func TestPostMergeQueuesReTickAfterUnparkingASibling(t *testing.T) {
	root := initDemo(t)
	t.Setenv("GOOBERS_RUN_ID", "run-postmerge-2492")
	t.Setenv("GOOBERS_WORKFLOW", "merge-review")
	t.Setenv("GOOBERS_GAGGLE", "acme-web")

	repo := providers.RepositoryRef{Owner: "your-org", Name: "your-repo"}
	server := newFakeGitHubServer(t, repo.Owner, repo.Name)
	// #700 is the pull request this post-merge run is reporting on: merged, so
	// the blocker it represents no longer blocks.
	server.addIssue(700, "blocker that merged")
	server.addOpenPR(700, "goobers/impl/blocker", "main", "h700", "b700", false, nil, nil)
	server.closeIssue(700)
	// #810 was parked behind it and is freed by this merge.
	server.addIssue(810, "parked behind #700")
	server.addOpenPR(810, "goobers/impl/a", "main", "h810", "b810", false, []string{blockedOnSiblingLabel}, nil)
	server.addComment(810, blockedOnSiblingCommentFor(t, 700))
	provider := server.newGitHubProvider("token")

	var stdout, stderr strings.Builder
	poll := providers.PullRequestPollResult{Number: 700, BaseBranch: "main"}
	errs := performPostMerge(context.Background(), provider, provider, repo, root, "700", poll, &stdout, &stderr)
	if len(errs) != 0 {
		t.Fatalf("performPostMerge errs = %v, want none (stderr = %q)", errs, stderr.String())
	}
	if !strings.Contains(stdout.String(), "unparked 1 blocked-on-sibling pr(s)") {
		t.Fatalf("stdout = %q, want PR #810 unparked by the merge of its blocker", stdout.String())
	}
	if !strings.Contains(stdout.String(), "queued an immediate merge-review re-tick") {
		t.Fatalf("stdout = %q, want the freed pull request re-ticked rather than left for the next trigger", stdout.String())
	}
	if got := countRequests(t, layoutFor(root).SchedulerDir()); got != 1 {
		t.Fatalf("pending trigger requests = %d, want 1", got)
	}
}
