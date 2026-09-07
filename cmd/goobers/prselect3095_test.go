package main

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/providers"
)

// TestBlockedOnSiblingSelectionHoldFailsClosedOnMissingRecord is #3095's unit
// pin: the three states selection must distinguish, and the one that changed.
//
// recordedBlockedOnSiblingBlockers deliberately fails OPEN for an absent
// record and still does — it also feeds election scoring and unpark, where
// inventing a blocker would distort a winner or refuse a legitimate unpark.
// Selection is the caller that must fail CLOSED: a PR holding the label with
// no readable record has had the label applied by something, and selecting it
// sends the PR through review and merge ahead of whatever it was parked
// behind.
func TestBlockedOnSiblingSelectionHoldFailsClosedOnMissingRecord(t *testing.T) {
	repo := providers.RepositoryRef{Owner: "your-org", Name: "your-repo"}
	server := newFakeGitHubServer(t, repo.Owner, repo.Name)
	server.addIssue(10, "pr 10")
	server.addIssue(11, "pr 11")
	server.addIssue(9, "blocker still open")
	provider := server.newGitHubProvider("token")

	labelled := providers.PullRequestSummary{
		Number: 10, State: "open", Base: "main",
		Head: "goobers/implementation/run-10", Labels: []string{blockedOnSiblingLabel},
	}
	unlabelled := providers.PullRequestSummary{
		Number: 11, State: "open", Base: "main",
		Head: "goobers/implementation/run-11",
	}

	ctx := context.Background()

	t.Run("no label is never held", func(t *testing.T) {
		held, reason, err := blockedOnSiblingSelectionHold(ctx, provider, repo, unlabelled)
		if err != nil {
			t.Fatalf("blockedOnSiblingSelectionHold: %v", err)
		}
		if held {
			t.Fatalf("held = true (%q), want false: no label means nothing to reconcile", reason)
		}
	})

	t.Run("label with no readable record is held", func(t *testing.T) {
		held, reason, err := blockedOnSiblingSelectionHold(ctx, provider, repo, labelled)
		if err != nil {
			t.Fatalf("blockedOnSiblingSelectionHold: %v", err)
		}
		if !held {
			t.Fatal("held = false, want true: a label with no readable blocker record must exclude, not clear")
		}
		// The reason must name the escape hatch, or an operator cannot tell a
		// permanent exclusion from a transient one.
		if !strings.Contains(reason, blockedOnSiblingLabel) || !strings.Contains(reason, "label is removed") {
			t.Fatalf("reason = %q, want it to name %s and how to clear it", reason, blockedOnSiblingLabel)
		}
	})

	t.Run("label with a live blocker is held and names it", func(t *testing.T) {
		server.addComment(10, blockedOnSiblingCommentFor(t, 9))
		held, reason, err := blockedOnSiblingSelectionHold(ctx, provider, repo, labelled)
		if err != nil {
			t.Fatalf("blockedOnSiblingSelectionHold: %v", err)
		}
		if !held {
			t.Fatal("held = false, want true: blocker #9 is still open")
		}
		if !strings.Contains(reason, "#9") {
			t.Fatalf("reason = %q, want it to name blocker #9", reason)
		}
	})
}

// TestPRSelectExcludesSiblingBlockedPRAheadOfYoungerPeer is #3095's end-to-end
// pin, and the reported shape: an older parked PR was repeatedly selected
// ahead of an independent, green, younger one, and the only reliable
// workaround was closing the parked PR.
//
// PR #10 is older (lower number, which is the ordering selection uses) and
// carries goobers:blocked-on-sibling with no readable blocker record — the
// state that used to fail open and let it win. PR #20 is independent and
// clean. Selection must take #20, and must say why it passed over #10.
func TestPRSelectExcludesSiblingBlockedPRAheadOfYoungerPeer(t *testing.T) {
	root := initDemo(t)
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	server.addOpenPR(10, "goobers/implementation/run-10", "main", "head10", "base10", false,
		[]string{blockedOnSiblingLabel}, nil)
	server.addOpenPR(20, "goobers/implementation/run-20", "main", "head20", "base20", false, nil, nil)
	// The comment endpoints back the label's blocker record; #10 deliberately
	// has the label and no record at all, which is the state under test.
	server.addIssue(10, "pr 10")
	server.addIssue(20, "pr 20")

	providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_PR_WRITE", "merge-review-run")
	t.Setenv("GOOBERS_WORKFLOW", "merge-review")
	workDir := t.TempDir()
	t.Chdir(workDir)
	resultFile := filepath.Join(workDir, "selected-pr.json")
	t.Setenv(executor.InputEnvVar(executor.InputResultFile), resultFile)

	code, stdout, stderr := runArgs(t, "pr-select", root)
	if code != 0 {
		t.Fatalf("pr-select: code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "selected PR #20") {
		t.Fatalf("stdout = %q, want PR #20 selected: the parked older PR must not outrank an independent peer", stdout)
	}
	// Acceptance criterion: diagnostics list excluded candidates and the reason.
	if !strings.Contains(stdout, "excluded PR #10") {
		t.Fatalf("stdout = %q, want an exclusion line naming PR #10 and its reason", stdout)
	}
}
