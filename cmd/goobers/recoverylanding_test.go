package main

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/recovery"
	"github.com/goobers/goobers/providers"
)

func TestRecoveryLandingHeadsUsesTerminalOwningJournal(t *testing.T) {
	for _, mode := range []string{"paired", "recovered", "busy", "active", "foreign-repository", "failed-receipt", "intent-only", "split-journals"} {
		t.Run(mode, func(t *testing.T) {
			root := t.TempDir()
			repo := apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "app"}
			key := providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: repo.Owner, Name: repo.Name}.CanonicalKey()
			route, err := recoveryLandingRoute(repo)
			if err != nil {
				t.Fatal(err)
			}
			intent := providers.LandingIntent{ID: "intent", Operation: "merge", RepositoryAPIURL: route, PullID: "42", ExpectedHeadSHA: strings.Repeat("a", 40)}
			confirmation := providers.MergeConfirmation{IntentID: intent.ID, RepositoryAPIURL: route, PullID: "42", MergeSHA: strings.Repeat("b", 40)}
			if mode == "foreign-repository" {
				repo.Name = "other"
			}
			create := func(id string, includeIntent, includeReceipt bool) {
				t.Helper()
				run, err := journal.Create(root, journal.RunIdentity{RunID: id, Workflow: "restore", WorkflowVersion: 1, WorkspaceRepository: &repo}, nil)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = run.Close() })
				appendEvent := func(fields map[string]any, receipt bool) {
					t.Helper()
					event := journal.Event{Type: journal.EventRefTouched, ExternalRef: &journal.ExternalRef{Provider: "github", Kind: "pr", ID: "42"}, Runner: fields}
					if mode == "recovered" {
						event.Type = journal.EventRunnerMutationRecovered
					}
					if mode == "failed-receipt" && receipt {
						event.Type = journal.EventError
						event.Error = &journal.ErrorDetail{Code: "failed", Message: "not confirmed"}
					}
					if err := run.Append(event); err != nil {
						t.Fatal(err)
					}
				}
				if includeIntent {
					appendEvent(providers.MutationRunnerFields("merge-intent", nil, nil, &intent), false)
				}
				if includeReceipt {
					appendEvent(providers.MutationRunnerFields("merge", &confirmation, nil, nil), true)
				}
				if mode != "active" {
					if err := run.Append(journal.Event{Type: journal.EventRunFinished, Status: string(journal.PhaseCompleted)}); err != nil {
						t.Fatal(err)
					}
				}
				if mode != "busy" {
					if err := run.Close(); err != nil {
						t.Fatal(err)
					}
				}
			}
			create("receiving-run", true, mode != "intent-only" && mode != "split-journals")
			if mode == "split-journals" {
				create("different-run", false, true)
			}
			heads, err := recoveryLandingHeads(t.Context(), root, recovery.Record{RunID: "source-run", RepositoryKey: key}, route)
			if err != nil {
				t.Fatal(err)
			}
			want := 0
			if mode == "paired" || mode == "recovered" {
				want = 1
			}
			if len(heads) != want {
				t.Fatalf("heads=%+v, want %d", heads, want)
			}
		})
	}
}

// TestRecoveryLandingHeadsResumesPastScanBudget pins #5943: a runs directory
// larger than the scan budget (any long-lived instance) failed every landing
// scan with "recovery landing run scan exceeds budget", so landing-proof
// retirement never ran again. The scan now reads one budget-sized window per
// call and resumes where it left off, so a receipt beyond the first window is
// found on a later pass instead of never.
func TestRecoveryLandingHeadsResumesPastScanBudget(t *testing.T) {
	root := t.TempDir()
	repo := apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "app"}
	key := providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: repo.Owner, Name: repo.Name}.CanonicalKey()
	route, err := recoveryLandingRoute(repo)
	if err != nil {
		t.Fatal(err)
	}
	finished := func(identity journal.RunIdentity, events ...journal.Event) {
		t.Helper()
		run, err := journal.Create(root, identity, nil)
		if err != nil {
			t.Fatal(err)
		}
		for _, event := range append(events, journal.Event{Type: journal.EventRunFinished, Status: string(journal.PhaseCompleted)}) {
			if err := run.Append(event); err != nil {
				t.Fatal(err)
			}
		}
		if err := run.Close(); err != nil {
			t.Fatal(err)
		}
	}
	// Unrelated history (no workspace repository) fills more than one window.
	const filler = recoveryLandingScanBudget + 40
	for i := range filler {
		finished(journal.RunIdentity{RunID: fmt.Sprintf("a-run-%04d", i), Workflow: "other", WorkflowVersion: 1})
	}
	// The receiving run sorts after every filler run: outside the first window.
	intent := providers.LandingIntent{ID: "intent", Operation: "merge", RepositoryAPIURL: route, PullID: "42", ExpectedHeadSHA: strings.Repeat("a", 40)}
	confirmation := providers.MergeConfirmation{IntentID: intent.ID, RepositoryAPIURL: route, PullID: "42", MergeSHA: strings.Repeat("b", 40)}
	ref := &journal.ExternalRef{Provider: "github", Kind: "pr", ID: "42"}
	finished(journal.RunIdentity{RunID: "z-receiving-run", Workflow: "restore", WorkflowVersion: 1, WorkspaceRepository: &repo},
		journal.Event{Type: journal.EventRefTouched, ExternalRef: ref, Runner: providers.MutationRunnerFields("merge-intent", nil, nil, &intent)},
		journal.Event{Type: journal.EventRefTouched, ExternalRef: ref, Runner: providers.MutationRunnerFields("merge", &confirmation, nil, nil)},
	)
	record := recovery.Record{RunID: "source-run", RepositoryKey: key}

	first, err := recoveryLandingHeads(t.Context(), root, record, route)
	if err != nil {
		t.Fatalf("first pass over %d runs: %v (an over-budget runs directory must not fail the scan)", filler+1, err)
	}
	if len(first) != 0 {
		t.Fatalf("first pass heads=%+v; the receipt lies outside the first window, so the fixture no longer exercises resumption", first)
	}
	second, err := recoveryLandingHeads(t.Context(), root, record, route)
	if err != nil {
		t.Fatal(err)
	}
	if len(second) != 1 {
		t.Fatalf("second pass heads=%+v, want the receipt found once the scan resumes past the first window", second)
	}
}

// TestRecoveryEntryFailuresCollapseSharedCause pins #5943's second half: one
// cause shared by every snapshot is reported once, with how many snapshots it
// affected and which runs, instead of the same line joined once per entry.
func TestRecoveryEntryFailuresCollapseSharedCause(t *testing.T) {
	var failures recoveryEntryFailures
	shared := errors.New("shared cause")
	for i := range 19 {
		failures.add(fmt.Sprintf("run-%02d", i), shared)
	}
	failures.add("run-odd", errors.New("own cause"))
	err := failures.err()
	message := err.Error()
	if got := strings.Count(message, "shared cause"); got != 1 {
		t.Fatalf("shared cause reported %d times, want once:\n%s", got, message)
	}
	if !strings.Contains(message, "failed for 19 snapshot(s) (runs: run-00, ") || !strings.Contains(message, "+11 more") {
		t.Fatalf("collapsed failure lacks its scope:\n%s", message)
	}
	if !strings.Contains(message, "failed for 1 snapshot(s) (runs: run-odd): own cause") {
		t.Fatalf("distinct cause lost:\n%s", message)
	}
	if !errors.Is(err, shared) {
		t.Fatalf("collapsed failure no longer wraps its cause")
	}
	if (&recoveryEntryFailures{}).err() != nil {
		t.Fatal("no failures must report nil")
	}
}
