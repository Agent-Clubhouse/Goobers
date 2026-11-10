package runner

import (
	"slices"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/runsemantics"
)

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
	return runsemantics.BarrenUpstream(upstreamProductionFromJournal(ws.jr, t.Name, t.ContextFrom, fanIn))
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
func upstreamProductionFromJournal(jr executionJournal, stage string, sources []string, branchOnly bool) []runsemantics.StageProduction {
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
			// A no-work branch explicitly evaluated its source and found it
			// empty. At a fan-in, that is sufficient evidence for the join to
			// truthfully report that every source was empty.
			record(event.Stage, len(event.Artifacts) > 0 || len(event.Outputs) > 0 ||
				(branchOnly && event.Status == string(apiv1.ResultNoWork)))
		case journal.EventGateEvaluated:
			if !named || event.Gate == "" || !slices.Contains(sources, event.Gate) {
				continue
			}
			record(event.Gate, true)
		}
	}
	production := make([]runsemantics.StageProduction, 0, len(order))
	for _, name := range order {
		production = append(production, runsemantics.StageProduction{Stage: name, Delivered: delivered[name]})
	}
	return production
}
