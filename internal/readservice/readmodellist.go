package readservice

import (
	"context"
	"errors"
	"time"

	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/readmodel"
)

// The read-model cutover (design §6.6 step 3).
//
// "Cut reads over behind a config flag, defaulting on, with the old path
// intact." Rollback is a flag flip or deleting a file; no journal is touched.
//
// # What it removes
//
// Measured at 1x, after Wave 1: a 50-row list page opens 51 journals — one per
// returned row, plus the lookahead — because the telemetry.db row is not
// complete enough to answer the list contract, so each run is hydrated from its
// journal. That is the diagnosis's "lists open and parse a journal per returned
// row", and it is the last uncorrected item from it. The read-model path opens
// zero.
//
// # Why the default is OFF here, against the design's "defaulting on"
//
// The design's step 3 assumes steps 1-2 leave the store continuously current.
// They do not yet. read.db is built on first start and updated by the writer
// seam when a run finishes, which matches telemetry.db's freshness — but §6.1's
// projector, driven by a durable intake watermark, is Wave 3. Until it exists,
// runs written while the daemon was down are invisible to both stores, and the
// read model has no repair sweep to notice.
//
// Defaulting on before that would trade a slow-but-complete answer for a fast
// one that can silently omit — which is precisely the failure §14.7 exists to
// prevent, and a worse trade than the latency it buys. The flag flips to on in
// Wave 3, when the projector and bidirectional repair make completeness
// something the store can establish rather than assume.

// ErrReadModelUnavailable reports that the read-model path was requested but the
// store is not attached.
var ErrReadModelUnavailable = errors.New("readservice: read model is not available")

// listRunsFromReadModel answers a page from read.db, with zero journal opens.
//
// It returns ErrUnsupportedFilterCombination for anything outside the closed set
// (§5.7). That is deliberate rather than a fallback to the journal path: falling
// back would mean an unsupported filter silently costs a full scan, which is the
// behaviour the closed set exists to end, and the caller could not tell the two
// apart.
func (s *Local) listRunsFromReadModel(ctx context.Context, options RunListOptions, cursor *runCursor, limit int) (RunList, error) {
	if s.sources.ReadModel == nil {
		return RunList{}, ErrReadModelUnavailable
	}

	request := readmodel.ListOptions{
		Gaggle:   options.Gaggle,
		Workflow: options.Workflow,
		Phase:    options.Phase,
		Since:    options.Since,
		Until:    options.Until,
		Limit:    limit,
		Stage:    options.Stage,
		// Population is validated by canonicalStagePopulation upstream, and the
		// read model refuses any value its own switch does not recognise, so a
		// bad value cannot reach the query as a column name.
		Population: readmodel.Population(options.StagePopulation),
	}
	// One `outcome` on the wire, two meanings, disambiguated by whether a stage
	// is also named: with a stage it is that STAGE's last status, without one it
	// is the run's terminal verdict. Resolving it here means the ambiguity is
	// settled at the boundary rather than at every use -- and the run-level form
	// is not in the closed set, so it is left for the path that can serve it.
	if options.Stage != "" {
		request.StageOutcome = string(options.Outcome)
	}
	if cursor != nil {
		request.Cursor = readmodel.ListCursor{StartedAt: cursor.StartedAt, RunID: cursor.RunID}
	}

	page, err := s.sources.ReadModel.ListRuns(ctx, request)
	if err != nil {
		return RunList{}, err
	}

	observedAt := s.now()
	out := RunList{Runs: make([]RunSummary, 0, len(page.Runs))}
	for _, row := range page.Runs {
		out.Runs = append(out.Runs, summaryFromReadModel(row, observedAt))
	}
	if page.HasMore && len(out.Runs) > 0 {
		// The cursor is encoded from the last RETURNED summary, using the same
		// helper the journal-derived paths use. Sharing it is what keeps a cursor
		// portable across the cutover: a client paging when the flag flips must
		// not have its position invalidated.
		next, err := encodeRunCursor(out.Runs[len(out.Runs)-1])
		if err != nil {
			return RunList{}, err
		}
		out.NextCursor = next
	}
	return out, nil
}

// summaryFromReadModel maps a projected row onto the wire contract.
//
// Every field comes from the row. If one had to be filled from a journal, the
// cutover would not have removed the per-row open — it would have moved it.
func summaryFromReadModel(row readmodel.RunRow, observedAt time.Time) RunSummary {
	return RunSummary{
		ID:              row.RunID,
		Workflow:        row.Workflow,
		WorkflowVersion: row.WorkflowVersion,
		WorkflowDigest:  row.WorkflowDigest,
		Gaggle:          row.Gaggle,
		Trigger: journal.Trigger{
			Kind: journal.TriggerKind(row.TriggerKind),
			Ref:  row.TriggerRef,
		},
		Phase:        row.Phase,
		Terminal:     row.Terminal,
		CurrentStage: row.CurrentStage,
		StartedAt:    row.StartedAt,
		FinishedAt:   row.FinishedAt,
		// Computed at read time, not stored, so a quiet in-flight run keeps
		// ageing rather than freezing at projection time (§5.3).
		DurationMillis:   row.Duration(observedAt).Milliseconds(),
		LastActivityAt:   row.LastActivity,
		LastSeq:          row.LastSeq,
		RepassCount:      row.RepassCount,
		RetryCount:       row.RetryCount,
		PolicyRetryCount: row.PolicyRetryCount,
		InfraRetryCount:  row.InfraRetryCount,
		Stages:           row.Stages,
	}
}

// readModelEligible reports whether this request can be served from read.db.
//
// Two conditions, and both are about honesty rather than capability.
//
// The store must be attached and the flag on. And the request must not use
// LatestPerWorkflow, which is a different query shape with its own aggregate
// (#1891) — serving it from the read model's plain list would silently return
// the wrong thing rather than refuse.
func (s *Local) readModelEligible(options RunListOptions) bool {
	if !s.readModelReads || s.sources.ReadModel == nil {
		return false
	}
	if options.LatestPerWorkflow {
		return false
	}
	// Filters outside the closed set are refused by the read model itself, but
	// checking here keeps the refusal a property of the request rather than a
	// surprise from a lower layer — and lets the caller decide.
	if _, err := readmodel.Require(readModelDims(options)); err != nil {
		return false
	}
	return true
}

// readModelDims maps a list request to the filter dimensions the closed set is
// keyed on.
//
// Stage and stage-population are in the supported set as of #1782: they are
// answered from run_stage, which carries the stage predicate, the gaggle scope,
// and the run's own recency on one index. They used to be mapped deliberately
// OUT of the set so a request carrying them took the journal path, because
// serving them there cost ~143 MB read, ~1M unmarshals and ~19,852 journal opens
// in a single request.
//
// Run-level outcome (an `outcome` with no `stage`) and trigger are still outside
// the set and still resolve to a refusal here.
func readModelDims(options RunListOptions) []readmodel.Dim {
	var dims []readmodel.Dim
	if options.Gaggle != "" {
		dims = append(dims, readmodel.DimGaggle)
	}
	if options.Workflow != "" {
		dims = append(dims, readmodel.DimWorkflow)
	}
	if options.Phase != "" {
		dims = append(dims, readmodel.DimPhase)
	}
	if options.Stage != "" {
		dims = append(dims, readmodel.DimStage)
	}
	if options.Outcome != "" {
		dims = append(dims, readmodel.DimOutcome)
	}
	if options.StagePopulation != "" {
		dims = append(dims, readmodel.DimPopulation)
	}
	if !options.Since.IsZero() {
		dims = append(dims, readmodel.DimSince)
	}
	if !options.Until.IsZero() {
		dims = append(dims, readmodel.DimUntil)
	}
	if options.Trigger != "" {
		// Trigger has no covering index and is not in the closed set; naming a
		// dimension the set does not contain is how an unlisted filter becomes a
		// refusal rather than a scan.
		dims = append(dims, readmodel.Dim("trigger"))
	}
	return dims
}

// EnableReadModelReads turns the cutover on for this service.
//
// On by default when a read model is attached (see NewLocal). Kept as an
// explicit call because tests construct services with a store attached and want
// to be unambiguous about which path they are exercising.
func (s *Local) EnableReadModelReads() { s.readModelReads = true }

// DisableReadModelReads forces the journal-derived paths.
//
// This is the rollback §6.6 requires, and it is deliberately a runtime switch
// rather than a rebuild: rolling back must be a flag flip or deleting a file,
// never a deploy. No journal is touched at any step, so the old path is always
// exactly as correct as it was.
func (s *Local) DisableReadModelReads() { s.readModelReads = false }
