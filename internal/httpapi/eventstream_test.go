package httpapi

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/readservice"
)

type sseMessage struct {
	ID    string
	Event string
	Data  Invalidation
}

func newEventStreamFixture(t *testing.T, opts ...eventStreamOption) (instance.Layout, *journal.InstanceLog, *EventStream) {
	t.Helper()
	layout := instance.NewLayout(t.TempDir())
	if err := os.MkdirAll(layout.RunsDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	instanceLog, _, err := journal.OpenInstanceLog(layout.SchedulerDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := instanceLog.Close(); err != nil {
			t.Errorf("close instance log: %v", err)
		}
	})
	stream, err := newEventStream(layout, log.New(io.Discard, "", 0), opts...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		stream.Close()
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := stream.Wait(ctx); err != nil {
			t.Errorf("wait for event stream: %v", err)
		}
	})
	return layout, instanceLog, stream
}

func newEventTestServer(t *testing.T, stream *EventStream, reader *fakeReader) *httptest.Server {
	t.Helper()
	handler, err := NewHandler(reader, AllowAll, discardLogger(), WithEventStream(stream))
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return server
}

func openEventResponse(t *testing.T, client *http.Client, url, cursor string) (*http.Response, *bufio.Reader) {
	t.Helper()
	request, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	if cursor != "" {
		request.Header.Set("Last-Event-ID", cursor)
	}
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	return response, bufio.NewReader(response.Body)
}

func readSSEMessage(t *testing.T, reader *bufio.Reader) sseMessage {
	t.Helper()
	message := sseMessage{}
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatalf("read SSE message: %v", err)
		}
		line = strings.TrimSuffix(line, "\n")
		line = strings.TrimSuffix(line, "\r")
		if line == "" {
			return message
		}
		switch {
		case strings.HasPrefix(line, "id: "):
			message.ID = strings.TrimPrefix(line, "id: ")
		case strings.HasPrefix(line, "event: "):
			message.Event = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &message.Data); err != nil {
				t.Fatalf("decode SSE data: %v", err)
			}
		}
	}
}

func readUntilEvent(t *testing.T, reader *bufio.Reader, eventType string) sseMessage {
	t.Helper()
	for {
		message := readSSEMessage(t, reader)
		if message.Event == eventType {
			return message
		}
	}
}

func waitForCursor(t *testing.T, stream *EventStream, want string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if stream.Cursor() == want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("cursor = %q, want %q", stream.Cursor(), want)
}

func waitForSubscribers(t *testing.T, stream *EventStream, want int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		stream.mu.Lock()
		count := len(stream.subscribers)
		stream.mu.Unlock()
		if count == want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	stream.mu.Lock()
	count := len(stream.subscribers)
	stream.mu.Unlock()
	t.Fatalf("subscriber count = %d, want %d", count, want)
}

func TestEventsSnapshotAppendResumeHeartbeatAndDisconnect(t *testing.T) {
	_, instanceLog, stream := newEventStreamFixture(
		t,
		withEventSession("test"),
		withEventPollInterval(10*time.Millisecond),
		withHeartbeatInterval(40*time.Millisecond),
	)
	server := newEventTestServer(t, stream, &fakeReader{})
	client := &http.Client{Timeout: 3 * time.Second}

	response, reader := openEventResponse(t, client, server.URL+EventsPath, "")
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", response.StatusCode)
	}
	if got := response.Header.Get("Content-Type"); got != "text/event-stream" {
		t.Fatalf("Content-Type = %q", got)
	}
	snapshot := readSSEMessage(t, reader)
	if snapshot.Event != "snapshot" || snapshot.ID != "test:0" || snapshot.Data.Cursor != snapshot.ID {
		t.Fatalf("snapshot = %+v", snapshot)
	}
	if got := strings.Join(snapshot.Data.Models, ","); got != "instance,run,workflow" {
		t.Fatalf("snapshot models = %q", got)
	}

	appendedAt := time.Now()
	if err := instanceLog.Append(journal.Event{
		Type:     journal.EventRunStarted,
		Gaggle:   "core",
		Workflow: "implementation",
		RunID:    "run-1",
	}); err != nil {
		t.Fatal(err)
	}
	update := readUntilEvent(t, reader, "invalidate")
	if elapsed := time.Since(appendedAt); elapsed >= time.Second {
		t.Fatalf("append-to-stream latency = %s, want under 1s", elapsed)
	}
	if update.ID != "test:1" || update.Data.Cursor != update.ID {
		t.Fatalf("update = %+v", update)
	}
	if got := strings.Join(update.Data.Models, ","); got != "instance,run,workflow" {
		t.Fatalf("update models = %q", got)
	}
	if len(update.Data.RunIDs) != 1 || update.Data.RunIDs[0] != "run-1" {
		t.Fatalf("update run IDs = %+v", update.Data.RunIDs)
	}
	if len(update.Data.Workflows) != 1 || update.Data.Workflows[0].Name != "implementation" {
		t.Fatalf("update workflows = %+v", update.Data.Workflows)
	}
	if err := response.Body.Close(); err != nil {
		t.Fatal(err)
	}
	waitForSubscribers(t, stream, 0)

	if err := instanceLog.Append(journal.Event{Type: journal.EventTickSkipped, Reason: "conditions: budget"}); err != nil {
		t.Fatal(err)
	}
	waitForCursor(t, stream, "test:2")

	response, reader = openEventResponse(t, client, server.URL+EventsPath, update.ID)
	replayed := readSSEMessage(t, reader)
	if replayed.Event != "invalidate" || replayed.ID != "test:2" {
		t.Fatalf("replayed event = %+v", replayed)
	}
	heartbeat := readUntilEvent(t, reader, "heartbeat")
	if heartbeat.ID != "" || heartbeat.Data.Cursor != "test:2" {
		t.Fatalf("heartbeat = %+v", heartbeat)
	}
	if err := response.Body.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestEventsRejectExpiredCursorAndPollingRecoversCurrentState(t *testing.T) {
	_, instanceLog, stream := newEventStreamFixture(
		t,
		withEventSession("test"),
		withEventPollInterval(5*time.Millisecond),
		withEventHistoryLimit(2),
	)
	reader := &fakeReader{instance: readservice.Instance{Name: "current"}}
	server := newEventTestServer(t, stream, reader)

	for i := 0; i < 3; i++ {
		if err := instanceLog.Append(journal.Event{Type: journal.EventTickSkipped, Reason: fmt.Sprintf("skip-%d", i)}); err != nil {
			t.Fatal(err)
		}
	}
	waitForCursor(t, stream, "test:3")

	request, err := http.NewRequest(http.MethodGet, server.URL+EventsPath, nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Last-Event-ID", "test:0")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	var envelope ErrorEnvelope
	if err := json.NewDecoder(response.Body).Decode(&envelope); err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusConflict || envelope.Error.Code != "stale_cursor" {
		t.Fatalf("stale response = %d %+v", response.StatusCode, envelope)
	}

	response, err = http.Get(server.URL + InstancePath)
	if err != nil {
		t.Fatal(err)
	}
	var current readservice.Instance
	if err := json.NewDecoder(response.Body).Decode(&current); err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK || current.Name != "current" {
		t.Fatalf("poll response = %d %+v", response.StatusCode, current)
	}

	request, err = http.NewRequest(http.MethodGet, server.URL+EventsPath, nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Last-Event-ID", "not-a-cursor")
	response, err = http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("invalid cursor status = %d", response.StatusCode)
	}
	_ = response.Body.Close()

	request, err = http.NewRequest(http.MethodGet, server.URL+EventsPath, nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Last-Event-ID", "prior-session:3")
	response, err = http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusConflict {
		t.Fatalf("prior-session cursor status = %d", response.StatusCode)
	}
	_ = response.Body.Close()
}

func TestAppendToStreamP95UnderOneSecond(t *testing.T) {
	_, instanceLog, stream := newEventStreamFixture(
		t,
		withEventSession("test"),
		withEventPollInterval(5*time.Millisecond),
	)
	server := newEventTestServer(t, stream, &fakeReader{})
	client := &http.Client{Timeout: 5 * time.Second}
	response, reader := openEventResponse(t, client, server.URL+EventsPath, "")
	defer func() {
		if err := response.Body.Close(); err != nil {
			t.Errorf("close event response body: %v", err)
		}
	}()
	if snapshot := readSSEMessage(t, reader); snapshot.Event != "snapshot" {
		t.Fatalf("snapshot = %+v", snapshot)
	}

	const samples = 20
	latencies := make([]time.Duration, 0, samples)
	for i := 0; i < samples; i++ {
		if err := instanceLog.Append(journal.Event{
			Type:   journal.EventTickSkipped,
			Reason: fmt.Sprintf("latency-%d", i),
		}); err != nil {
			t.Fatal(err)
		}
		durableAt := time.Now()
		readUntilEvent(t, reader, "invalidate")
		latencies = append(latencies, time.Since(durableAt))
	}
	sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })
	p95 := latencies[(samples*95+99)/100-1]
	if p95 >= time.Second {
		t.Fatalf("append-to-stream p95 = %s, want under 1s", p95)
	}
}

func TestSlowSubscriberDoesNotBlockJournalAppends(t *testing.T) {
	_, instanceLog, stream := newEventStreamFixture(
		t,
		withEventSession("test"),
		withEventPollInterval(5*time.Millisecond),
		withSubscriberBuffer(1),
	)
	_, events, cancel, err := stream.Subscribe("")
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()

	appendDone := make(chan error, 1)
	go func() {
		for i := 0; i < 4; i++ {
			if err := instanceLog.Append(journal.Event{
				Type:   journal.EventTickSkipped,
				Reason: fmt.Sprintf("slow-client-%d", i),
			}); err != nil {
				appendDone <- err
				return
			}
		}
		appendDone <- nil
	}()
	select {
	case err := <-appendDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("journal appends blocked on slow subscriber")
	}
	waitForCursor(t, stream, "test:4")
	for range events {
	}
	waitForSubscribers(t, stream, 0)
}

func TestDefinitionInvalidationFollowsExplicitReadModelCommit(t *testing.T) {
	_, instanceLog, stream := newEventStreamFixture(
		t,
		withEventSession("test"),
		withEventPollInterval(time.Hour),
	)
	_, events, cancel, err := stream.Subscribe("")
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()

	if err := instanceLog.Append(journal.Event{Type: journal.EventConfigReloaded}); err != nil {
		t.Fatal(err)
	}
	if err := stream.scan(); err != nil {
		t.Fatal(err)
	}
	if got := stream.Cursor(); got != "test:0" {
		t.Fatalf("cursor before read-model commit = %q", got)
	}

	stream.PublishDefinitionsChanged()
	event := <-events
	if event.ID != "test:1" || strings.Join(event.Data.Models, ",") != "instance,workflow" {
		t.Fatalf("definition invalidation = %+v", event)
	}
}

func TestRunStateCheckpointProducesInvalidationWithoutJournalAppend(t *testing.T) {
	layout := instance.NewLayout(t.TempDir())
	run, err := journal.Create(layout.RunsDir(), journal.RunIdentity{
		RunID:    "run-state",
		Workflow: "implementation",
		Gaggle:   "core",
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := run.Close(); err != nil {
			t.Errorf("close run: %v", err)
		}
	}()
	stream, err := newEventStream(
		layout,
		log.New(io.Discard, "", 0),
		withEventSession("test"),
		withEventPollInterval(5*time.Millisecond),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		stream.Close()
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := stream.Wait(ctx); err != nil {
			t.Errorf("wait for event stream: %v", err)
		}
	}()
	_, events, cancel, err := stream.Subscribe("")
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()

	run.SetMachineState("approval")
	if err := run.Checkpoint(); err != nil {
		t.Fatal(err)
	}
	select {
	case event := <-events:
		if event.ID != "test:1" || len(event.Data.RunIDs) != 1 || event.Data.RunIDs[0] != "run-state" {
			t.Fatalf("state invalidation = %+v", event)
		}
	case <-time.After(time.Second):
		t.Fatal("run state checkpoint was not invalidated")
	}
}

func TestConcurrentRunJournalsProduceOrderedDeduplicableEvents(t *testing.T) {
	layout, _, stream := newEventStreamFixture(
		t,
		withEventSession("test"),
		withEventPollInterval(5*time.Millisecond),
	)
	_, events, cancel, err := stream.Subscribe("")
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()

	runIDs := []string{"run-a", "run-b"}
	runs := make(chan *journal.Run, len(runIDs))
	errs := make(chan error, len(runIDs))
	var wg sync.WaitGroup
	for _, runID := range runIDs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			run, err := journal.Create(layout.RunsDir(), journal.RunIdentity{
				RunID:    runID,
				Workflow: "implementation",
				Gaggle:   "core",
			}, nil)
			if err != nil {
				errs <- err
				return
			}
			runs <- run
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	close(runs)
	for run := range runs {
		currentRun := run
		t.Cleanup(func() {
			if err := currentRun.Close(); err != nil {
				t.Errorf("close run: %v", err)
			}
		})
	}

	seen := map[string]bool{}
	lastSequence := uint64(0)
	deadline := time.After(2 * time.Second)
	for len(seen) < len(runIDs) {
		select {
		case event, ok := <-events:
			if !ok {
				t.Fatal("event stream disconnected")
			}
			if event.ID != event.Data.Cursor {
				t.Fatalf("event ID/cursor mismatch = %+v", event)
			}
			_, rawSequence, ok := strings.Cut(event.ID, ":")
			if !ok {
				t.Fatalf("event ID = %q", event.ID)
			}
			sequence, err := strconv.ParseUint(rawSequence, 10, 64)
			if err != nil {
				t.Fatal(err)
			}
			if sequence <= lastSequence {
				t.Fatalf("event sequence = %d after %d", sequence, lastSequence)
			}
			lastSequence = sequence
			for _, runID := range event.Data.RunIDs {
				seen[runID] = true
			}
			if len(event.Data.RunIDs) > 0 {
				if got := strings.Join(event.Data.Models, ","); got != "instance,run,workflow" {
					t.Fatalf("run invalidation models = %q", got)
				}
				if len(event.Data.Workflows) != 1 ||
					event.Data.Workflows[0] != (WorkflowRef{Gaggle: "core", Name: "implementation"}) {
					t.Fatalf("run invalidation workflow = %+v", event.Data.Workflows)
				}
			}
		case <-deadline:
			t.Fatalf("saw run invalidations for %+v, want %v", seen, runIDs)
		}
	}
}

func TestServerShutdownClosesActiveEventStreams(t *testing.T) {
	_, _, stream := newEventStreamFixture(
		t,
		withEventSession("test"),
		withEventPollInterval(5*time.Millisecond),
	)
	handler, err := NewHandler(&fakeReader{}, AllowAll, discardLogger(), WithEventStream(stream))
	if err != nil {
		t.Fatal(err)
	}
	server, err := NewServer("127.0.0.1:0", handler, discardLogger())
	if err != nil {
		t.Fatal(err)
	}
	if err := server.Start(); err != nil {
		t.Fatal(err)
	}

	client := &http.Client{Timeout: 3 * time.Second}
	response, reader := openEventResponse(t, client, "http://"+server.Address()+EventsPath, "")
	if snapshot := readSSEMessage(t, reader); snapshot.Event != "snapshot" {
		t.Fatalf("snapshot = %+v", snapshot)
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := server.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := reader.ReadByte(); !errorsIsEOFOrClosed(err) {
		t.Fatalf("stream read after shutdown = %v", err)
	}
	_ = response.Body.Close()
	if _, ok := <-server.Errors(); ok {
		t.Fatal("errors channel should close after graceful shutdown")
	}
}

func errorsIsEOFOrClosed(err error) bool {
	return errors.Is(err, io.EOF) || errors.Is(err, context.Canceled) ||
		(err != nil && strings.Contains(err.Error(), "closed"))
}

// An up-to-date resume has no events to replay, so the response only arrives if
// the handler flushes the SSE headers on its own rather than waiting for the
// next heartbeat.
func TestResumeWithUpToDateCursorEstablishesBeforeHeartbeat(t *testing.T) {
	_, instanceLog, stream := newEventStreamFixture(
		t,
		withEventSession("test"),
		withEventPollInterval(5*time.Millisecond),
		withHeartbeatInterval(30*time.Second),
	)
	server := newEventTestServer(t, stream, &fakeReader{})
	client := &http.Client{Timeout: 3 * time.Second}

	if err := instanceLog.Append(journal.Event{
		Type:     journal.EventRunStarted,
		Gaggle:   "core",
		Workflow: "implementation",
		RunID:    "run-1",
	}); err != nil {
		t.Fatal(err)
	}
	waitForCursor(t, stream, "test:1")

	openedAt := time.Now()
	response, _ := openEventResponse(t, client, server.URL+EventsPath, "test:1")
	elapsed := time.Since(openedAt)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", response.StatusCode)
	}
	if got := response.Header.Get("Content-Type"); got != "text/event-stream" {
		t.Fatalf("Content-Type = %q", got)
	}
	if elapsed >= time.Second {
		t.Fatalf("idle resume handshake took %s, want well under the heartbeat", elapsed)
	}
	if err := response.Body.Close(); err != nil {
		t.Fatal(err)
	}
}

// slowResponseWriter models a client that reads steadily but slowly: every
// write succeeds, so the per-write deadline never trips and only an explicit
// shutdown check can stop the handler.
type slowResponseWriter struct {
	header   http.Header
	interval time.Duration

	mu     sync.Mutex
	writes int
}

func (w *slowResponseWriter) Header() http.Header { return w.header }

func (w *slowResponseWriter) WriteHeader(int) {}

func (w *slowResponseWriter) Write(p []byte) (int, error) {
	time.Sleep(w.interval)
	w.mu.Lock()
	w.writes++
	w.mu.Unlock()
	return len(p), nil
}

func (w *slowResponseWriter) Flush() {}

func (w *slowResponseWriter) SetWriteDeadline(time.Time) error { return nil }

func (w *slowResponseWriter) writeCount() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.writes
}

// A slow-but-live client keeps resetting the per-write deadline, so a large
// queued replay must be abandoned when the stream closes rather than written
// out in full.
func TestShutdownAbandonsQueuedReplayForSlowClient(t *testing.T) {
	const backlog = 400
	_, instanceLog, stream := newEventStreamFixture(
		t,
		withEventSession("test"),
		withEventPollInterval(5*time.Millisecond),
		withHeartbeatInterval(30*time.Second),
		withEventHistoryLimit(backlog+64),
	)
	handler, err := NewHandler(&fakeReader{}, AllowAll, discardLogger(), WithEventStream(stream))
	if err != nil {
		t.Fatal(err)
	}

	for i := 0; i < backlog; i++ {
		if err := instanceLog.Append(journal.Event{
			Type:     journal.EventRunStarted,
			Gaggle:   "core",
			Workflow: "implementation",
			RunID:    "run-" + strconv.Itoa(i),
		}); err != nil {
			t.Fatal(err)
		}
	}
	waitForCursor(t, stream, "test:"+strconv.Itoa(backlog))

	request := httptest.NewRequest(http.MethodGet, EventsPath, nil)
	request.Header.Set("Last-Event-ID", "test:0")
	writer := &slowResponseWriter{header: http.Header{}, interval: 5 * time.Millisecond}
	served := make(chan struct{})
	go func() {
		defer close(served)
		handler.ServeHTTP(writer, request)
	}()

	waitForSubscribers(t, stream, 1)
	for writer.writeCount() < 5 {
		time.Sleep(5 * time.Millisecond)
	}

	closedAt := time.Now()
	stream.Close()
	select {
	case <-served:
	case <-time.After(2 * time.Second):
		t.Fatalf("handler still writing %s after stream close", time.Since(closedAt))
	}
	if got := writer.writeCount(); got >= backlog {
		t.Fatalf("handler wrote %d of %d queued events, want the replay abandoned", got, backlog)
	}
}
