package httpapi

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/goobers/goobers/internal/apicontract"
	"github.com/goobers/goobers/internal/readmodel"
)

// The SSE stream, backed by the change feed (#1929, design §8.1).
//
// # What it replaces
//
// eventstream.go discovers change by polling the filesystem on a 100ms ticker,
// with a 5-second sweep that stats every historical run it has ever seen,
// per-run offset and digest state retained for all history, an in-memory event
// ring, and a random per-process session id. Its cost grows with the number of
// runs that have ever existed.
//
// This tails the `change` table, which the projector writes in the same
// transaction as the fact it describes. Cost is bounded by ACTIVE WORK: a quiet
// instance with 40,665 runs does an indexed seek that returns nothing.
//
// # Why the old stream is still here
//
// It is the source for topologies with no read model — the standalone portal
// reads an instance directory directly. Deleting it now would remove live
// updates from that topology entirely. §13.1's "one read topology" (#1933) is
// what makes the deletion safe, and it is the issue that owns it.
//
// Both implement eventSource, so the handler does not know which it has.

// eventSource is what the SSE handler needs.
//
// Deliberately the shape the filesystem stream already had, so the cutover
// changes the SOURCE without touching the ~140-line handler that frames,
// flushes, and heartbeats. A handler rewritten at the same time as its source
// would make a bisect useless.
type eventSource interface {
	Subscribe(lastEventID string) ([]StreamEvent, <-chan StreamEvent, func(), error)
	Done() <-chan struct{}
	// WriteTimeout bounds a single SSE frame write. On the interface because the
	// handler applies it via http.NewResponseController per frame, and the two
	// sources reach it differently — one from its config struct, one from a
	// constant.
	WriteTimeout() time.Duration
	// Heartbeat is contractual (§8.1): the client arms a liveness deadline
	// against it (#1711), so both sources must agree on the interval or a
	// healthy stream on one of them trips the other's watchdog.
	Heartbeat() time.Duration
	// Cursor is the position stamped on a heartbeat, so a client that has been
	// idle still learns where the feed is.
	Cursor() string
	Close()
}

// feedStream serves SSE from the read model's change feed.
type feedStream struct {
	feed  *readmodel.Feed
	store *readmodel.Store

	mu     sync.Mutex
	closed bool
	done   chan struct{}
	ctx    context.Context
	cancel context.CancelFunc
	pumps  sync.WaitGroup
}

// newFeedStream constructs a change-feed-backed event source.
func newFeedStream(store *readmodel.Store) *feedStream {
	ctx, cancel := context.WithCancel(context.Background())
	return &feedStream{
		feed: readmodel.NewFeed(store), store: store, done: make(chan struct{}),
		ctx: ctx, cancel: cancel,
	}
}

// Done reports shutdown.
func (s *feedStream) Done() <-chan struct{} { return s.done }

// WriteTimeout bounds one SSE frame write.
func (s *feedStream) WriteTimeout() time.Duration { return defaultEventWriteTimeout }

// Heartbeat is the contractual interval.
func (s *feedStream) Heartbeat() time.Duration { return readmodel.HeartbeatInterval }

// Cursor reports the feed head, for heartbeat framing.
//
// Best-effort: a heartbeat exists to prove the connection is alive, so failing
// to read the head must not stop one being sent. An empty cursor is a valid
// heartbeat — the client re-reads its own position on the next update.
func (s *feedStream) Cursor() string {
	head, err := s.feed.Head(context.Background())
	if err != nil {
		return ""
	}
	return head.String()
}

// PublishDefinitionsChanged records a config reload in the feed.
//
// Replaces the deleted poller's out-of-band publish. Errors are returned rather
// than swallowed: unlike a projection, this has no repair sweep behind it — a
// lost definitions change is simply never noticed by any client.
func (s *feedStream) PublishDefinitionsChanged() error {
	return s.store.PublishDefinitionsChanged(context.Background())
}

// Close stops the stream.
func (s *feedStream) Close() {
	s.mu.Lock()
	if !s.closed {
		s.closed = true
		close(s.done)
		s.cancel()
	}
	s.mu.Unlock()
	s.pumps.Wait()
}

// Subscribe resumes from a cursor, or starts a fresh snapshot.
//
// # Why a refused cursor is an error rather than a silent snapshot
//
// A client whose cursor is unusable must be TOLD which of the three conditions
// applies (§8.2), because its correct response differs and because silently
// restarting it at head would hide a rebuild or a truncation behind what looks
// like an ordinary reconnect. The handler maps each condition to its own wire
// code.
func (s *feedStream) Subscribe(lastEventID string) ([]StreamEvent, <-chan StreamEvent, func(), error) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil, nil, nil, ErrEventStreamClosed
	}
	s.mu.Unlock()

	ctx, cancel := context.WithCancel(s.ctx)

	cursor, initial, err := s.start(ctx, lastEventID)
	if err != nil {
		cancel()
		return nil, nil, nil, err
	}

	events := make(chan StreamEvent, subscriberBufferForFeed)
	stopped := make(chan struct{})
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		cancel()
		return nil, nil, nil, ErrEventStreamClosed
	}
	s.pumps.Add(1)
	s.mu.Unlock()
	go func() {
		defer s.pumps.Done()
		defer close(stopped)
		s.pump(ctx, cursor, events)
	}()

	// The returned cancel WAITS for the pump to exit.
	//
	// Cancelling only signalled the context and returned immediately, so the
	// pump kept running — still holding the store — after its caller believed
	// the subscription was over. In the daemon that means a subscription
	// outliving shutdown; in tests it means a goroutine reading the store while
	// cleanup closes it, which is the data race the race detector caught on
	// main.
	//
	// Waiting here is cheap: the pump is blocked in Since, which returns as soon
	// as the context is done.
	return initial, events, func() {
		cancel()
		<-stopped
	}, nil
}

// start resolves the opening position and any snapshot event.
func (s *feedStream) start(ctx context.Context, lastEventID string) (readmodel.Cursor, []StreamEvent, error) {
	if lastEventID == "" {
		// No cursor: the client gets a snapshot instruction and starts from the
		// CURRENT head, not from zero. Starting at zero would replay the entire
		// retained feed into a client that is about to refetch everything anyway.
		head, err := s.feed.Head(ctx)
		if err != nil {
			return readmodel.Cursor{}, nil, err
		}
		return head, []StreamEvent{{
			ID:   head.String(),
			Type: "snapshot",
			Data: apicontract.Invalidation{
				Cursor: head.String(),
				Models: []string{"instance", "run", "workflow"},
			},
		}}, nil
	}

	cursor, err := readmodel.ParseCursor(lastEventID)
	if err != nil {
		return readmodel.Cursor{}, nil, ErrInvalidEventCursor
	}
	// Resolve the cursor NOW rather than on the first pump iteration, so a
	// refusal is an HTTP status the client can act on instead of a stream that
	// opens and immediately dies.
	state, err := s.store.State(ctx)
	if err != nil {
		return readmodel.Cursor{}, nil, err
	}
	if condition := readmodel.EvaluateCursor(cursor, state); condition != readmodel.CursorOK {
		return readmodel.Cursor{}, nil, &readmodel.CursorError{
			Condition: condition, Cursor: cursor, State: state,
		}
	}
	return cursor, nil, nil
}

// pump forwards committed changes until the subscription ends.
func (s *feedStream) pump(ctx context.Context, cursor readmodel.Cursor, events chan<- StreamEvent) {
	defer close(events)
	for {
		page, err := s.feed.Since(ctx, cursor, feedPageLimit)
		if err != nil {
			// Cancellation and shutdown are ordinary endings. Anything else ends
			// the subscription too: the client reconnects with its cursor, which
			// is exactly the recovery path, and holding a broken subscription
			// open would look alive while delivering nothing.
			return
		}
		cursor = page.Cursor
		for _, event := range invalidationsFor(page) {
			select {
			case <-ctx.Done():
				return
			case <-s.done:
				return
			case events <- event:
			}
		}
	}
}

// invalidationsFor coalesces a page of changes into one event per cursor
// position.
//
// One event carrying every affected entity, rather than one event per change:
// a client applies a batch as a single invalidation, and splitting it would
// make N refetches out of what the projector committed as one advance. The
// cursor is the LAST position in the page, so a client that applies the event
// and reconnects resumes past all of it.
func invalidationsFor(page readmodel.FeedPosition) []StreamEvent {
	if len(page.Changes) == 0 {
		return nil
	}
	var (
		runIDs      []string
		workflows   []apicontract.WorkflowRef
		seenRun     = map[string]bool{}
		seenFlow    = map[string]bool{}
		definitions bool
	)
	for _, change := range page.Changes {
		if change.Kind == readmodel.ChangeDefinitionsChanged {
			// A config reload can affect any workflow, so it widens the whole
			// batch rather than naming entities. Scoping it would under-report.
			definitions = true
			continue
		}
		if change.RunID != "" && !seenRun[change.RunID] {
			seenRun[change.RunID] = true
			runIDs = append(runIDs, change.RunID)
		}
		if change.Workflow != "" {
			key := change.Gaggle + "\x00" + change.Workflow
			if !seenFlow[key] {
				seenFlow[key] = true
				workflows = append(workflows,
					apicontract.WorkflowRef{Gaggle: change.Gaggle, Name: change.Workflow})
			}
		}
	}
	cursor := page.Cursor.String()
	models := []string{"run", "workflow"}
	if definitions {
		// Instance too: a reload changes gaggle and workflow inventory, which
		// the run and workflow models alone do not cover.
		models = []string{"instance", "run", "workflow"}
	}
	return []StreamEvent{{
		ID:   cursor,
		Type: "update",
		Data: apicontract.Invalidation{
			Cursor:    cursor,
			Models:    models,
			RunIDs:    runIDs,
			Workflows: workflows,
		},
	}}
}

const (
	// feedPageLimit bounds one read of the change feed. A burst is delivered
	// across several events rather than one unbounded one.
	feedPageLimit = 256
	// subscriberBufferForFeed matches the filesystem stream's, so a slow client
	// behaves the same on either source.
	subscriberBufferForFeed = 64
)

// cursorConditionStatus maps a refused cursor onto its wire code.
//
// Three codes, not one generic stale_cursor: the client's correct response
// differs per condition, and collapsing them would make a rebuilt store
// indistinguishable from a pruned feed or a client that is simply too old.
func cursorConditionStatus(err error) (code string, message string, ok bool) {
	var refusal *readmodel.CursorError
	if !errors.As(err, &refusal) {
		return "", "", false
	}
	switch refusal.Condition {
	case readmodel.CursorEpochChanged:
		return string(refusal.Condition),
			"the read model was rebuilt; refetch current read endpoints and reconnect without Last-Event-ID", true
	case readmodel.CursorFeedTruncated:
		return string(refusal.Condition),
			"the change feed no longer retains this position; refetch current read endpoints and reconnect without Last-Event-ID", true
	case readmodel.CursorSchemaChanged:
		return string(refusal.Condition),
			"the read model schema changed; reload the client", true
	default:
		return string(refusal.Condition), "the cursor cannot be resumed", true
	}
}
