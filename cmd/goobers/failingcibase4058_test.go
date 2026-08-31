package main

import (
	"context"
	"strings"
	"testing"

	"github.com/goobers/goobers/providers"
)

// TestFailingCIParkReleasesOnBaseAdvance covers the reader half of #4058: CI is
// evaluated against merge(base, head), not against head alone, so a base
// advance is real new evidence about a failing-ci park — including one the base
// itself caused.
//
// Live on 2026-08-31 the remediation lane held four PRs and #4045, a repo-wide
// CI break in the module-priming step, escalated two of them (#4040, #4044) on
// cause failing-ci with failures that were identical on every PR in the repo
// and had nothing to do with either diff. #4045 was fixed on main and the base
// advanced five times, and both PRs stayed parked forever: pr-remediation
// excludes escalated PRs upstream, so the head could never move, so the "parked
// until this PR's head changes" exit the escalation comment advertises was
// unreachable.
func TestFailingCIParkReleasesOnBaseAdvance(t *testing.T) {
	repo := providers.RepositoryRef{Owner: "your-org", Name: "your-repo"}

	park := func(t *testing.T, causes []remediationCause, outcome remediationEscalationOutcome, liveBase string) bool {
		t.Helper()
		comment, err := remediationStateComment(remediationState{
			Cycles:               3,
			AttemptsByCause:      remediationAttempts{FailingCI: 2},
			Escalated:            true,
			EscalatedReason:      "remediation cause \"failing-ci\" exhausted its budget after 2/2 attempts",
			EscalationOutcome:    outcome,
			EscalatedHeadSHA:     "h1",
			EscalatedBaseSHA:     "base-at-escalation",
			EscalationCauses:     causes,
			EscalationGeneration: 1,
		})
		if err != nil {
			t.Fatalf("remediationStateComment: %v", err)
		}
		server := newFakeGitHubServer(t, repo.Owner, repo.Name)
		server.addIssue(1, "pr parked on failing ci")
		server.addComment(1, comment)
		server.setBranchTip("main", liveBase)
		pr := providers.PullRequestSummary{
			Number: 1, Base: "main", HeadSHA: "h1", BaseSHA: "base-at-escalation",
			Labels: []string{remediationEscalatedLabel},
		}
		blocked, err := escalationStillBlocks(context.Background(), server.newGitHubProvider("token"), repo, pr)
		if err != nil {
			t.Fatalf("escalationStillBlocks: %v", err)
		}
		return blocked
	}

	t.Run("a failing-ci park survives an unchanged base", func(t *testing.T) {
		if !park(t, []remediationCause{remediationCauseFailingCI}, remediationOutcomeBudgetExhausted, "base-at-escalation") {
			t.Fatal("blocked = false, want true — nothing changed, so the park must hold")
		}
	})

	t.Run("a failing-ci park releases once the base advances", func(t *testing.T) {
		if park(t, []remediationCause{remediationCauseFailingCI}, remediationOutcomeBudgetExhausted, "base-after-the-ci-fix-landed") {
			t.Fatal("blocked = true, want false — the base moved, so CI has not been re-decided at the new base")
		}
	})

	t.Run("failing-ci mixed with sibling-overlap releases too", func(t *testing.T) {
		causes := []remediationCause{remediationCauseFailingCI, remediationCauseSiblingOverlap}
		if park(t, causes, remediationOutcomeBudgetExhausted, "base-after-the-ci-fix-landed") {
			t.Fatal("blocked = true, want false — every recorded cause is rebase-curable")
		}
	})

	t.Run("failing-ci mixed with a substantive rejection still holds", func(t *testing.T) {
		causes := []remediationCause{remediationCauseFailingCI, remediationCauseSubstantive}
		if !park(t, causes, remediationOutcomeBudgetExhausted, "base-after-the-ci-fix-landed") {
			t.Fatal("blocked = false, want true — a reviewer rejection of the PR's own content is not rebase-curable")
		}
	})

	t.Run("a forced escalation recording no cause still holds", func(t *testing.T) {
		// #4040's shape: the in-run reviewer returned a terminal fail, so the
		// record carries no escalationCauses at all.
		if !park(t, nil, remediationOutcomeDidNotConverge, "base-after-the-ci-fix-landed") {
			t.Fatal("blocked = false, want true — a forced escalation observed no rebase-curable cause")
		}
	})

	t.Run("an infrastructure outcome still holds", func(t *testing.T) {
		causes := []remediationCause{remediationCauseFailingCI}
		if !park(t, causes, remediationOutcomeInfrastructure, "base-after-the-ci-fix-landed") {
			t.Fatal("blocked = false, want true — the PR was never evaluated on its merits")
		}
	})
}

// TestBaseAdvanceUnparkResetsRemediationBudget covers the writer half of #4058.
// escalationStillBlocks releasing the park is inert on its own: the counters
// that escalated the PR live in the sticky comment payload, not in the label,
// so the re-admitted PR arrives with failing-ci already at 2/2 and the very
// next cycle re-escalates with budget-exhausted before an agent ever runs —
// and LastDiffDigest re-escalates it through the stall check the same way.
//
// This is the dead end #1808 fixed for the operator-clear exit, one exit over.
func TestBaseAdvanceUnparkResetsRemediationBudget(t *testing.T) {
	tests := []struct {
		name       string
		causes     []remediationCause
		baseMoved  bool
		wantReset  bool
		wantParked bool
	}{
		{
			name:      "a released failing-ci park gets a fresh budget",
			causes:    []remediationCause{remediationCauseFailingCI},
			baseMoved: true, wantReset: true,
		},
		{
			name:       "an unchanged base keeps the spent budget",
			causes:     []remediationCause{remediationCauseFailingCI},
			wantParked: true,
		},
		{
			name:      "a park no rebase cures keeps the spent budget",
			causes:    []remediationCause{remediationCauseSubstantive},
			baseMoved: true, wantParked: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			baseSHA, headSHA := initRemediationCheckpointRepo(t, "goobers/impl/remediation-364")
			liveBase := baseSHA
			if tt.baseMoved {
				liveBase = "0000000000000000000000000000000000004058"
			}
			priorComment, err := remediationStateComment(remediationState{
				Cycles:               3,
				AttemptsByCause:      remediationAttempts{FailingCI: 2, SiblingOverlap: 2},
				LastDiffDigest:       "sha256:the-digest-that-escalated-it",
				Escalated:            true,
				EscalatedReason:      "remediation cause \"failing-ci\" exhausted its budget after 2/2 attempts",
				EscalationOutcome:    remediationOutcomeBudgetExhausted,
				EscalatedHeadSHA:     headSHA,
				EscalatedBaseSHA:     baseSHA,
				EscalationCauses:     tt.causes,
				EscalationGeneration: 1,
			})
			if err != nil {
				t.Fatalf("remediationStateComment: %v", err)
			}
			st := &remediationCheckpointServerState{
				number: 77, headSHA: headSHA, baseSHA: baseSHA, liveBaseSHA: liveBase,
				// Still labelled: the operator-clear reset must not be what
				// fires here.
				labels:   []string{needsRemediationLabel, remediationEscalatedLabel},
				comments: []string{priorComment},
			}
			server := newRemediationCheckpointServer(t, "your-org", "your-repo", st)

			instanceRoot := remediationCheckpointEnv(t, server.URL, false)
			t.Setenv("GOOBERS_INPUT_REMEDIATIONCAUSES", "failing-ci")
			code, stdout, stderr := runArgs(t, "remediation-checkpoint", instanceRoot)
			if code != 0 {
				t.Fatalf("code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
			}

			announced := strings.Contains(stdout, "escalation released by a base advance")
			if announced != tt.wantReset {
				t.Fatalf("stdout announced a base-advance budget reset = %t, want %t (stdout = %q)",
					announced, tt.wantReset, stdout)
			}

			st.mu.Lock()
			defer st.mu.Unlock()
			state, ok := parseRemediationStateComment(st.comments[0])
			if !ok {
				t.Fatalf("sticky comment %q carries no state payload", st.comments[0])
			}
			if state.Escalated != tt.wantParked {
				t.Fatalf("escalated = %t, want %t (comment = %q)", state.Escalated, tt.wantParked, st.comments[0])
			}
			if !tt.wantReset {
				return
			}
			// The reset must produce a real attempt, not an instant re-park:
			// the counter restarts at 1 and the stale stall digest is gone.
			if got := state.AttemptsByCause.forCause(remediationCauseFailingCI); got != 1 {
				t.Fatalf("failing-ci attempts = %d, want 1 — the released PR must get a fresh budget", got)
			}
			if state.Cycles != 1 {
				t.Fatalf("cycles = %d, want 1 — the whole prior record is dropped", state.Cycles)
			}
			if state.LastDiffDigest == "sha256:the-digest-that-escalated-it" {
				t.Fatal("the stale stall digest survived the reset — the next cycle would re-escalate on an unchanged diff")
			}
		})
	}
}
