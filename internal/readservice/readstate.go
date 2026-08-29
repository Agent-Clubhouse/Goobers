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

// Optional freshness metadata must not consume the primary read's route budget.
const readStateTimeout = 100 * time.Millisecond

// AttachIntakeDepth supplies the pending-intake counter used by the envelope.
//
// Optional. Without it the envelope still reports epoch, applied sequence,
// retention floor, and sweep age — it simply cannot report how many watermarks
// are waiting, and says so by leaving pendingIntake at zero with the sweep-age
// bound still in force.
func (s *Local) AttachIntakeDepth(depth intakeDepth) { s.intakeDepth = depth }

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
