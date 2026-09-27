// Package executiondeadline records actual bounded runtime execution without
// deriving deadlines from definition configuration or stage start timestamps.
package executiondeadline

import (
	"context"
	"crypto/rand"
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
	writer, ok := recorder.(appender)
	if !ok || stage == "" {
		return ctx
	}
	if attempt < 1 {
		attempt = 1
	}
	return invoke.WithExecutionDeadlineObserver(ctx, func(deadline time.Time) func() {
		id := rand.Text()
		appendEvent := func(state string) {
			_ = writer.Append(journal.Event{Type: journal.EventRunnerAnnotation, Stage: stage, Attempt: attempt, Time: time.Now().UTC(), Runner: map[string]any{"kind": Kind, "executionId": id, "deadline": deadline.UTC().Format(time.RFC3339Nano), "executionState": state}})
		}
		appendEvent("active")
		var once sync.Once
		return func() { once.Do(func() { appendEvent("finished") }) }
	})
}
