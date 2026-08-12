package main

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/providers"
)

type fakeWorkItemUpdater func(context.Context, providers.UpdateWorkItemRequest) (providers.WorkItem, error)

func (f fakeWorkItemUpdater) UpdateWorkItem(ctx context.Context, req providers.UpdateWorkItemRequest) (providers.WorkItem, error) {
	return f(ctx, req)
}

func newRunAbortLabelJournal(t *testing.T, prID string) (string, string, *journal.Run) {
	t.Helper()
	const runID = "run-abort-label-run"
	runsDir := t.TempDir()
	jr, err := journal.Create(runsDir, journal.RunIdentity{
		RunID:    runID,
		Workflow: "implementation",
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = jr.Close() })
	if prID != "" {
		if err := jr.Append(journal.Event{
			Type:   journal.EventStageFinished,
			Stage:  "open-pr",
			Status: string(apiv1.ResultSuccess),
		}); err != nil {
			t.Fatal(err)
		}
		if err := jr.Append(journal.Event{
			Type:        journal.EventRefTouched,
			Stage:       "open-pr",
			ExternalRef: &journal.ExternalRef{Provider: "github", Kind: "pr", ID: prID},
			Runner:      map[string]any{"operation": prOpenOperation},
		}); err != nil {
			t.Fatal(err)
		}
	}
	return runsDir, runID, jr
}

func runAbortLabelEvents(t *testing.T, runsDir, runID string) []journal.Event {
	t.Helper()
	rd, err := journal.OpenRead(filepath.Join(runsDir, runID))
	if err != nil {
		t.Fatal(err)
	}
	events, err := rd.Events()
	if err != nil {
		t.Fatal(err)
	}
	var out []journal.Event
	for _, ev := range events {
		if ev.ExternalRef != nil && ev.ExternalRef.Kind == "pr" && ev.Runner["operation"] == runAbortLabelOperation {
			out = append(out, ev)
		}
	}
	return out
}

func TestLabelAbortedRunPRLabelsThePROnlyWhenAborted(t *testing.T) {
	tests := []struct {
		name      string
		phase     journal.RunPhase
		prID      string
		wantCalls int
	}{
		{name: "aborted run with a PR gets labeled", phase: journal.PhaseAborted, prID: "42", wantCalls: 1},
		{name: "completed run is left alone", phase: journal.PhaseCompleted, prID: "42", wantCalls: 0},
		{name: "aborted run without a PR has nothing to label", phase: journal.PhaseAborted, prID: "", wantCalls: 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			runsDir, runID, jr := newRunAbortLabelJournal(t, tc.prID)
			var calls int
			var gotReq providers.UpdateWorkItemRequest
			labelPR := func(_ context.Context, req providers.UpdateWorkItemRequest) (providers.WorkItem, error) {
				calls++
				gotReq = req
				return providers.WorkItem{}, nil
			}
			repo := providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "acme", Name: "app"}
			if err := labelAbortedRunPR(runsDir, runID, tc.phase, jr, repo, labelPR); err != nil {
				t.Fatalf("labelAbortedRunPR: %v", err)
			}
			if calls != tc.wantCalls {
				t.Fatalf("label calls = %d, want %d", calls, tc.wantCalls)
			}
			if tc.wantCalls == 1 {
				if gotReq.ID != tc.prID || len(gotReq.AddLabels) != 1 || gotReq.AddLabels[0] != abortedRunLabel {
					t.Fatalf("request = %+v, want ID=%s AddLabels=[%s]", gotReq, tc.prID, abortedRunLabel)
				}
			}
			events := runAbortLabelEvents(t, runsDir, runID)
			if len(events) != tc.wantCalls {
				t.Fatalf("journaled label events = %d, want %d", len(events), tc.wantCalls)
			}
		})
	}
}

// TestLabelAbortedRunPROnlyOwnsPRsItOpened pins the scope of "the PR this run
// opened". A merge-review run journals a kind="pr" ref for the PR it reviews
// with runner.operation=="label" (apply-verdict); that PR belongs to the
// originating implementation run, not to the reviewer. Labeling it on abort
// stamped the permanent, non-self-healing abortedRunLabel on a PR that a
// needs-changes verdict had just sent back for remediation, blocking
// pr-select and merge-pr forever. Only operation=="open" confers ownership.
func TestLabelAbortedRunPROnlyOwnsPRsItOpened(t *testing.T) {
	type ref struct {
		id        string
		operation string
	}
	tests := []struct {
		name      string
		refs      []ref
		wantCalls int
		wantID    string
	}{
		{
			name:      "merge-review run that only labeled a PR owns nothing",
			refs:      []ref{{id: "31", operation: "label"}},
			wantCalls: 0,
		},
		{
			name:      "implementation run that opened the PR owns it",
			refs:      []ref{{id: "31", operation: prOpenOperation}},
			wantCalls: 1,
			wantID:    "31",
		},
		{
			name:      "mixed refs select the opened PR, not the merely-labeled one",
			refs:      []ref{{id: "31", operation: "label"}, {id: "99", operation: prOpenOperation}},
			wantCalls: 1,
			wantID:    "99",
		},
		{
			name:      "other non-open operations confer no ownership either",
			refs:      []ref{{id: "31", operation: "comment"}, {id: "32", operation: "close"}},
			wantCalls: 0,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			runsDir, runID, jr := newRunAbortLabelJournal(t, "")
			for _, r := range tc.refs {
				if err := jr.Append(journal.Event{
					Type:        journal.EventRefTouched,
					ExternalRef: &journal.ExternalRef{Provider: "github", Kind: "pr", ID: r.id},
					Runner:      map[string]any{"operation": r.operation},
				}); err != nil {
					t.Fatal(err)
				}
			}
			var calls int
			var gotReq providers.UpdateWorkItemRequest
			labelPR := func(_ context.Context, req providers.UpdateWorkItemRequest) (providers.WorkItem, error) {
				calls++
				gotReq = req
				return providers.WorkItem{}, nil
			}
			repo := providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "acme", Name: "app"}
			if err := labelAbortedRunPR(runsDir, runID, journal.PhaseAborted, jr, repo, labelPR); err != nil {
				t.Fatalf("labelAbortedRunPR: %v", err)
			}
			if calls != tc.wantCalls {
				t.Fatalf("label calls = %d, want %d", calls, tc.wantCalls)
			}
			if tc.wantCalls == 1 && gotReq.ID != tc.wantID {
				t.Fatalf("labeled PR ID = %q, want %q", gotReq.ID, tc.wantID)
			}
			// The label event is the durable cross-run block; it must
			// not be appended for a PR the run does not own.
			if events := runAbortLabelEvents(t, runsDir, runID); len(events) != tc.wantCalls {
				t.Fatalf("journaled %s events = %d, want %d", runAbortLabelOperation, len(events), tc.wantCalls)
			}
		})
	}
}

func TestLabelAbortedRunPRIsIdempotent(t *testing.T) {
	runsDir, runID, jr := newRunAbortLabelJournal(t, "42")
	var calls int
	labelPR := func(context.Context, providers.UpdateWorkItemRequest) (providers.WorkItem, error) {
		calls++
		return providers.WorkItem{}, nil
	}
	repo := providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "acme", Name: "app"}
	for range 2 {
		if err := labelAbortedRunPR(runsDir, runID, journal.PhaseAborted, jr, repo, labelPR); err != nil {
			t.Fatal(err)
		}
	}
	if calls != 1 {
		t.Fatalf("label calls = %d, want 1", calls)
	}
	if events := runAbortLabelEvents(t, runsDir, runID); len(events) != 1 {
		t.Fatalf("journaled label events = %d, want 1", len(events))
	}
}

func TestLabelAbortedRunPRJournalsProviderFailure(t *testing.T) {
	runsDir, runID, jr := newRunAbortLabelJournal(t, "42")
	providerErr := errors.New("label denied")
	labelPR := func(context.Context, providers.UpdateWorkItemRequest) (providers.WorkItem, error) {
		return providers.WorkItem{}, providerErr
	}
	repo := providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "acme", Name: "app"}
	err := labelAbortedRunPR(runsDir, runID, journal.PhaseAborted, jr, repo, labelPR)
	if err == nil {
		t.Fatal("expected error to propagate")
	}
	events := runAbortLabelEvents(t, runsDir, runID)
	if len(events) != 1 {
		t.Fatalf("journaled label events = %d, want 1", len(events))
	}
	if events[0].Error == nil || events[0].Error.Code != "run_abort_label_failed" {
		t.Fatalf("event = %+v, want run_abort_label_failed error", events[0])
	}
}

// TestRunAbortLabelsOpenPR is #2238's end-to-end acceptance criterion: `goobers
// run abort` on a run that already opened a PR must stamp abortedRunLabel on
// that PR, alongside the existing branch-cleanup skip (pull-request-opened).
func TestRunAbortLabelsOpenPR(t *testing.T) {
	t.Setenv("GOOBERS_GITHUB_TOKEN", "ghp_abort_label_fixture_dummy_token")
	root := initDeterministicDemo(t)
	l := instance.NewLayout(root)
	const runID = "manually-aborted-run-with-pr"

	jr, err := journal.Create(l.RunsDir(), journal.RunIdentity{
		RunID: runID, Workflow: "implementation", WorkflowVersion: 1, Gaggle: "example",
		Trigger: journal.Trigger{Kind: journal.TriggerManual},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := jr.Append(journal.Event{
		Type: journal.EventRefTouched,
		ExternalRef: &journal.ExternalRef{
			Provider: "github",
			Kind:     "branch",
			ID:       providers.BranchName("implementation", runID),
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := jr.Append(journal.Event{
		Type:   journal.EventStageFinished,
		Stage:  "push-branch",
		Status: string(apiv1.ResultSuccess),
	}); err != nil {
		t.Fatal(err)
	}
	if err := jr.Append(journal.Event{
		Type:   journal.EventStageFinished,
		Stage:  "open-pr",
		Status: string(apiv1.ResultSuccess),
	}); err != nil {
		t.Fatal(err)
	}
	if err := jr.Append(journal.Event{
		Type:        journal.EventRefTouched,
		Stage:       "open-pr",
		ExternalRef: &journal.ExternalRef{Provider: "github", Kind: "pr", ID: "77"},
		Runner:      map[string]any{"operation": prOpenOperation},
	}); err != nil {
		t.Fatal(err)
	}
	if err := jr.Close(); err != nil {
		t.Fatal(err)
	}

	previousLabeler := newRunAbortLabelProvider
	var gotReq providers.UpdateWorkItemRequest
	var calls int
	newRunAbortLabelProvider = func(providers.TokenSource) workItemUpdater {
		return fakeWorkItemUpdater(func(_ context.Context, req providers.UpdateWorkItemRequest) (providers.WorkItem, error) {
			calls++
			gotReq = req
			return providers.WorkItem{}, nil
		})
	}
	t.Cleanup(func() { newRunAbortLabelProvider = previousLabeler })

	code, _, stderr := runArgs(t, "run", "abort", runID, root)
	if code != 0 {
		t.Fatalf("code = %d, stderr = %q", code, stderr)
	}
	if calls != 1 {
		t.Fatalf("label calls = %d, want 1", calls)
	}
	if gotReq.ID != "77" || len(gotReq.AddLabels) != 1 || gotReq.AddLabels[0] != abortedRunLabel {
		t.Fatalf("request = %+v, want ID=77 AddLabels=[%s]", gotReq, abortedRunLabel)
	}

	events := runAbortLabelEvents(t, l.RunsDir(), runID)
	if len(events) != 1 {
		t.Fatalf("journaled label events = %d, want 1: %+v", len(events), events)
	}
}
