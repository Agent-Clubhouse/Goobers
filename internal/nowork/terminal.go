// Package nowork classifies and records per-item no-work verdicts.
package nowork

import (
	"strings"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
)

// Terminal describes a completed run that ended on a no-work verdict:
// which stage declined, the rationale it recorded, its own classification of
// the verdict (for example "already-fixed") and the evidence it cited (for
// example an existing commit). Each is empty when the stage journaled none.
type Terminal struct {
	Stage    string
	Reason   string
	Verdict  string
	Evidence string
}

// TerminalFromEvents reports whether a COMPLETED run ended because its
// TERMINAL stage answered no-work, and carries that stage's recorded
// rationale.
//
// Why the run's own terminal disposition cannot answer this: finishNoWork
// (internal/runner/run.go) marks a run journal.RunDispositionNoWork only when
// `ws.steps == 1` or a fan-in settled every branch empty. The implementation
// workflow reaches `implement` at step 3, so a no-work verdict there completes
// the run as RunDispositionProduced — indistinguishable, at the terminal, from
// a run that actually opened a pull request. That is precisely why #5379's
// loop was invisible: 28 consecutive claims all reported as productive.
//
// The stage.finished event is the honest signal. It carries the executor's
// literal apiv1.ResultNoWork status (internal/runner/run.go's runTask appends
// `Status: string(result.Status)`), and it carries the stage's Outputs, which
// is where a rationale lives.
//
// Two filters make this the run's verdict rather than merely A no-work event
// somewhere in its history, and BOTH are load-bearing:
//
//   - `event.Stage == finalState`. finalState is the stage the run actually
//     ended on, handed to the terminal notifier. Without this, a run that
//     no-worked at `implement`, repassed through a gate (#5107) and then went
//     on to produce a pull request still looks like a no-work run, because the
//     stale event remains in the journal forever.
//   - `event.Branch == 0`. Parallel branch stages append to the SAME run
//     journal (internal/runner/parallel_run.go stamps ev.Branch and forwards
//     to the run journal), and a branch that returns no-work ends only that
//     branch — its siblings keep running and the join proceeds. Counting a
//     branch's verdict as the run's would park an item whose fan-out run
//     produced real findings on every other branch.
//
// Scanning for the newest matching event rather than the first is what makes a
// repass converge on its final attempt.
func TerminalFromEvents(events []journal.Event, finalState string) (Terminal, bool) {
	if finalState == "" {
		return Terminal{}, false
	}
	var found Terminal
	var ok bool
	for _, event := range events {
		if event.Type != journal.EventStageFinished || event.Branch != 0 {
			continue
		}
		if event.Stage != finalState || event.Status != string(apiv1.ResultNoWork) {
			continue
		}
		found = Terminal{
			Stage:    event.Stage,
			Reason:   noWorkReasonFromOutputs(event.Outputs),
			Verdict:  stringOutput(event.Outputs, "status"),
			Evidence: stringOutput(event.Outputs, "existingCommit"),
		}
		ok = true
	}
	return found, ok
}

// noWorkReasonFromOutputs extracts the rationale a stage recorded alongside a
// no-work verdict. Deterministic stages already write this key
// (cmd/goobers/backlogquery.go's writeNoWorkResult), and internal/diagnostics
// already allowlists it for surfacing, so an agentic stage that emits the same
// key is picked up by both without further plumbing.
//
// An agentic stage that names its rationale "reason" instead (#5643's
// already-fixed verdicts did) is read as a fallback, so the verdict it wrote
// is not discarded for using the other spelling.
func noWorkReasonFromOutputs(outputs map[string]any) string {
	if reason := stringOutput(outputs, "noWorkReason"); reason != "" {
		return reason
	}
	return stringOutput(outputs, "reason")
}

// stringOutput returns a stage output's trimmed string value, "" when the key
// is absent or not a string.
func stringOutput(outputs map[string]any, key string) string {
	text, _ := outputs[key].(string)
	return strings.TrimSpace(text)
}
