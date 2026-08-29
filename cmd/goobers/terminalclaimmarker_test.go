package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/runner"
	"github.com/goobers/goobers/internal/worktree"
	"github.com/goobers/goobers/providers"
)

// fakeClaimMarkerRelease records every provider claim-epoch release a terminal
// run issues, plus whether the ledger still held that run's claims at the
// moment of the call — the ordering invariant releaseTerminalClaimMarkers
// depends on to prove the epoch it ends is still its own.
type fakeClaimMarkerRelease struct {
	ledgerPath string
	runID      string
	requests   []providers.ClaimWorkItemRequest
	heldAtCall []bool
	err        error
}

func (f *fakeClaimMarkerRelease) release(_ context.Context, req providers.ClaimWorkItemRequest) (providers.WorkItem, error) {
	f.requests = append(f.requests, req)
	if f.ledgerPath != "" {
		ledger, err := localscheduler.OpenClaimLedger(f.ledgerPath)
		if err != nil {
			return providers.WorkItem{}, err
		}
		f.heldAtCall = append(f.heldAtCall, len(ledger.ForRunAll(f.runID)) > 0)
	}
	return providers.WorkItem{}, f.err
}

func openTestClaimLedger(t *testing.T, path string) *localscheduler.ClaimLedger {
	t.Helper()
	ledger, err := localscheduler.OpenClaimLedger(path)
	if err != nil {
		t.Fatal(err)
	}
	return ledger
}

// TestTerminalCleanupReleasesClaimMarkerWithLedger is #3347's regression shape:
// a run reaches a terminal phase still holding its backlog claim (the `no-work`
// outcome short-circuits to completed without ever executing issue-close-out,
// the only stage that removed the claim marker), so terminal cleanup itself has
// to retire the provider-visible marker in the same step that releases the
// ledger lease. Before the fix the ledger released here and the label survived
// until the next backlog-curation cycle reconciled it.
func TestTerminalCleanupReleasesClaimMarkerWithLedger(t *testing.T) {
	root := initDeterministicDemo(t)
	l := instance.NewLayout(root)
	log, _, err := journal.OpenInstanceLog(l.SchedulerDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = log.Close() })

	const runID = "no-work-terminal-run"
	newStaleTerminalRun(t, l, runID, "default-implement", journal.PhaseCompleted, "local-ci")
	ledgerPath := filepath.Join(l.SchedulerDir(), claimLedgerFileName)
	ledger := openTestClaimLedger(t, ledgerPath)
	if ok, _, err := ledger.ClaimScoped(localscheduler.ClaimKey{
		Gaggle: "example", Provider: "github", ExternalID: "3347",
	}, runID, "default-implement", time.Hour); err != nil || !ok {
		t.Fatalf("seed backlog claim: ok=%v err=%v", ok, err)
	}
	// A PR claim this run also holds: pr-claim keys the ledger by pr/<number>
	// and never writes a provider claim marker, so releasing one would post a
	// claim-release breadcrumb onto a PR that never carried a claim.
	if ok, _, err := ledger.Claim(pullRequestClaimKey(77), runID, "pr-remediation", time.Hour); err != nil || !ok {
		t.Fatalf("seed PR claim: ok=%v err=%v", ok, err)
	}
	if ok, _, err := ledger.Claim("999", "other-run", "default-implement", time.Hour); err != nil || !ok {
		t.Fatalf("seed other run's claim: ok=%v err=%v", ok, err)
	}

	manager, err := worktree.NewManager(l.WorkcopiesDir())
	if err != nil {
		t.Fatal(err)
	}
	fake := &fakeClaimMarkerRelease{ledgerPath: ledgerPath, runID: runID}
	repo := providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "your-org", Name: "your-repo"}
	if err := finalizeTerminalRunWithClaimMarkers(l, log, manager, runID, repo, fake.release); err != nil {
		t.Fatalf("finalize terminal run: %v", err)
	}

	if len(fake.requests) != 1 {
		t.Fatalf("provider claim releases = %+v, want exactly the backlog item (PR claims carry no marker)", fake.requests)
	}
	req := fake.requests[0]
	if req.ID != "3347" || req.RunID != runID {
		t.Fatalf("release request = %+v, want item 3347 released by %s", req, runID)
	}
	if req.Repository != repo {
		t.Fatalf("release repository = %+v, want %+v", req.Repository, repo)
	}
	if req.LedgerAuthorized {
		t.Fatal("terminal marker release must not claim ledger authorization: the provider's own winner check is what stops it retiring a claim a recovery sweep already handed to a new run")
	}
	if len(fake.heldAtCall) != 1 || !fake.heldAtCall[0] {
		t.Fatalf("ledger held at provider call = %v, want true: the epoch must be ended while this run still owns the claim", fake.heldAtCall)
	}

	reopened := openTestClaimLedger(t, ledgerPath)
	if entries := reopened.ForRunAll(runID); len(entries) != 0 {
		t.Fatalf("terminal run still holds claims: %+v", entries)
	}
	if entry, ok := reopened.Lookup("999"); !ok || entry.RunID != "other-run" {
		t.Fatalf("other run's claim = (%+v, %v), want preserved", entry, ok)
	}
}

// TestTerminalCleanupSkipsClaimMarkerWhenAlreadyReleased covers the ordinary
// implementation run: issue-close-out already released both the marker and the
// ledger lease, so terminal cleanup must not issue a second provider call (and
// must not post a release breadcrumb onto an item a later run may now hold).
func TestTerminalCleanupSkipsClaimMarkerWhenAlreadyReleased(t *testing.T) {
	root := initDeterministicDemo(t)
	l := instance.NewLayout(root)
	const runID = "closed-out-run"
	newStaleTerminalRun(t, l, runID, "default-implement", journal.PhaseCompleted, "local-ci")
	manager, err := worktree.NewManager(l.WorkcopiesDir())
	if err != nil {
		t.Fatal(err)
	}
	fake := &fakeClaimMarkerRelease{}
	repo := providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "your-org", Name: "your-repo"}
	if err := finalizeTerminalRunWithClaimMarkers(l, nil, manager, runID, repo, fake.release); err != nil {
		t.Fatalf("finalize terminal run: %v", err)
	}
	if len(fake.requests) != 0 {
		t.Fatalf("provider claim releases = %+v, want none for a run holding no claims", fake.requests)
	}
}

// TestTerminalCleanupClaimMarkerFailureStillReleasesLedger pins the best-effort
// contract: the ledger is the truth, so a provider hiccup must neither hold the
// lease nor fail the terminal transition — it degrades to exactly the
// pre-fix behavior (curation reconciles the marker later) and says so in the
// instance journal instead of failing silently.
func TestTerminalCleanupClaimMarkerFailureStillReleasesLedger(t *testing.T) {
	root := initDeterministicDemo(t)
	l := instance.NewLayout(root)
	log, _, err := journal.OpenInstanceLog(l.SchedulerDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = log.Close() })

	const runID = "marker-release-fails"
	newStaleTerminalRun(t, l, runID, "default-implement", journal.PhaseCompleted, "local-ci")
	ledgerPath := filepath.Join(l.SchedulerDir(), claimLedgerFileName)
	ledger := openTestClaimLedger(t, ledgerPath)
	if ok, _, err := ledger.Claim("3347", runID, "default-implement", time.Hour); err != nil || !ok {
		t.Fatalf("seed claim: ok=%v err=%v", ok, err)
	}

	manager, err := worktree.NewManager(l.WorkcopiesDir())
	if err != nil {
		t.Fatal(err)
	}
	fake := &fakeClaimMarkerRelease{err: errors.New("provider claim is held by run other-run")}
	repo := providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "your-org", Name: "your-repo"}
	if err := finalizeTerminalRunWithClaimMarkers(l, log, manager, runID, repo, fake.release); err != nil {
		t.Fatalf("provider failure must not fail terminal cleanup: %v", err)
	}

	reopened := openTestClaimLedger(t, ledgerPath)
	if entry, ok := reopened.Lookup("3347"); ok {
		t.Fatalf("ledger claim held after a failed marker release: %+v", entry)
	}
	events, err := journal.ReadInstanceLog(l.SchedulerDir())
	if err != nil {
		t.Fatal(err)
	}
	var recorded int
	for _, event := range events {
		if event.Type == journal.EventError && event.Error != nil &&
			event.Error.Code == claimMarkerReleaseErrorCode && event.RunID == runID {
			recorded++
		}
	}
	if recorded != 1 {
		t.Fatalf("%s events = %d, want 1 (events: %+v)", claimMarkerReleaseErrorCode, recorded, events)
	}
}

func TestBuildTerminalClaimMarkerReleaseScope(t *testing.T) {
	githubRepo := instance.RepoRef{Provider: "github", Owner: "your-org", Name: "your-repo"}
	adoRepo := instance.RepoRef{Provider: "ado", Owner: "org", Project: "proj", Name: "repo"}
	tests := []struct {
		name     string
		cfg      *instance.Config
		project  apiv1.RepoRef
		wantFunc bool
		wantRepo providers.RepositoryRef
	}{
		{
			name:     "repo-less instance has no marker to retire",
			cfg:      &instance.Config{},
			wantFunc: false,
		},
		{
			name:     "github instance targets the configured repo",
			cfg:      &instance.Config{Repos: []instance.RepoRef{githubRepo}},
			wantFunc: true,
			wantRepo: providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "your-org", Name: "your-repo"},
		},
		{
			name:     "gaggle project overrides the first configured repo",
			cfg:      &instance.Config{Repos: []instance.RepoRef{githubRepo}},
			project:  apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "gaggle-org", Name: "gaggle-repo"},
			wantFunc: true,
			wantRepo: providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "gaggle-org", Name: "gaggle-repo"},
		},
		{
			name:     "ado keeps deferring to curation reconciliation",
			cfg:      &instance.Config{Repos: []instance.RepoRef{adoRepo}},
			wantFunc: false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			registrar, _ := journal.DefaultScrubber()
			release, repo, err := buildTerminalClaimMarkerRelease(tc.cfg, tc.project, registrar, nil)
			if err != nil {
				t.Fatalf("build terminal claim-marker release: %v", err)
			}
			if (release != nil) != tc.wantFunc {
				t.Fatalf("release func present = %t, want %t", release != nil, tc.wantFunc)
			}
			if tc.wantFunc && repo != tc.wantRepo {
				t.Fatalf("repo = %+v, want %+v", repo, tc.wantRepo)
			}
		})
	}
}

// noWorkWorkflowYAML terminates through the first-class `no-work` outcome:
// query-work reports noWork through its declared result file, which
// short-circuits the run straight to completed (issue #233) without ever
// reaching close-out — the exact live shape #3347 pinned via GitHub's label
// event log. close-out exits nonzero so a run that did NOT short-circuit fails
// the test loudly instead of quietly passing it.
const noWorkWorkflowYAML = `apiVersion: goobers.dev/v1alpha1
kind: Workflow
dslVersion: "2.0"
metadata:
  name: default-implement
spec:
  gaggle: example
  triggers:
    - type: schedule
      schedule: "@every 24h"
  start: query-work
  tasks:
    - name: query-work
      type: deterministic
      goal: report an empty tick
      run:
        command: ["sh", "-c", "printf '{\"noWork\": true}' > empty-tick.json"]
      inputs:
        resultFile: "empty-tick.json"
      next: close-out
    - name: close-out
      type: deterministic
      goal: must never run after a no-work tick
      run:
        command: ["false"]
`

// TestNoWorkTerminalRunReleasesClaimMarker drives the daemon's real composition
// root (buildSchedulerSetup wires FinalizeTerminal) through a run that ends on
// `no-work` while holding a backlog claim, and asserts the provider claim epoch
// is retired by the run's own terminal cleanup rather than left for the next
// backlog-curation cycle (#3347).
func TestNoWorkTerminalRunReleasesClaimMarker(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fixture workflow uses a POSIX shell to write its declared result file")
	}
	root := initDeterministicDemo(t)
	workflowPath := filepath.Join(root, "config", "gaggles", "example", "workflows", "default-implement.yaml")
	if err := os.WriteFile(workflowPath, []byte(noWorkWorkflowYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GOOBERS_GITHUB_TOKEN", "fixture-token")

	l := instance.NewLayout(root)
	const runID = "no-work-tick"
	ledgerPath := filepath.Join(l.SchedulerDir(), claimLedgerFileName)
	fake := &fakeClaimMarkerRelease{ledgerPath: ledgerPath, runID: runID}
	previous := newTerminalClaimMarkerProvider
	newTerminalClaimMarkerProvider = func(providers.TokenSource) workItemClaimReleaser {
		return claimReleaserFunc(fake.release)
	}
	t.Cleanup(func() { newTerminalClaimMarkerProvider = previous })

	var wg sync.WaitGroup
	setup, err := buildSchedulerSetup(context.Background(), l, &wg)
	if err != nil {
		t.Fatal(err)
	}
	defer setup.Shutdown(context.Background())

	ledger := openTestClaimLedger(t, ledgerPath)
	if ok, _, err := ledger.Claim("3347", runID, "default-implement", time.Hour); err != nil || !ok {
		t.Fatalf("seed claim: ok=%v err=%v", ok, err)
	}

	identity := localscheduler.WorkflowIdentity{Gaggle: "example", Workflow: "default-implement"}
	res, err := setup.Runner.Start(context.Background(), runner.StartInput{
		RunID:   runID,
		Machine: setup.Machines[identity],
		Gaggle:  "example",
		Trigger: journal.Trigger{Kind: journal.TriggerManual},
		RepoRef: setup.RepoRefs[identity],
		Item:    &apiv1.BacklogItem{ID: "3347"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Phase != journal.PhaseCompleted || res.FinalState != "query-work" {
		t.Fatalf("result = (%q, %q), want a no-work short-circuit at query-work", res.Phase, res.FinalState)
	}

	if len(fake.requests) != 1 || fake.requests[0].ID != "3347" || fake.requests[0].RunID != runID {
		t.Fatalf("provider claim releases = %+v, want item 3347 released by the no-work run itself", fake.requests)
	}
	reopened := openTestClaimLedger(t, ledgerPath)
	if entry, ok := reopened.Lookup("3347"); ok {
		t.Fatalf("no-work run's claim leaked: %+v", entry)
	}
}

// claimReleaserFunc adapts a bare func to the workItemClaimReleaser seam.
type claimReleaserFunc func(context.Context, providers.ClaimWorkItemRequest) (providers.WorkItem, error)

func (f claimReleaserFunc) ReleaseWorkItemClaim(ctx context.Context, req providers.ClaimWorkItemRequest) (providers.WorkItem, error) {
	return f(ctx, req)
}
