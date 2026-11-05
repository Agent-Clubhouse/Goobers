package intervention

import (
	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/journal"
)

// Resolution happens before acquiring the per-run intervention slot. Re-read
// under that slot so a request delayed behind an already completed rerun cannot
// append a second acceptance marker from its stale terminal snapshot.
func recheckRerunAcceptance(resolved resolvedInterventionRun, input httpapi.InterventionRequest) (bool, error) {
	rd, err := journal.OpenRead(resolved.runDir)
	if err != nil {
		return false, err
	}
	current, err := rd.Events()
	if err != nil {
		return false, err
	}
	fresh := resolved
	fresh.events = current
	_, replayed, err := replayIntervention(fresh, "rerun", input)
	if replayed || err != nil {
		return replayed, err
	}
	phase, err := rd.Phase()
	if err != nil {
		return false, err
	}
	if phase != journal.PhaseEscalated {
		return false, interventionConflict("run_not_escalated", "run is no longer escalated")
	}
	if latestTerminalSequence(current) != resolved.terminalSeq {
		return false, interventionConflict("terminal_generation_changed", "run terminal generation changed before the rerun was accepted")
	}
	return false, nil
}
