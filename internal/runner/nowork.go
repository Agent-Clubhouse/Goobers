package runner

import (
	"fmt"
	"slices"
	"strings"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/telemetry"
)

// NoWorkUnsubstantiatedCode is the terminal cause of a run whose no-work claim
// the runner refused (#2736). A no-work claim short-circuits the run straight
// to completed, so an accepted one is indistinguishable — in the run list, in
// metrics, in every health number an operator looks at — from a healthy empty
// tick. That is the truth when the stage looked and there was genuinely
// nothing to act on; it is silent data loss when the stage found nothing
// because the evidence never reached it. The runner already holds, in the
// journal, what every upstream stage produced, so it looks before accepting.
// Classified in internal/telemetry so the refusal is reportable in operator
// health numbers instead of counting as an unknown failure.
const NoWorkUnsubstantiatedCode = telemetry.ErrCodeNoWorkUnsubstantiated

// StageProduction is what one upstream stage delivered to the run before a
// downstream stage claimed no-work.
type StageProduction struct {
	Stage string
	// Delivered reports that the stage handed this run something a downstream
	// stage could act on — an artifact or an output.
	Delivered bool
}

// DeliveredEvidence reports whether a finished stage's result carried anything
// a downstream stage could act on.
func DeliveredEvidence(result apiv1.ResultEnvelope) bool {
	return len(result.Artifacts) > 0 || len(result.Outputs) > 0
}

// BarrenUpstream names the upstream stages that delivered nothing, and only
// when NONE of them delivered anything: one producing upstream stage is enough
// to make a no-work claim the honest "looked and found nothing", while upstream
// stages that all journaled nothing leave the claim with no basis at all — a
// fan-in stage in that position has no evidence to form any verdict from. Nil
// when the claim is substantiated, and nil when there was no upstream to check.
func BarrenUpstream(upstream []StageProduction) []string {
	if len(upstream) == 0 {
		return nil
	}
	barren := make([]string, 0, len(upstream))
	for _, produced := range upstream {
		if produced.Delivered {
			return nil
		}
		barren = append(barren, produced.Stage)
	}
	return barren
}

// NoWorkUnsubstantiatedMessage is the operator-facing cause both runners record
// for a refused no-work claim, naming the upstream stages that delivered
// nothing so the missing evidence is diagnosable from the terminal row alone.
func NoWorkUnsubstantiatedMessage(stage string, barren []string) string {
	return fmt.Sprintf(
		"stage %q reported no-work, but no upstream stage delivered evidence to act on: %s produced no artifacts and no outputs",
		stage, strings.Join(barren, ", "),
	)
}

// noWorkEvidenceGap reports the barren upstream behind t's no-work claim, and
// nil when the claim stands on its own.
//
// Only two stage shapes are checked, deliberately: a stage's evidence does not
// always travel as artifacts and outputs — a stage handing its successor
// commits in the shared workspace returns an empty envelope and is still
// entirely productive. Both checked shapes consume upstream evidence BY
// DECLARATION, so an empty upstream is unambiguous there:
//   - a task naming its producers with contextFrom, which is the workflow
//     author saying "these stages' artifacts are my input"; and
//   - a parallel's fan-in join, whose branch analyses reach it only as the
//     artifacts and outputs they journaled.
func (r *Runner) noWorkEvidenceGap(ws *walkState, t apiv1.Task) []string {
	fanIn := ws.fanIn != nil && t.Name == ws.fanIn.spec.Join
	if len(t.ContextFrom) == 0 && !fanIn {
		return nil
	}
	return BarrenUpstream(upstreamProductionFromJournal(ws.jr, t.Name, t.ContextFrom, fanIn))
}

// upstreamProductionFromJournal reads back what the stages feeding stage
// produced in this run: the tasks named by sources, or — when sources is empty
// and branchOnly is set — every task that ran on a parallel branch. A stage
// counts as upstream once it has a stage.finished event, and as delivering if
// any of its attempts journaled outputs or artifacts on its result; the
// runner's own pre-dispatch artifact bookkeeping is not the stage's production
// and never counts. A gate named by sources delivers its verdict, so an
// evaluated one always counts as delivered. Order is first-finished order, so
// the recorded cause reads in run order.
func upstreamProductionFromJournal(jr executionJournal, stage string, sources []string, branchOnly bool) []StageProduction {
	rd, err := journal.OpenRead(jr.Dir())
	if err != nil {
		return nil
	}
	events, err := rd.Events()
	if err != nil {
		return nil
	}
	named := len(sources) > 0
	var order []string
	seen := make(map[string]bool)
	delivered := make(map[string]bool)
	record := func(name string, produced bool) {
		if !seen[name] {
			seen[name] = true
			order = append(order, name)
		}
		if produced {
			delivered[name] = true
		}
	}
	for _, event := range events {
		switch event.Type {
		case journal.EventStageFinished:
			if event.Stage == "" || event.Stage == stage {
				continue
			}
			if named && !slices.Contains(sources, event.Stage) {
				continue
			}
			if !named && event.Branch == 0 {
				continue
			}
			record(event.Stage, len(event.Artifacts) > 0 || len(event.Outputs) > 0)
		case journal.EventGateEvaluated:
			if !named || event.Gate == "" || !slices.Contains(sources, event.Gate) {
				continue
			}
			record(event.Gate, true)
		}
	}
	production := make([]StageProduction, 0, len(order))
	for _, name := range order {
		production = append(production, StageProduction{Stage: name, Delivered: delivered[name]})
	}
	return production
}
