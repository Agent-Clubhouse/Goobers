package httpapi

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/goobers/goobers/internal/apicontract"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
)

const (
	defaultEventPollInterval = 100 * time.Millisecond
	defaultHeartbeatInterval = 15 * time.Second
	defaultEventHistoryLimit = 512
	defaultSubscriberBuffer  = 64
	defaultEventWriteTimeout = time.Second
)

var (
	// ErrInvalidEventCursor means Last-Event-ID is not a stream cursor.
	ErrInvalidEventCursor = errors.New("http API: invalid event cursor")
	// ErrStaleEventCursor means a cursor belongs to another daemon session or
	// has fallen outside the bounded replay window.
	ErrStaleEventCursor = errors.New("http API: stale event cursor")
	// ErrEventStreamClosed means the daemon event stream is shutting down.
	ErrEventStreamClosed = errors.New("http API: event stream closed")
)

// Invalidation identifies versioned read models that clients should refetch.
type Invalidation struct {
	Cursor    string        `json:"cursor"`
	Models    []string      `json:"models"`
	RunIDs    []string      `json:"runIds,omitempty"`
	Workflows []WorkflowRef `json:"workflows,omitempty"`
}

// WorkflowRef identifies one workflow read model. An empty list with the
// workflow model means all workflow inventory should be refetched.
type WorkflowRef struct {
	Gaggle string `json:"gaggle,omitempty"`
	Name   string `json:"name"`
}

// StreamEvent is one SSE message with a stable ID for client deduplication.
type StreamEvent struct {
	ID   string
	Type string
	Data Invalidation

	sequence uint64
}

type eventStreamConfig struct {
	pollInterval     time.Duration
	heartbeat        time.Duration
	historyLimit     int
	subscriberBuffer int
	writeTimeout     time.Duration
	session          string
}

type eventStreamOption func(*eventStreamConfig)

func withEventPollInterval(interval time.Duration) eventStreamOption {
	return func(c *eventStreamConfig) { c.pollInterval = interval }
}

func withHeartbeatInterval(interval time.Duration) eventStreamOption {
	return func(c *eventStreamConfig) { c.heartbeat = interval }
}

func withEventHistoryLimit(limit int) eventStreamOption {
	return func(c *eventStreamConfig) { c.historyLimit = limit }
}

func withSubscriberBuffer(size int) eventStreamOption {
	return func(c *eventStreamConfig) { c.subscriberBuffer = size }
}

func withEventSession(session string) eventStreamOption {
	return func(c *eventStreamConfig) { c.session = session }
}

type journalSource struct {
	path     string
	runID    string
	gaggle   string
	workflow string
}

type sourceState struct {
	offset       int64
	stateDigest  string
	stateVersion fileVersion
	source       journalSource
}

type fileVersion struct {
	size    int64
	modTime int64
}

type eventSubscriber struct {
	ch chan StreamEvent
}

// EventStream tails complete journal records after they become visible on disk
// and fans out coarse invalidations without entering the journal write path.
type EventStream struct {
	layout instance.Layout
	log    *log.Logger
	config eventStreamConfig

	ctx    context.Context
	cancel context.CancelFunc
	done   chan struct{}

	mu          sync.Mutex
	closed      bool
	sequence    uint64
	history     []StreamEvent
	subscribers map[uint64]*eventSubscriber
	nextSubID   uint64
	sources     map[string]sourceState
	closeOnce   sync.Once
}

// NewEventStream starts a bounded replay stream over the instance and run
// journals. Existing records are represented by the initial snapshot cursor;
// records appended after construction become invalidation events.
func NewEventStream(layout instance.Layout, errorLog *log.Logger) (*EventStream, error) {
	return newEventStream(layout, errorLog)
}

func newEventStream(layout instance.Layout, errorLog *log.Logger, opts ...eventStreamOption) (*EventStream, error) {
	if errorLog == nil {
		return nil, errors.New("http API event stream logger is required")
	}
	session, err := newEventSession()
	if err != nil {
		return nil, err
	}
	config := eventStreamConfig{
		pollInterval:     defaultEventPollInterval,
		heartbeat:        defaultHeartbeatInterval,
		historyLimit:     defaultEventHistoryLimit,
		subscriberBuffer: defaultSubscriberBuffer,
		writeTimeout:     defaultEventWriteTimeout,
		session:          session,
	}
	for _, opt := range opts {
		opt(&config)
	}
	if config.pollInterval <= 0 {
		return nil, errors.New("http API event poll interval must be positive")
	}
	if config.heartbeat <= 0 {
		return nil, errors.New("http API event heartbeat interval must be positive")
	}
	if config.historyLimit < 1 {
		return nil, errors.New("http API event history limit must be positive")
	}
	if config.subscriberBuffer < 1 {
		return nil, errors.New("http API event subscriber buffer must be positive")
	}
	if config.writeTimeout <= 0 {
		return nil, errors.New("http API event write timeout must be positive")
	}
	if config.session == "" || strings.Contains(config.session, ":") {
		return nil, errors.New("http API event session is invalid")
	}

	ctx, cancel := context.WithCancel(context.Background())
	stream := &EventStream{
		layout:      layout,
		log:         errorLog,
		config:      config,
		ctx:         ctx,
		cancel:      cancel,
		done:        make(chan struct{}),
		subscribers: make(map[uint64]*eventSubscriber),
		sources:     make(map[string]sourceState),
	}
	if err := stream.baseline(); err != nil {
		cancel()
		return nil, err
	}
	go stream.run()
	return stream, nil
}

func newEventSession() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", fmt.Errorf("http API: create event stream session: %w", err)
	}
	return hex.EncodeToString(value[:]), nil
}

func (s *EventStream) baseline() error {
	sources, err := s.journalSources()
	if err != nil {
		return err
	}
	for _, source := range sources {
		source, err = enrichJournalSource(source)
		if err != nil {
			return fmt.Errorf("http API: baseline event journal %q: %w", source.path, err)
		}
		offset, err := lastCompleteRecordOffset(source.path)
		if err != nil {
			return fmt.Errorf("http API: baseline event journal %q: %w", source.path, err)
		}
		stateDigest, stateVersion, err := readRunState(source, false)
		if err != nil {
			return fmt.Errorf("http API: baseline run state %q: %w", source.runID, err)
		}
		s.sources[source.path] = sourceState{
			offset: offset, stateDigest: stateDigest, stateVersion: stateVersion, source: source,
		}
	}
	return nil
}

func (s *EventStream) run() {
	defer close(s.done)
	ticker := time.NewTicker(s.config.pollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-ticker.C:
			if err := s.scan(); err != nil {
				s.log.Printf("event stream journal scan failed: %v", err)
			}
		}
	}
}

func (s *EventStream) scan() error {
	sources, err := s.journalSources()
	if err != nil {
		return err
	}
	seen := make(map[string]struct{}, len(sources))
	var scanErrors []error
	for _, source := range sources {
		seen[source.path] = struct{}{}
		state, ok := s.sources[source.path]
		if !ok {
			source, err = enrichJournalSource(source)
			if err != nil {
				scanErrors = append(scanErrors, err)
				continue
			}
			state = sourceState{source: source}
		} else {
			source = state.source
		}
		events, offset, err := readNewJournalEvents(source.path, state.offset)
		if err != nil {
			scanErrors = append(scanErrors, err)
			continue
		}
		state.offset = offset
		for _, event := range events {
			if invalidation, ok := invalidationFor(source, event); ok {
				s.publish(invalidation)
			}
		}
		s.sources[source.path] = state
		stateVersion, err := runStateVersion(source)
		if err != nil {
			scanErrors = append(scanErrors, err)
			continue
		}
		if stateVersion == state.stateVersion {
			continue
		}
		stateDigest, stateVersion, err := readRunState(source, true)
		if err != nil {
			scanErrors = append(scanErrors, err)
			continue
		}
		state.stateVersion = stateVersion
		if stateDigest != "" && stateDigest != state.stateDigest {
			state.stateDigest = stateDigest
			invalidation, _ := invalidationFor(source, journal.Event{})
			s.publish(invalidation)
		}
		s.sources[source.path] = state
	}
	for path := range s.sources {
		if _, ok := seen[path]; !ok {
			delete(s.sources, path)
		}
	}
	return errors.Join(scanErrors...)
}

func enrichJournalSource(source journalSource) (journalSource, error) {
	if source.runID == "" {
		return source, nil
	}
	reader, err := journal.OpenRead(filepath.Dir(source.path))
	if err != nil {
		return journalSource{}, err
	}
	identity, err := reader.Identity()
	if err != nil {
		return journalSource{}, err
	}
	source.gaggle = identity.Gaggle
	source.workflow = identity.Workflow
	return source, nil
}

func runStateVersion(source journalSource) (fileVersion, error) {
	if source.runID == "" {
		return fileVersion{}, nil
	}
	path := filepath.Join(filepath.Dir(source.path), "state.json")
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return fileVersion{}, nil
	}
	if err != nil {
		return fileVersion{}, err
	}
	return fileVersion{size: info.Size(), modTime: info.ModTime().UnixNano()}, nil
}

func readRunState(source journalSource, durable bool) (digest string, version fileVersion, err error) {
	if source.runID == "" {
		return "", fileVersion{}, nil
	}
	path := filepath.Join(filepath.Dir(source.path), "state.json")
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return "", fileVersion{}, nil
	}
	if err != nil {
		return "", fileVersion{}, err
	}
	defer func() {
		err = errors.Join(err, file.Close())
	}()
	data, err := io.ReadAll(file)
	if err != nil {
		return "", fileVersion{}, err
	}
	if durable {
		if err := syncObservedFile(file, path); err != nil {
			return "", fileVersion{}, err
		}
	}
	info, err := file.Stat()
	if err != nil {
		return "", fileVersion{}, err
	}
	version = fileVersion{size: info.Size(), modTime: info.ModTime().UnixNano()}
	return journal.Digest(data), version, nil
}

func (s *EventStream) journalSources() ([]journalSource, error) {
	sources := make([]journalSource, 0)
	instancePath := filepath.Join(s.layout.SchedulerDir(), "events.jsonl")
	if _, err := os.Stat(instancePath); err == nil {
		sources = append(sources, journalSource{path: instancePath})
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("http API: stat instance journal: %w", err)
	}

	entries, err := os.ReadDir(s.layout.RunsDir())
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("http API: read runs directory: %w", err)
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		path := filepath.Join(s.layout.RunsDir(), entry.Name(), "events.jsonl")
		if _, err := os.Stat(path); err == nil {
			sources = append(sources, journalSource{path: path, runID: entry.Name()})
		} else if !errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("http API: stat run journal %q: %w", entry.Name(), err)
		}
	}
	sort.Slice(sources, func(i, j int) bool { return sources[i].path < sources[j].path })
	return sources, nil
}

func lastCompleteRecordOffset(path string) (offset int64, err error) {
	file, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer func() {
		err = errors.Join(err, file.Close())
	}()
	info, err := file.Stat()
	if err != nil {
		return 0, err
	}
	size := info.Size()
	if size == 0 {
		return 0, nil
	}

	const chunkSize int64 = 4096
	for end := size; end > 0; {
		start := end - chunkSize
		if start < 0 {
			start = 0
		}
		chunk := make([]byte, end-start)
		if _, err := file.ReadAt(chunk, start); err != nil && !errors.Is(err, io.EOF) {
			return 0, err
		}
		if index := bytes.LastIndexByte(chunk, '\n'); index >= 0 {
			return start + int64(index) + 1, nil
		}
		end = start
	}
	return 0, nil
}

func readNewJournalEvents(path string, offset int64) (events []journal.Event, nextOffset int64, err error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, offset, fmt.Errorf("http API: stat event journal %q: %w", path, err)
	}
	if info.Size() == offset {
		return nil, offset, nil
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, offset, fmt.Errorf("http API: open event journal %q: %w", path, err)
	}
	defer func() {
		err = errors.Join(err, file.Close())
	}()
	info, err = file.Stat()
	if err != nil {
		return nil, offset, fmt.Errorf("http API: stat event journal %q: %w", path, err)
	}
	if info.Size() < offset {
		offset = 0
	}
	if _, err := file.Seek(offset, io.SeekStart); err != nil {
		return nil, offset, fmt.Errorf("http API: seek event journal %q: %w", path, err)
	}
	data, err := io.ReadAll(file)
	if err != nil {
		return nil, offset, fmt.Errorf("http API: read event journal %q: %w", path, err)
	}
	lastNewline := bytes.LastIndexByte(data, '\n')
	if lastNewline < 0 {
		return nil, offset, nil
	}
	// A complete line is visible after Write but before the journal's Sync
	// returns. Force the observed bytes durable before publishing them so the
	// stream never announces an append that can still be lost.
	if err := syncObservedFile(file, path); err != nil {
		return nil, offset, fmt.Errorf("http API: sync event journal %q: %w", path, err)
	}
	complete := data[:lastNewline+1]
	lines := bytes.Split(bytes.TrimSuffix(complete, []byte{'\n'}), []byte{'\n'})
	events = make([]journal.Event, 0, len(lines))
	for _, line := range lines {
		if len(line) == 0 {
			continue
		}
		var event journal.Event
		if err := json.Unmarshal(line, &event); err != nil {
			return nil, offset, fmt.Errorf("http API: parse event journal %q: %w", path, err)
		}
		events = append(events, event)
	}
	return events, offset + int64(len(complete)), nil
}

func syncObservedFile(file *os.File, path string) error {
	if err := file.Sync(); err != nil {
		return err
	}
	directory, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	if err := directory.Sync(); err != nil {
		_ = directory.Close()
		if errors.Is(err, syscall.EINVAL) || errors.Is(err, syscall.ENOTSUP) {
			return nil
		}
		return err
	}
	return directory.Close()
}

func invalidationFor(source journalSource, event journal.Event) (Invalidation, bool) {
	// Scheduler.Reload journals before readservice.ReloadDefinitions commits the
	// new projection. The config reloader publishes this invalidation explicitly
	// after that swap so a refetch cannot observe the old snapshot and miss it.
	if event.Type == journal.EventConfigReloaded {
		return Invalidation{}, false
	}
	models := map[string]bool{"instance": true}
	invalidation := Invalidation{}
	if source.runID != "" {
		models["run"] = true
		invalidation.RunIDs = []string{source.runID}
		if source.workflow != "" {
			models["workflow"] = true
			invalidation.Workflows = []WorkflowRef{{Gaggle: source.gaggle, Name: source.workflow}}
		}
	} else {
		if event.RunID != "" {
			models["run"] = true
			invalidation.RunIDs = []string{event.RunID}
		}
		if event.Workflow != "" {
			models["workflow"] = true
			invalidation.Workflows = []WorkflowRef{{Gaggle: event.Gaggle, Name: event.Workflow}}
		}
	}
	for _, model := range []string{"instance", "run", "workflow"} {
		if models[model] {
			invalidation.Models = append(invalidation.Models, model)
		}
	}
	return invalidation, true
}

func (s *EventStream) publish(invalidation Invalidation) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	s.sequence++
	event := StreamEvent{
		ID:       s.cursorLocked(s.sequence),
		Type:     "invalidate",
		sequence: s.sequence,
	}
	invalidation.Cursor = event.ID
	event.Data = invalidation
	s.history = append(s.history, event)
	if len(s.history) > s.config.historyLimit {
		s.history = append([]StreamEvent(nil), s.history[len(s.history)-s.config.historyLimit:]...)
	}
	for id, subscriber := range s.subscribers {
		select {
		case subscriber.ch <- event:
		default:
			delete(s.subscribers, id)
			close(subscriber.ch)
		}
	}
}

// PublishDefinitionsChanged invalidates definition-backed models after the read
// service atomically commits a validated config reload.
func (s *EventStream) PublishDefinitionsChanged() {
	s.publish(Invalidation{Models: []string{"instance", "workflow"}})
}

// Subscribe atomically captures either a full snapshot cursor or replay events
// and registers for all later events, closing the snapshot-to-live race.
func (s *EventStream) Subscribe(lastEventID string) ([]StreamEvent, <-chan StreamEvent, func(), error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, nil, nil, ErrEventStreamClosed
	}

	var initial []StreamEvent
	if lastEventID == "" {
		cursor := s.cursorLocked(s.sequence)
		initial = []StreamEvent{{
			ID:       cursor,
			Type:     "snapshot",
			Data:     Invalidation{Cursor: cursor, Models: []string{"instance", "run", "workflow"}},
			sequence: s.sequence,
		}}
	} else {
		sequence, err := s.parseCursorLocked(lastEventID)
		if err != nil {
			return nil, nil, nil, err
		}
		for _, event := range s.history {
			if event.sequence > sequence {
				initial = append(initial, event)
			}
		}
	}

	s.nextSubID++
	id := s.nextSubID
	subscriber := &eventSubscriber{ch: make(chan StreamEvent, s.config.subscriberBuffer)}
	s.subscribers[id] = subscriber
	var once sync.Once
	cancel := func() {
		once.Do(func() {
			s.mu.Lock()
			defer s.mu.Unlock()
			current, ok := s.subscribers[id]
			if !ok || current != subscriber {
				return
			}
			delete(s.subscribers, id)
			close(subscriber.ch)
		})
	}
	return initial, subscriber.ch, cancel, nil
}

func (s *EventStream) parseCursorLocked(cursor string) (uint64, error) {
	session, rawSequence, ok := strings.Cut(cursor, ":")
	if !ok || session == "" || rawSequence == "" {
		return 0, ErrInvalidEventCursor
	}
	sequence, err := strconv.ParseUint(rawSequence, 10, 64)
	if err != nil {
		return 0, ErrInvalidEventCursor
	}
	if session != s.config.session || sequence > s.sequence {
		return 0, ErrStaleEventCursor
	}
	oldest := s.sequence
	if len(s.history) > 0 {
		oldest = s.history[0].sequence - 1
	}
	if sequence < oldest {
		return 0, ErrStaleEventCursor
	}
	return sequence, nil
}

// Cursor returns the latest stream cursor.
func (s *EventStream) Cursor() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cursorLocked(s.sequence)
}

func (s *EventStream) cursorLocked(sequence uint64) string {
	return s.config.session + ":" + strconv.FormatUint(sequence, 10)
}

// Close initiates shutdown and disconnects every subscriber without waiting on
// filesystem I/O in the scanner.
func (s *EventStream) Close() {
	s.closeOnce.Do(func() {
		s.mu.Lock()
		s.closed = true
		for id, subscriber := range s.subscribers {
			delete(s.subscribers, id)
			close(subscriber.ch)
		}
		s.mu.Unlock()
		s.cancel()
	})
}

// Done reports stream shutdown. It closes when Close is called, letting write
// paths abandon in-flight replay and buffered events instead of holding an
// HTTP shutdown open for a slow or non-reading client.
func (s *EventStream) Done() <-chan struct{} {
	return s.ctx.Done()
}

// Wait blocks until journal scanning exits or ctx expires.
func (s *EventStream) Wait(ctx context.Context) error {
	select {
	case <-s.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func registerEventRoute(router *Router, stream *EventStream) {
	router.Handle(apicontract.RouteEvents, func(w http.ResponseWriter, request *http.Request) {
		initial, events, cancel, err := stream.Subscribe(request.Header.Get("Last-Event-ID"))
		if err != nil {
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
			if err := writeAndFlushSSE(controller, w, event, stream.config.writeTimeout); err != nil {
				return
			}
		}

		heartbeat := time.NewTicker(stream.config.heartbeat)
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
				if err := writeAndFlushSSE(controller, w, event, stream.config.writeTimeout); err != nil {
					return
				}
			case <-heartbeat.C:
				event := StreamEvent{
					Type: "heartbeat",
					Data: Invalidation{Cursor: stream.Cursor()},
				}
				if err := writeAndFlushSSE(controller, w, event, stream.config.writeTimeout); err != nil {
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
