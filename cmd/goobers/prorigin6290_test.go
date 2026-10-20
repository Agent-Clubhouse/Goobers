package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/journalclient"
	"github.com/goobers/goobers/internal/worktree"
	"github.com/goobers/goobers/providers"
)

const liveOrigin6290 = "0123456789abcdef0123456789abcdef"

// Drive the real journal, selector, claim ledger, and worktree manager through
// the review -> implement overlap. The origin has no claim or registered
// worktree during review, so neither older mitigation can hide the defect.
func TestRemediationDefersClaimlessOriginThroughReviewReentry(t *testing.T) {
	root := initDemo(t)
	layout := layoutFor(root).ForGaggle("goobers")
	branch := providers.BranchNameIn("goobers", "implementation", liveOrigin6290)
	origin, head, base := initPRBranchOrigin(t, branch)
	at := time.Now().Add(-5 * time.Minute)
	run, err := journal.Create(layout.RunsDir(), journal.RunIdentity{
		RunID: liveOrigin6290, Workflow: "implementation", Gaggle: "goobers",
	}, nil, journal.WithClock(func() time.Time { return at }))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = run.Close() }()
	appendEvent := func(event journal.Event) {
		t.Helper()
		if err := run.Append(event); err != nil {
			t.Fatal(err)
		}
	}
	appendEvent(journal.Event{Type: journal.EventRefTouched, ExternalRef: &journal.ExternalRef{Provider: "github", Kind: "branch", ID: branch}})
	appendEvent(journal.Event{Type: journal.EventGateStarted, Gate: "review"})
	manager, err := worktree.NewManager(layout.WorkcopiesDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(executor.GaggleEnvVar, "goobers")
	t.Setenv(executor.RunIDEnvVar, "remediation-run")
	t.Setenv("GOOBERS_WORKFLOW", "pr-remediation")
	t.Chdir(t.TempDir())
	resultPath := filepath.Join(t.TempDir(), "result.json")
	t.Setenv(executor.InputEnvVar(executor.InputResultFile), resultPath)
	pr := providers.PullRequestSummary{Number: 7, Head: branch, HeadSHA: head, Base: "main", BaseSHA: base, CheckState: providers.CheckStateFailing}
	selection := gatherPRContextCandidateSelection{
		root: root, repo: prClaimTestRepo(), listed: []providers.PullRequestSummary{pr},
		eligible: []providers.PullRequestSummary{pr},
	}
	// Repeated scheduled and pinned selections span minutes of journal time,
	// longer than the incident's 15-second task retry backoff.
	for _, pinned := range []bool{false, true, false} {
		at = at.Add(time.Minute)
		selection.hasPinnedCandidate = pinned
		var stdout, stderr bytes.Buffer
		_, done, code := gatherPRContextRankCandidates(selection, &stdout, &stderr)
		if !done || code != 0 {
			t.Fatalf("selection pinned=%t: done=%t code=%d stdout=%s stderr=%s", pinned, done, code, stdout.String(), stderr.String())
		}
		var result struct {
			NoWork bool   `json:"noWork"`
			Reason string `json:"noWorkReason"`
		}
		body, err := os.ReadFile(resultPath)
		if err != nil || json.Unmarshal(body, &result) != nil || !result.NoWork ||
			!strings.Contains(result.Reason, liveOrigin6290) || !strings.Contains(result.Reason, "workflow implementation") {
			t.Fatalf("deferral omitted owner evidence: %s (%v)", body, err)
		}
		if selected, err := claimPullRequestInOrder(root, prClaimTestRepo(), []providers.PullRequestSummary{pr}, "remediation-run", "pr-remediation", time.Hour); err != nil || selected != nil {
			t.Fatalf("direct admission seized live origin: %+v %v", selected, err)
		}
	}
	appendEvent(journal.Event{Type: journal.EventGateEvaluated, Gate: "review", Verdict: "needs-changes", Target: "implement"})
	wt, err := manager.Create(context.Background(), worktree.CreateOptions{
		RepoURL: origin, RunID: "implement-reentry", OwnerRunID: liveOrigin6290,
		BaseRef: "main", Branch: branch, AcquireRemoteBranch: true,
	})
	if err != nil {
		t.Fatalf("origin could not re-enter implement: %v", err)
	}
	defer func() { _ = wt.Remove(context.Background(), worktree.RemoveOptions{}) }()
	if _, err := manager.Create(context.Background(), worktree.CreateOptions{
		RepoURL: origin, RunID: "conflicting-remediation", OwnerRunID: "remediation-run",
		BaseRef: "main", Branch: branch,
	}); err == nil || !strings.Contains(err.Error(), "owned by another run") {
		t.Fatalf("hard ownership refusal changed: %v", err)
	}
	if available, _, err := stageLiveOriginPullRequests(root, liveOrigin6290, []providers.PullRequestSummary{pr}); err != nil || len(available) != 1 {
		t.Fatalf("origin blocked from own branch: %v %v", available, err)
	}
	if err := wt.Remove(context.Background(), worktree.RemoveOptions{}); err != nil {
		t.Fatal(err)
	}
	at = at.Add(time.Minute)
	appendEvent(journal.Event{Type: journal.EventRunFinished, Status: string(journal.PhaseCompleted)})
	if selected, err := claimPullRequestInOrder(root, prClaimTestRepo(), []providers.PullRequestSummary{pr}, "remediation-run", "pr-remediation", time.Hour); err != nil || selected == nil {
		t.Fatalf("terminal origin did not unblock remediation: %+v %v", selected, err)
	}
	next, err := manager.Create(context.Background(), worktree.CreateOptions{
		RepoURL: origin, RunID: "remediation-stage", OwnerRunID: "remediation-run",
		BaseRef: "main", Branch: branch, AcquireRemoteBranch: true,
	})
	if err != nil {
		t.Fatalf("remediation could not acquire released branch: %v", err)
	}
	_ = next.Remove(context.Background(), worktree.RemoveOptions{})
}

func TestLiveOriginGuardUsesJournalPlaneWithoutClaim(t *testing.T) {
	layout := crossRunTestLayout(t)
	seedCrossRunRun(t, layout, crossRunTestGaggle, "asking-run", journal.PhaseRunning)
	branch := providers.BranchNameIn("goobers", "implementation", liveOrigin6290)
	run, err := journal.Create(layout.ForGaggle(crossRunTestGaggle).RunsDir(), journal.RunIdentity{
		RunID: liveOrigin6290, Workflow: "implementation", Gaggle: crossRunTestGaggle,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := run.Append(journal.Event{Type: journal.EventRefTouched, ExternalRef: &journal.ExternalRef{Provider: "github", Kind: "branch", ID: branch}}); err != nil {
		t.Fatal(err)
	}
	_ = run.Close()
	service := newDaemonRunJournalService(layout, nil)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request journalclient.BranchOwnershipRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		response, err := service.BranchOwnership(r.Context(), request)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		_ = json.NewEncoder(w).Encode(response)
	}))
	defer server.Close()
	reader, err := journalclient.NewHTTP(journalclient.HTTPConfig{BaseURL: server.URL, Token: "test", RunID: "asking-run", Gaggle: crossRunTestGaggle})
	if err != nil {
		t.Fatal(err)
	}
	prs, deferred, err := filterLiveOriginPullRequests(context.Background(), reader, crossRunTestGaggle, "asking-run", []providers.PullRequestSummary{{Number: 7, Head: branch}})
	if err != nil || len(prs) != 0 || len(deferred) != 1 || !strings.Contains(deferred[0], liveOrigin6290) {
		t.Fatalf("plane guard: prs=%v deferred=%v err=%v", prs, deferred, err)
	}
}
