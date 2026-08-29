package e2e

import (
	"fmt"

	"github.com/goobers/goobers/internal/readservice"
)

// LedgerNeverOnWindowsObserver is the named observer for
// goobernetes-architecture.md §11 item 7's first fact ("A ledger-touching
// stage is never observed on a Windows runner") and goobernetes-smoke.md §5
// D12 solver rule: the run's StageAttempt.Placement.OS crossed with which
// stages are ledger-touching.
const LedgerNeverOnWindowsObserver = "run's StageAttempt.Placement.OS (internal/readservice/runs.go) crossed with the workflow's ledger-touching stage set (internal/dispatcher.Attempt.LedgerTouching, internal/bootstrap/placement.go taskLedgerTouching)"

// LedgerTouching decides whether a named stage is ledger-touching. This is
// deliberately a caller-supplied predicate, not a reimplementation: the real
// classification already exists at
// internal/bootstrap/placement.go:46 (taskLedgerTouching, unexported) and is
// carried per-attempt as internal/dispatcher.Attempt.LedgerTouching
// (internal/dispatcher/dispatcher.go:159) — a SelectRunner call already
// refuses to place such a stage on a Windows-only eligible set
// (internal/dispatcher/selection.go:49-67). This helper is a REDUNDANT
// evidence check over the placement record a live run leaves behind, not the
// enforcement mechanism itself; it must consume whatever the live driver's
// compiled-workflow view says is ledger-touching (derived the same way
// bootstrap already does), never invent its own second notion of the term.
type LedgerTouching func(stage string) bool

// AssertNoLedgerTouchingOnWindows checks the structural fact
// goobernetes-architecture.md §11 item 7 names, over one run's placement
// provenance. stages is the same per-stage AttemptList shape
// AssertFreshPodPerAttempt takes; ledgerTouching classifies each stage name.
//
// A violation here would mean the dispatcher's own SelectRunner refusal
// (internal/dispatcher/selection.go) was bypassed or never wired for this
// stage — this check is the smoke's independent witness of that fact from
// the evidence trail, not a re-derivation of the dispatcher's own logic.
func AssertNoLedgerTouchingOnWindows(stages []readservice.AttemptList, ledgerTouching LedgerTouching) AssertionResult {
	if len(stages) == 0 {
		return invalid("no stage attempt lists supplied", nil)
	}
	if ledgerTouching == nil {
		return invalid("no ledger-touching classification supplied — cannot distinguish a ledger-touching stage from any other", nil)
	}

	var violations []PodAttemptRef
	var uncertain []string
	checked := 0
	for _, list := range stages {
		if !ledgerTouching(list.Stage) {
			continue
		}
		for _, a := range list.Attempts {
			if a.Placement == nil || a.Placement.OS == "" {
				uncertain = append(uncertain, fmt.Sprintf("stage %q attempt %d: no Placement.OS recorded", list.Stage, a.Number))
				continue
			}
			checked++
			if a.Placement.OS == "windows" {
				violations = append(violations, PodAttemptRef{Stage: list.Stage, Number: a.Number, Class: a.Class, Pod: a.Placement.Pod})
			}
		}
	}

	if len(uncertain) > 0 {
		return classify(PreconditionFailure(fmt.Sprintf("ledger-touching attempt(s) with no recorded OS: %v", uncertain)), false, "", nil, uncertain)
	}
	if checked == 0 {
		return invalid("no ledger-touching stage attempt carried placement provenance to check — the run may not have exercised any ledger-touching stage, which the smoke workflow is expected to declare", nil)
	}
	if len(violations) > 0 {
		return classify("", false, fmt.Sprintf("ledger-touching stage attempt(s) placed on Windows: %v", violations), nil, violations)
	}
	return classify("", true, "", checked, nil)
}
