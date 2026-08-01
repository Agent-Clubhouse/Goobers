package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/goobers/goobers/internal/apicontract"
)

// The SSE endpoint: framing, flushing, and heartbeats.
//
// # The filesystem poller is gone (#1929)
//
// This file used to be 1,025 lines. Most of it was a SECOND change detector:
// a 100ms ticker over the run directories, byte-offset tailing, a 5-second
// sweep that stat'd every run that had ever existed, per-run offset and digest
// state retained for all history, an in-memory event ring, and a random
// per-process session id that meant a client could only resume against the
// process that served it.
//
// It discovered change independently of the projection, so the two had
// different latency, completeness, and failure modes. That is why an in-flight
// run was visible to one and not the other, and why several symptoms that
// presented as interface bugs were downstream of there being no single ordered
// notion of change.
//
// What remains is the transport: taking events from an eventSource and writing
// them as SSE frames. The only source now is the change feed (feedstream.go),
// which tails rows the projector wrote in the same transaction as the facts
// they describe.
//
// A topology with no read model gets no live updates rather than a second
// detector. That absence is reported by the freshness surface (#1928) rather
// than being silent.

// StreamEvent is one SSE frame.
type StreamEvent struct {
	ID   string
	Type string
	Data Invalidation
}

// Invalidation and WorkflowRef are the wire payload.
type Invalidation = apicontract.Invalidation

// WorkflowRef identifies one workflow read model.
type WorkflowRef = apicontract.WorkflowRef

// Cursor refusals surfaced by a source.
var (
	// ErrInvalidEventCursor is a Last-Event-ID that cannot be parsed.
	ErrInvalidEventCursor = errors.New("httpapi: invalid event cursor")
	// ErrStaleEventCursor is a cursor a source cannot resume from. The change
	// feed supersedes it with three NAMED conditions (epoch_changed,
	// feed_truncated, schema_changed), which is why the generic form survives
	// only as a fallback arm in the handler.
	ErrStaleEventCursor = errors.New("httpapi: stale event cursor")
	// ErrEventStreamClosed is a subscription attempt against a stopped source.
	ErrEventStreamClosed = errors.New("httpapi: event stream closed")
)

// defaultEventWriteTimeout bounds one SSE frame write.
const defaultEventWriteTimeout = 5 * time.Second

func registerEventRoute(router *Router, stream eventSource) {
	router.Handle(apicontract.RouteEvents, func(w http.ResponseWriter, request *http.Request) {
		initial, events, cancel, err := stream.Subscribe(request.Header.Get("Last-Event-ID"))
		if err != nil {
			// A change-feed refusal names WHICH condition applies
			// (epoch_changed / feed_truncated / schema_changed), because the
			// client's correct response differs per condition. Checked before
			// the generic arms so it is not flattened into stale_cursor.
			if code, message, ok := cursorConditionStatus(err); ok {
				writeError(w, http.StatusConflict, code, message)
				return
			}
			switch {
			case errors.Is(err, ErrInvalidEventCursor):
				writeError(w, http.StatusBadRequest, "invalid_cursor", "Last-Event-ID is invalid")
			case errors.Is(err, ErrStaleEventCursor):
				writeError(w, http.StatusConflict, "stale_cursor", "event history expired; refetch current read endpoints and reconnect without Last-Event-ID")
			default:
				writeError(w, http.StatusServiceUnavailable, "stream_unavailable", "event stream is shutting down")
			}
			return
		}
		defer cancel()

		_, ok := w.(http.Flusher)
		if !ok {
			writeError(w, http.StatusInternalServerError, "stream_unsupported", "streaming is not supported by this server")
			return
		}
		controller := http.NewResponseController(w)
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("X-Accel-Buffering", "no")
		w.WriteHeader(http.StatusOK)
		// Flush the headers before any event so a resume whose cursor is
		// already up to date — and therefore has no initial events — still
		// establishes immediately instead of waiting for the heartbeat.
		if err := controller.Flush(); err != nil {
			return
		}

		shutdown := stream.Done()
		for _, event := range initial {
			if stopped(shutdown, request.Context()) {
				return
			}
			if err := writeAndFlushSSE(controller, w, event, stream.WriteTimeout()); err != nil {
				return
			}
		}

		heartbeat := time.NewTicker(stream.Heartbeat())
		defer heartbeat.Stop()
		for {
			// Checked ahead of the select so a closed-but-buffered event
			// channel cannot keep draining past shutdown.
			if stopped(shutdown, request.Context()) {
				return
			}
			select {
			case <-shutdown:
				return
			case <-request.Context().Done():
				return
			case event, ok := <-events:
				if !ok {
					return
				}
				if err := writeAndFlushSSE(controller, w, event, stream.WriteTimeout()); err != nil {
					return
				}
			case <-heartbeat.C:
				event := StreamEvent{
					Type: "heartbeat",
					Data: Invalidation{Cursor: stream.Cursor()},
				}
				if err := writeAndFlushSSE(controller, w, event, stream.WriteTimeout()); err != nil {
					return
				}
			}
		}
	})
}

// stopped reports whether the stream is shutting down or the client has gone
// away, without blocking on either.
func stopped(shutdown <-chan struct{}, request context.Context) bool {
	select {
	case <-shutdown:
		return true
	case <-request.Done():
		return true
	default:
		return false
	}
}

func writeAndFlushSSE(controller *http.ResponseController, w io.Writer, event StreamEvent, timeout time.Duration) error {
	if err := controller.SetWriteDeadline(time.Now().Add(timeout)); err != nil {
		return fmt.Errorf("set SSE write deadline: %w", err)
	}
	if err := writeSSE(w, event); err != nil {
		return err
	}
	if err := controller.Flush(); err != nil {
		return fmt.Errorf("flush SSE event: %w", err)
	}
	return nil
}

func writeSSE(w io.Writer, event StreamEvent) error {
	data, err := json.Marshal(event.Data)
	if err != nil {
		return fmt.Errorf("marshal SSE event: %w", err)
	}
	if event.ID != "" {
		if _, err := fmt.Fprintf(w, "id: %s\n", event.ID); err != nil {
			return err
		}
	}
	if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event.Type, data); err != nil {
		return err
	}
	return nil
}
