package tracefollow

import (
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/readservice"
)

// EventsTerminal reports whether the latest finish/resume event is a finish.
func EventsTerminal(events []readservice.RunEvent) bool {
	for i := len(events) - 1; i >= 0; i-- {
		switch events[i].Type {
		case journal.EventRunResumed:
			return false
		case journal.EventRunFinished:
			return true
		}
	}
	return false
}
