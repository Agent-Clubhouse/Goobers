// Package executiondeadline records actual bounded runtime execution without
// deriving deadlines from definition configuration or stage start timestamps.
package executiondeadline

import (
	"context"
	"crypto/rand"
	"strings"
	"sync"
	"time"

	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/journal"
)

// Kind identifies observational execution boundary annotations.
const Kind = "execution-deadline.v1"

type appender interface{ Append(journal.Event) error }

// clock stamps a boundary's start; replaced only by this package's tests.
var clock = time.Now

// WithRecorder routes the boundary through the existing local or remote journal
// recorder. Failure to retain evidence leaves health unknown, not a failed run.
func WithRecorder(ctx context.Context, recorder any, stage string, attempt int) context.Context {
	if _, ok := recorder.(appender); !ok || stage == "" {
		return ctx
	}
	ctx, _ = Observe(ctx, recorder, stage, attempt)
	return ctx
}

// Window is the bounded execution an invocation actually ran under, as the
// runtime observed it: when the boundary began and the context deadline it
// carried. It is the in-process twin of the journaled annotation, for a caller
// that must judge a claim against the real deadline before the result leaves
// the process (#5871) rather than re-deriving it from configuration.
type Window struct {
	mu       sync.Mutex
	started  time.Time
	deadline time.Time
}

// Observed reports the invocation's FIRST bounded execution: when it began and
// the deadline it carried. A later boundary — a completion-repair turn, whose
// adapters bound it by whatever time the stage has left — never replaces it,
// because judging against a repair turn's leftover budget would make a claim
// made late in the stage look early. ok is false when no bounded execution
// began, in which case there is no deadline to judge by. Both times keep
// their monotonic clock readings, so durations between them are immune to
// wall-clock steps.
func (w *Window) Observed() (started, deadline time.Time, ok bool) {
	if w == nil {
		return time.Time{}, time.Time{}, false
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.started, w.deadline, !w.deadline.IsZero()
}

func (w *Window) observe(started, deadline time.Time) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.deadline.IsZero() {
		return
	}
	w.started, w.deadline = started, deadline
}

// Observe installs the execution-boundary observer and returns the Window it
// fills. The journal annotation is emitted exactly as WithRecorder emits it
// when recorder can append and stage is named; the Window is filled either
// way, so the caller's judgment never depends on journal plumbing.
func Observe(ctx context.Context, recorder any, stage string, attempt int) (context.Context, *Window) {
	window := &Window{}
	writer, journaled := recorder.(appender)
	journaled = journaled && stage != ""
	if attempt < 1 {
		attempt = 1
	}
	return invoke.WithExecutionDeadlineObserver(ctx, func(deadline time.Time) func() {
		window.observe(clock(), deadline)
		if !journaled {
			return func() {}
		}
		// Preserve correlation across journal scrubbing: uppercase random IDs
		// can accidentally resemble credential patterns such as AWS keys.
		id := strings.ToLower(rand.Text())
		appendEvent := func(state string) {
			_ = writer.Append(journal.Event{Type: journal.EventRunnerAnnotation, Stage: stage, Attempt: attempt, Time: time.Now().UTC(), Runner: map[string]any{"kind": Kind, "executionId": id, "deadline": deadline.UTC().Format(time.RFC3339Nano), "executionState": state}})
		}
		appendEvent("active")
		var once sync.Once
		return func() { once.Do(func() { appendEvent("finished") }) }
	}), window
}
