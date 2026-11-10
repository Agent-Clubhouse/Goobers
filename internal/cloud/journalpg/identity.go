package journalpg

import (
	"context"

	"github.com/goobers/goobers/internal/journal"
)

// RunRecorded checks the durable row, including runs whose writer was retired.
// A mismatched gaggle still counts as existing: identity validation must refuse
// that request rather than attempting to replace an existing run.
func (b *Backend) RunRecorded(ctx context.Context, key journal.RunKey) (bool, error) {
	var recorded bool
	err := b.config.Pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM journal.run WHERE instance_id=$1 AND run_id=$2)`, b.config.InstanceID, key.RunID).Scan(&recorded)
	return recorded, err
}

// OpenRunReader uses the same immutable primary-backed header as OpenRun.
func (b *Backend) OpenRunReader(ctx context.Context, key journal.RunKey) (journal.RunReader, error) {
	return b.OpenRun(ctx, journal.RunRef{ID: key.RunID})
}
