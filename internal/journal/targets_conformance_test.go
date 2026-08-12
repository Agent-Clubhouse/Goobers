package journal_test

import (
	"testing"

	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/workflow"
)

// TestReservedTargetConstantsMatchWorkflow pins journal's locally-declared
// reserved-target constants to the workflow interpreter's canonical ones.
//
// internal/journal is a leaf package the interpreters and runner depend on, so
// it cannot import internal/workflow without inverting the layering; the values
// are therefore duplicated. This test is the guard that makes the duplication
// safe: if the interpreter ever renames a reserved target, phase
// reconstruction would silently stop recognizing terminal gates and aborted
// runs would go back to reporting PhaseRunning forever. That failure mode is
// invisible at the type level, so it needs an explicit assertion.
//
// This lives in package journal_test (external) precisely because importing
// workflow from the internal test package would create the cycle the
// duplication exists to avoid.
func TestReservedTargetConstantsMatchWorkflow(t *testing.T) {
	tests := []struct {
		name    string
		journal string
		want    string
	}{
		{name: "abort", journal: journal.TargetAbort, want: workflow.TargetAbort},
		{name: "escalate", journal: journal.TargetEscalate, want: workflow.TargetEscalate},
		{name: "join", journal: journal.TargetJoin, want: workflow.TargetJoin},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if tc.journal != tc.want {
				t.Fatalf("journal constant = %q, workflow constant = %q", tc.journal, tc.want)
			}
		})
	}
}

// TestJournalTerminalTargetsAgreeWithWorkflowReservedSet asserts the
// terminal/branch split itself, not just the spellings: every target journal
// treats as run-terminal must be one workflow also calls terminal, and
// "@join" must be reserved-but-not-terminal on both sides.
func TestJournalTerminalTargetsAgreeWithWorkflowReservedSet(t *testing.T) {
	for _, target := range []string{journal.TargetAbort, journal.TargetEscalate} {
		if !workflow.IsReservedTarget(target) {
			t.Fatalf("journal treats %q as run-terminal but workflow does not", target)
		}
	}
	if workflow.IsReservedTarget(journal.TargetJoin) {
		t.Fatalf("%q must end a branch, not the run", journal.TargetJoin)
	}
	if !workflow.IsReservedAnyTarget(journal.TargetJoin) {
		t.Fatalf("%q must still be a reserved target", journal.TargetJoin)
	}
}
