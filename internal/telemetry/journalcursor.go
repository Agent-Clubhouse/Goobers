package telemetry

import "github.com/goobers/goobers/internal/journal"

// JournalStreamCursor is the cloud catch-up position. Local catch-up keeps its
// path/generation/byte-offset cursor behind the existing file adapter.
type JournalStreamCursor = journal.StreamCursor

// JournalStreamCursorStore is the persistence seam used by cloud export
// catch-up. PostgreSQL keys it by (consumer, stream); local storage remains
// path-keyed and offset-based.
type JournalStreamCursorStore = journal.StreamCursorStore
