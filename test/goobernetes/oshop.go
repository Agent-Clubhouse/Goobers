package goobernetes

import (
	"fmt"
	"sort"

	"github.com/goobers/goobers/internal/readservice"
)

// OSHopObserver is S2's named observer (goobernetes-smoke.md §4 S2):
// "runner.* events for the same run ID showing os: linux and os: windows
// attempts; the run's terminal journal event shows success."
const OSHopObserver = "run's StageAttempt.Placement.OS across stages (internal/readservice/runs.go StageAttempt.Placement) + run terminal phase (internal/journal RunPhase)"

// AssertOSHop is S2: "A single run executes at least one stage on a Linux
// node and at least one on a Windows node, and completes."
//
// stages is the same per-stage AttemptList shape AssertFreshPodPerAttempt
// takes (one entry per stage in the applied workflow); runCompleted is
// whether the run's terminal journal event reported success
// (journal.PhaseCompleted, internal/journal/run.go — S2's observer requires
// BOTH the OS hop and completion, so a run that touched both OSes but never
// finished is a fail, not a pass with an asterisk).
func AssertOSHop(stages []readservice.AttemptList, runCompleted bool) AssertionResult {
	if len(stages) == 0 {
		return invalid("no stage attempt lists supplied", nil)
	}

	seenOS := make(map[string][]string) // os -> stage names observed on it
	missing := 0
	total := 0
	for _, list := range stages {
		for _, a := range list.Attempts {
			total++
			if a.Placement == nil || a.Placement.OS == "" {
				missing++
				continue
			}
			seenOS[a.Placement.OS] = append(seenOS[a.Placement.OS], list.Stage)
		}
	}
	if total == 0 {
		return invalid("every supplied stage had zero attempts", nil)
	}
	if missing == total {
		return invalid("no attempt carries a Placement.OS — the placement-provenance observer never fired for this run", seenOS)
	}

	osNames := make([]string, 0, len(seenOS))
	for os := range seenOS {
		osNames = append(osNames, os)
	}
	sort.Strings(osNames)

	_, hasLinux := seenOS["linux"]
	_, hasWindows := seenOS["windows"]
	if !hasLinux || !hasWindows {
		return classify("", false,
			fmt.Sprintf("run did not hop OS: observed OS set %v (want both \"linux\" and \"windows\")", osNames),
			nil, seenOS)
	}
	if !runCompleted {
		return classify("", false, "run executed on both linux and windows but did not complete (terminal phase was not success)", nil, seenOS)
	}
	return classify("", true, "", seenOS, nil)
}
