package runsemantics

import (
	"github.com/goobers/goobers/internal/journal"
)

// IsInterruptedAttemptMarker recognizes the journal marker for interrupted work.
func IsInterruptedAttemptMarker(e journal.Event) bool {
	marker, ok := e.Runner[InterruptedAttemptMarkerKey].(bool)
	return e.Type == journal.EventStageFinished && ok && marker
}
