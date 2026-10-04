package runner

import (
	"time"

	"github.com/goobers/goobers/internal/journal"
)

// RunExecutionElapsed excludes durable child waiting from the ordinary maximum
// run duration. Stage process timeouts already end when the stopped invocation
// joins. Explicit caller cancellation and deadlines are not changed here.
func RunExecutionElapsed(events []journal.Event, startedAt, now time.Time) (time.Duration, error) {
	return journal.ChildExecutionElapsed(events, startedAt, now, nil)
}
