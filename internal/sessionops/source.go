package sessionops

import (
	"context"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/interactiveaccess"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/sessioning"
	"github.com/goobers/goobers/internal/workbench"
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

// BacklogWriter is bound to the same exact live lease as source reads. The
// command key is namespaced by the trusted bridge to session+turn before use.
// Implementations retain acceptance and one-attempt custody before effects.
type BacklogWriter interface {
	Capabilities(context.Context, string) (workbench.BacklogWriteCapabilities, error)
	Patch(context.Context, string, string, workbench.BacklogPatchRequest) (workbench.BacklogEditCommand, error)
	Command(context.Context, string, string) (workbench.BacklogEditCommand, error)
}

// WriterFactory returns nil when no native edit is permitted for this turn.
// It uses the existing execution lease and must not reacquire policy locking.
type WriterFactory func(context.Context, SourceContext) (BacklogWriter, error)
