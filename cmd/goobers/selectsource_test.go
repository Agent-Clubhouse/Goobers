package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/decomposition"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/providers"
)

// selectSourceRunOptions describes one hand-built escalated (or non-escalated,
// for exclusion fixtures) run journal.
type selectSourceRunOptions struct {
	runID          string
	startedAt      time.Time
	claimedIssueID string // empty skips the query-backlog claim stage entirely
	claimProvider  string
	finalPhase     journal.RunPhase
	// events, appended in order between the claim stage and the terminal
	// run.finished event, model the run's own disposition.
	events []journal.Event
}

func buildSelectSourceRun(t *testing.T, root string, opts selectSourceRunOptions) {
	t.Helper()
	run, err := journal.Create(instance.NewLayout(root).RunsDir(), journal.RunIdentity{
		RunID:           opts.runID,
		Workflow:        "implementation",
		WorkflowVersion: 1,
		Gaggle:          "goobers",
		Trigger:         journal.Trigger{Kind: journal.TriggerSchedule},
		StartedAt:       opts.startedAt,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	appendEvent := func(event journal.Event) {
		t.Helper()
		if err := run.Append(event); err != nil {
			t.Fatal(err)
		}
	}

	if opts.claimedIssueID != "" {
		appendEvent(journal.Event{Type: journal.EventStageStarted, Stage: "query-backlog", Attempt: 1})
		appendEvent(journal.Event{
			Type:    journal.EventStageFinished,
			Stage:   "query-backlog",
			Attempt: 1,
			Status:  string(apiv1.ResultSuccess),
			Outputs: map[string]any{"id": opts.claimedIssueID, "provider": opts.claimProvider, "title": "a claimed issue"},
		})
	}

	for _, event := range opts.events {
		appendEvent(event)
	}

	appendEvent(journal.Event{Type: journal.EventRunFinished, Status: string(opts.finalPhase)})
	if err := run.Close(); err != nil {
		t.Fatal(err)
	}
}

// nonRetryableEscalationEvents is the exact shape internal/runner produces for
// a #415 non-retryable disposition (run_test.go's
// TestRunnerEscalatesNonRetryableFailureDisposition): the implement stage
// finishes with status:failure, the recognized code, and no mediating gate —
// the runner bypasses the Next gate entirely and finishes straight to
// PhaseEscalated.
func nonRetryableEscalationEvents(code, message string) []journal.Event {
	return []journal.Event{
		{Type: journal.EventStageStarted, Stage: "implement", Attempt: 1},
		{
			Type:    journal.EventStageFinished,
			Stage:   "implement",
			Attempt: 1,
			Status:  string(apiv1.ResultFailure),
			Error:   &journal.ErrorDetail{Code: code, Message: message},
		},
	}
}

func decompositionInstanceEnv(t *testing.T, root string) {
	t.Helper()
	t.Setenv("GOOBERS_RUN_ID", "decomposition-run-1")
	t.Setenv("GOOBERS_WORKFLOW", "decomposition")
	t.Setenv("GOOBERS_INPUT_TRUSTLABEL", providers.LabelApproved)
}

func TestSelectSourceClaimsEligibleEscalation(t *testing.T) {
	const trustLabel = "acme:maintainer-approved"
	root := t.TempDir()
	buildSelectSourceRun(t, root, selectSourceRunOptions{
		runID:          "escalated-1",
		startedAt:      time.Date(2026, time.July, 1, 0, 0, 0, 0, time.UTC),
		claimedIssueID: "501",
		claimProvider:  "github",
		finalPhase:     journal.PhaseEscalated,
		events:         nonRetryableEscalationEvents("ISSUE_OVER_SCOPE", "too large to implement as one PR"),
	})

	server := newFakeGitHubServer(t, "acme", "widgets")
	server.addIssue(501, "A very large issue", trustLabel)
	providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_ISSUES_WRITE", "decomposition-run-1")
	decompositionInstanceEnv(t, root)
	t.Setenv("GOOBERS_INPUT_TRUSTLABEL", trustLabel)

	workDir := t.TempDir()
	t.Chdir(workDir)

	code, stdout, stderr := runArgs(t, "select-source", root)
	if code != 0 {
		t.Fatalf("select-source: code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}

	data, err := os.ReadFile(filepath.Join(workDir, "selection.json"))
	if err != nil {
		t.Fatalf("read selection.json: %v", err)
	}
	var got decomposition.Selection
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal selection.json: %v", err)
	}
	if got.Mode != decomposition.SelectionModeEscalation ||
		got.SourceRunID != "escalated-1" ||
		got.SourceStage != "implement" ||
		got.ErrorCode != "ISSUE_OVER_SCOPE" ||
		got.Parent.ID != "501" ||
		got.Parent.Provider != "github" ||
		got.Parent.Repository != "acme/widgets" ||
		got.IssueSnapshotDigest == "" {
		t.Fatalf("selection = %+v", got)
	}

	ledger, err := localscheduler.OpenClaimLedger(filepath.Join(root, "scheduler", "claims.json"))
	if err != nil {
		t.Fatalf("open claim ledger: %v", err)
	}
	// Same-project/same-backlog topology: the backlog identity still resolves
	// (to this very repository), so the parent claim is backlog-scoped exactly
	// like backlog-query's own claim on the same item would be. That shared key
	// is what makes the two mutually exclusive.
	identity := apiv1.BacklogIdentity{
		Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "widgets",
	}
	entry, ok := ledger.LookupScoped(backlogClaimKey(identity, "", "501"))
	if !ok || entry.RunID != "decomposition-run-1" {
		t.Fatalf("ledger entry for 501 = %+v, ok=%v, want a backlog-scoped claim held by decomposition-run-1", entry, ok)
	}
	if backlog, scoped := entry.BacklogIdentity(); !scoped || !backlog.Equal(identity) {
		t.Fatalf("claim backlog = %+v (scoped=%v), want %+v", backlog, scoped, identity)
	}
}

func TestSelectSourceFailsClosedWithoutTrustLabel(t *testing.T) {
	root := t.TempDir()
	providerCmdEnv(t, newFakeGitHubServer(t, "acme", "widgets"), "GOOBERS_CRED_GITHUB_ISSUES_WRITE", "decomposition-run-1")
	t.Setenv("GOOBERS_RUN_ID", "decomposition-run-1")
	t.Setenv("GOOBERS_WORKFLOW", "decomposition")
	t.Chdir(t.TempDir())

	code, _, stderr := runArgs(t, "select-source", root)
	if code != 1 || !strings.Contains(stderr, "trustLabel is required") {
		t.Fatalf("select-source: code = %d, stderr = %q, want missing trustLabel error", code, stderr)
	}
}

func TestSelectSourceExcludesResumedThenCompletedRun(t *testing.T) {
	root := t.TempDir()
	run, err := journal.Create(instance.NewLayout(root).RunsDir(), journal.RunIdentity{
		RunID: "resumed-1", Workflow: "implementation", WorkflowVersion: 1, Gaggle: "goobers",
		Trigger:   journal.Trigger{Kind: journal.TriggerSchedule},
		StartedAt: time.Date(2026, time.June, 1, 0, 0, 0, 0, time.UTC),
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	appendEvent := func(event journal.Event) {
		t.Helper()
		if err := run.Append(event); err != nil {
			t.Fatal(err)
		}
	}
	appendEvent(journal.Event{Type: journal.EventStageStarted, Stage: "query-backlog", Attempt: 1})
	appendEvent(journal.Event{
		Type: journal.EventStageFinished, Stage: "query-backlog", Attempt: 1,
		Status: string(apiv1.ResultSuccess), Outputs: map[string]any{"id": "777", "provider": "github"},
	})
	appendEvent(journal.Event{Type: journal.EventStageStarted, Stage: "implement", Attempt: 1})
	appendEvent(journal.Event{
		Type: journal.EventStageFinished, Stage: "implement", Attempt: 1,
		Status: string(apiv1.ResultFailure),
		Error:  &journal.ErrorDetail{Code: "ISSUE_OVER_SCOPE", Message: "too large"},
	})
	appendEvent(journal.Event{Type: journal.EventRunFinished, Status: string(journal.PhaseEscalated)})
	// A later manual resume re-attempts and this time completes — the run's
	// CURRENT phase is Completed, not Escalated, so it must not surface as a
	// decomposition source even though an old ISSUE_OVER_SCOPE event is still
	// in its history.
	appendEvent(journal.Event{Type: journal.EventRunResumed})
	appendEvent(journal.Event{Type: journal.EventStageStarted, Stage: "implement", Attempt: 1})
	appendEvent(journal.Event{
		Type: journal.EventStageFinished, Stage: "implement", Attempt: 1, Status: string(apiv1.ResultSuccess),
	})
	appendEvent(journal.Event{Type: journal.EventRunFinished, Status: string(journal.PhaseCompleted)})
	if err := run.Close(); err != nil {
		t.Fatal(err)
	}

	server := newFakeGitHubServer(t, "acme", "widgets")
	server.addIssue(777, "Resumed and completed", providers.LabelApproved)
	providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_ISSUES_WRITE", "decomposition-run-1")
	decompositionInstanceEnv(t, root)

	workDir := t.TempDir()
	t.Chdir(workDir)
	code, stdout, stderr := runArgs(t, "select-source", root)
	if code != 0 {
		t.Fatalf("select-source: code = %d, stderr = %q", code, stderr)
	}
	assertSelectSourceNoWork(t, stdout, workDir)
}

func TestSelectSourceExcludesGateMediatedEscalation(t *testing.T) {
	// Repass-budget exhaustion and reviewer rejection are the same mechanism
	// in this runner: the review gate returns needs-changes repeatedly until
	// the repass budget is exhausted, and the run escalates via a
	// gate.evaluated event (Selector.Kind "gate"), never a bare stage
	// failure. Neither carries a recognized L6 error code, so both must be
	// excluded the same way.
	root := t.TempDir()
	buildSelectSourceRun(t, root, selectSourceRunOptions{
		runID:          "gate-escalated-1",
		startedAt:      time.Date(2026, time.July, 1, 0, 0, 0, 0, time.UTC),
		claimedIssueID: "888",
		claimProvider:  "github",
		finalPhase:     journal.PhaseEscalated,
		events: []journal.Event{
			{Type: journal.EventStageStarted, Stage: "implement", Attempt: 1},
			{Type: journal.EventStageFinished, Stage: "implement", Attempt: 1, Status: string(apiv1.ResultSuccess)},
			{
				Type: journal.EventGateEvaluated, Gate: "review", Verdict: string(apiv1.VerdictNeedsChanges),
				Runner: map[string]any{"escalated": true, "repassAttempt": 3},
			},
		},
	})

	server := newFakeGitHubServer(t, "acme", "widgets")
	server.addIssue(888, "Repeatedly rejected", providers.LabelApproved)
	providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_ISSUES_WRITE", "decomposition-run-1")
	decompositionInstanceEnv(t, root)

	workDir := t.TempDir()
	t.Chdir(workDir)
	code, stdout, stderr := runArgs(t, "select-source", root)
	if code != 0 {
		t.Fatalf("select-source: code = %d, stderr = %q", code, stderr)
	}
	assertSelectSourceNoWork(t, stdout, workDir)
}

func TestSelectSourceExcludesDependencyBlock(t *testing.T) {
	root := t.TempDir()
	buildSelectSourceRun(t, root, selectSourceRunOptions{
		runID:          "blocked-1",
		startedAt:      time.Date(2026, time.July, 1, 0, 0, 0, 0, time.UTC),
		claimedIssueID: "999",
		claimProvider:  "github",
		finalPhase:     journal.PhaseEscalated,
		events: []journal.Event{
			{Type: journal.EventStageStarted, Stage: "implement", Attempt: 1},
			{
				Type: journal.EventStageFinished, Stage: "implement", Attempt: 1,
				// A dependency block is ResultBlocked, not ResultFailure — the
				// design doc requires "status: failure" specifically, so this
				// must be excluded even though its error code coincidentally
				// matches a recognized one.
				Status: string(apiv1.ResultBlocked),
				Error:  &journal.ErrorDetail{Code: "ISSUE_OVER_SCOPE", Message: "blocked on an open dependency"},
			},
		},
	})

	server := newFakeGitHubServer(t, "acme", "widgets")
	server.addIssue(999, "Blocked on a dependency", providers.LabelApproved)
	providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_ISSUES_WRITE", "decomposition-run-1")
	decompositionInstanceEnv(t, root)

	workDir := t.TempDir()
	t.Chdir(workDir)
	code, stdout, stderr := runArgs(t, "select-source", root)
	if code != 0 {
		t.Fatalf("select-source: code = %d, stderr = %q", code, stderr)
	}
	assertSelectSourceNoWork(t, stdout, workDir)
}

func TestSelectSourceExcludesCITimeout(t *testing.T) {
	t.Run("realistic: retryable timeout never reaches escalated", func(t *testing.T) {
		root := t.TempDir()
		buildSelectSourceRun(t, root, selectSourceRunOptions{
			runID:          "ci-timeout-1",
			startedAt:      time.Date(2026, time.July, 1, 0, 0, 0, 0, time.UTC),
			claimedIssueID: "111",
			claimProvider:  "github",
			// A retryable infra failure that exhausts retries ends PhaseFailed,
			// not PhaseEscalated — it never becomes a decomposition candidate.
			finalPhase: journal.PhaseFailed,
			events: []journal.Event{
				{Type: journal.EventStageStarted, Stage: "ci-poll", Attempt: 1},
				{
					Type: journal.EventStageFinished, Stage: "ci-poll", Attempt: 1,
					Status: string(apiv1.ResultFailure),
					Error:  &journal.ErrorDetail{Code: "ci_timeout", Message: "checks did not complete"},
				},
			},
		})

		server := newFakeGitHubServer(t, "acme", "widgets")
		server.addIssue(111, "CI never finished", providers.LabelApproved)
		providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_ISSUES_WRITE", "decomposition-run-1")
		decompositionInstanceEnv(t, root)

		workDir := t.TempDir()
		t.Chdir(workDir)
		code, stdout, stderr := runArgs(t, "select-source", root)
		if code != 0 {
			t.Fatalf("select-source: code = %d, stderr = %q", code, stderr)
		}
		assertSelectSourceNoWork(t, stdout, workDir)
	})

	t.Run("defensive: unrecognized code even if it somehow reached escalated", func(t *testing.T) {
		root := t.TempDir()
		buildSelectSourceRun(t, root, selectSourceRunOptions{
			runID:          "ci-timeout-2",
			startedAt:      time.Date(2026, time.July, 1, 0, 0, 0, 0, time.UTC),
			claimedIssueID: "112",
			claimProvider:  "github",
			finalPhase:     journal.PhaseEscalated,
			events: []journal.Event{
				{Type: journal.EventStageStarted, Stage: "ci-poll", Attempt: 1},
				{
					Type: journal.EventStageFinished, Stage: "ci-poll", Attempt: 1,
					Status: string(apiv1.ResultFailure),
					Error:  &journal.ErrorDetail{Code: "ci_timeout", Message: "checks did not complete"},
				},
			},
		})

		server := newFakeGitHubServer(t, "acme", "widgets")
		server.addIssue(112, "CI never finished", providers.LabelApproved)
		providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_ISSUES_WRITE", "decomposition-run-1")
		decompositionInstanceEnv(t, root)

		workDir := t.TempDir()
		t.Chdir(workDir)
		code, stdout, stderr := runArgs(t, "select-source", root)
		if code != 0 {
			t.Fatalf("select-source: code = %d, stderr = %q", code, stderr)
		}
		assertSelectSourceNoWork(t, stdout, workDir)
	})
}

func TestSelectSourceOldestEligibleRunOwnsFirstPass(t *testing.T) {
	root := t.TempDir()
	buildSelectSourceRun(t, root, selectSourceRunOptions{
		runID:          "escalated-newer",
		startedAt:      time.Date(2026, time.July, 10, 0, 0, 0, 0, time.UTC),
		claimedIssueID: "222",
		claimProvider:  "github",
		finalPhase:     journal.PhaseEscalated,
		events:         nonRetryableEscalationEvents("NEEDS_DECOMPOSITION", "newer escalation"),
	})
	buildSelectSourceRun(t, root, selectSourceRunOptions{
		runID:          "escalated-older",
		startedAt:      time.Date(2026, time.July, 1, 0, 0, 0, 0, time.UTC),
		claimedIssueID: "222",
		claimProvider:  "github",
		finalPhase:     journal.PhaseEscalated,
		events:         nonRetryableEscalationEvents("NEEDS_DECOMPOSITION", "older escalation"),
	})

	server := newFakeGitHubServer(t, "acme", "widgets")
	server.addIssue(222, "Escalated twice", providers.LabelApproved)
	providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_ISSUES_WRITE", "decomposition-run-1")
	decompositionInstanceEnv(t, root)

	workDir := t.TempDir()
	t.Chdir(workDir)
	code, _, stderr := runArgs(t, "select-source", root)
	if code != 0 {
		t.Fatalf("select-source: code = %d, stderr = %q", code, stderr)
	}
	data, err := os.ReadFile(filepath.Join(workDir, "selection.json"))
	if err != nil {
		t.Fatalf("read selection.json: %v", err)
	}
	var got decomposition.Selection
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal selection.json: %v", err)
	}
	if got.SourceRunID != "escalated-older" {
		t.Fatalf("selection.SourceRunID = %q, want the oldest eligible run %q", got.SourceRunID, "escalated-older")
	}
}

func TestSelectSourceSkipsParentWithExistingBatchMarker(t *testing.T) {
	root := t.TempDir()
	buildSelectSourceRun(t, root, selectSourceRunOptions{
		runID:          "escalated-batched",
		startedAt:      time.Date(2026, time.July, 1, 0, 0, 0, 0, time.UTC),
		claimedIssueID: "333",
		claimProvider:  "github",
		finalPhase:     journal.PhaseEscalated,
		events:         nonRetryableEscalationEvents("ISSUE_OVER_SCOPE", "already decomposed"),
	})

	server := newFakeGitHubServer(t, "acme", "widgets")
	server.addIssue(333, "Already decomposed", providers.LabelApproved)
	server.addComment(333, decomposition.PublishedBatchMarkerPrefix+" parent=333 digest=sha256:deadbeef children=1,2,3")
	providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_ISSUES_WRITE", "decomposition-run-1")
	decompositionInstanceEnv(t, root)

	workDir := t.TempDir()
	t.Chdir(workDir)
	code, stdout, stderr := runArgs(t, "select-source", root)
	if code != 0 {
		t.Fatalf("select-source: code = %d, stderr = %q", code, stderr)
	}
	assertSelectSourceNoWork(t, stdout, workDir)
}

func TestSelectSourceFailsClosedOnIneligibleParent(t *testing.T) {
	for _, tc := range []struct {
		name    string
		issueID int
		setup   func(server *fakeGitHubServer)
	}{
		{
			name:    "closed",
			issueID: 401,
			setup: func(server *fakeGitHubServer) {
				server.addIssue(401, "Closed parent", providers.LabelApproved)
				server.setIssueState(401, "closed")
			},
		},
		{
			name:    "unapproved",
			issueID: 402,
			setup: func(server *fakeGitHubServer) {
				server.addIssue(402, "Never approved")
			},
		},
		{
			name:    "in review",
			issueID: 403,
			setup: func(server *fakeGitHubServer) {
				server.addIssue(403, "Mid-review", providers.LabelApproved, inReviewStatusLabel)
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			buildSelectSourceRun(t, root, selectSourceRunOptions{
				runID:          "escalated-" + tc.name,
				startedAt:      time.Date(2026, time.July, 1, 0, 0, 0, 0, time.UTC),
				claimedIssueID: itoa(tc.issueID),
				claimProvider:  "github",
				finalPhase:     journal.PhaseEscalated,
				events:         nonRetryableEscalationEvents("ISSUE_OVER_SCOPE", "ineligible parent"),
			})

			server := newFakeGitHubServer(t, "acme", "widgets")
			tc.setup(server)
			providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_ISSUES_WRITE", "decomposition-run-1")
			decompositionInstanceEnv(t, root)

			workDir := t.TempDir()
			t.Chdir(workDir)
			code, stdout, stderr := runArgs(t, "select-source", root)
			if code != 0 {
				t.Fatalf("select-source: code = %d, stderr = %q", code, stderr)
			}
			assertSelectSourceNoWork(t, stdout, workDir)
		})
	}
}

// TestSelectSourceClaimPreventsDoubleClaim uses a fresh ledger snapshot for
// every attempt, matching separate select-source processes racing on one file.
func TestSelectSourceClaimPreventsDoubleClaim(t *testing.T) {
	root := t.TempDir()
	schedulerDir := filepath.Join(root, "scheduler")
	instanceLog, _, err := journal.OpenInstanceLog(schedulerDir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = instanceLog.Close() }()
	key := localscheduler.ClaimKey{Gaggle: "goobers", Provider: "github", ExternalID: "555"}

	const attempts = 8
	var wg sync.WaitGroup
	results := make([]bool, attempts)
	for i := 0; i < attempts; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ok, _, err := claimSelectSourceParent(
				schedulerDir,
				instanceLog,
				key,
				"decomposition-run-"+itoa(i),
				"decomposition",
				time.Hour,
			)
			if err != nil {
				t.Errorf("claim run %d: %v", i, err)
				return
			}
			results[i] = ok
		}(i)
	}
	wg.Wait()

	wins := 0
	for _, ok := range results {
		if ok {
			wins++
		}
	}
	if wins != 1 {
		t.Fatalf("wins = %d across %d concurrent claimants, want exactly 1", wins, attempts)
	}
}

func assertSelectSourceNoWork(t *testing.T, stdout, workDir string) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(workDir, "selection.json"))
	if err != nil {
		t.Fatalf("read selection.json: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal selection.json: %v", err)
	}
	if noWork, _ := got["noWork"].(bool); !noWork {
		t.Fatalf("selection.json = %v, want noWork:true; stdout = %q", got, stdout)
	}
}

func itoa(n int) string {
	return strconv.Itoa(n)
}
