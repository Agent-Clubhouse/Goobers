package creditgraph

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/telemetry"
)

// stageScenario appends a multi-stage run (a retried stage, a gate verdict,
// stage artifacts and a transcript span) and calls checkpoint after each
// stage.finished, the way the asynchronous observer would.
func stageScenario(t *testing.T, run *journal.Run, checkpoint func(stage string, attempt int)) {
	t.Helper()
	appendEvent := func(event journal.Event) {
		t.Helper()
		if err := run.Append(event); err != nil {
			t.Fatal(err)
		}
	}
	appendEvent(journal.Event{Type: journal.EventStageStarted, Stage: "plan", Attempt: 1})
	if _, err := run.RecordStageArtifact("plan", 1, "", "context.json", []byte(`{"issue":"7118"}`)); err != nil {
		t.Fatal(err)
	}
	plan, err := run.RecordStageArtifact("plan", 1, "", "plan.md", []byte("plan"))
	if err != nil {
		t.Fatal(err)
	}
	appendEvent(journal.Event{
		Type: journal.EventStageFinished, Stage: "plan", Attempt: 1, Status: "success",
		Outputs: map[string]any{"summary": "planned"}, Artifacts: []journal.Ref{plan},
	})
	checkpoint("plan", 1)
	appendEvent(journal.Event{Type: journal.EventGateEvaluated, Gate: "plan-review", Verdict: "pass", Target: "implement"})
	appendEvent(journal.Event{Type: journal.EventStageStarted, Stage: "implement", Attempt: 1})
	if _, err := run.RecordSpanWithSchema("implement", "transcript", telemetry.GenAIEventSchema,
		[]byte(`{"role":"assistant","model":"gpt-5.6-sol"}`)); err != nil {
		t.Fatal(err)
	}
	appendEvent(journal.Event{Type: journal.EventStageFinished, Stage: "implement", Attempt: 1, Status: "failure"})
	checkpoint("implement", 1)
	appendEvent(journal.Event{Type: journal.EventStageStarted, Stage: "implement", Attempt: 2, AttemptClass: journal.AttemptPolicy})
	appendEvent(journal.Event{Type: journal.EventStageFinished, Stage: "implement", Attempt: 2, Status: "success"})
	checkpoint("implement", 2)
	appendEvent(journal.Event{Type: journal.EventStageStarted, Stage: "publish", Attempt: 1})
	appendEvent(journal.Event{Type: journal.EventStageFinished, Stage: "publish", Attempt: 1, Status: "success"})
	checkpoint("publish", 1)
}

func stageBackprop(granularity string) map[string]any {
	config := map[string]any{"enabled": true, "version": "v1"}
	if granularity != "" {
		config["granularity"] = granularity
	}
	return config
}

func canonicalRecord(t *testing.T, record RunRecord) []byte {
	t.Helper()
	record.StageComposition = nil
	data, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// TestStageComposedAttributionEqualsRunLevel is the #7118 conformance test:
// for the same journal, the end-of-run record composed from stage
// checkpoints equals the run-level computation, whichever checkpoints the
// asynchronous writer managed to persist before the run finished.
func TestStageComposedAttributionEqualsRunLevel(t *testing.T) {
	for _, test := range []struct {
		name            string
		granularity     string
		checkpointAfter map[string]bool
		wantCheckpoints int
		wantComposition bool
	}{
		{name: "run granularity", granularity: "run",
			checkpointAfter: map[string]bool{"plan#1": true, "implement#1": true}},
		{name: "default granularity", checkpointAfter: map[string]bool{"plan#1": true}},
		{name: "every stage checkpointed", granularity: "stage", wantComposition: true, wantCheckpoints: 4,
			checkpointAfter: map[string]bool{"plan#1": true, "implement#1": true, "implement#2": true, "publish#1": true}},
		{name: "checkpoints lag the run", granularity: "stage", wantComposition: true, wantCheckpoints: 3,
			checkpointAfter: map[string]bool{"implement#1": true, "implement#2": true}},
		{name: "no checkpoint persisted", granularity: "stage", wantComposition: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			run, runDir := recordTestRun(t, stageBackprop(test.granularity))
			defer func() { _ = run.Close() }()
			stageScenario(t, run, func(stage string, attempt int) {
				if !test.checkpointAfter[stageAttemptKey(stage, attempt)] {
					return
				}
				if _, err := WriteStageCheckpoints(runDir); err != nil {
					t.Fatalf("WriteStageCheckpoints: %v", err)
				}
			})
			terminal := &journal.Event{Type: journal.EventRunFinished, Status: "completed"}
			if _, err := WriteRunRecord(runDir, terminal); err != nil {
				t.Fatalf("WriteRunRecord: %v", err)
			}
			composed, err := ReadRunRecord(runDir)
			if err != nil {
				t.Fatal(err)
			}
			runLevel, enrolled, err := analyzeRun(runDir, terminal, false)
			if err != nil || !enrolled {
				t.Fatalf("AnalyzeRun = %v, %v", enrolled, err)
			}
			if got, want := canonicalRecord(t, composed), canonicalRecord(t, runLevel); !bytes.Equal(got, want) {
				t.Fatalf("stage-composed record differs from run-level record:\n got %s\nwant %s", got, want)
			}
			if !test.wantComposition {
				if composed.StageComposition != nil {
					t.Fatalf("run-granular record has stage composition %+v", composed.StageComposition)
				}
				if _, err := os.Stat(filepath.Join(runDir, StageRecordsFileName)); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("run-granular run wrote stage checkpoints: %v", err)
				}
				return
			}
			if composed.StageComposition == nil || composed.StageComposition.Error != "" ||
				composed.StageComposition.Checkpoints != test.wantCheckpoints {
				t.Fatalf("stage composition = %+v, want %d verified checkpoints", composed.StageComposition, test.wantCheckpoints)
			}
		})
	}
}

// TestStageCompositionConsumesPersistedSegments pins that the end-of-run
// record is composed from what the checkpoints persisted: the checkpointed
// prefix is rebuilt from the persisted segments alone, and a missing or
// altered segment refuses composition even though the journal is intact.
func TestStageCompositionConsumesPersistedSegments(t *testing.T) {
	run, runDir := recordTestRun(t, stageBackprop("stage"))
	defer func() { _ = run.Close() }()
	stageScenario(t, run, func(stage string, _ int) {
		if stage == "publish" {
			return
		}
		if _, err := WriteStageCheckpoints(runDir); err != nil {
			t.Fatal(err)
		}
	})
	set, err := ReadVerifiedStageRecords(runDir)
	if err != nil || len(set.Records) != 3 {
		t.Fatalf("verified stage records = %+v, %v; want 3", set, err)
	}
	loaded, _, _, err := loadRun(runDir, nil)
	if err != nil {
		t.Fatal(err)
	}
	cursor := 0
	for _, record := range set.Records {
		cursor += record.EventCount
	}
	tail := loaded.events[cursor:]
	composed, err := composeStageRecords(runDir, set, tail)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := segmentBytes(composed), segmentBytes(loaded.events); !bytes.Equal(got, want) {
		t.Fatalf("segments plus tail do not reproduce the journal:\n got %s\nwant %s", got, want)
	}

	terminal := &journal.Event{Type: journal.EventRunFinished, Status: "completed"}
	runLevel, _, err := analyzeRun(runDir, terminal, false)
	if err != nil {
		t.Fatal(err)
	}
	segment, err := stageSegmentPath(runDir, set.Records[1].SegmentDigest)
	if err != nil {
		t.Fatal(err)
	}
	original, err := os.ReadFile(segment)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name  string
		alter func() error
		want  string
	}{
		{"missing segment", func() error { return os.Remove(segment) }, "read stage segment for implement#1"},
		{"altered segment", func() error {
			return os.WriteFile(segment, bytes.Replace(original, []byte(`"failure"`), []byte(`"success"`), 1), 0o600)
		}, "does not match its digest"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.alter(); err != nil {
				t.Fatal(err)
			}
			defer func() { _ = os.WriteFile(segment, original, 0o600) }()
			record, _, err := AnalyzeRun(runDir, terminal)
			if err != nil {
				t.Fatal(err)
			}
			if record.StageComposition == nil || record.StageComposition.Checkpoints != 0 ||
				!strings.Contains(record.StageComposition.Error, tc.want) {
				t.Fatalf("composition = %+v, want refusal %q", record.StageComposition, tc.want)
			}
			if !bytes.Equal(canonicalRecord(t, record), canonicalRecord(t, runLevel)) {
				t.Fatal("refused composition did not fall back to the whole journal")
			}
			if _, err := ReadVerifiedStageRecords(runDir); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("verified read = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestStageCheckpointsTileTheJournalAndCaptureStageFacts(t *testing.T) {
	run, runDir := recordTestRun(t, stageBackprop("stage"))
	defer func() { _ = run.Close() }()
	stageScenario(t, run, func(string, int) {})
	// One pass after the fact checkpoints every finished stage; a second pass
	// is idempotent.
	result, err := WriteStageCheckpoints(runDir)
	if err != nil || result.Written != 4 || result.Settled {
		t.Fatalf("first pass = %+v, %v; want 4 written, unsettled", result, err)
	}
	if result, err := WriteStageCheckpoints(runDir); err != nil || result.Written != 0 {
		t.Fatalf("second pass = %+v, %v; want nothing new", result, err)
	}
	set, err := ReadStageRecords(runDir)
	if err != nil {
		t.Fatal(err)
	}
	reader, err := journal.OpenReadOnly(runDir)
	if err != nil {
		t.Fatal(err)
	}
	events, err := reader.Events()
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"plan#1", "implement#1", "implement#2", "publish#1"}
	if len(set.Records) != len(want) {
		t.Fatalf("records = %d, want %d", len(set.Records), len(want))
	}
	next := events[0].Seq
	for i, record := range set.Records {
		if got := stageAttemptKey(record.Stage, record.Attempt); got != want[i] {
			t.Fatalf("record %d = %s, want %s", i, got, want[i])
		}
		if record.FromSequence != next {
			t.Fatalf("record %s starts at %d, want contiguous %d", want[i], record.FromSequence, next)
		}
		next = record.ThroughSequence + 1
		if len(record.Contributions) == 0 {
			t.Fatalf("record %s has no provisional contributions", want[i])
		}
	}
	plan, implement := set.Records[0], set.Records[1]
	if plan.Status != "success" || plan.Outputs["summary"] != "planned" || len(plan.Artifacts) != 1 ||
		len(plan.Inputs) != 1 || plan.Inputs[0].Name != "context.json" {
		t.Fatalf("plan record = %+v, want status, outputs, the declared artifact and the consumed context", plan)
	}
	if len(implement.Verdicts) != 1 || implement.Verdicts[0].Gate != "plan-review" || implement.Verdicts[0].Verdict != "pass" {
		t.Fatalf("implement verdicts = %+v, want the plan-review gate", implement.Verdicts)
	}
	if len(implement.Evidence) == 0 {
		t.Fatalf("implement record has no evidence links")
	}
	for _, link := range implement.Evidence {
		if link.Stage != "implement" || link.JournalSequence < int64(implement.FromSequence) {
			t.Fatalf("implement evidence %+v escapes its stage segment", link)
		}
	}
}

func TestStageCompositionRefusesTamperedCheckpoints(t *testing.T) {
	for _, tc := range []struct {
		name   string
		tamper func(*StageRecordSet)
		want   string
	}{
		{"removed", func(set *StageRecordSet) { set.Records = append(set.Records[:1], set.Records[2:]...) }, "does not match"},
		{"edited fact with fresh digest", func(set *StageRecordSet) {
			set.Records[0].Status = "failure"
			set.Records[0].RecordDigest = stageRecordDigest(set.Records[0])
		}, "facts do not match"},
		{"edited provisional attribution", func(set *StageRecordSet) {
			set.Records[0].Contributions = nil
		}, "record digest"},
		{"edited provisional attribution with fresh digest", func(set *StageRecordSet) {
			set.Records[1].Contributions = set.Records[1].Contributions[:1]
			set.Records[1].RecordDigest = stageRecordDigest(set.Records[1])
		}, "attribution does not match"},
		{"another run's checkpoints", func(set *StageRecordSet) { set.RunID = "run-other" }, `belong to run "run-other"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			run, runDir := recordTestRun(t, stageBackprop("stage"))
			defer func() { _ = run.Close() }()
			stageScenario(t, run, func(string, int) {})
			if _, err := WriteStageCheckpoints(runDir); err != nil {
				t.Fatal(err)
			}
			set, err := ReadVerifiedStageRecords(runDir)
			if err != nil {
				t.Fatalf("untampered checkpoints do not verify: %v", err)
			}
			tc.tamper(&set)
			data, err := json.Marshal(set)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(runDir, StageRecordsFileName), data, 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := ReadVerifiedStageRecords(runDir); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("verified read of tampered checkpoints = %v, want %q", err, tc.want)
			}
			terminal := &journal.Event{Type: journal.EventRunFinished, Status: "completed"}
			composed, _, err := AnalyzeRun(runDir, terminal)
			if err != nil {
				t.Fatal(err)
			}
			if composed.StageComposition == nil || composed.StageComposition.Checkpoints != 0 ||
				!strings.Contains(composed.StageComposition.Error, tc.want) {
				t.Fatalf("composition = %+v, want a disclosed refusal", composed.StageComposition)
			}
			runLevel, _, err := analyzeRun(runDir, terminal, false)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(canonicalRecord(t, composed), canonicalRecord(t, runLevel)) {
				t.Fatalf("refused composition did not fall back to the whole journal")
			}
			// The writer re-verifies only once another stage finishes.
			for _, event := range []journal.Event{
				{Type: journal.EventStageStarted, Stage: "review", Attempt: 1},
				{Type: journal.EventStageFinished, Stage: "review", Attempt: 1, Status: "success"},
			} {
				if err := run.Append(event); err != nil {
					t.Fatal(err)
				}
			}
			if result, err := WriteStageCheckpoints(runDir); err == nil || !result.Settled {
				t.Fatalf("checkpointing over tampered records = %+v, %v; want a settled error", result, err)
			}
		})
	}
}

func TestStageCheckpointsSkipAppendsThatFinishNoStage(t *testing.T) {
	run, runDir := recordTestRun(t, stageBackprop("stage"))
	defer func() { _ = run.Close() }()
	stageScenario(t, run, func(string, int) {})
	if _, err := WriteStageCheckpoints(runDir); err != nil {
		t.Fatal(err)
	}
	if err := run.Append(journal.Event{Type: journal.EventStageStarted, Stage: "review", Attempt: 1}); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(filepath.Join(runDir, StageRecordsFileName))
	if err != nil {
		t.Fatal(err)
	}
	result, err := WriteStageCheckpoints(runDir)
	if err != nil || result.Written != 0 || result.Settled {
		t.Fatalf("pass without a finished stage = %+v, %v; want nothing written, unsettled", result, err)
	}
	after, err := os.ReadFile(filepath.Join(runDir, StageRecordsFileName))
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("pass without a finished stage rewrote checkpoints: %v", err)
	}
}

// TestShadowRunsWriteNoStageCheckpoints pins that stage granularity applies
// only to active runs: a shadow run's checkpoint passes write nothing, and its
// on-demand shadow analysis still covers the whole journal.
func TestShadowRunsWriteNoStageCheckpoints(t *testing.T) {
	run, runDir := recordTestRun(t, map[string]any{"mode": "shadow", "version": "v1", "granularity": "stage"})
	defer func() { _ = run.Close() }()
	stageScenario(t, run, func(string, int) {
		result, err := WriteStageCheckpoints(runDir)
		if err != nil || result.Written != 0 {
			t.Fatalf("shadow checkpoint pass = %+v, %v; want nothing written", result, err)
		}
	})
	if err := run.Append(journal.Event{Type: journal.EventRunFinished, Status: "completed"}); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{StageRecordsFileName, RecordFileName} {
		if _, err := os.Stat(filepath.Join(runDir, name)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("shadow run wrote %s: %v", name, err)
		}
	}
	observation, observed, err := ObserveShadowRun(runDir, nil)
	if err != nil || !observed || observation.Record.Status == RecordFailed || observation.Record.StageComposition != nil {
		t.Fatalf("ObserveShadowRun = %+v, %v, %v; want an unfailed whole-journal shadow record", observation, observed, err)
	}
}
func TestStageCheckpointPublicationIsAtomicWithTerminalization(t *testing.T) {
	for _, tc := range []struct {
		phase       string
		checkpoints int
	}{
		// Finished before the pass re-checks under the lock: the write is dropped.
		{"before-lock", 0},
		// Finished while the pass holds the lock: the end-of-run record waits
		// for the checkpoints and composes them.
		{"locked", 4},
	} {
		t.Run(tc.phase, func(t *testing.T) {
			run, runDir := recordTestRun(t, stageBackprop("stage"))
			defer func() { _ = run.Close() }()
			stageScenario(t, run, func(string, int) {})
			reached, release := make(chan struct{}), make(chan struct{})
			stagePublishHook = func(phase string) {
				if phase == tc.phase {
					close(reached)
					<-release
				}
			}
			defer func() { stagePublishHook = nil }()
			type passResult struct {
				result StageCheckpointResult
				err    error
			}
			pass := make(chan passResult, 1)
			go func() {
				result, err := WriteStageCheckpoints(runDir)
				pass <- passResult{result, err}
			}()
			<-reached
			if err := run.Append(journal.Event{Type: journal.EventRunFinished, Status: "completed"}); err != nil {
				t.Fatal(err)
			}
			recorded := make(chan error, 1)
			go func() {
				_, err := WriteRunRecord(runDir, nil)
				recorded <- err
			}()
			if tc.checkpoints == 0 {
				if err := <-recorded; err != nil {
					t.Fatal(err)
				}
				close(release)
			} else {
				select {
				case err := <-recorded:
					t.Fatalf("end-of-run record published while a checkpoint pass held the lock: %v", err)
				case <-time.After(200 * time.Millisecond):
				}
				close(release)
				if err := <-recorded; err != nil {
					t.Fatal(err)
				}
			}
			got := <-pass
			if got.err != nil || got.result.Written != tc.checkpoints {
				t.Fatalf("checkpoint pass = %+v, %v; want %d written", got.result, got.err, tc.checkpoints)
			}
			data, err := os.ReadFile(filepath.Join(runDir, RecordFileName))
			if err != nil {
				t.Fatal(err)
			}
			var record RunRecord
			if err := json.Unmarshal(data, &record); err != nil {
				t.Fatal(err)
			}
			set, err := ReadStageRecords(runDir)
			if errors.Is(err, os.ErrNotExist) {
				err = nil
			}
			if err != nil || record.StageComposition == nil || record.StageComposition.Checkpoints != len(set.Records) ||
				len(set.Records) != tc.checkpoints {
				t.Fatalf("record composed %+v over %d persisted checkpoints (%v); want %d of each",
					record.StageComposition, len(set.Records), err, tc.checkpoints)
			}
		})
	}
}

func TestStageCheckpointLosingTheLockToTheRunRecordIsNotAFailure(t *testing.T) {
	run, runDir := recordTestRun(t, stageBackprop("stage"))
	defer func() { _ = run.Close() }()
	stageScenario(t, run, func(string, int) {})
	wait := stagePublishLockWait
	stagePublishLockWait = 50 * time.Millisecond
	var held interface{ Release() error }
	stagePublishHook = func(phase string) {
		if phase != "before-lock" {
			return
		}
		if err := run.Append(journal.Event{Type: journal.EventRunFinished, Status: "completed"}); err != nil {
			t.Error(err)
		}
		handle, err := lockStagePublication(runDir, time.Second)
		if err != nil {
			t.Error(err)
			return
		}
		held = handle
	}
	defer func() {
		stagePublishHook, stagePublishLockWait = nil, wait
		if held != nil {
			_ = held.Release()
		}
	}()
	if result, err := WriteStageCheckpoints(runDir); err != nil || result.Written != 0 {
		t.Fatalf("pass that lost the lock to a finished run = %+v, %v; want a quiet drop", result, err)
	}
	if _, err := os.Stat(filepath.Join(runDir, StageRecordsFileName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("dropped pass published checkpoints: %v", err)
	}
}

func TestStageCheckpointsStopOnceTheRunFinishedUntilItIsResumed(t *testing.T) {
	run, runDir := recordTestRun(t, stageBackprop("stage"))
	defer func() { _ = run.Close() }()
	stageScenario(t, run, func(string, int) {})
	if err := run.Append(journal.Event{Type: journal.EventRunFinished, Status: "escalated"}); err != nil {
		t.Fatal(err)
	}
	result, err := WriteStageCheckpoints(runDir)
	if err != nil || result.Written != 0 || result.Settled {
		t.Fatalf("finished run pass = %+v, %v; want nothing written, unsettled so a resume is seen", result, err)
	}
	for _, event := range []journal.Event{
		{Type: journal.EventRunResumed},
		{Type: journal.EventStageStarted, Stage: "review", Attempt: 1},
		{Type: journal.EventStageFinished, Stage: "review", Attempt: 1, Status: "success"},
	} {
		if err := run.Append(event); err != nil {
			t.Fatal(err)
		}
	}
	if result, err := WriteStageCheckpoints(runDir); err != nil || result.Written != 5 {
		t.Fatalf("resumed run pass = %+v, %v; want all five stages checkpointed", result, err)
	}
	if _, err := ReadVerifiedStageRecords(runDir); err != nil {
		t.Fatalf("resumed run checkpoints do not verify: %v", err)
	}
}

func TestStageCheckpointerIsAsyncAndBestEffort(t *testing.T) {
	run, runDir := recordTestRun(t, stageBackprop("stage"))
	defer func() { _ = run.Close() }()
	stageScenario(t, run, func(string, int) {})

	var mu sync.Mutex
	var reported []string
	release := make(chan struct{})
	checkpointer := NewStageCheckpointer(func(runID string) (string, error) {
		<-release
		if runID == "missing" {
			return "", os.ErrNotExist
		}
		return runDir, nil
	}, func(runID string, err error) {
		mu.Lock()
		defer mu.Unlock()
		reported = append(reported, runID+": "+err.Error())
	})
	// Observe must return while the worker is blocked: the journal append
	// path never waits on checkpointing.
	checkpointer.Observe(context.Background(), "run-enabled", 1)
	checkpointer.Observe(context.Background(), "run-enabled", 2)
	checkpointer.Observe(context.Background(), "missing", 1)
	close(release)
	checkpointer.worker.Wait()

	set, err := ReadStageRecords(runDir)
	if err != nil || len(set.Records) != 4 {
		t.Fatalf("stage records = %+v, %v; want 4", set, err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(reported) != 1 || !strings.HasPrefix(reported[0], "missing: ") {
		t.Fatalf("reported = %v, want only the unresolvable run", reported)
	}
}
