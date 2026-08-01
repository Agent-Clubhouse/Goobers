package journal

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestExportOutboxWritesPathPreservingFiles(t *testing.T) {
	run, root := newRun(t)
	t.Cleanup(func() { _ = run.Close() })

	refs, err := run.ExportOutbox("build", 1, AttemptPolicy, []OutboxFile{
		{RelPath: "report.json", Data: []byte(`{"ok":true}`)},
		{RelPath: "nested/debug.log", Data: []byte("log line")},
	})
	if err != nil {
		t.Fatalf("ExportOutbox: %v", err)
	}
	if len(refs) != 2 {
		t.Fatalf("refs = %d, want 2", len(refs))
	}

	want := filepath.Join(root, testIdentity().RunID, "artifacts", "outbox", "build", "attempt-1", "report.json")
	if _, err := os.Stat(want); err != nil {
		t.Fatalf("expected outbox file at %s: %v", want, err)
	}
	wantNested := filepath.Join(root, testIdentity().RunID, "artifacts", "outbox", "build", "attempt-1", "nested", "debug.log")
	if _, err := os.Stat(wantNested); err != nil {
		t.Fatalf("expected nested outbox file at %s: %v", wantNested, err)
	}
}

func TestExportOutboxEmptyIsNoOp(t *testing.T) {
	run, _ := newRun(t)
	t.Cleanup(func() { _ = run.Close() })

	refs, err := run.ExportOutbox("build", 1, AttemptPolicy, nil)
	if err != nil {
		t.Fatalf("ExportOutbox: %v", err)
	}
	if refs != nil {
		t.Fatalf("refs = %v, want nil", refs)
	}
}

func TestExportOutboxRejectsPathEscape(t *testing.T) {
	run, _ := newRun(t)
	t.Cleanup(func() { _ = run.Close() })

	cases := []string{"../escape.txt", "/etc/passwd", "a/../../b", ".."}
	for _, rel := range cases {
		if _, err := run.ExportOutbox("build", 1, AttemptPolicy, []OutboxFile{{RelPath: rel, Data: []byte("x")}}); err == nil {
			t.Fatalf("ExportOutbox(%q) succeeded, want a path-escape error", rel)
		}
	}
}

func TestExportOutboxRejectsUnsafeStageName(t *testing.T) {
	run, _ := newRun(t)
	t.Cleanup(func() { _ = run.Close() })

	cases := []string{"", ".", "..", "a/b", "a\\b"}
	for _, stage := range cases {
		if _, err := run.ExportOutbox(stage, 1, AttemptPolicy, []OutboxFile{{RelPath: "f.txt", Data: []byte("x")}}); err == nil {
			t.Fatalf("ExportOutbox(stage=%q) succeeded, want an error", stage)
		}
	}
}

func TestExportOutboxRejectsInvalidAttempt(t *testing.T) {
	run, _ := newRun(t)
	t.Cleanup(func() { _ = run.Close() })

	if _, err := run.ExportOutbox("build", 0, AttemptPolicy, []OutboxFile{{RelPath: "f.txt", Data: []byte("x")}}); err == nil {
		t.Fatal("ExportOutbox(attempt=0) succeeded, want an error")
	}
}

func TestExportOutboxRejectsDuplicateRelPath(t *testing.T) {
	run, _ := newRun(t)
	t.Cleanup(func() { _ = run.Close() })

	_, err := run.ExportOutbox("build", 1, AttemptPolicy, []OutboxFile{
		{RelPath: "a.txt", Data: []byte("1")},
		{RelPath: "./a.txt", Data: []byte("2")},
	})
	if err == nil {
		t.Fatal("ExportOutbox with duplicate relPath succeeded, want an error")
	}
}

func TestExportOutboxEnforcesFileCountLimit(t *testing.T) {
	run, root := newRun(t)
	t.Cleanup(func() { _ = run.Close() })

	files := make([]OutboxFile, MaxOutboxFilesPerAttempt+1)
	for i := range files {
		files[i] = OutboxFile{RelPath: filepath.Join("many", strconv.Itoa(i)+".txt"), Data: []byte("x")}
	}
	if _, err := run.ExportOutbox("build", 1, AttemptPolicy, files); err == nil {
		t.Fatal("ExportOutbox over the file-count limit succeeded, want an error")
	}
	// Fail-closed on the WHOLE batch: nothing from the over-limit batch should
	// have been written, closing the "aggregate limits bypassable" class of
	// defect the prior escalation flagged.
	outboxDir := filepath.Join(root, testIdentity().RunID, "artifacts", "outbox", "build")
	if _, err := os.Stat(outboxDir); err == nil {
		t.Fatal("outbox directory exists after a rejected over-limit batch")
	}
}

func TestExportOutboxEnforcesAggregateByteLimit(t *testing.T) {
	run, root := newRun(t)
	t.Cleanup(func() { _ = run.Close() })

	big := make([]byte, MaxOutboxBytesPerAttempt/2+1)
	files := []OutboxFile{
		{RelPath: "a.bin", Data: big},
		{RelPath: "b.bin", Data: big},
	}
	if _, err := run.ExportOutbox("build", 1, AttemptPolicy, files); err == nil {
		t.Fatal("ExportOutbox over the aggregate byte limit succeeded, want an error")
	}
	outboxDir := filepath.Join(root, testIdentity().RunID, "artifacts", "outbox", "build")
	if _, err := os.Stat(outboxDir); err == nil {
		t.Fatal("outbox directory exists after a rejected over-limit batch")
	}
}

func TestExportOutboxRecordsArtifactEvents(t *testing.T) {
	run, root := newRun(t)
	t.Cleanup(func() { _ = run.Close() })

	if _, err := run.ExportOutbox("build", 2, AttemptInfra, []OutboxFile{
		{RelPath: "r.json", Data: []byte("{}")},
	}); err != nil {
		t.Fatalf("ExportOutbox: %v", err)
	}
	if err := run.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	reader, err := OpenRead(filepath.Join(root, testIdentity().RunID))
	if err != nil {
		t.Fatal(err)
	}
	events, err := reader.Events()
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, ev := range events {
		if ev.Type != EventArtifactRecorded || ev.Stage != "build" {
			continue
		}
		found = true
		if ev.Attempt != 2 || ev.AttemptClass != AttemptInfra {
			t.Fatalf("event attempt/class = %d/%s, want 2/%s", ev.Attempt, ev.AttemptClass, AttemptInfra)
		}
		if !strings.HasPrefix(ev.Name, "outbox/") {
			t.Fatalf("event name = %q, want an outbox/ prefix", ev.Name)
		}
	}
	if !found {
		t.Fatal("no artifact.recorded event found for the outbox export")
	}
}
