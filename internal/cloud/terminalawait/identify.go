package terminalawait

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/startintent"
	"github.com/goobers/goobers/internal/triggerqueue"
)

// ErrUnidentifiable means a run has no ordinary-start acceptance (a generated
// child, or a run this instance did not accept). The awaiter leaves it to its
// own owner.
var ErrUnidentifiable = errors.New("terminal awaiter: run has no ordinary start acceptance")

// ByRun reads the acceptance that owns a run (acceptance.Store.ByRun).
type ByRun interface {
	ByRun(ctx context.Context, runID string) (triggerqueue.Record, error)
}

// IdentifyFromAcceptance resolves a run's identity from its acceptance's start
// envelope; repoFor supplies the gaggle's pinned repository (the applied
// configuration).
func IdentifyFromAcceptance(store ByRun, repoFor func(gaggle string) (apiv1.RepoRef, bool)) func(ctx context.Context, runID string) (Identity, error) {
	return func(ctx context.Context, runID string) (Identity, error) {
		record, err := store.ByRun(ctx, runID)
		if errors.Is(err, sql.ErrNoRows) {
			return Identity{}, fmt.Errorf("%w: %s", ErrUnidentifiable, runID)
		}
		if err != nil {
			return Identity{}, err
		}
		envelope, err := startintent.Parse(record.Payload)
		if err != nil {
			return Identity{}, fmt.Errorf("%w: %s: %w", ErrUnidentifiable, runID, err)
		}
		id := Identity{Gaggle: envelope.Target.Gaggle, Workflow: envelope.Target.Workflow}
		if repo, ok := repoFor(id.Gaggle); ok {
			id.RepoRef = repo
		}
		return id, nil
	}
}

// RunPin is the part of the start fence's pin the frame needs.
type RunPin struct {
	Gaggle, Workflow string
	RepoRef          apiv1.RepoRef
}

// IdentifyFromPins resolves a run's identity from the pin the start fence
// recorded (acceptance.Store.RunPinsOf, through pinsOf). It names the repository
// the run was started against, exactly as the provider writer binds it, instead
// of re-deriving it from whatever configuration is applied now. A run with no
// pin (a generated child, or a run this instance did not start) is
// ErrUnidentifiable.
func IdentifyFromPins(pinsOf func(ctx context.Context, runID string) (RunPin, error)) func(ctx context.Context, runID string) (Identity, error) {
	return func(ctx context.Context, runID string) (Identity, error) {
		pin, err := pinsOf(ctx, runID)
		if errors.Is(err, sql.ErrNoRows) {
			return Identity{}, fmt.Errorf("%w: %s", ErrUnidentifiable, runID)
		}
		if err != nil {
			return Identity{}, err
		}
		return Identity{Gaggle: pin.Gaggle, Workflow: pin.Workflow, RepoRef: pin.RepoRef}, nil
	}
}
