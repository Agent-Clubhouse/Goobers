package executor

import (
	"context"

	"github.com/goobers/goobers/internal/journal"
)

type journalPlaneKey struct{}

// JournalPlane carries only a run-scoped journal token, never a worker signing
// key or the privileged stage-pod surrender/credential token.
type JournalPlane struct{ Endpoint, Token string }

// WithJournalPlane attaches trusted run-scoped authority to one execution context.
func WithJournalPlane(ctx context.Context, plane JournalPlane) context.Context {
	return context.WithValue(ctx, journalPlaneKey{}, plane)
}

// JournalPlaneFromContext returns the authority attached by the worker runtime.
func JournalPlaneFromContext(ctx context.Context) (JournalPlane, bool) {
	plane, ok := ctx.Value(journalPlaneKey{}).(JournalPlane)
	return plane, ok
}

func registerJournalPlane(ctx context.Context, registry *journal.RegistryScrubber) {
	if plane, ok := JournalPlaneFromContext(ctx); ok {
		registry.Register([]byte(plane.Token))
	}
}
