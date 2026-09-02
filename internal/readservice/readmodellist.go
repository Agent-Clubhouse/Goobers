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
// intact." Rollback is DisableReadModelReads below, wired to `goobers up
// --disable-read-model-reads` (#2036) — a flag flip or deleting read.db; no
// journal is touched.
//
// For why the default is ON now (it was OFF through Waves 2 and early 3,
// until the projector and bidirectional repair sweep made completeness
// something the store could establish rather than assume), see the
// readModelReads field's doc comment on Local, in readservice.go — the single
// source for that reasoning, not duplicated here.
//
// # What it removes
//
// Measured at 1x, after Wave 1: a 50-row list page opens 51 journals — one per
// returned row, plus the lookahead — because the telemetry.db row is not
// complete enough to answer the list contract, so each run is hydrated from its
// journal. That is the diagnosis's "lists open and parse a journal per returned
// row", and it is the last uncorrected item from it. The read-model path opens
// zero.

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
	if _, err := readmodel.Require(readModelDims(options)); err != nil {
		return RunList{}, err
	}

	request := readmodel.ListOptions{
		Gaggle:   options.Gaggle,
		Workflow: options.Workflow,
		Phase:    options.Phase,
		Since:    options.Since,
		Until:    options.Until,
		Limit:    limit,
		Stage:    options.Stage,
		Outcome:  readmodel.Outcome(options.Outcome),
		// Population is validated by canonicalStagePopulation upstream, and the
		// read model refuses any value its own switch does not recognise, so a
		// bad value cannot reach the query as a column name.
		Population:    readmodel.Population(options.StagePopulation),
		IncludeNoWork: options.ShowNoWork,
	}
	if options.OrderByActivity {
		request.OrderBy = readmodel.OrderLastActivity
	}
	if cursor != nil {
		request.Cursor = readmodel.ListCursor{Key: cursor.StartedAt, RunID: cursor.RunID}
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
	if err := s.decorateOperatorClaims(ctx, out.Runs, observedAt); err != nil {
		return RunList{}, err
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
		NoWork:           row.Disposition == readmodel.DispositionNoWork,
		Operator:         operatorFromReadModel(row, observedAt),
		Stages:           row.Stages,
	}
}

func operatorFromReadModel(row readmodel.RunRow, observedAt time.Time) OperatorRunSummary {
	facts := row.Operator
	operator := OperatorRunSummary{
		CurrentStage:      row.CurrentStage,
		Trajectory:        operatorTrajectory(row.CurrentStage, row.Phase),
		Liveness:          "no-heartbeat",
		PullRequest:       facts.PullRequest,
		PullRequestTitle:  facts.PullRequestTitle,
		PROpenerStage:     facts.PROpenerStage,
		Claim:             OperatorClaim{LeaseStatus: "none", ProviderMarker: "not-recorded"},
		LatestError:       facts.LatestError,
		PotentialBlockers: []string{},
	}
	if facts.IssueNumber != "" || facts.IssueTitle != "" {
		operator.Issue = &OperatorIssue{
			Number: facts.IssueNumber,
			Title:  facts.IssueTitle,
			URL:    facts.IssueURL,
		}
	}
	if facts.LastHeartbeatAt != nil {
		heartbeat := *facts.LastHeartbeatAt
		operator.LastHeartbeatAt = &heartbeat
		age := max(observedAt.Sub(heartbeat).Milliseconds(), 0)
		operator.HeartbeatAgeMillis = &age
	}
	if row.Phase != journal.PhaseRunning {
		operator.Liveness = "terminal"
	} else if row.CurrentStage == "" {
		operator.NextTransition = "start the next workflow stage"
	} else {
		operator.NextTransition = "finish " + row.CurrentStage
	}
	if facts.ProviderClaimRecorded {
		operator.Claim.ProviderMarker = "recorded"
	}
	if facts.ReviewVerdict != "" {
		operator.Review = &OperatorReview{
			Verdict:   facts.ReviewVerdict,
			Rationale: facts.ReviewRationale,
		}
	}
	if facts.ReviewProblem != "" {
		operator.PotentialBlockers = append(operator.PotentialBlockers, facts.ReviewProblem)
	}
	if operator.LatestError != nil {
		operator.PotentialBlockers = append(operator.PotentialBlockers,
			operator.LatestError.Code+": "+operator.LatestError.Message)
	}
	if operator.Review != nil && operator.Review.Verdict != "" &&
		operator.Review.Verdict != "pass" && operator.Review.Verdict != "approve" {
		operator.PotentialBlockers = append(operator.PotentialBlockers,
			"review "+operator.Review.Verdict+": "+operator.Review.Rationale)
	}
	return operator
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
// Run-scoped outcome and trigger are still outside the set and resolve to a
// refusal. Stage-scoped outcome is supported by cumulative per-attempt flags.
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
	if options.OrderByActivity {
		dims = append(dims, readmodel.DimActivity)
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

// DisableReadModelReads forces authoritative journal scans.
//
// This is the rollback §6.6 requires, and it is deliberately a runtime switch
// rather than a rebuild: rolling back must be a flag flip or deleting a file,
// never a deploy. No journal is touched at any step, so the old path is always
// exactly as correct as it was. Reachable via `goobers up
// --disable-read-model-reads` (cmd/goobers/up.go, #2036) — before that the
// only caller was this file's own doc comment.
func (s *Local) DisableReadModelReads() {
	s.readModelReads = false
	s.SetReadMode(ReadModeAuthoritative)
}
