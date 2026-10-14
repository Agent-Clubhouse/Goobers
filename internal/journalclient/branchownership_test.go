package journalclient

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
)

const ownershipBranch = "goobers/implementation/run-owned"

func seedOwnershipRun(t *testing.T, layout instance.Layout, runID, workflow string, events ...journal.Event) string {
	t.Helper()
	runsDir := layout.ForGaggle("web").RunsDir()
	at := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	run, err := journal.Create(runsDir, journal.RunIdentity{RunID: runID, Workflow: workflow, Gaggle: "web"},
		nil, journal.WithClock(func() time.Time { return at }))
	if err != nil {
		t.Fatalf("create %s: %v", runID, err)
	}
	for _, ev := range events {
		if err := run.Append(ev); err != nil {
			t.Fatalf("append %s: %v", ev.Type, err)
		}
	}
	if err := run.Close(); err != nil {
		t.Fatal(err)
	}
	return filepath.Join(runsDir, runID)
}

func branchTouch(branch string) journal.Event {
	return journal.Event{Type: journal.EventRefTouched, ExternalRef: &journal.ExternalRef{Provider: "github", Kind: "branch", ID: branch}}
}

func TestFileCrossRunBranchOwnership(t *testing.T) {
	finished := journal.Event{Type: journal.EventRunFinished, Status: string(journal.PhaseCompleted)}
	tests := []struct {
		name       string
		events     []journal.Event
		workflow   string
		target     string
		branch     string
		wantReason string
		wantPhase  string
		wantTime   bool
		wantDetail string
	}{
		{name: "owned terminal", events: []journal.Event{branchTouch(ownershipBranch), finished}, wantPhase: "completed", wantTime: true},
		{name: "owned ado branch", events: []journal.Event{{Type: journal.EventRefTouched, ExternalRef: &journal.ExternalRef{Provider: "ado", Kind: "branch", ID: ownershipBranch}}, finished}, wantPhase: "completed", wantTime: true},
		{name: "owned active run", events: []journal.Event{branchTouch(ownershipBranch)}, wantPhase: "running"},
		{name: "missing run", target: "run-missing", wantReason: "ambiguous-ownership", wantDetail: "open owning run run-missing"},
		{name: "invalid run id", target: "../escape", wantReason: "ambiguous-ownership", wantDetail: "invalid run id"},
		{name: "workflow mismatch", workflow: "other", events: []journal.Event{branchTouch(ownershipBranch)}, wantReason: "ambiguous-ownership", wantDetail: "does not match owning run identity"},
		{name: "no branch reference", events: []journal.Event{finished}, wantReason: "ambiguous-ownership", wantDetail: "no journaled reference to branch"},
		{name: "other branch reference", events: []journal.Event{branchTouch("goobers/implementation/other"), finished}, wantReason: "ambiguous-ownership", wantDetail: "no journaled reference to branch"},
		{
			name: "terminal phase without terminal event",
			events: []journal.Event{
				branchTouch(ownershipBranch),
				{Type: journal.EventGateStarted, Gate: "review"},
				{Type: journal.EventGateEvaluated, Gate: "review", Target: journal.TargetAbort},
			},
			wantReason: "run-journal-unreadable", wantDetail: "no timestamped terminal event",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			layout := instance.NewLayout(t.TempDir())
			workflow := tc.workflow
			if workflow == "" {
				workflow = "implementation"
			}
			seedOwnershipRun(t, layout, "run-owned", "implementation", tc.events...)
			target := tc.target
			if target == "" {
				target = "run-owned"
			}
			got, err := NewFileCrossRun(layout).BranchOwnership(t.Context(), BranchOwnershipRequest{
				RunID: "asker", Gaggle: "web", TargetRunID: target, Workflow: workflow, Branch: ownershipBranch,
			})
			if err != nil {
				t.Fatal(err)
			}
			if got.Reason != tc.wantReason {
				t.Fatalf("reason = %q (%s), want %q", got.Reason, got.Detail, tc.wantReason)
			}
			if tc.wantReason != "" {
				if got.Owner != nil || !strings.Contains(got.Detail, tc.wantDetail) {
					t.Fatalf("owner=%v detail=%q, want refusal containing %q", got.Owner, got.Detail, tc.wantDetail)
				}
				return
			}
			if got.Owner == nil || got.Owner.Phase != tc.wantPhase || got.Owner.RunID != "run-owned" ||
				got.Owner.Workflow != "implementation" || got.Owner.StartedAt.IsZero() || got.Owner.TerminalAt.IsZero() == tc.wantTime {
				t.Fatalf("owner = %+v, want phase %q terminal=%v", got.Owner, tc.wantPhase, tc.wantTime)
			}
		})
	}
}

func TestFileCrossRunBranchOwnershipUnreadableEvents(t *testing.T) {
	layout := instance.NewLayout(t.TempDir())
	dir := seedOwnershipRun(t, layout, "run-owned", "implementation", branchTouch(ownershipBranch))
	if err := os.WriteFile(filepath.Join(dir, "events.jsonl"), []byte("not json\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := NewFileCrossRun(layout).BranchOwnership(t.Context(), BranchOwnershipRequest{
		Gaggle: "web", TargetRunID: "run-owned", Workflow: "implementation", Branch: ownershipBranch,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Owner != nil || got.Reason != "run-journal-unreadable" || !strings.Contains(got.Detail, "events") {
		t.Fatalf("got %+v, want run-journal-unreadable refusal", got)
	}
}

func TestFileCrossRunBranchOwnershipCancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := NewFileCrossRun(instance.NewLayout(t.TempDir())).BranchOwnership(ctx, BranchOwnershipRequest{}); err == nil {
		t.Fatal("want context error")
	}
}
