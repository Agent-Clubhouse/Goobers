package e2e

import (
	"fmt"

	"github.com/goobers/goobers/internal/readservice"
)

// FreshPodObserver is S1's named observer (goobernetes-smoke.md §4 S1): "the
// run's runner.* journal events and StageAttempt placement fields. The check
// is mechanical: project (attempt -> pod identity) over the whole run; the
// mapping is injective."
const FreshPodObserver = "run's runner.* journal events + StageAttempt.Placement.Pod (internal/readservice/runs.go StageAttempt.Placement, internal/journal/placement.go Placement.Pod)"

// PodAttemptRef names one stage attempt in a fresh-pod report: enough to
// point a reader at the exact attempt in `goobers trace`.
type PodAttemptRef struct {
	Stage  string
	Number int
	Class  string // "initial"/"policy"/"infra"/"human" — readservice.StageAttempt.Class
	Pod    string
}

// AssertFreshPodPerAttempt is S1: "every stage attempt of every run —
// including repasses (S5) and infra retries (S6) — executes in a pod created
// for that attempt... No pod identity appears under two attempts."
//
// stages is one run's full attempt list, ONE readservice.AttemptList per
// stage (internal/readservice/runs.go:413-452 — AttemptList.Stage names the
// stage, AttemptList.Attempts is that stage's StageAttempt rows, each
// carrying its own Placement). A live driver calls the read-model's
// per-stage attempts query once per stage in the applied workflow and passes
// the results straight through; this fixture-shaped input is what makes the
// check unit-testable today without a cluster.
//
// Per S1's observer text the check is MECHANICAL: project (attempt -> pod
// identity) over the WHOLE RUN and require the mapping injective. An attempt
// with no recorded Placement or an empty Pod name is not evidence of a fresh
// pod — modes 1/2 (a bare host or a daemon-local run) never populate Pod at
// all (journal.Placement.Pod's own doc comment: "empty on bare hosts"), so
// on a DISTRIBUTED-shape run (what the smoke exercises) a missing Pod means
// the placement-provenance observer itself did not fire for that attempt —
// that is INVALID (the observer-machinery gap goobernetes-smoke.md §5 rule 2
// names), never a silent pass.
func AssertFreshPodPerAttempt(stages []readservice.AttemptList) AssertionResult {
	if len(stages) == 0 {
		return invalid("no stage attempt lists supplied — the run produced no stages to check, or the read-model query itself failed", nil)
	}

	var missing []PodAttemptRef
	seen := make(map[string]PodAttemptRef)
	var duplicates []string
	total := 0
	for _, list := range stages {
		for _, a := range list.Attempts {
			total++
			ref := PodAttemptRef{Stage: list.Stage, Number: a.Number, Class: a.Class}
			if a.Placement == nil || a.Placement.Pod == "" {
				missing = append(missing, ref)
				continue
			}
			ref.Pod = a.Placement.Pod
			if prior, dup := seen[ref.Pod]; dup {
				duplicates = append(duplicates, fmt.Sprintf(
					"pod %q reused: stage %q attempt %d (%s) and stage %q attempt %d (%s)",
					ref.Pod, prior.Stage, prior.Number, prior.Class, ref.Stage, ref.Number, ref.Class))
				continue
			}
			seen[ref.Pod] = ref
		}
	}
	if total == 0 {
		return invalid("every supplied stage had zero attempts", nil)
	}

	if len(missing) > 0 {
		return classify(
			PreconditionFailure(fmt.Sprintf("%d of %d attempt(s) carry no placement provenance (Placement nil or Pod empty): %v", len(missing), total, missing)),
			false, "", nil, missing)
	}
	if len(duplicates) > 0 {
		return classify("", false, fmt.Sprintf("pod identity reused across attempts: %v", duplicates), nil, duplicates)
	}
	return classify("", true, "", seen, nil)
}
