package rollup

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestIngestRunCapturesHarnessTranscripts is issue #128's first defect:
// production records within-stage harness data (agent transcripts, tool
// output) via journal.Run.RecordSpan, which appends a span.recorded event —
// but v1's ingest.go had no case for it at all, so that data existed on disk
// and was completely unqueryable through the rollup.
func TestIngestRunCapturesHarnessTranscripts(t *testing.T) {
	tmp := t.TempDir()
	runsDir := filepath.Join(tmp, "runs")
	dir := filepath.Join(runsDir, fixtureRunID)
	events := strings.Join([]string{
		rawEventLine(1, "run.started"),
		`{"schema":"goobers.dev/journal/event/v1","seq":2,"branch":0,"time":"2026-07-13T00:00:02Z","type":"span.recorded","stage":"implement","name":"copilot-transcript","ref":{"path":"spans/sha256/ab/cdef","digest":"sha256:abcdef","size":4096}}`,
		rawEventLine(3, "run.finished"),
	}, "\n") + "\n"
	writeRunWithRawEvents(t, runsDir, fixtureRunID, events, "")

	db := openTestDB(t, tmp)
	if err := db.IngestRun(dir); err != nil {
		t.Fatalf("IngestRun: %v", err)
	}

	transcripts, err := db.HarnessTranscripts(fixtureRunID)
	if err != nil {
		t.Fatalf("HarnessTranscripts: %v", err)
	}
	if len(transcripts) != 1 {
		t.Fatalf("transcripts = %#v, want exactly 1", transcripts)
	}
	tr := transcripts[0]
	if tr.Stage != "implement" || tr.Name != "copilot-transcript" || tr.RefDigest != "sha256:abcdef" || tr.RefSize != 4096 {
		t.Fatalf("unexpected transcript row: %#v", tr)
	}
}

// TestIngestRunCapturesGateRunnerDetail is issue #128's third defect: a
// gate.evaluated event's Runner{repassAttempt, escalated} plus its verdict
// artifact Ref (decision/rationale/evidence, for an agentic gate) were both
// discarded on ingest — gate_verdicts.runner_json was permanently NULL, so
// "gate X failed 3 repasses then escalated" (the TUT-010 gate-noise signal)
// was unanswerable from telemetry.db even though the journal had it.
func TestIngestRunCapturesGateRunnerDetail(t *testing.T) {
	tmp := t.TempDir()
	runsDir := filepath.Join(tmp, "runs")
	dir := filepath.Join(runsDir, fixtureRunID)
	events := strings.Join([]string{
		rawEventLine(1, "run.started"),
		`{"schema":"goobers.dev/journal/event/v1","seq":2,"branch":0,"time":"2026-07-13T00:00:02Z","type":"gate.evaluated","gate":"review","verdict":"fail","target":"implement","name":"verdict/review-2.json","ref":{"path":"artifacts/sha256/12/3456","digest":"sha256:123456","size":512},"runner":{"repassAttempt":2,"escalated":true}}`,
		rawEventLine(3, "run.finished"),
	}, "\n") + "\n"
	writeRunWithRawEvents(t, runsDir, fixtureRunID, events, "")

	db := openTestDB(t, tmp)
	if err := db.IngestRun(dir); err != nil {
		t.Fatalf("IngestRun: %v", err)
	}

	verdicts, err := db.GateVerdicts(fixtureRunID)
	if err != nil {
		t.Fatalf("GateVerdicts: %v", err)
	}
	if len(verdicts) != 1 {
		t.Fatalf("verdicts = %#v, want exactly 1", verdicts)
	}
	rj := verdicts[0].RunnerJSON
	if rj == "" {
		t.Fatal("runner_json is empty, want repassAttempt/escalated/verdictRef captured")
	}
	for _, want := range []string{`"repassAttempt":2`, `"escalated":true`, `"verdictRef"`, `"sha256:123456"`} {
		if !strings.Contains(rj, want) {
			t.Fatalf("runner_json = %q, want it to contain %q", rj, want)
		}
	}
}

// TestIngestSchedulerLogCapturesDecisions is issue #128's second defect:
// scheduler decisions (trigger.fired/tick.skipped) and claim-ledger
// transitions (claim.acquired/claim.released) live in
// <instance-root>/scheduler/events.jsonl, but Rebuild only ever scanned run
// directories — "why didn't a run start at tick N" was unanswerable from
// telemetry.db no matter how long the daemon had run.
func TestIngestSchedulerLogCapturesDecisions(t *testing.T) {
	tmp := t.TempDir()
	schedulerDir := filepath.Join(tmp, "scheduler")
	if err := writeInstanceEvents(t, schedulerDir, []string{
		instanceEventLine(1, "trigger.fired", `"workflow":"nominate","reason":"scheduled"`),
		instanceEventLine(2, "tick.skipped", `"workflow":"nominate","reason":"conditions: max-parallel"`),
		instanceEventLine(3, "claim.acquired", `"runId":"`+fixtureRunID+`"`),
		instanceEventLine(4, "run.started", `"workflow":"nominate","runId":"`+fixtureRunID+`"`),
		instanceEventLine(5, "run.finished", `"workflow":"nominate","runId":"`+fixtureRunID+`","status":"completed"`),
		instanceEventLine(6, "claim.released", `"runId":"`+fixtureRunID+`"`),
	}); err != nil {
		t.Fatal(err)
	}

	db := openTestDB(t, tmp)
	if err := db.IngestSchedulerLog(schedulerDir); err != nil {
		t.Fatalf("IngestSchedulerLog: %v", err)
	}

	events, err := db.SchedulerEvents("")
	if err != nil {
		t.Fatalf("SchedulerEvents: %v", err)
	}
	if len(events) != 6 {
		t.Fatalf("scheduler events = %d, want 6: %#v", len(events), events)
	}
	if events[1].Type != "tick.skipped" || events[1].Reason != "conditions: max-parallel" {
		t.Fatalf("tick.skipped row = %#v", events[1])
	}
	if events[4].Type != "run.finished" || events[4].Status != "completed" || events[4].RunID != fixtureRunID {
		t.Fatalf("run.finished row = %#v", events[4])
	}

	// Filtering to one workflow excludes claim.* events, which carry no
	// workflow field (only runId) — this is the "why didn't a run start"
	// per-workflow query shape callers actually need.
	filtered, err := db.SchedulerEvents("nominate")
	if err != nil {
		t.Fatalf("SchedulerEvents filtered: %v", err)
	}
	if len(filtered) != 4 {
		t.Fatalf("filtered scheduler events = %d, want 4 (trigger.fired/tick.skipped/run.started/run.finished): %#v", len(filtered), filtered)
	}
}

func TestIngestSchedulerLogToleratesDuplicateSequence(t *testing.T) {
	tmp := t.TempDir()
	schedulerDir := filepath.Join(tmp, "scheduler")
	if err := writeInstanceEvents(t, schedulerDir, []string{
		instanceEventLine(1, "trigger.fired", `"workflow":"nominate","reason":"scheduled"`),
		instanceEventLine(2, "claim.acquired", `"runId":"`+fixtureRunID+`"`),
		instanceEventLine(2, "trigger.fired", `"workflow":"implement","reason":"scheduled"`),
		instanceEventLine(3, "run.started", `"workflow":"nominate","runId":"`+fixtureRunID+`"`),
	}); err != nil {
		t.Fatal(err)
	}

	db := openTestDB(t, tmp)
	for i := 0; i < 2; i++ {
		if err := db.IngestSchedulerLog(schedulerDir); err != nil {
			t.Fatalf("IngestSchedulerLog attempt %d: %v", i+1, err)
		}
	}

	events, err := db.SchedulerEvents("")
	if err != nil {
		t.Fatalf("SchedulerEvents: %v", err)
	}
	if len(events) != 3 {
		t.Fatalf("scheduler events = %d, want 3: %#v", len(events), events)
	}
	for i, event := range events {
		if event.Seq != uint64(i+1) {
			t.Fatalf("event %d seq = %d, want %d", i, event.Seq, i+1)
		}
	}
	if events[1].Type != "claim.acquired" || events[1].RunID != fixtureRunID {
		t.Fatalf("duplicate seq retained %#v, want first journal record", events[1])
	}
}

// TestRebuildIngestsSchedulerLog proves Rebuild — not just the incremental
// IngestSchedulerLog call — picks up the instance journal too, since
// `goobers telemetry --rebuild` is the documented recovery path for an
// instance whose daemon predates issue #128's incremental wiring.
func TestRebuildIngestsSchedulerLog(t *testing.T) {
	tmp := t.TempDir()
	runsDir := filepath.Join(tmp, "runs")
	schedulerDir := filepath.Join(tmp, "scheduler")
	writeMinimalFixtureRun(t, runsDir, fixtureRunID, fixtureStart)
	if err := writeInstanceEvents(t, schedulerDir, []string{
		instanceEventLine(1, "trigger.fired", `"workflow":"nominate","reason":"scheduled"`),
	}); err != nil {
		t.Fatal(err)
	}

	dbPath := filepath.Join(tmp, "telemetry.db")
	if err := Rebuild(dbPath, runsDir, schedulerDir); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	db := openTestDB(t, tmp)
	events, err := db.SchedulerEvents("")
	if err != nil {
		t.Fatalf("SchedulerEvents: %v", err)
	}
	if len(events) != 1 || events[0].Type != "trigger.fired" {
		t.Fatalf("scheduler events after Rebuild = %#v, want 1 trigger.fired", events)
	}
}

// instanceEventLine builds one raw scheduler/events.jsonl line.
func instanceEventLine(seq int, typ, extraFields string) string {
	ts := fixtureStart.Add(time.Duration(seq) * time.Second).UTC().Format(time.RFC3339Nano)
	return fmt.Sprintf(`{"schema":"goobers.dev/journal/event/v1","seq":%d,"branch":0,"time":%q,"type":%q,%s}`, seq, ts, typ, extraFields)
}

func writeInstanceEvents(t *testing.T, schedulerDir string, lines []string) error {
	t.Helper()
	if err := os.MkdirAll(schedulerDir, 0o755); err != nil {
		return err
	}
	body := strings.Join(lines, "\n") + "\n"
	return os.WriteFile(filepath.Join(schedulerDir, fileEvents), []byte(body), 0o644)
}
