package journal

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type committedTestSink struct{ events chan CommittedEvent }

func (s *committedTestSink) Commit(event CommittedEvent) {
	select {
	case s.events <- event:
	default:
		panic("test sink capacity exceeded")
	}
}

func (s *committedTestSink) drain() []CommittedEvent {
	var events []CommittedEvent
	for {
		select {
		case event := <-s.events:
			events = append(events, event)
		default:
			return events
		}
	}
}

func registerTestCommitSink(t *testing.T, root string) (*committedTestSink, func()) {
	t.Helper()
	sink := &committedTestSink{events: make(chan CommittedEvent, 256)}
	unregister, err := RegisterCommittedEventSink(root, "known-instance", sink)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(unregister)
	return sink, unregister
}

func assertCommittedFile(t *testing.T, path string, events []CommittedEvent) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := bytes.Split(bytes.TrimSuffix(data, []byte{'\n'}), []byte{'\n'})
	if len(lines) != len(events) {
		t.Fatalf("%d file records, %d exported records", len(lines), len(events))
	}
	for i, event := range events {
		if !bytes.Equal(event.Body, lines[i]) {
			t.Fatalf("record %d differs: exported %s, file %s", i, event.Body, lines[i])
		}
		var body Event
		if err := json.Unmarshal(event.Body, &body); err != nil {
			t.Fatal(err)
		}
		if event.Seq != body.Seq || !event.Time.Equal(body.Time) || event.ObservedTime.IsZero() {
			t.Fatalf("record %d has incorrect commit metadata: %+v", i, event)
		}
	}
}

func TestCommittedRunBodyAndRecovery(t *testing.T) {
	root := t.TempDir()
	sink, _ := registerTestCommitSink(t, root)
	registry := NewRegistryScrubber()
	const secret = "synthetic-registered-secret-value"
	registry.Register([]byte(secret))
	id := testIdentity()
	id.InstanceID = "pinned-instance"
	r, err := Create(filepath.Join(root, "runs"), id, nil, WithScrubber(registry), WithClock(fixedClock()))
	if err != nil {
		t.Fatal(err)
	}
	dir := r.Dir()
	payload := map[string]any{"secret": secret, "integer": uint64(18446744073709551615)}
	if err := r.Append(Event{Type: EventRunnerAnnotation, Runner: payload}); err != nil {
		t.Fatal(err)
	}
	payload["secret"] = "mutated-after-append"
	if _, err := r.RecordArtifact("note", []byte("synthetic")); err != nil {
		t.Fatal(err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	events := sink.drain()
	if len(events) != 3 {
		t.Fatalf("got %d events; want started, annotation, artifact", len(events))
	}
	for _, event := range events {
		if event.Kind != "run" || event.JournalID != id.RunID || event.RunID != id.RunID ||
			event.InstanceID != id.InstanceID || event.Gaggle != id.Gaggle {
			t.Fatalf("incorrect run identity: %+v", event)
		}
	}
	if bytes.Contains(events[1].Body, []byte(secret)) || bytes.Contains(events[1].Body, []byte("mutated-after-append")) ||
		!bytes.Contains(events[1].Body, []byte("18446744073709551615")) {
		t.Fatalf("scrubbed immutable JSON not preserved: %s", events[1].Body)
	}
	assertCommittedFile(t, filepath.Join(dir, fileEvents), events)
	r, _, err = Recover(dir, WithScrubber(registry))
	if err != nil {
		t.Fatal(err)
	}
	if got := sink.drain(); len(got) != 0 {
		t.Fatalf("recovery replayed %d historical events", len(got))
	}
	if err := r.Append(Event{Type: EventRunnerAnnotation}); err != nil {
		t.Fatal(err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	events = append(events, sink.drain()...)
	assertCommittedFile(t, filepath.Join(dir, fileEvents), events)
	f, err := os.OpenFile(filepath.Join(dir, fileEvents), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(`{"torn":`); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	r, report, err := TryRecover(dir, WithScrubber(registry))
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	repaired := sink.drain()
	if !report.Repaired || len(repaired) != 1 || !bytes.Contains(repaired[0].Body, []byte(`"type":"repaired"`)) {
		t.Fatalf("recovery did not export exactly its new repair: %+v", repaired)
	}
	assertCommittedFile(t, filepath.Join(dir, fileEvents), append(events, repaired...))
}

func TestCommittedSchedulerRecoveryCompactionAndMultipleHandles(t *testing.T) {
	root := t.TempDir()
	sink, _ := registerTestCommitSink(t, root)
	dir := filepath.Join(root, "scheduler")
	now := time.Now().UTC()
	l, _, err := OpenInstanceLog(dir, WithClock(func() time.Time { return now }))
	if err != nil {
		t.Fatal(err)
	}
	if err := l.Append(Event{Type: EventRunnerAnnotation}); err != nil {
		t.Fatal(err)
	}
	other, _, err := OpenInstanceLog(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := other.Append(Event{Type: EventRunnerAnnotation}); err != nil {
		t.Fatal(err)
	}
	if err := other.Close(); err != nil {
		t.Fatal(err)
	}
	first := sink.drain()
	if len(first) != 2 || first[0].JournalID == "" || first[0].JournalID != first[1].JournalID {
		t.Fatalf("incorrect scheduler commit identities: %+v", first)
	}
	assertCommittedFile(t, currentInstanceEventsPath(t, dir), first)
	if _, err := CompactInstanceEvents(dir, now.Add(-time.Hour), now.Add(-time.Hour), false); err != nil {
		t.Fatal(err)
	}
	if err := l.Append(Event{Type: EventRunnerAnnotation}); err != nil {
		t.Fatal(err)
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	path := currentInstanceEventsPath(t, dir)
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(`{"torn":`); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	l, report, err := OpenInstanceLog(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	if !report.Repaired {
		t.Fatal("expected newly committed repair")
	}
	events := append(first, sink.drain()...)
	if len(events) != 4 {
		t.Fatalf("got %d events after compaction and repair, want 4", len(events))
	}
	for i, event := range events {
		if event.Seq != uint64(i+1) || event.JournalID != first[0].JournalID ||
			event.Kind != "scheduler" || event.InstanceID != "known-instance" ||
			event.RunID != "" || event.Gaggle != "" {
			t.Fatalf("incorrect scheduler context after reopen/compaction: %+v", event)
		}
	}
	assertCommittedFile(t, path, events)
}

func TestCommittedFailedAppendAndUnpublishedRun(t *testing.T) {
	root := t.TempDir()
	sink, _ := registerTestCommitSink(t, root)
	l, _, err := OpenInstanceLog(filepath.Join(root, "scheduler"))
	if err != nil {
		t.Fatal(err)
	}
	if err := l.Append(Event{Type: EventGateOverridden}); err == nil {
		t.Fatal("expected marshal validation failure")
	}
	if err := l.file.Close(); err != nil {
		t.Fatal(err)
	}
	if err := l.Append(Event{Type: EventRunnerAnnotation}); err == nil {
		t.Fatal("expected file write failure")
	}
	if got := sink.drain(); len(got) != 0 {
		t.Fatalf("failed appends exported %d records", len(got))
	}
	id := testIdentity()
	runs := filepath.Join(root, "runs")
	r, err := Create(runs, id, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	sink.drain()
	if _, err := Create(runs, id, nil); err == nil {
		t.Fatal("duplicate run unexpectedly published")
	}
	if got := sink.drain(); len(got) != 0 {
		t.Fatalf("failed creation exported %d records", len(got))
	}
}

func TestCommittedRootIsolationAndRegistrationLifecycle(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "instance")
	sibling := filepath.Join(parent, "instance-suffix")
	for _, dir := range []string{root, sibling} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	sink, unregister := registerTestCommitSink(t, root)
	otherSink, _ := registerTestCommitSink(t, sibling)
	for _, overlap := range []string{root, filepath.Join(root, "."), parent} {
		if stop, err := RegisterCommittedEventSink(overlap, "", sink); err == nil {
			stop()
			t.Fatalf("overlapping registration accepted: %s", overlap)
		}
	}
	l, _, err := OpenInstanceLog(filepath.Join(root, "scheduler"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	other, _, err := OpenInstanceLog(filepath.Join(sibling, "scheduler"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = other.Close() }()
	var wg sync.WaitGroup
	for _, log := range []*InstanceLog{l, other} {
		wg.Go(func() {
			for range 10 {
				if err := log.Append(Event{Type: EventRunnerAnnotation}); err != nil {
					t.Error(err)
				}
			}
		})
	}
	wg.Wait()
	if len(sink.drain()) != 10 || len(otherSink.drain()) != 10 {
		t.Fatal("events crossed instance roots")
	}
	unregister()
	unregister()
	replacement, stop := registerTestCommitSink(t, root)
	if err := l.Append(Event{Type: EventRunnerAnnotation}); err != nil {
		t.Fatal(err)
	}
	if len(sink.drain()) != 0 || len(replacement.drain()) != 0 {
		t.Fatal("old handle emitted after unregister or redirected to a new owner")
	}
	reopened, _, err := OpenInstanceLog(filepath.Join(root, "scheduler"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reopened.Close() }()
	if err := reopened.Append(Event{Type: EventRunnerAnnotation}); err != nil {
		t.Fatal(err)
	}
	if got := replacement.drain(); len(got) != 1 || got[0].Seq != 12 {
		t.Fatalf("reopened handle emitted incorrect events: %+v", got)
	}
	stop()
}

func TestCommittedSchedulerFollowsRecreatedJournalIdentity(t *testing.T) {
	root := t.TempDir()
	sink, _ := registerTestCommitSink(t, root)
	dir := filepath.Join(root, "scheduler")
	old, _, err := OpenInstanceLog(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = old.Close() }()
	if err := old.Append(Event{Type: EventRunnerAnnotation}); err != nil {
		t.Fatal(err)
	}
	first := sink.drain()
	if err := advanceInstanceEventsPointer(dir, 1); err != nil {
		t.Fatal(err)
	}
	recreated, _, err := OpenInstanceLog(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = recreated.Close() }()
	if err := recreated.Append(Event{Type: EventRunnerAnnotation}); err != nil {
		t.Fatal(err)
	}
	if err := old.Append(Event{Type: EventRunnerAnnotation}); err != nil {
		t.Fatal(err)
	}
	events := sink.drain()
	if len(first) != 1 || len(events) != 2 || first[0].JournalID == events[0].JournalID ||
		events[0].JournalID != events[1].JournalID {
		t.Fatalf("old handle used stale journal identity: before=%+v after=%+v", first, events)
	}
	assertCommittedFile(t, currentInstanceEventsPath(t, dir), events)
}

func TestCommittedConcurrentUnregisterDoesNotAffectAppends(t *testing.T) {
	root := t.TempDir()
	sink, unregister := registerTestCommitSink(t, root)
	dir := filepath.Join(root, "scheduler")
	var writers []*InstanceLog
	for range 4 {
		writer, _, err := OpenInstanceLog(dir)
		if err != nil {
			t.Fatal(err)
		}
		writers = append(writers, writer)
		t.Cleanup(func() { _ = writer.Close() })
	}
	start := make(chan struct{})
	var wg sync.WaitGroup
	for _, writer := range writers {
		wg.Go(func() {
			<-start
			for range 20 {
				if err := writer.Append(Event{Type: EventRunnerAnnotation}); err != nil {
					t.Error(err)
				}
			}

		})
	}
	wg.Go(func() {
		<-start
		unregister()
	})
	close(start)
	wg.Wait()
	events, err := ReadInstanceLog(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 80 {
		t.Fatalf("unregister affected durable writes: got %d, want 80", len(events))
	}
	var seq uint64
	for _, event := range sink.drain() {
		if event.Seq <= seq {
			t.Fatalf("out-of-order or duplicate commit: previous=%d current=%d", seq, event.Seq)
		}
		seq = event.Seq
	}
}

type failedSyncEventFile struct{ bytes.Buffer }

func (*failedSyncEventFile) Sync() error { return errors.New("synthetic sync failure") }

func TestCommittedFailedSyncDoesNotEmit(t *testing.T) {
	t.Setenv(envDisableFsync, "0")
	root := t.TempDir()
	sink, _ := registerTestCommitSink(t, root)
	target := instanceCommitTarget(root, "synthetic-journal")
	writer := &failedSyncEventFile{}
	var seq uint64
	_, err := appendEvent(writer, &seq, Chain(), time.Now, Event{Type: EventRunnerAnnotation}, target)
	if err == nil {
		t.Fatal("expected sync failure")
	}
	if !strings.Contains(err.Error(), "fsync event") {
		t.Fatalf("expected fsync failure after write, got %v", err)
	}
	if got := sink.drain(); len(got) != 0 {
		t.Fatal("failed fsync exported a record")
	}
}

func TestCommittedRegistrationRejectsInvalidRoot(t *testing.T) {
	root := t.TempDir()
	file := filepath.Join(root, "not-a-directory")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	sink := &committedTestSink{events: make(chan CommittedEvent, 1)}
	for _, invalid := range []string{"", filepath.Join(root, "missing"), file} {
		if stop, err := RegisterCommittedEventSink(invalid, "", sink); err == nil {
			stop()
			t.Fatalf("invalid registration root accepted: %q", invalid)
		}
	}
	if HasCommittedEventSink("") {
		t.Fatal("empty root must not resolve to an ambient current-directory sink")
	}
}

// panickingSink panics on every Commit, the shape a buggy sink takes. Commit
// runs inside appendEvent with the journal write lock held, so an uncontained
// panic would unwind into the journal writer after the event is already durable.
type panickingSink struct{ calls int }

func (s *panickingSink) Commit(CommittedEvent) {
	s.calls++
	panic("synthetic sink failure")
}

func TestCommittedPanickingSinkDoesNotReachJournalWriter(t *testing.T) {
	root := t.TempDir()
	sink := &panickingSink{}
	unregister, err := RegisterCommittedEventSink(root, "known-instance", sink)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(unregister)

	before := CommittedSinkPanicCount()
	id := testIdentity()
	r, err := Create(filepath.Join(root, "runs"), id, nil, WithClock(fixedClock()))
	if err != nil {
		t.Fatalf("Create must survive a panicking sink: %v", err)
	}
	dir := r.Dir()
	// Every one of these appends drives a Commit that panics.
	for i := range 3 {
		if err := r.Append(Event{Type: EventRunnerAnnotation, Runner: map[string]any{"i": i}}); err != nil {
			t.Fatalf("Append %d must survive a panicking sink: %v", i, err)
		}
	}
	if err := r.Close(); err != nil {
		t.Fatalf("Close must survive a panicking sink: %v", err)
	}

	// The journal is still the source of truth: run.started plus three
	// annotations are durable even though every export attempt panicked.
	data, err := os.ReadFile(filepath.Join(dir, fileEvents))
	if err != nil {
		t.Fatal(err)
	}
	if got := len(bytes.Split(bytes.TrimSuffix(data, []byte{'\n'}), []byte{'\n'})); got != 4 {
		t.Fatalf("%d durable records; want 4", got)
	}
	// The sink stays registered, so a data-dependent panic does not silently
	// disable export for the rest of the process.
	if sink.calls != 4 {
		t.Fatalf("sink saw %d commits; want 4 (it must not be unregistered by a panic)", sink.calls)
	}
	if got := CommittedSinkPanicCount() - before; got != 4 {
		t.Fatalf("counted %d contained panics; want 4", got)
	}
}

func TestCommittedIdentitySurvivesTransientReadFailure(t *testing.T) {
	root := t.TempDir()
	sink, _ := registerTestCommitSink(t, root)
	dir := filepath.Join(root, "scheduler")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Events must predate the compaction cutoff below, or nothingCanAgeOut
	// short-circuits, the generation never rotates, and the identity refresh
	// this test exercises is never reached.
	base := time.Now().Add(-2 * time.Hour)
	l, _, err := OpenInstanceLog(dir, WithClock(func() time.Time { return base }))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	if err := l.Append(Event{Type: EventRunnerAnnotation, Runner: map[string]any{"n": 1}}); err != nil {
		t.Fatal(err)
	}
	first := sink.drain()
	if len(first) != 1 || first[0].JournalID == "" {
		t.Fatalf("expected one exported event with an identity, got %+v", first)
	}
	identity := first[0].JournalID

	// Corrupt the identity file so readInstanceLogID reports an error rather
	// than an absent identity (an absent one is legitimately ""), then rotate
	// so the refresh path actually runs. A transient failure must not latch
	// JournalID to "": the sink drops every record with an empty identity, and
	// it is only recomputed on the next rotation, so latching would silence
	// export for this handle's remaining lifetime while writes continued.
	if err := os.WriteFile(filepath.Join(dir, fileInstanceLogID), []byte("truncated"), 0o644); err != nil {
		t.Fatal(err)
	}
	cutoff := base.Add(time.Hour)
	if _, err := CompactInstanceEvents(dir, cutoff, cutoff, false); err != nil {
		t.Fatal(err)
	}
	if err := l.Append(Event{Type: EventRunnerAnnotation, Runner: map[string]any{"n": 2}}); err != nil {
		t.Fatalf("Append must not fail on an unreadable identity: %v", err)
	}
	after := sink.drain()
	if len(after) != 1 {
		t.Fatalf("got %d events after the failed identity read; want 1", len(after))
	}
	if after[0].JournalID != identity {
		t.Fatalf("JournalID = %q after a transient read failure; want the previous %q", after[0].JournalID, identity)
	}
}
