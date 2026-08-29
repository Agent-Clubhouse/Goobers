package e2e

import (
	"fmt"

	"github.com/goobers/goobers/internal/runnersolve"
)

// CapabilityGapObserver is the named observer for
// goobernetes-architecture.md §11 item 8 ("Boot proportionality"): the
// solver's per-stage Result — "A config declaring a requirement no runner
// claims fails at apply/validate with an error... and — for the undeclared
// legacy case — refuses the affected run at admission without killing the
// daemon."
const CapabilityGapObserver = "runnersolve.Result.Unsatisfiable() (internal/runnersolve/runnersolve.go Solve/SolveExecutable — the one solver behind all three admission checkpoints, dsl-3.0.md §5)"

// UnsatStage is one unsatisfiable stage's report: enough to show a reader
// which requirement no runner could meet and why.
type UnsatStage struct {
	Stage      string
	Kind       runnersolve.UnsatKind
	Diagnostic string
}

// AssertCapabilityGapEnforced is architecture §11 item 8: a stage requiring
// a capability the declared inventory cannot satisfy must be caught — not
// silently accepted, and never by killing the daemon.
//
// result is what internal/runnersolve.Solve (checkpoint 1: `goobers
// validate`/apply) or SolveExecutable (checkpoints 2/3: per-run admission,
// boot refusal) actually returned for a config carrying a stage whose
// requirement the fixture/live inventory cannot meet. wantUnsatStage is the
// stage name the caller expected to be caught (the fixture's deliberately
// unsatisfiable stage, or — live — the smoke's own negative-control stage,
// if the procedure chooses to exercise this item that way; goobernetes-
// smoke.md does not name a dedicated S-item for this, so a live procedure
// wires it in as part of its own precondition checks, per architecture §11
// item 8 rather than §4).
//
// This never re-implements the solve — it is a pure function of a
// runnersolve.Result a real Solve/SolveExecutable call already produced
// (fixture today, live apply/admission output tomorrow), consistent with
// the "shared solver, not a second implementation" rule runnersolve.go's own
// package doc states (the CAP003/scheduler mirror lesson, #3497).
func AssertCapabilityGapEnforced(result runnersolve.Result, wantUnsatStage string) AssertionResult {
	if len(result.Stages) == 0 {
		return invalid("solver result carries no stages — the solve was never run against the requirement-bearing config", nil)
	}
	if wantUnsatStage == "" {
		return invalid("no target stage named — cannot tell an incidental unsat from the intended capability-gap probe", nil)
	}

	unsat := result.Unsatisfiable()
	var report []UnsatStage
	for _, s := range unsat {
		report = append(report, UnsatStage{Stage: s.Stage, Kind: s.Unsat.Kind, Diagnostic: s.Unsat.Diagnostic})
	}

	for _, s := range unsat {
		if s.Stage == wantUnsatStage {
			return classify("", true, "", report, nil)
		}
	}
	return classify("", false,
		fmt.Sprintf("stage %q was not reported unsatisfiable (solver found %d unsatisfiable stage(s): %v) — the capability gap was not caught", wantUnsatStage, len(unsat), report),
		nil, report)
}
