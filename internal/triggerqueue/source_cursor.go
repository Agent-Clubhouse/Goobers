package triggerqueue

import (
	"context"
	"database/sql"
)

const sourceCursorSchema = `CREATE TABLE source_start_cursors (scope TEXT PRIMARY KEY NOT NULL, cursor_ns INTEGER NOT NULL, legacy_pending INTEGER NOT NULL DEFAULT 0);`

func advanceSourceCursor(ctx context.Context, tx *sql.Tx, a *SourceAdvance) error {
	if a == nil {
		return nil
	}
	result, err := tx.ExecContext(ctx, `UPDATE source_start_cursors SET cursor_ns=?,legacy_pending=0 WHERE scope=? AND cursor_ns=? AND revision=?`, a.After.UnixNano(), a.Scope, a.Before.UnixNano(), a.Revision)
	return changed(result, err)
}
