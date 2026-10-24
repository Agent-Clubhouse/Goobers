package acceptance

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/goobers/goobers/internal/triggerqueue"
)

// ErrStaleChildEpoch means child custody is still owned by an earlier lease
// epoch: the current controller has not adopted it yet. It is retryable, and
// distinct from a cancelled or settled parent family.
var ErrStaleChildEpoch = errors.New("cloud acceptance: child custody belongs to an earlier lease epoch")

// childCustodyTables are every table whose rows carry child custody's owner
// epoch. A new child custody table with an owner_epoch column joins this list.
var childCustodyTables = []string{
	"child_parent", "child_occurrence", "child_lineage", "child_authority",
	"child_blob", "child_blob_read", "child_publication",
	"child_disposition", "child_disposition_history", "child_engine_start",
}

// AdoptStaleChildCustody moves every child custody row owned by ANY earlier
// epoch to the current one, in one fenced transaction, so a controller that
// took over the lease (possibly after several failovers) can serve the
// families its predecessors accepted. It returns the rows adopted.
func (s *Store) AdoptStaleChildCustody(ctx context.Context) (int64, error) {
	if s.handle == nil {
		return 0, triggerqueue.ErrTransition
	}
	var adopted int64
	err := s.withFence(ctx, func(txCtx context.Context, tx pgx.Tx, epoch int64) error {
		for _, table := range childCustodyTables {
			tag, err := tx.Exec(txCtx, fmt.Sprintf(`
				UPDATE control.%s SET owner_epoch=$2
				WHERE instance_id=$1 AND owner_epoch<$2`, table),
				s.instanceID, epoch)
			if err != nil {
				return err
			}
			adopted += tag.RowsAffected()
		}
		return nil
	})
	return adopted, err
}
