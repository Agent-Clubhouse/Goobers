package main

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/baseline"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/providers"
)

func seedBaselineBlocker(t *testing.T, root string, subjects ...string) *baseline.Store {
	t.Helper()
	store, err := baseline.OpenStore(filepath.Join(root, baselineStateFileName))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	command := []string{"make", "ci"}
	signature := "failed test: TestAgentInstructions"
	observation := baseline.Observation{
		Repo:        baseline.RepoKey("acme", "web"),
		BaseSHA:     "abc123def456",
		Command:     baseline.CommandKey(command),
		Signature:   signature,
		Fingerprint: baseline.Fingerprint(command, signature),
		ObservedAt:  time.Date(2026, 8, 30, 7, 0, 0, 0, time.UTC),
	}
	for _, subject := range subjects {
		if _, err := store.Park(observation, baseline.Waiter{
			Subject:  subject,
			RunID:    "run-" + subject,
			BaseSHA:  observation.BaseSHA,
			ParkedAt: observation.ObservedAt,
		}); err != nil {
			t.Fatalf("Park %s: %v", subject, err)
		}
	}
	return store
}

var baselineTestRepo = providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "acme", Name: "web"}

// TestParkedSubjectsAreNotSelected is the budget half of #2971: an item waiting
// on a repair to the target branch must not be claimed again, because the
// attempt cannot succeed and would spend a full agentic cycle re-deriving that.
func TestParkedSubjectsAreNotSelected(t *testing.T) {
	root := t.TempDir()
	seedBaselineBlocker(t, root, "101", "202")
	eligible := []providers.WorkItem{{ID: "101"}, {ID: "303"}, {ID: "202"}}

	filtered, skipped, warnings := filterBaselineParkedItems(instance.Layout{Root: root}, baselineTestRepo, eligible)

	if len(warnings) != 0 {
		t.Fatalf("warnings = %v, want none", warnings)
	}
	if len(filtered) != 1 || filtered[0].ID != "303" {
		t.Fatalf("filtered = %+v, want only the unaffected item", filtered)
	}
	if len(skipped) != 2 || !strings.Contains(skipped[0], "shared baseline failure") {
		t.Fatalf("skipped = %v, want both parked subjects reported with their blocker", skipped)
	}
}

// TestReleasedSubjectsBecomeSelectableAgain pins the self-healing half: once
// the runner releases a waiter (the base advanced, or the baseline went green)
// selection stops skipping it, with no human and no relabelling involved.
func TestReleasedSubjectsBecomeSelectableAgain(t *testing.T) {
	root := t.TempDir()
	store := seedBaselineBlocker(t, root, "101")
	layout := instance.Layout{Root: root}
	eligible := []providers.WorkItem{{ID: "101"}}

	if filtered, _, _ := filterBaselineParkedItems(layout, baselineTestRepo, eligible); len(filtered) != 0 {
		t.Fatalf("filtered = %+v, want the parked subject skipped while the base is still red", filtered)
	}
	if err := store.Release(baseline.RepoKey("acme", "web"), "101"); err != nil {
		t.Fatalf("Release: %v", err)
	}

	filtered, skipped, _ := filterBaselineParkedItems(layout, baselineTestRepo, eligible)
	if len(filtered) != 1 || len(skipped) != 0 {
		t.Fatalf("filtered = %+v, skipped = %v; want the released subject selectable again", filtered, skipped)
	}
}

// TestNoBaselineStoreLeavesSelectionUntouched keeps the guard inert on an
// instance that never enabled base-health detection, and fails open when the
// store cannot be read: hiding work is worse than one wasted attempt.
func TestNoBaselineStoreLeavesSelectionUntouched(t *testing.T) {
	eligible := []providers.WorkItem{{ID: "101"}, {ID: "202"}}

	filtered, skipped, warnings := filterBaselineParkedItems(instance.Layout{Root: t.TempDir()}, baselineTestRepo, eligible)
	if len(filtered) != 2 || len(skipped) != 0 || len(warnings) != 0 {
		t.Fatalf("filtered = %+v, skipped = %v, warnings = %v; want selection untouched with no store", filtered, skipped, warnings)
	}

	root := t.TempDir()
	writeFile(t, root, baselineStateFileName, "{not json")
	filtered, _, warnings = filterBaselineParkedItems(instance.Layout{Root: root}, baselineTestRepo, eligible)
	if len(filtered) != 2 {
		t.Fatalf("filtered = %+v, want every item kept when the store is unreadable", filtered)
	}
	if len(warnings) != 1 {
		t.Fatalf("warnings = %v, want the unreadable store surfaced once", warnings)
	}
}

// TestOtherRepositoriesAreNotSkipped keeps blockers namespaced: one
// repository's red base must never suppress another repository's backlog.
func TestOtherRepositoriesAreNotSkipped(t *testing.T) {
	root := t.TempDir()
	seedBaselineBlocker(t, root, "101")
	other := providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "acme", Name: "api"}

	filtered, _, _ := filterBaselineParkedItems(instance.Layout{Root: root}, other, []providers.WorkItem{{ID: "101"}})
	if len(filtered) != 1 {
		t.Fatalf("filtered = %+v, want another repository's item untouched", filtered)
	}
}

// TestBaselineBlockerStatusShowsWaitingSubjects is the operator-visibility half
// of #2971: several quiet runs parked on one red base has to read as one shared
// failure with a named waiting list, not as work silently vanishing.
func TestBaselineBlockerStatusShowsWaitingSubjects(t *testing.T) {
	root := t.TempDir()
	seedBaselineBlocker(t, root, "101", "202")
	now := time.Date(2026, 8, 30, 8, 0, 0, 0, time.UTC)

	snapshot, err := loadStatusBaselineBlockers(instance.Layout{Root: root})
	if err != nil {
		t.Fatalf("loadStatusBaselineBlockers: %v", err)
	}
	if snapshot.Total != 1 || snapshot.Waiting != 2 {
		t.Fatalf("snapshot = %+v, want one blocker with two waiting subjects", snapshot)
	}
	if got := snapshot.Blockers[0].Command; got != "make ci" {
		t.Fatalf("command = %q, want the readable CI command", got)
	}

	text := baselineBlockerStatusText(snapshot, now)
	for _, want := range []string{"Shared baseline failures", "make ci", "101, 202", "abc123def456"} {
		if !strings.Contains(text, want) {
			t.Fatalf("status text %q, want it to mention %q", text, want)
		}
	}
}

// TestBaselineBlockerStatusIsSilentWithoutBlockers keeps the section out of the
// default status output on a healthy instance.
func TestBaselineBlockerStatusIsSilentWithoutBlockers(t *testing.T) {
	snapshot, err := loadStatusBaselineBlockers(instance.Layout{Root: t.TempDir()})
	if err != nil {
		t.Fatalf("loadStatusBaselineBlockers: %v", err)
	}
	if text := baselineBlockerStatusText(snapshot, time.Now()); text != "" {
		t.Fatalf("status text = %q, want nothing rendered without a shared baseline failure", text)
	}
}
