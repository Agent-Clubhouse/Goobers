package rollup

import "github.com/goobers/goobers/internal/journal"

// InstanceCursor is the cloud rollup position. The local scheduler ingest path
// retains generation and byte offset in scheduler_ingest_cursor.
type InstanceCursor = journal.StreamCursor

// InstanceCursorStore lets cloud rollup persist (stream, seq) independently of
// the local SQLite offset implementation.
type InstanceCursorStore = journal.StreamCursorStore
