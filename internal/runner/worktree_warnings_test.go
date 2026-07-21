package runner

import (
	"testing"

	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/worktree"
)

// TestWorktreeWarningEvent covers the runner-side surfacing of a provisioned
// worktree's non-fatal warnings (#643): when the worktree carries warnings they
// become a runner.annotation event carrying them verbatim under Runner (the
// conformance-excluded payload); when it carries none — the darwin/linux case —
// nothing is emitted.
func TestWorktreeWarningEvent(t *testing.T) {
	t.Run("warnings become a runner.annotation event", func(t *testing.T) {
		warnings := []string{"1 symlink(s) were checked out as plain files: link.txt"}
		ev, ok := worktreeWarningEvent("implement", &worktree.Worktree{Warnings: warnings})
		if !ok {
			t.Fatal("ok = false, want an event for a worktree with warnings")
		}
		if ev.Type != journal.EventRunnerAnnotation {
			t.Errorf("event type = %q, want %q", ev.Type, journal.EventRunnerAnnotation)
		}
		if ev.Stage != "implement" {
			t.Errorf("event stage = %q, want %q", ev.Stage, "implement")
		}
		if got, _ := ev.Runner["kind"].(string); got != "worktree.warnings" {
			t.Errorf("runner.kind = %q, want %q", got, "worktree.warnings")
		}
		got, _ := ev.Runner["warnings"].([]string)
		if len(got) != 1 || got[0] != warnings[0] {
			t.Errorf("runner.warnings = %v, want %v", ev.Runner["warnings"], warnings)
		}
	})

	t.Run("no warnings emits nothing", func(t *testing.T) {
		if _, ok := worktreeWarningEvent("implement", &worktree.Worktree{}); ok {
			t.Error("ok = true for a worktree with no warnings, want false")
		}
	})

	t.Run("nil worktree emits nothing", func(t *testing.T) {
		if _, ok := worktreeWarningEvent("implement", nil); ok {
			t.Error("ok = true for a nil worktree, want false")
		}
	})
}
