package journal

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestExportReaderBoundedRawRecordsAndCommittedWatermark(t *testing.T) {
	var data bytes.Buffer
	for seq := uint64(1); seq <= 300; seq++ {
		line, _ := json.Marshal(Event{Schema: EventSchema, Seq: seq, Time: time.Now(), Type: EventRunnerAnnotation})
		data.Write(line)
		data.WriteByte('\n')
	}
	data.WriteString(`{"schema":"torn`)
	position := ExportPosition{Identity: "test", Generation: fileEvents}
	base := CommittedEvent{Kind: "run", JournalID: "test"}
	batch, err := scanExportBatch(t.Context(), bytes.NewReader(data.Bytes()), base, position, 37)
	if err != nil || len(batch.Events) != 37 || batch.Position.Seq != 37 {
		t.Fatalf("committed bound: records=%d seq=%d err=%v", len(batch.Events), batch.Position.Seq, err)
	}
	position = batch.Position
	for want := uint64(38); want <= 300; {
		batch, err = scanExportBatch(t.Context(), bytes.NewReader(data.Bytes()[position.Offset:]), base, position, 0)
		if err != nil || len(batch.Events) == 0 || len(batch.Events) > 128 {
			t.Fatalf("batch=%+v err=%v", batch, err)
		}
		for _, event := range batch.Events {
			if event.Seq != want || !json.Valid(event.Body) {
				t.Fatalf("invalid raw record: %+v", event)
			}
			want++
		}
		position = batch.Position
	}
	batch, err = scanExportBatch(t.Context(), bytes.NewReader(data.Bytes()[position.Offset:]), base, position, 0)
	if err != nil || len(batch.Events) != 0 || batch.Position != position {
		t.Fatalf("torn tail advanced cursor: %+v %v", batch, err)
	}
}

func TestExportReaderRunAndSchedulerIdentity(t *testing.T) {
	path := t.TempDir()
	root, err := os.OpenRoot(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	id := testIdentity()
	run, err := Create(filepath.Join(path, "runs"), id, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = run.Append(Event{Type: EventRunnerAnnotation, Stage: "test", Attempt: 2}); err != nil {
		t.Fatal(err)
	}
	if err = run.Close(); err != nil {
		t.Fatal(err)
	}
	batch, err := ReadExportBatch(t.Context(), root, filepath.Join("runs", id.RunID), false, ExportPosition{}, 0)
	if err != nil || len(batch.Events) == 0 {
		t.Fatalf("read run: %+v %v", batch, err)
	}
	last := batch.Events[len(batch.Events)-1]
	if last.RunID != id.RunID || last.Workflow != id.Workflow || last.Stage != "test" || last.Attempt != 2 {
		t.Fatalf("lost context: %+v", last)
	}
	appendInstanceEvents(t, filepath.Join(path, "scheduler"), Event{Type: EventDaemonStarted})
	batch, err = ReadExportBatch(t.Context(), root, "scheduler", true, ExportPosition{}, 0)
	if err != nil || len(batch.Events) != 1 || len(batch.Position.Identity) != 32 {
		t.Fatalf("read scheduler: %+v %v", batch, err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err = ReadExportBatch(ctx, root, "scheduler", true, ExportPosition{}, 0); err == nil {
		t.Fatal("ignored cancellation")
	}
}

func TestExportReaderCompactionAndReplacement(t *testing.T) {
	path := t.TempDir()
	root, err := os.OpenRoot(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	dir := filepath.Join(path, "scheduler")
	now := time.Now().Add(-time.Hour)
	log, _, err := OpenInstanceLog(dir, WithClock(func() time.Time { return now }))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = log.Close() }()
	for range 3 {
		if err = log.Append(Event{Type: EventTickSkipped}); err != nil {
			t.Fatal(err)
		}
	}
	first, err := ReadExportBatch(t.Context(), root, "scheduler", true, ExportPosition{}, 1)
	if err != nil || first.Position.Seq != 1 {
		t.Fatalf("first: %+v %v", first, err)
	}
	now = now.Add(time.Hour)
	if err = log.Append(Event{Type: EventDaemonStarted}); err != nil {
		t.Fatal(err)
	}
	if _, err = log.Compact(now.Add(-time.Minute), now.Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	batch, err := ReadExportBatch(t.Context(), root, "scheduler", true, first.Position, 0)
	if err != nil || len(batch.Events) != 1 || batch.Events[0].Seq != 4 || !batch.Gap || batch.Position.Identity != first.Position.Identity {
		t.Fatalf("compaction: %+v %v", batch, err)
	}
	// A different durable identity invalidates even a numerically larger cursor.
	old := ExportPosition{Identity: "different", Seq: 999, Offset: 999}
	batch, err = ReadExportBatch(t.Context(), root, "scheduler", true, old, 0)
	if err != nil || len(batch.Events) != 1 || batch.Position.Seq != 4 {
		t.Fatalf("replacement: %+v %v", batch, err)
	}
}

func TestExportReaderRejectsEscapingDirectory(t *testing.T) {
	root, err := os.OpenRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	if _, err = ReadExportBatch(t.Context(), root, "../outside", true, ExportPosition{}, 0); err == nil {
		t.Fatal("accepted escaping journal directory")
	}
}

func TestExportReaderFingerprintTracksMetadata(t *testing.T) {
	path := t.TempDir()
	root, err := os.OpenRoot(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	id := testIdentity()
	run, err := Create(filepath.Join(path, "runs"), id, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = run.Close(); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join("runs", id.RunID)
	fingerprint := func() string {
		t.Helper()
		info, err := root.Stat(filepath.Join(dir, fileEvents))
		if err != nil {
			t.Fatal(err)
		}
		value, err := ExportRunFingerprint(t.Context(), root, dir, info)
		if err != nil {
			t.Fatal(err)
		}
		return value
	}
	previous := fingerprint()
	for _, name := range []string{fileEvents, fileRunYAML, fileSchema} {
		info, err := root.Stat(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		changed := info.ModTime().Add(time.Hour)
		if err = os.Chtimes(filepath.Join(path, dir, name), changed, changed); err != nil {
			t.Fatal(err)
		}
		next := fingerprint()
		if next == previous {
			t.Fatalf("same-size %s change was hidden", name)
		}
		previous = next
	}
	info, err := root.Stat(filepath.Join(dir, fileEvents))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = ExportRunFingerprint(t.Context(), root, "../outside", info); err == nil {
		t.Fatal("accepted escaping metadata")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err = ExportRunFingerprint(ctx, root, dir, info); err == nil {
		t.Fatal("ignored cancellation")
	}
	if _, err = ExportRunFingerprint(t.Context(), root, dir, nil); err == nil {
		t.Fatal("accepted missing event metadata")
	}
	if err = os.Remove(filepath.Join(path, dir, fileSchema)); err != nil {
		t.Fatal(err)
	}
	if err = os.Mkdir(filepath.Join(path, dir, fileSchema), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err = ExportRunFingerprint(t.Context(), root, dir, info); err == nil {
		t.Fatal("accepted nonregular metadata")
	}
}
