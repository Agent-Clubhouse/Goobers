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
