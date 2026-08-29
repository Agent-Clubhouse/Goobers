package readmodel

import (
	"context"

	"github.com/goobers/goobers/internal/journal"
)

// The read-model store seam (#1921, design §13.2).
//
// # What this is, and what it is not
//
// It is the semantic surface later waves write their queries against. It is NOT
// a portability layer, and it does not come with a second maintained backend —
// that is the hosted wave (#652). Nothing here runs a second database in CI.
//
// # Why a seam now rather than at the hosted wave
//
// Writing another dozen queries against SQLite and then adding a seam is the
// expensive order. The cost worth paying early is SEMANTIC: the interface fixes
// what a read-model backend must MEAN, so that the next twenty queries are
// written against behaviour that travels rather than against whatever SQLite
// happened to do.
//
// # The SQLite shapes that do not travel
//
// The issue names four. Their status here, checked rather than assumed:
//
//   - `julianday()` — in rollup's migrations v14/v15, not in the read model.
//     The read model stores RFC3339 nanosecond strings and does all time
//     arithmetic in Go, so no date function crosses the seam.
//   - `INSERT OR IGNORE` — not used. The upsert is an explicit ON CONFLICT with
//     a guard predicate, because "ignore" and "ignore unless this is newer" are
//     different operations and only the second is correct (see write.go).
//   - `_pragma` DSN configuration — confined to store.go's two DSN constants.
//     It is construction, and construction is deliberately NOT on this
//     interface: a backend is CONFIGURED in backend-specific terms and then
//     USED in neutral ones.
//   - raw-DDL migration strings — confined to schema.go, behind migrate().
//     Also construction.
//
// # ATTACH
//
// The acceptance criterion asks that ATTACH be isolated so it can become two
// schemas in one database later. The read model uses none today — the finding
// is that the ATTACH debt is rollup's, not this store's. Wave 3 is where it
// would arrive, with intake.db beside read.db, and the seam is what keeps that
// choice from reaching the queries: Reader and Writer name no file and no
// schema, so intake living in a separate file, an attached schema, or a second
// table set is a construction detail on the far side of this interface.

// Reader is the bounded read surface.
//
// Every method is bounded by construction: a page limit, a workflow-scoped
// aggregate, a single run, or a phase histogram. There is deliberately no
// "run arbitrary query" escape hatch — one would make §5.7's closed set
// advisory, and the closed set is the only thing standing between the portal
// and the scans this whole design exists to remove.
type Reader interface {
	// State reports the projection's identity and retention floor: the epoch,
	// the oldest change still readable, and the floor positions have to clear.
	State(ctx context.Context) (State, error)

	// ListRuns returns one bounded page. It MUST refuse any filter combination
	// outside the closed set with *UnsupportedCombinationError rather than
	// answering it slowly.
	ListRuns(ctx context.Context, options ListOptions) (ListPage, error)

	// LatestPerWorkflow returns each workflow's latest run and active count in
	// one request, with no per-workflow follow-up read.
	LatestPerWorkflow(ctx context.Context, options AggregateOptions) ([]WorkflowLatest, error)

	// CreditAssignment ranks graph nodes by their contribution to adverse run
	// outcomes and repeated attempts.
	CreditAssignment(ctx context.Context, options CreditOptions) ([]NodeCredit, error)
	CausalCredit(ctx context.Context, options CausalOptions) ([]CausalNodeCredit, error)

	// GetRun returns one projected run. The bool distinguishes absent from
	// failed — a run that is not in the read model is an ordinary answer, and
	// collapsing it into an error would make retention indistinguishable from
	// breakage.
	GetRun(ctx context.Context, runID string) (RunRow, bool, error)

	// CountByPhase is the Overview's histogram, as one aggregate.
	CountByPhase(ctx context.Context) (map[journal.RunPhase]int, error)

	// ActiveRunCounts returns the number of running runs for each workflow.
	ActiveRunCounts(ctx context.Context) ([]WorkflowCount, error)

	// Changes returns committed transitions after a position, in commit order.
	Changes(ctx context.Context, afterSeq uint64, limit int) ([]Change, error)

	// LatestChangeSeq is the newest committed position.
	LatestChangeSeq(ctx context.Context) (uint64, error)
}

// WorkflowCount is an active-run count keyed by workflow identity.
type WorkflowCount struct {
	Gaggle   string
	Workflow string
	Count    int
}

// Writer is the projection surface.
//
// It is separate from Reader because they have different callers and different
// numbers of them: exactly one projector writes, and everything else reads.
// Splitting them is what lets a read path be handed something it provably
// cannot mutate.
type Writer interface {
	// UpsertRun applies a projection.
	//
	// It MUST be a guarded acknowledgement, not a blind write: a projection
	// carrying a source position at or behind the stored one must not overwrite
	// it. That is what makes replay safe, and replay is not exceptional — it is
	// how a rebuild, a resumed build, and a repair sweep all work.
	UpsertRun(ctx context.Context, p Projection) error

	// PruneChanges drops change rows below keepFrom and advances the retention
	// floor in the same commit. The floor MUST only advance; a floor that moved
	// backwards would let a client resume from a position whose changes are
	// already gone.
	PruneChanges(ctx context.Context, keepFrom uint64) (int64, error)
}

// Intake is the source-watermark surface (#1922, §3.1).
//
// It is the THIRD capability, separate from both Reader and Writer, and the
// separation is the design: intake is written by processes that are neither the
// projector nor a read path — `goobers run`, the stalled-run terminalizer, and
// retention — while holding no ability to touch projected facts.
//
// It is also a different DATABASE (intake.db), not merely a different interface.
// That is forced rather than chosen: SQLite's WAL gives no atomic commit across
// attached databases, so the acknowledgement cannot join the projection
// transaction and must be a guarded post-commit write instead. See the intake
// package for the protocol.
//
// Both methods are advisory. A failure here is logged and counted, never fatal
// to the run — the run's discovery falls back to the repair sweep. Making
// execution depend on a read-model hint would invert the dependency this whole
// design exists to establish.
type Intake interface {
	// Observed records that a run's journal has advanced to journalSeq.
	Observed(ctx context.Context, runID string, journalSeq uint64) error

	// Removing records retention's intent to delete a run, BEFORE the journal
	// is unlinked. Ordering is intent → unlink → project → confirm; recording
	// intent first is what makes an interrupted retention pass recoverable.
	Removing(ctx context.Context, runID string) error
}

// FreshnessReporter is the freshness surface (#1927).
//
// Separate from Reader because it is optional: a backend that cannot describe
// its own currency is still a usable read model, it simply cannot carry a
// readState envelope. Folding these into Reader would make every future backend
// implement them before it could answer a single query.
type FreshnessReporter interface {
	ReadState(ctx context.Context, input ReadStateInput) (ReadState, error)
	SourceApplied(ctx context.Context, runID string) (SourcePosition, bool, error)
	SatisfiesSourceApplied(ctx context.Context, required SourcePosition) (bool, error)
}

// Store satisfies FreshnessReporter.
var _ FreshnessReporter = (*Store)(nil)

// Backend is a complete read-model store.
type Backend interface {
	Reader
	Writer
}

// Store satisfies Backend. The assertion is here rather than nowhere so that a
// signature drifting away from the seam fails to compile instead of silently
// leaving the interface behind.
var _ Backend = (*Store)(nil)
