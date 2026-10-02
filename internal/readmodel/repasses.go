package readmodel

import (
	"strings"

	"github.com/goobers/goobers/internal/journal"
)

func (r *RunRow) observeGateOutcome(event journal.Event, stages map[string]*StageRow) {
	r.OutcomeVerdict = event.Verdict
	r.OutcomeTarget = event.Target
	if gateRepassesStage(event, stages) {
		r.RepassCount++
	}
}

// gateRepassesStage counts the route taken, not the cumulative budget value.
// A charged target survives an escalation override in the journal, but that
// override did not send the stage back for another pass.
func gateRepassesStage(event journal.Event, stages map[string]*StageRow) bool {
	if event.Actor != "" || event.Target == "" || strings.HasPrefix(event.Target, "@") ||
		event.Verdict == "pass" || event.Verdict == "timeout" || event.RepassAttempt() <= 0 {
		return false
	}
	if interrupted, _ := event.Runner["interrupted"].(bool); interrupted {
		return false
	}
	if target, exists := event.Runner["repassTarget"]; exists {
		return target == event.Target
	}
	// Older journals carry only the gate-local attempt count. Require a
	// previously completed stage at the selected target so a forward route
	// with a non-pass verdict is not mistaken for a repass.
	stage := stages[event.Target]
	return stage != nil && stage.FinishedAt != nil
}
