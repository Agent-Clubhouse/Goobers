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

// Observed reports the most recent bounded execution. A later boundary (a
// completion-repair turn) replaces an earlier one, because the completion the
// caller is judging was produced inside the latest. ok is false when no
// bounded execution began, in which case there is no deadline to judge by.
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
		window.observe(time.Now().UTC(), deadline)
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
