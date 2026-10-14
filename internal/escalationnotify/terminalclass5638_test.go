package escalationnotify

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/gate"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/runner"
	"github.com/goobers/goobers/internal/telemetry"
)

// writeTerminalJournal records a run that ended with events after its
// identity, in a real journal under runsDir.
func writeTerminalJournal(t *testing.T, runsDir, runID string, events ...journal.Event) {
	t.Helper()
	jr, err := journal.Create(runsDir, journal.RunIdentity{RunID: runID, Workflow: "implementation", Gaggle: "acme-web"}, nil)
	if err != nil {
		t.Fatalf("journal.Create: %v", err)
	}
	for _, event := range events {
		if err := jr.Append(event); err != nil {
			t.Fatalf("append %s: %v", event.Type, err)
		}
	}
	if err := jr.Close(); err != nil {
		t.Fatalf("close journal: %v", err)
	}
}

// expiredDuringRetry is #5638's second reproduction as the journal records
// it: local-gate judged a local-ci timeout infra and routed back to local-ci,
// and the run expired during that retry. retryClass varies the gate's
// classification.
func expiredDuringRetry(retryClass journal.AttemptClass) []journal.Event {
	return []journal.Event{
		{Type: journal.EventStageStarted, Stage: "local-ci", Attempt: 1},
		{Type: journal.EventStageFinished, Stage: "local-ci", Attempt: 1, Status: "failure", Error: &journal.ErrorDetail{Code: telemetry.ErrCodeTimeout, Message: "local-ci exceeded its wall-clock limit"}},
		{Type: journal.EventGateStarted, Gate: "local-gate"},
		{Type: journal.EventGateEvaluated, Gate: "local-gate", Verdict: "infra", Target: "local-ci"},
		{Type: journal.EventRunnerAnnotation, Stage: "local-ci", Gate: "local-gate", Runner: map[string]any{
			"kind": runner.RetryDecisionKind, runner.RetryFailureClassKey: string(retryClass), "failureCode": telemetry.ErrCodeTimeout, "target": "local-ci",
		}},
		{Type: journal.EventStageStarted, Stage: "local-ci", Attempt: 1},
		{Type: journal.EventError, Error: &journal.ErrorDetail{Code: runner.RunDurationExceededErrorCode, Message: "run exceeded maximum duration"}},
		{Type: journal.EventRunFinished, Status: string(journal.PhaseAborted), TerminalCause: &journal.TerminalCause{
			Schema: journal.TerminalCauseSchema, Phase: journal.PhaseAborted, Classification: journal.TerminalPolicyExhaustion,
			SelectorKind: "condition", Code: runner.RunDurationExceededErrorCode,
		}},
	}
}

// TestAbortedTerminalHonorsGateInfraClassification pins #5638's second
// reproduction at the breaker: an aborted terminal the journal attributes to
// the gate's infrastructure retry posts no genuine/work-failure strike, and
// refunds a remediation charge exactly as an infra failed terminal does.
func TestAbortedTerminalHonorsGateInfraClassification(t *testing.T) {
	p, poster, state := newTestPolicy(t)
	writeTerminalJournal(t, p.RunsDir, "run-infra", expiredDuringRetry(journal.AttemptInfra)...)
	state.items["run-infra"] = []Item{{ItemID: "3271", Repo: webRepo}}

	if err := p.TerminalNotifier(nil)("run-infra", journal.PhaseAborted, "local-ci"); err != nil {
		t.Fatalf("aborted: %v", err)
	}
	if state.streaks["web#3271"] != 0 || len(poster.updates) != 0 {
		t.Fatalf("streaks=%v updates=%+v; an infrastructure terminal must not charge the item", state.streaks, poster.updates)
	}
	if !slices.Equal(state.voided, []string{"run-infra"}) {
		t.Fatalf("voided = %v, want the run's remediation charge refunded", state.voided)
	}
}

// The same expiry after a work-classified retry is still a strike, and the
// item gets the genuine/work-failure comment.
func TestAbortedTerminalWithoutInfraClassificationStillCounts(t *testing.T) {
	p, poster, state := newTestPolicy(t)
	writeTerminalJournal(t, p.RunsDir, "run-work", expiredDuringRetry(journal.AttemptPolicy)...)
	state.items["run-work"] = []Item{{ItemID: "3271", Repo: webRepo}}

	if err := p.TerminalNotifier(nil)("run-work", journal.PhaseAborted, "local-ci"); err != nil {
		t.Fatalf("aborted: %v", err)
	}
	if state.streaks["web#3271"] != 1 || len(state.voided) != 0 {
		t.Fatalf("streaks=%v voided=%v, want one strike and no refund", state.streaks, state.voided)
	}
	if len(poster.updates) == 0 || !strings.Contains(poster.updates[len(poster.updates)-1].Comment, "genuine/work failure") {
		t.Fatalf("updates = %+v, want the failure-streak comment", poster.updates)
	}
}

// An escalation on an exhausted infrastructure repass budget carries its
// classification on the terminal cause itself.
func TestEscalatedInfraBudgetTerminalIsExempt(t *testing.T) {
	p, _, state := newTestPolicy(t)
	writeTerminalJournal(t, p.RunsDir, "run-budget", journal.Event{
		Type: journal.EventRunFinished, Status: string(journal.PhaseEscalated), TerminalCause: &journal.TerminalCause{
			Schema: journal.TerminalCauseSchema, Phase: journal.PhaseEscalated, Classification: journal.TerminalInfrastructureFailure,
			SelectorKind: "gate", Selector: "local-gate", Code: gate.ReasonInfrastructureBudgetExhausted,
		},
	})
	state.items["run-budget"] = []Item{{ItemID: "3271", Repo: webRepo}}
	if err := p.TerminalNotifier(nil)("run-budget", journal.PhaseEscalated, "park-escalated"); err != nil {
		t.Fatalf("escalated: %v", err)
	}
	if state.streaks["web#3271"] != 0 {
		t.Fatalf("streaks = %v, want no strike for an infrastructure-budget escalation", state.streaks)
	}
}

// A journal that exists but cannot be read leaves the terminal charged, as
// before, and reports why it could not be classified.
func TestUnreadableTerminalJournalStillCountsAndReports(t *testing.T) {
	p, _, state := newTestPolicy(t)
	if err := os.MkdirAll(filepath.Join(p.RunsDir, "run-broken"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(p.RunsDir, "run-broken", "run.yaml"), []byte("::: not yaml"), 0o644); err != nil {
		t.Fatal(err)
	}
	state.items["run-broken"] = []Item{{ItemID: "3271", Repo: webRepo}}
	err := p.TerminalNotifier(nil)("run-broken", journal.PhaseAborted, "local-ci")
	if err == nil || !strings.Contains(err.Error(), "read terminal classification") {
		t.Fatalf("err = %v, want the classification read failure reported", err)
	}
	if state.streaks["web#3271"] != 1 {
		t.Fatalf("streaks = %v, want the unclassified terminal charged", state.streaks)
	}
}
