package journal

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestConformanceRecordSpanRoundTrip(t *testing.T) {
	run, root := newRun(t)

	transcript := []byte("harness transcript: implementing the change...")
	ref, err := run.RecordSpan("implement", "copilot-cli.transcript", transcript)
	if err != nil {
		t.Fatalf("RecordSpan: %v", err)
	}

	if !strings.HasPrefix(ref.Path, dirSpans+"/") {
		t.Fatalf("span Ref.Path = %q, want it under %q", ref.Path, dirSpans)
	}
	if ref.Digest != Digest(transcript) {
		t.Fatalf("span Ref.Digest = %q, want %q", ref.Digest, Digest(transcript))
	}
	if err := run.Append(Event{Type: EventRunFinished, Status: string(PhaseCompleted)}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if err := run.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	rd, err := OpenRead(filepath.Join(root, testIdentity().RunID))
	if err != nil {
		t.Fatalf("OpenRead: %v", err)
	}
	events, err := rd.Events()
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	var found *Event
	for i := range events {
		if events[i].Type == EventSpanRecorded {
			found = &events[i]
		}
	}

	if found == nil {
		t.Fatal("no span.recorded event found")
	}
	if found.Stage != "implement" || found.Name != "copilot-cli.transcript" {
		t.Fatalf("span event = %+v, want stage=implement name=copilot-cli.transcript", found)
	}
	if found.DataSchema != "" {
		t.Fatalf("legacy span dataSchema = %q, want empty", found.DataSchema)
	}
	if found.IsConformanceNormative() {
		t.Fatal("span.recorded must be excluded from conformance (harness/LLM output, §3.3)")
	}

	got, err := rd.SpanBytes(*found.Ref)
	if err != nil {
		t.Fatalf("SpanBytes: %v", err)
	}
	if string(got) != string(transcript) {
		t.Fatalf("SpanBytes = %q, want %q", got, transcript)
	}
}

func TestRecordSpanWithSchemaRoundTrip(t *testing.T) {
	run, root := newRun(t)

	const dataSchema = "goobers.dev/telemetry/genai-event/v1"
	if _, err := run.RecordSpanWithSchema("implement", "copilot-cli.transcript", dataSchema, []byte(`{"role":"user"}`)); err != nil {
		t.Fatalf("RecordSpanWithSchema: %v", err)
	}
	if err := run.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	rd, err := OpenRead(filepath.Join(root, testIdentity().RunID))
	if err != nil {
		t.Fatalf("OpenRead: %v", err)
	}
	events, err := rd.Events()
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	if got := events[len(events)-1].DataSchema; got != dataSchema {
		t.Fatalf("span dataSchema = %q, want %q", got, dataSchema)
	}
}

func TestRecordSpanScrubsSecret(t *testing.T) {
	reg, scrub := DefaultScrubber()
	root := t.TempDir()
	run, err := Create(root, testIdentity(), nil, WithScrubber(scrub), WithClock(fixedClock()))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	secret := "super-secret-token-value"
	reg.Register([]byte(secret))
	transcript := []byte("auth: token=" + secret + " ok")

	ref, err := run.RecordSpan("implement", "transcript", transcript)
	if err != nil {
		t.Fatalf("RecordSpan: %v", err)
	}
	if err := run.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	rd, err := OpenRead(filepath.Join(root, testIdentity().RunID))
	if err != nil {
		t.Fatalf("OpenRead: %v", err)
	}
	got, err := rd.SpanBytes(ref)
	if err != nil {
		t.Fatalf("SpanBytes: %v", err)
	}
	if strings.Contains(string(got), secret) {
		t.Fatalf("span blob still contains the raw secret: %q", got)
	}
	if !strings.Contains(string(got), Redacted) {
		t.Fatalf("span blob does not contain the redaction placeholder: %q", got)
	}
}

func TestSpansOnlyRunCleanupPreservesJournalsAndRevalidatesCandidates(t *testing.T) {
	runsDir := t.TempDir()
	spansOnly := filepath.Join(runsDir, "spans-only")
	becameReal := filepath.Join(runsDir, "became-real")
	realRun := filepath.Join(runsDir, "real-run")
	checkpointedRun := filepath.Join(runsDir, "checkpointed-run")
	for _, dir := range []string{spansOnly, becameReal, realRun, checkpointedRun} {
		if err := os.MkdirAll(filepath.Join(dir, dirSpans), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, dirSpans, "spans.jsonl"), []byte("{}\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(realRun, fileEvents), []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(checkpointedRun, fileState), []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	candidates, err := SpansOnlyRunCandidates([]string{runsDir})
	if err != nil {
		t.Fatalf("SpansOnlyRunCandidates: %v", err)
	}
	if len(candidates) != 2 || candidates[0] != becameReal || candidates[1] != spansOnly {
		t.Fatalf("candidates = %#v, want [%s %s]", candidates, becameReal, spansOnly)
	}
	if err := os.WriteFile(filepath.Join(becameReal, fileEvents), []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	removed, err := RemoveSpansOnlyRuns(candidates)
	if err != nil {
		t.Fatalf("RemoveSpansOnlyRuns: %v", err)
	}
	if removed != 1 {
		t.Fatalf("removed = %d, want 1", removed)
	}
	if _, err := os.Stat(spansOnly); !os.IsNotExist(err) {
		t.Fatalf("spans-only directory still exists: %v", err)
	}
	for _, dir := range []string{becameReal, realRun, checkpointedRun} {
		if _, err := os.Stat(dir); err != nil {
			t.Fatalf("preserved run %s: %v", dir, err)
		}
	}
}
