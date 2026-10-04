package sessionops

import (
	"context"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/interactiveaccess"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/sessioning"
)

// SourceContext is supplied by the actual runner, never an HTTP/tool caller.
// RetainedGaggle locates the archived model turn's source declarations; the
// factory must intersect those with the live lease's permitted current targets.
type SourceContext struct {
	Identity       journal.RunIdentity
	Actor          sessioning.Actor
	Lease          *interactiveaccess.ExecutionLease
	RetainedGaggle apiv1.Gaggle
}

// ReaderFactory binds source reads to an already-open live human lease. It must
// not resolve a broad automation credential or reacquire the policy RW lock.
type ReaderFactory func(context.Context, SourceContext) (BacklogReader, error)
