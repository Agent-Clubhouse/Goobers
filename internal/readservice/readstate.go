package readservice

import (
	"context"
	"time"

	"github.com/goobers/goobers/internal/readmodel"
)

// The readState envelope on the wire (#1927, design §7.2).
//
// # Why an embedded struct rather than a field per type
//
// Twelve read response types, no shared base. Embedding puts the field on all of
// them with one line each, and — the part that matters — the JSON field is
// PROMOTED, so `readState` appears at the top level of every response rather
// than nested. A client reads `response.readState` uniformly.
//
// It also keeps the generated TypeScript contract working: the generator walks
// Go types, so an embedded struct produces the field in `wire.generated.ts`
// automatically. Injecting the field at encode time would have been fewer lines
// and would have produced an untyped field the portal could not consume, which
// is the shortcut this avoids.
//
// # Why the field is a pointer
//
// Nil when no read model is attached — the CLI and standalone topologies, and a
// daemon whose store failed to open. A zero-valued envelope would be worse than
// none: it would report epoch "" and lag 0, which reads as "perfectly current"
// rather than "unknown".

// ReadStateEnvelope is embedded in every read response.
type ReadStateEnvelope struct {
	// ReadState describes how current the data in this response is. Absent when
	// the response was not served from the read model.
	ReadState *readmodel.ReadState `json:"readState,omitempty"`
}

// intakeDepth is what the service can learn about pending watermarks.
//
// An interface rather than *intake.Store so readservice does not depend on the
// intake package: the read path must not be able to reach a store it is not
// allowed to write, and importing it would put that capability one keystroke
// away (§3.1).
type intakeDepth interface {
	Count(ctx context.Context) (int, error)
}

// intakeAge is the optional half of the intake surface: how long the oldest
// waiting watermark has waited.
//
// Separate from intakeDepth rather than folded into it so a depth source that
// cannot answer it still attaches — the envelope degrades to "count only"
// instead of losing the count as well.
type intakeAge interface {
	OldestPending(ctx context.Context) (time.Time, bool, error)
}

// ProjectionHealth is what the projector knows about its own currency.
//
// Declared here rather than reusing the projector's Stats so the read path does
// not import the package that writes the projection (§3.1) — the same reason
// intakeDepth is an interface.
type ProjectionHealth struct {
	// ApplyFailures counts runs the projector could not apply. Each one is a
	// known gap in the projection.
	ApplyFailures int
	// LastDrainAt is when the projector last completed an intake pass. Zero
	// means it has not completed one since start.
	LastDrainAt time.Time
}

// Optional freshness metadata must not consume the primary read's route budget.
const readStateTimeout = 100 * time.Millisecond

// AttachIntakeDepth supplies the pending-intake counter used by the envelope.
//
// Optional. Without it the envelope still reports epoch, applied sequence,
// retention floor, and sweep age — it simply cannot report how many watermarks
// are waiting, and says so by leaving pendingIntake at zero with the sweep-age
// bound still in force.
func (s *Local) AttachIntakeDepth(depth intakeDepth) { s.intakeDepth = depth }

// AttachProjectionHealth supplies the projector's view of its own currency.
//
// Optional, and for the same reason as AttachIntakeDepth: a topology with no
// projector (the CLI, a standalone reader) has nothing to report. Without it the
// envelope cannot see apply failures, so a gap the projector already knows about
// stays invisible to the operator reading the response.
func (s *Local) AttachProjectionHealth(health func() ProjectionHealth) {
	s.projectionHealth = health
}

// readStateEnvelope builds the envelope for a response.
//
// Errors are swallowed deliberately. This is metadata ABOUT an answer that has
// already been computed successfully; failing the request because the freshness
// annotation could not be assembled would let a diagnostic break the thing it
// exists to describe.
func (s *Local) readStateEnvelope(ctx context.Context) ReadStateEnvelope {
	if s.sources.ReadModel == nil {
		return ReadStateEnvelope{}
	}
	stateful, ok := s.sources.ReadModel.(readmodel.FreshnessReporter)
	if !ok {
		return ReadStateEnvelope{}
	}

	ctx, cancel := context.WithTimeout(ctx, readStateTimeout)
	defer cancel()

	input := readmodel.ReadStateInput{}
	if s.intakeDepth != nil {
		if pending, err := s.intakeDepth.Count(ctx); err == nil {
			input.PendingIntake = pending
		}
		if aged, ok := s.intakeDepth.(intakeAge); ok {
			if oldest, found, err := aged.OldestPending(ctx); err == nil && found {
				input.OldestPendingAt = oldest
			}
		}
	}
	if s.projectionHealth != nil {
		health := s.projectionHealth()
		input.ProjectFailures = health.ApplyFailures
		// Time since the projector last finished a pass. A projector that has
		// stopped shows a lag that keeps growing, which is the signal a pending
		// count alone cannot give: an empty intake table and a dead projector
		// look identical without it.
		if !health.LastDrainAt.IsZero() {
			input.ProjectionLagSeconds = s.now().Sub(health.LastDrainAt).Seconds()
		}
	}
	state, err := stateful.ReadState(ctx, input)
	if err != nil {
		return ReadStateEnvelope{}
	}
	return ReadStateEnvelope{ReadState: &state}
}

// setReadState is promoted onto every type embedding ReadStateEnvelope, which
// is what lets one helper annotate all twelve response types without a switch
// or a setter per type.
func (e *ReadStateEnvelope) setReadState(state ReadStateEnvelope) { *e = state }

// annotated attaches the freshness envelope to a response.
//
// The type-parameter pair is Go's idiom for "a T whose pointer implements this
// interface": P constrains *T, so the promoted setter is reachable while callers
// still pass and receive a value.
func annotated[T any, P interface {
	*T
	setReadState(ReadStateEnvelope)
}](ctx context.Context, s *Local, value T) T {
	P(&value).setReadState(s.readStateEnvelope(ctx))
	return value
}
