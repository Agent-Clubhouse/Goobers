package harness

import (
	"errors"
	"fmt"
	"sync"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
)

// maxTranscriptWindow bounds the redacted tail handed to a window observer.
const maxTranscriptWindow = 32 << 10

// TranscriptWindowObserver receives a bounded, redacted tail of a running
// stage's transcript each time a checkpoint delta arrives, and a final label
// saying whether the stage hit its time budget. It is advisory: it rides the
// existing checkpoint cadence (no per-turn cost), must not block, and cannot
// change the stage.
type TranscriptWindowObserver interface {
	ObserveWindow(env apiv1.InvocationEnvelope, stream string, window []byte)
	FinishStage(env apiv1.InvocationEnvelope, budgetHit bool)
}

// WithTranscriptWindowObserver registers an advisory window observer. Nil
// leaves behavior unchanged.
func WithTranscriptWindowObserver(o TranscriptWindowObserver) Option {
	return func(e *Executor) { e.windows = o }
}

// transcriptWindowFeed keeps a raw bounded tail per stream and redacts the
// whole tail on every update, so a secret split across two checkpoint deltas
// is still caught before any observer sees it.
type transcriptWindowFeed struct {
	mu       sync.Mutex
	env      apiv1.InvocationEnvelope
	scrubber journal.Scrubber
	observer TranscriptWindowObserver
	tails    map[string][]byte
}

func (f *transcriptWindowFeed) add(delta InvocationTranscriptDelta) {
	if len(delta.Data) == 0 {
		return
	}
	stream := fmt.Sprintf("%s/%d", delta.Source, delta.Invocation)
	f.mu.Lock()
	tail := append(f.tails[stream], delta.Data...)
	if len(tail) > maxTranscriptWindow {
		tail = append([]byte(nil), tail[len(tail)-maxTranscriptWindow:]...)
	}
	f.tails[stream] = tail
	window := f.scrubber.Scrub(append([]byte(nil), tail...))
	f.mu.Unlock()
	f.observer.ObserveWindow(f.env, stream, window)
}

func (f *transcriptWindowFeed) safeAdd(delta InvocationTranscriptDelta) {
	defer func() { _ = recover() }()
	f.add(delta)
}

func (e *Executor) teeTranscriptWindows(env apiv1.InvocationEnvelope, req *RunRequest) {
	if e.windows == nil {
		return
	}
	feed := &transcriptWindowFeed{env: env, scrubber: e.scrubber, observer: e.windows, tails: map[string][]byte{}}
	inner := req.TranscriptCheckpoint
	req.TranscriptCheckpoint = func(delta InvocationTranscriptDelta) error {
		feed.safeAdd(delta)
		if inner != nil {
			return inner(delta)
		}
		return nil
	}
}

func (e *Executor) finishTranscriptWindows(env apiv1.InvocationEnvelope, runErr error) {
	if e.windows == nil {
		return
	}
	defer func() { _ = recover() }()
	e.windows.FinishStage(env, errors.Is(runErr, ErrTimeout))
}
