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

	// GetRun returns one projected run. The bool distinguishes absent from
	// failed — a run that is not in the read model is an ordinary answer, and
	// collapsing it into an error would make retention indistinguishable from
	// breakage.
	GetRun(ctx context.Context, runID string) (RunRow, bool, error)

	// CountByPhase is the Overview's histogram, as one aggregate.
	CountByPhase(ctx context.Context) (map[journal.RunPhase]int, error)

	// Changes returns committed transitions after a position, in commit order.
	Changes(ctx context.Context, afterSeq uint64, limit int) ([]Change, error)

	// LatestChangeSeq is the newest committed position.
	LatestChangeSeq(ctx context.Context) (uint64, error)
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

// Backend is a complete read-model store.
type Backend interface {
	Reader
	Writer
}

// Store satisfies Backend. The assertion is here rather than nowhere so that a
// signature drifting away from the seam fails to compile instead of silently
// leaving the interface behind.
var _ Backend = (*Store)(nil)
