package journal

import (
	"context"
	"errors"
)

const (
	// StreamScheduler is the ordinary instance event stream.
	StreamScheduler = "scheduler"
	// StreamHealth is the gaggle-health transition stream.
	StreamHealth = "health"
)

// StreamCursor is a portable durable instance-log position.
type StreamCursor struct {
	Stream string
	Seq    uint64
}

// StreamCursorStore is the cloud consumer-cursor seam. Local consumers retain
// file generation and byte-offset positions behind their existing adapter.
type StreamCursorStore interface {
	LoadStreamCursor(context.Context, string, string) (StreamCursor, error)
	AdvanceStreamCursor(context.Context, string, StreamCursor, uint64) error
}

// ErrStreamCursorConflict means another consumer incarnation advanced first.
var ErrStreamCursorConflict = errors.New("journal: stream cursor changed")
