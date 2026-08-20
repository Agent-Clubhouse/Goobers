package readservice

import (
	"context"
	"sort"

	"github.com/goobers/goobers/internal/readmodel"
)

// The latest-outcome aggregate, served from read.db (#1891, design §5.2/§14.4).
//
// # What this replaces
//
// The Workflows page's request costs three unbounded things today, and the
// aggregate replaces all three with one indexed query:
//
//  1. rollup's LatestWorkflowRunRefs — §5.2's "unindexed window function over all
//     history". It ranks every row in every partition to return one row each.
//  2. latestTerminalWorkflowOutcome — a backwards walk that opens the newest
//     run's journal, and if that run is not terminal, pages further back and
//     opens more. §12 lists it for deletion. On a workflow whose recent runs are
//     in flight, it opens journals until it finds an outcome, and nothing bounds
//     how many.
//  3. the active-run count, which #1741 already moved off the directory walk onto
//     a sampler; here it is an indexed aggregate in the same query, so it is
//     consistent with the outcomes it is reported beside rather than sampled
//     independently.
//
// # The contract ruling: latestPerWorkflow does not join the closed set
//
// §5.7's closed set enumerates FILTER COMBINATIONS for the run list. It was open
// whether `latestPerWorkflow` — a query parameter on the same endpoint — had to
// be enumerated there before #1920 could close.
//
// Ruling: it does not, and enumerating it would be a category error. The closed
// set exists because filter dimensions COMBINE — eight of them make 256
// combinations, and the set's job is to say which of those the indexes actually
// cover. `latestPerWorkflow` does not combine: it selects a different query
// SHAPE, and the shape it selects already rejects every other dimension except
// gaggle and workflow (runs.go's guard predates this work). So it contributes no
// combinatorial surface at all.
//
// What it does need — and what it lacked — is its own declared index and its own
// bound, which is what this file and the v3 partial indexes are. The rule §5.7
// is really enforcing is "no unbounded read ships without a named index", and
// that is satisfied here by a different mechanism than enumeration.
//
// `workflowActivity` likewise stays on the response. It is a separate FIELD
// rather than a filter, it is now answered by the same single query rather than
// by a second scan, and removing it would break the Workflows page for no gain.

// listLatestWorkflowOutcomesFromReadModel answers the Workflows page from
// read.db, opening zero journals.
func (s *Local) listLatestWorkflowOutcomesFromReadModel(ctx context.Context, options RunListOptions) (RunList, error) {
	if s.sources.ReadModel == nil {
		return RunList{}, ErrReadModelUnavailable
	}

	rows, err := s.sources.ReadModel.LatestPerWorkflow(ctx, readmodel.AggregateOptions{
		Gaggle:   options.Gaggle,
		Workflow: options.Workflow,
		// The page reports last OUTCOMES. A workflow whose newest run is still
		// in flight keeps showing what it last did, with the in-flight run
		// counted in workflowActivity instead.
		TerminalOnly: true,
	})
	if err != nil {
		return RunList{}, err
	}

	observedAt := s.now().UTC()
	result := RunList{Runs: make([]RunSummary, 0, len(rows))}
	activity := make([]WorkflowRunActivity, 0, len(rows))
	for _, row := range rows {
		// The row is COMPLETE — every field the summary needs came back with the
		// aggregate. No second query, and above all no journal open: that is what
		// makes a 2,000-workflow page cost one request rather than 2,001.
		result.Runs = append(result.Runs, summaryFromReadModel(row.Run, observedAt))
		if row.ActiveRuns > 0 {
			activity = append(activity, WorkflowRunActivity{
				Gaggle:     row.Run.Gaggle,
				Workflow:   row.Run.Workflow,
				ActiveRuns: row.ActiveRuns,
			})
		}
	}

	// The list is newest-first with RunID ascending as the tiebreak, matching
	// every other run list. The aggregate returns gaggle/workflow order because
	// that is what its indexes produce; re-sorting here is O(workflows) on an
	// already-bounded result, not a scan.
	sort.Slice(result.Runs, func(i, j int) bool {
		if result.Runs[i].StartedAt.Equal(result.Runs[j].StartedAt) {
			return result.Runs[i].ID < result.Runs[j].ID
		}
		return result.Runs[i].StartedAt.After(result.Runs[j].StartedAt)
	})
	if len(activity) > 0 {
		result.WorkflowActivity = activity
	}
	return result, nil
}
