package readmodel

import (
	"sort"
	"time"

	"github.com/goobers/goobers/internal/journal"
)

// Projection: journal events to a run row.
//
// §10 requires projector units to be "a pure function (prevRow, events) →
// nextRow", and §14.9 makes determinism a tested property — rebuilding from the
// same journals must produce byte-identical canonical rows.
//
// Purity is not decoration here. It is what makes three separate things
// possible: incremental projection (apply only the tail past last_seq),
// rebuild-equals-incremental (the same function over all events), and a
// differential oracle against the journal-derived path (§14.7). A projection
// that reached for the clock or the filesystem would satisfy none of them.
//
// # What is deliberately absent
//
// No duration. It is now-relative for a running run, so storing it would freeze
// a quiet in-flight run's age at projection time (§5.3). The read contract
// computes it at query time from started_at and finished_at.
//
// No filesystem access. state.json is NOT consulted, even though
// summarizeRunForStage reads it to refine current_stage for a running run. The
// journal is the authoritative record (§3.2) and state.json can lag a
// crash-fsynced run.finished; a projection that mixed the two would be
// non-deterministic in exactly the case that matters. The projector's
// current_stage therefore comes from events alone.

// RunRow is one projected run — the complete row a list page is answered from,
// with no journal open.
type RunRow struct {
	RunID           string
	Gaggle          string
	Workflow        string
	WorkflowVersion int
	WorkflowDigest  string
	GooberDigest    string
	TriggerKind     string
	TriggerRef      string

	Phase        journal.RunPhase
	Terminal     bool
	CurrentStage string
	StartedAt    time.Time
	FinishedAt   *time.Time
	LastActivity time.Time
	LastSeq      uint64

	RepassCount      int
	RetryCount       int
	PolicyRetryCount int
	InfraRetryCount  int

	OutcomeVerdict string
	OutcomeTarget  string

	// Stages is every stage or gate the run has touched, sorted. It backs the
	// stage filter without a join (§5.7's run-level rollup principle).
	Stages []string

	// scratch holds nullable columns between Scan and decode. Unexported and
	// cleared by finishScan, so it never escapes into a returned row.
	scratch *nullables
}

// StageRow is one projected (run, stage) pair.
type StageRow struct {
	RunID            string
	Stage            string
	Attempts         int
	LastStatus       string
	LastAttemptClass string
	StartedAt        *time.Time
	FinishedAt       *time.Time
}

// Projection is the full result of projecting a run.
type Projection struct {
	Run    RunRow
	Stages []StageRow
}

// ProjectRun folds a run's identity and events into a projection.
//
// prev is the FULL previous projection — run row and stage rows — or the zero
// value for a first projection. events must be the tail beyond
// prev.Run.LastSeq, in sequence order, or the complete history when prev is
// zero. Both produce the same result for the same total event set, which is what
// makes rebuild and incremental projection interchangeable.
//
// prev carries the stage rows, not just the run row, and that is not an
// ergonomic choice. Stage accumulators (attempt counts, first started_at) are
// per-run state that spans batches: an earlier version took only the run row,
// and a run projected incrementally had its stage attempt counts reset on every
// tail — while the same run projected in one pass was correct. The
// incremental-equals-whole test is what surfaced it, which is precisely why that
// test enumerates every split point rather than checking one.
func ProjectRun(identity journal.RunIdentity, prev Projection, events []journal.Event) Projection {
	row := prev.Run
	row.RunID = identity.RunID
	row.Gaggle = identity.Gaggle
	row.Workflow = identity.Workflow
	row.WorkflowVersion = identity.WorkflowVersion
	row.WorkflowDigest = identity.WorkflowDigest
	row.TriggerKind = string(identity.Trigger.Kind)
	row.TriggerRef = identity.Trigger.Ref
	row.StartedAt = identity.StartedAt

	// A run with no terminal event is running. Seeding here rather than only on
	// an event means a first projection of an in-flight run is correct, and a
	// re-projection of a previously-running run does not lose its phase.
	if row.Phase == "" {
		row.Phase = journal.PhaseRunning
	}

	seenStages := stageSet(prev.Run.Stages)
	stages := carryStages(prev.Stages)

	for _, event := range events {
		if event.Seq > row.LastSeq {
			row.LastSeq = event.Seq
			row.LastActivity = event.Time
		}
		// An event from a schema this build does not know still advances the
		// sequence and activity time — it happened — but its semantics are not
		// guessed at. Mirrors summarizeRunForStage.
		if !event.KnownSchema() {
			continue
		}
		if event.Stage != "" {
			seenStages[event.Stage] = struct{}{}
		}
		if event.Gate != "" {
			seenStages[event.Gate] = struct{}{}
		}

		switch event.Type {
		case journal.EventRunResumed:
			// A resume reopens a terminal run. Clearing finished_at matters:
			// leaving it would make a live run look finished to every list.
			row.Phase = journal.PhaseRunning
			row.FinishedAt = nil
			row.CurrentStage = event.Target
			row.OutcomeVerdict, row.OutcomeTarget = "", ""
		case journal.EventStageStarted:
			row.CurrentStage = event.Stage
			s := stageRow(stages, row.RunID, event.Stage)
			s.Attempts++
			if s.StartedAt == nil {
				at := event.Time
				s.StartedAt = &at
			}
		case journal.EventStageFinished:
			if row.CurrentStage == event.Stage {
				row.CurrentStage = ""
			}
			s := stageRow(stages, row.RunID, event.Stage)
			s.LastStatus = event.Status
			s.LastAttemptClass = string(event.AttemptClass)
			at := event.Time
			s.FinishedAt = &at
			countAttempt(&row, event)
		case journal.EventGateStarted:
			row.CurrentStage = event.Gate
		case journal.EventGateEvaluated:
			if row.CurrentStage == event.Gate {
				row.CurrentStage = ""
			}
		case journal.EventStageRerunRequested:
			// A repass reopens the run for the same reason a resume does.
			row.Phase = journal.PhaseRunning
			row.FinishedAt = nil
			row.CurrentStage = event.Stage
			row.RepassCount++
		case journal.EventRunFinished:
			phase := journal.RunPhase(event.Status)
			// An unrecognised terminal status is NOT silently accepted: writing
			// it would make phase — an indexed, filtered column — carry a value
			// no query predicate expects, and the run would quietly vanish from
			// every phase-filtered list. Leave the run as it was; repair and the
			// differential oracle surface the discrepancy.
			if !terminalPhase(phase) {
				continue
			}
			row.Phase = phase
			finished := event.Time
			row.FinishedAt = &finished
			row.CurrentStage = ""
		}
	}

	row.Terminal = row.Phase != journal.PhaseRunning
	row.Stages = sortedStages(seenStages)

	out := make([]StageRow, 0, len(stages))
	for _, s := range stages {
		out = append(out, *s)
	}
	// Sorted so a rebuild produces byte-identical rows in a stable order
	// (§14.9); map iteration order would defeat that.
	sort.Slice(out, func(i, j int) bool { return out[i].Stage < out[j].Stage })

	return Projection{Run: row, Stages: out}
}

// countAttempt folds a finished stage attempt into the run's retry counters.
//
// The three classes are distinct on purpose (#849): a policy retry is the
// workflow deciding to try again, an infra retry is the machinery failing, and
// the total matters for neither. Collapsing them would make "is this workflow
// flaky or is the runner" unanswerable.
func countAttempt(row *RunRow, event journal.Event) {
	switch event.AttemptClass {
	case journal.AttemptPolicy:
		row.PolicyRetryCount++
		row.RetryCount++
	case journal.AttemptInfra:
		row.InfraRetryCount++
		row.RetryCount++
	}
}

// carryStages rebuilds the mutable stage accumulators from a previous
// projection, so an incremental pass continues counting rather than restarting.
func carryStages(prev []StageRow) map[string]*StageRow {
	out := make(map[string]*StageRow, len(prev))
	for i := range prev {
		row := prev[i]
		out[row.Stage] = &row
	}
	return out
}

// stageRow fetches or creates the accumulator for a stage.
func stageRow(stages map[string]*StageRow, runID, stage string) *StageRow {
	if s, ok := stages[stage]; ok {
		return s
	}
	s := &StageRow{RunID: runID, Stage: stage}
	stages[stage] = s
	return s
}

// stageSet rebuilds the seen-stage set from a previously projected row, so an
// incremental projection does not forget stages it has already recorded.
func stageSet(stages []string) map[string]struct{} {
	out := make(map[string]struct{}, len(stages))
	for _, s := range stages {
		out[s] = struct{}{}
	}
	return out
}

// sortedStages returns the stage set in a stable order.
func sortedStages(set map[string]struct{}) []string {
	out := make([]string, 0, len(set))
	for s := range set {
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

// terminalPhase reports whether phase is a recognised terminal run phase.
//
// PhaseRunning is excluded deliberately: a run.finished carrying "running" is
// contradictory, and accepting it would leave a row marked terminal while its
// phase says otherwise.
func terminalPhase(phase journal.RunPhase) bool {
	switch phase {
	case journal.PhaseCompleted, journal.PhaseFailed, journal.PhaseEscalated, journal.PhaseAborted:
		return true
	default:
		return false
	}
}
