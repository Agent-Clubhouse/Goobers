package httpapi

import (
	"bytes"
	"encoding/json"
	"log"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/livejournal"
	"github.com/goobers/goobers/internal/readservice"
)

// journalplane_adopt_test.go tests decision 003 ruling 5's first bullet at the
// seam the change actually crosses: a mode-3 stage pod POSTing to the write
// API's journal plane for a run the daemon's RUNNER is driving in-process.
//
// The seam has two sides in one process. The runner holds an open
// *journal.Run for the run's whole life, and that handle holds the per-run-dir
// file lock for the run's whole life (journal/run.go acquireRunLock). The
// live writer, reached through this HTTP plane, opens journals of its own.
// Without livejournal.Writer.Adopt those are two independent opens of one
// events.jsonl, which the lock exists to prevent — so the pod's emit does not
// get a second writer, it gets journal.ErrLockTimeout after a 30-second poll,
// and the bytes it was surrendering pointers to are never stored.
//
// A shortened lock timeout keeps the negative control at test speed; the
// production bound is 30s (journal/lock.go journalLockTimeout).

// runnerHeldRun is a trigger-started run the daemon's runner is driving: a real
// journal created through journal.Create and HELD OPEN, the way internal/runner
// holds it from Start to terminal. Returns the runs directory and the runner's
// handle.
func runnerHeldRun(t *testing.T, runID string) (string, *journal.Run) {
	t.Helper()
	runsDir := filepath.Join(t.TempDir(), "runs")
	jr, err := journal.Create(runsDir, journal.RunIdentity{
		RunID:           runID,
		Workflow:        "implementation",
		WorkflowVersion: 1,
		WorkflowDigest:  "sha256:abc",
		Gaggle:          "web",
		Trigger:         journal.Trigger{Kind: journal.TriggerSchedule},
	}, nil)
	if err != nil {
		t.Fatalf("create runner-held journal: %v", err)
	}
	t.Cleanup(func() { _ = jr.Close() })
	// The runner's own first append for the placed stage's attempt.
	if err := jr.Append(journal.Event{Type: journal.EventStageStarted, Stage: "open-pr", Attempt: 1}); err != nil {
		t.Fatalf("runner stage.started: %v", err)
	}
	return runsDir, jr
}

// journalPlaneOver wires the REAL live writer behind the REAL journal-plane
// route, with a capturing error log so a writer refusal can be named (the
// plane answers 500/write_failed and logs the cause).
func journalPlaneOver(t *testing.T, runsDir string) (http.Handler, *livejournal.Writer, *bytes.Buffer) {
	t.Helper()
	writer, err := livejournal.NewWriter(func(gaggle string) (string, bool) {
		if gaggle != "web" {
			return "", false
		}
		return runsDir, true
	})
	if err != nil {
		t.Fatalf("new live writer: %v", err)
	}
	t.Cleanup(writer.Close)
	errorLog := &bytes.Buffer{}
	handler, err := NewHandler(
		&fakeReader{health: readservice.Health{Ready: true}}, AllowAll,
		log.New(errorLog, "", 0), WithJournalService(writer),
	)
	if err != nil {
		t.Fatalf("new handler: %v", err)
	}
	return handler, writer, errorLog
}

// podArtifactEmit is the batch cmd/goobers's recordStageArtifacts sends from a
// stage pod: one content-addressed stage artifact under the attempt's
// idempotency key.
func podArtifactEmit(t *testing.T, runID, name string, data []byte) *http.Request {
	t.Helper()
	body, err := json.Marshal(livejournal.EmitRequest{
		RunID:  runID,
		Gaggle: "web",
		Ops: []livejournal.Op{{
			Kind: livejournal.OpArtifact,
			Key:  runID + "|0|open-pr|1|0",
			Time: time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC),
			Artifact: &livejournal.ArtifactOp{
				Stage: "open-pr", Attempt: 1, Name: name, Data: data,
			},
		}},
	})
	if err != nil {
		t.Fatalf("marshal emit: %v", err)
	}
	return jsonRequest(http.MethodPost, "/api/v1/runs/"+runID+"/journal/emit", string(body))
}

func runEvents(t *testing.T, runsDir, runID string) []journal.Event {
	t.Helper()
	reader, err := journal.OpenRead(filepath.Join(runsDir, runID))
	if err != nil {
		t.Fatalf("open run for reading: %v", err)
	}
	events, err := reader.Events()
	if err != nil {
		t.Fatalf("read events: %v", err)
	}
	return events
}

// TestAdoptedRunTakesPodPlaneEmitsThroughTheRunnersOpenHandle is the fix at the
// seam: with the runner's handle adopted, a pod's HTTP emit for that run
// appends through it — promptly (no lock is contended), durably (the artifact
// bytes land under the run), and interleaved with the runner's own appends on
// the same handle, which is far-side evidence item 3's exact shape.
func TestAdoptedRunTakesPodPlaneEmitsThroughTheRunnersOpenHandle(t *testing.T) {
	// Shortened so "did not wait out the lock" is a claim this test can make
	// in well under a second rather than by out-waiting the 30s production
	// bound. The assertion below is that the emit finished far inside even
	// this shrunken window.
	t.Cleanup(journal.SetLockTimeoutForTest(2*time.Second, 20*time.Millisecond))

	runsDir, jr := runnerHeldRun(t, "run-adopted")
	handler, writer, errorLog := journalPlaneOver(t, runsDir)

	release, err := writer.Adopt("run-adopted", "web", jr)
	if err != nil {
		t.Fatalf("Adopt: %v", err)
	}
	defer release()

	payload := []byte(`{"number":42,"url":"https://example.invalid/pr/42"}`)
	started := time.Now()
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, podArtifactEmit(t, "run-adopted", "pr.json", payload))
	elapsed := time.Since(started)
	if response.Code != http.StatusOK {
		t.Fatalf("emit status = %d, body = %s, error log = %s", response.Code, response.Body, errorLog)
	}
	if elapsed > time.Second {
		t.Fatalf("emit took %s: it contended the run lock instead of using the adopted handle", elapsed)
	}
	if errorLog.Len() != 0 {
		t.Fatalf("journal plane logged an error: %s", errorLog)
	}
	var decoded livejournal.EmitResponse
	if err := json.NewDecoder(response.Body).Decode(&decoded); err != nil {
		t.Fatalf("decode emit response: %v", err)
	}
	if decoded.Applied != 1 || decoded.Deduplicated != 0 || decoded.Terminal {
		t.Fatalf("emit response = %+v, want one applied op on a live run", decoded)
	}
	if !writer.IsOpen("run-adopted") {
		t.Fatal("an adopted run must read as open to the repair projection")
	}

	// The runner keeps driving the same attempt through the same handle: the
	// pod's append did not take the journal away from it.
	if err := jr.Append(journal.Event{Type: journal.EventStageHeartbeat, Stage: "open-pr", Attempt: 1}); err != nil {
		t.Fatalf("runner heartbeat after the pod's emit: %v", err)
	}

	events := runEvents(t, runsDir, "run-adopted")
	if err := journal.MonotonicSeq(events); err != nil {
		t.Fatalf("one handle, one seq counter: %v", err)
	}
	var types []journal.EventType
	for _, ev := range events {
		types = append(types, ev.Type)
	}
	want := []journal.EventType{
		journal.EventRunStarted,       // journal.Create, by the runner
		journal.EventStageStarted,     // the runner
		journal.EventArtifactRecorded, // the POD, through the plane
		journal.EventStageHeartbeat,   // the runner again, after the pod
	}
	if len(types) != len(want) {
		t.Fatalf("events = %v, want %v", types, want)
	}
	for i := range want {
		if types[i] != want[i] {
			t.Fatalf("events = %v, want %v", types, want)
		}
	}

	recorded := events[2]
	if key, _ := recorded.Runner[livejournal.EmitKeyRunnerField].(string); key != "run-adopted|0|open-pr|1|0" {
		t.Fatalf("artifact.recorded emit key = %q", key)
	}
	if recorded.Stage != "open-pr" || recorded.Attempt != 1 {
		t.Fatalf("artifact.recorded stage/attempt = %s/%d", recorded.Stage, recorded.Attempt)
	}
	if recorded.Ref == nil {
		t.Fatal("artifact.recorded carries no ref")
	}
	// The bytes the pod surrendered a pointer to actually exist under the run.
	reader, err := journal.OpenRead(filepath.Join(runsDir, "run-adopted"))
	if err != nil {
		t.Fatal(err)
	}
	stored, err := reader.ArtifactBytes(*recorded.Ref)
	if err != nil {
		t.Fatalf("read stored artifact: %v", err)
	}
	if !bytes.Equal(stored, payload) {
		t.Fatalf("stored artifact = %s, want %s", stored, payload)
	}

	// Redelivery of the same batch dedupes against the keys Adopt derived, so
	// the pod's own retries cannot double-record.
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, podArtifactEmit(t, "run-adopted", "pr.json", payload))
	if response.Code != http.StatusOK {
		t.Fatalf("redelivered emit status = %d, body = %s", response.Code, response.Body)
	}
	decoded = livejournal.EmitResponse{}
	if err := json.NewDecoder(response.Body).Decode(&decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Applied != 0 || decoded.Deduplicated != 1 {
		t.Fatalf("redelivered emit = %+v, want it deduplicated", decoded)
	}

	// Release ends the loan and leaves the runner's handle open and usable.
	release()
	if writer.IsOpen("run-adopted") {
		t.Fatal("release must deregister the adoption")
	}
	if err := jr.Append(journal.Event{Type: journal.EventStageFinished, Stage: "open-pr", Attempt: 1, Status: "success"}); err != nil {
		t.Fatalf("runner append after release: %v", err)
	}
}

// TestPodPlaneEmitWithoutAdoptionWedgesOnTheRunnersLock is the negative
// control this fix exists for, and it must keep passing after it: with no
// adoption the very same request goes down Emit -> acquire -> rehydrate ->
// journal.Recover -> acquireRunLock, waits out the whole lock timeout against
// the handle the runner holds, and fails with journal.ErrLockTimeout — with
// nothing appended and no artifact bytes stored.
func TestPodPlaneEmitWithoutAdoptionWedgesOnTheRunnersLock(t *testing.T) {
	// 30s in production; shrunk here so the wedge is observable in a test.
	const timeout = 300 * time.Millisecond
	t.Cleanup(journal.SetLockTimeoutForTest(timeout, 20*time.Millisecond))

	runsDir, _ := runnerHeldRun(t, "run-unadopted")
	handler, writer, errorLog := journalPlaneOver(t, runsDir)
	if writer.IsOpen("run-unadopted") {
		t.Fatal("the writer must not hold a runner-driven run")
	}

	started := time.Now()
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, podArtifactEmit(t, "run-unadopted", "pr.json", []byte(`{"number":42}`)))
	elapsed := time.Since(started)

	if response.Code != http.StatusInternalServerError {
		t.Fatalf("emit status = %d, want 500; body = %s", response.Code, response.Body)
	}
	if logged := errorLog.String(); !strings.Contains(logged, journal.ErrLockTimeout.Error()) {
		t.Fatalf("error log = %q, want the run-dir lock timeout", logged)
	}
	if elapsed < timeout {
		t.Fatalf("emit failed in %s, faster than the %s lock timeout: it did not contend the run lock", elapsed, timeout)
	}

	// The pod surrendered a pointer to bytes that were never stored.
	for _, ev := range runEvents(t, runsDir, "run-unadopted") {
		if ev.Type == journal.EventArtifactRecorded {
			t.Fatalf("artifact.recorded landed without adoption: %+v", ev)
		}
	}
}
