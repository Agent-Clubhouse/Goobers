package creditgraph_test

import (
	"context"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/creditgraph"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/readmodel"
)

// The conformance contract between the two credit paths (#6355,
// docs/design/credit-graph.md "Conformance and compatibility"). Both are
// projections of the same run journal, so where their answers overlap they
// must agree:
//
//  1. Routed stages: the stages creditgraph records for a run are exactly the
//     stage nodes the readmodel rollup projects for it.
//  2. Cause location: every stage creditgraph names as a failure cause is a
//     stage the rollup routed the run through, so the rollup charges that
//     run's failure to it.
//  3. Aborts: a run whose last gate routed to @abort is a failure in both.
//
// Beyond that their failure signals differ by definition: the rollup's is the
// run's last gate verdict/target, and creditgraph's is the run.finished
// status. The last three cases pin each divergence so none changes silently:
// a stage failure with no failing gate (failed only in creditgraph), a
// rejecting gate that escalates (failed only in the rollup; creditgraph reads
// an escalated outcome as neutral), and a rejecting gate that routes back to
// a stage that then completes (failed only in the rollup).

var conformanceStart = time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

type conformanceRun struct {
	name   string
	events []journal.Event
	// rollupFailure is whether readmodel.CreditAssignment counts the run in
	// FailureRuns; creditFailed is whether creditgraph reads a failed outcome.
	rollupFailure bool
	creditFailed  bool
	// blamedStage, when set, must be among creditgraph's cause stages, so
	// rule 2 is exercised rather than vacuously true.
	blamedStage string
}

func conformanceEvents(steps ...journal.Event) []journal.Event {
	events := make([]journal.Event, 0, len(steps)+1)
	events = append(events, journal.Event{Type: journal.EventRunStarted})
	events = append(events, steps...)
	for i := range events {
		events[i].Schema = journal.EventSchema
		events[i].Seq = uint64(i + 1)
		events[i].Time = conformanceStart.Add(time.Duration(i) * time.Second)
	}
	return events
}

func conformanceRuns() []conformanceRun {
	stage := func(name, status string) []journal.Event {
		return []journal.Event{
			{Type: journal.EventStageStarted, Stage: name, Attempt: 1},
			{Type: journal.EventStageFinished, Stage: name, Attempt: 1, Status: status},
		}
	}
	gate := func(name, verdict, target string) journal.Event {
		return journal.Event{Type: journal.EventGateEvaluated, Gate: name, Verdict: verdict, Target: target}
	}
	finished := func(status string) journal.Event {
		return journal.Event{Type: journal.EventRunFinished, Status: status}
	}
	join := func(parts ...[]journal.Event) []journal.Event {
		return slices.Concat(parts...)
	}
	return []conformanceRun{
		{
			name: "passing run",
			events: conformanceEvents(join(stage("implement", "success"), stage("review", "success"),
				[]journal.Event{gate("ci", "pass", "complete"), finished("completed")})...),
		},
		{
			name: "gate rejects and aborts",
			events: conformanceEvents(join(stage("implement", "success"), stage("review", "success"),
				[]journal.Event{gate("ci", "reject", "@abort"), finished("failed")})...),
			rollupFailure: true, creditFailed: true,
		},
		{
			name: "stage fails and the gate aborts",
			events: conformanceEvents(join(stage("implement", "success"), stage("review", "failure"),
				[]journal.Event{gate("ci", "reject", "@abort"), finished("failed")})...),
			rollupFailure: true, creditFailed: true, blamedStage: "review",
		},
		{
			name: "stage fails without a failing gate",
			events: conformanceEvents(join(stage("implement", "success"), stage("review", "failure"),
				[]journal.Event{finished("failed")})...),
			rollupFailure: false, creditFailed: true, blamedStage: "review",
		},
		{
			name: "gate rejects and escalates",
			events: conformanceEvents(join(stage("implement", "success"), stage("review", "success"),
				[]journal.Event{gate("ci", "reject", "@escalate"), finished("escalated")})...),
			rollupFailure: true, creditFailed: false,
		},
		{
			name: "gate rejects back to a stage that then completes",
			events: conformanceEvents(join(stage("implement", "success"), stage("review", "success"),
				[]journal.Event{gate("ci", "reject", "implement")},
				[]journal.Event{
					{Type: journal.EventStageStarted, Stage: "implement", Attempt: 2},
					{Type: journal.EventStageFinished, Stage: "implement", Attempt: 2, Status: "success"},
				},
				[]journal.Event{finished("completed")})...),
			rollupFailure: true, creditFailed: false,
		},
	}
}

func TestCreditGraphConformsToReadmodelRollup(t *testing.T) {
	ctx := context.Background()
	for _, run := range conformanceRuns() {
		t.Run(run.name, func(t *testing.T) {
			store, err := readmodel.Open(filepath.Join(t.TempDir(), "read.db"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = store.Close() })
			identity := journal.RunIdentity{
				RunID: "run-conformance", Gaggle: "core", Workflow: "implementation",
				WorkflowVersion: 1, StartedAt: conformanceStart,
			}
			projection := readmodel.ProjectRun(identity, readmodel.Projection{}, run.events)
			if err := store.UpsertRun(ctx, projection); err != nil {
				t.Fatal(err)
			}
			credits, err := store.CreditAssignment(ctx, readmodel.CreditOptions{Gaggle: "core"})
			if err != nil {
				t.Fatal(err)
			}
			graph, err := creditgraph.Build(creditgraph.Input{
				RunID: identity.RunID, Gaggle: identity.Gaggle, Workflow: identity.Workflow, Events: run.events,
			})
			if err != nil {
				t.Fatal(err)
			}
			attribution := creditgraph.Attribute(graph)

			// 1. Routed stages agree.
			var rollupStages, graphStages []string
			for _, node := range projection.Nodes {
				if node.Kind == "stage" {
					rollupStages = append(rollupStages, node.Name)
				}
			}
			for _, node := range graph.Nodes {
				if node.Kind == creditgraph.KindStage && node.Provenance == creditgraph.ProvenanceRecorded {
					graphStages = append(graphStages, node.Stage)
				}
			}
			// The rollup keeps one node per stage; creditgraph one per attempt.
			slices.Sort(rollupStages)
			slices.Sort(graphStages)
			graphStages = slices.Compact(graphStages)
			if !slices.Equal(rollupStages, graphStages) {
				t.Fatalf("routed stages: rollup %v, creditgraph %v", rollupStages, graphStages)
			}

			// 3, and the pinned divergences.
			if got := attribution.OutcomeSign < 0; got != run.creditFailed {
				t.Fatalf("creditgraph failed outcome = %v (outcome %q), want %v", got, attribution.Outcome, run.creditFailed)
			}
			rollupFailed := false
			for _, credit := range credits {
				if credit.FailureRuns > 0 {
					rollupFailed = true
				}
			}
			if rollupFailed != run.rollupFailure {
				t.Fatalf("rollup failure = %v, want %v: %+v", rollupFailed, run.rollupFailure, credits)
			}
			if projection.Run.OutcomeTarget == "@abort" && (!rollupFailed || !run.creditFailed) {
				t.Fatalf("an aborted run must be a failure in both paths (rollup %v, creditgraph %v): rule 3 broken",
					rollupFailed, run.creditFailed)
			}

			// 2. Every stage cause is a stage the rollup routed the run through.
			blamed := false
			for _, cause := range attribution.Causes {
				if cause.Stage == "" {
					continue
				}
				blamed = blamed || cause.Stage == run.blamedStage
				if !slices.Contains(rollupStages, cause.Stage) {
					t.Fatalf("creditgraph cause %+v names stage %q the rollup never routed (%v)", cause, cause.Stage, rollupStages)
				}
				if rollupFailed && !creditCountsFailure(credits, cause.Stage) {
					t.Fatalf("creditgraph blames stage %q but the rollup does not charge the failure to it: %+v", cause.Stage, credits)
				}
			}
			if run.blamedStage != "" && !blamed {
				t.Fatalf("creditgraph causes %+v do not blame stage %q", attribution.Causes, run.blamedStage)
			}
			if run.creditFailed && len(attribution.Causes) == 0 {
				t.Fatal("a failed creditgraph outcome must carry at least one cause finding")
			}
		})
	}
}

func creditCountsFailure(credits []readmodel.NodeCredit, stage string) bool {
	for _, credit := range credits {
		if credit.Kind == "stage" && credit.Stage == stage && credit.FailureRuns > 0 {
			return true
		}
	}
	return false
}
