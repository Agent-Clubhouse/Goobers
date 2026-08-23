package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/e2e"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/runnercap"
)

// e2eTestStage is one fabricated stage attempt for the fixture runs below:
// enough to drive S1 (fresh pod) and S2 (OS hop).
type e2eTestStage struct {
	name   string
	number int
	pod    string
	node   string
	os     string
}

// newE2ETestRun writes a real run journal under root/runID with one attempt
// per stage — stage.started, runner.placement, stage.finished — followed by
// run.finished, so the read-side (readservice.NewOfflineRuns, the same path
// `goobers trace` uses) projects StageAttempt.Placement and a terminal phase
// exactly as a live distributed run would.
func newE2ETestRun(t *testing.T, root, runID, gaggle string, stages []e2eTestStage, finishedStatus journal.RunPhase) {
	t.Helper()
	l := instance.NewLayout(root)
	startedAt := time.Date(2026, time.August, 1, 12, 0, 0, 0, time.UTC)
	jr, err := journal.Create(l.RunsDir(), journal.RunIdentity{
		RunID:     runID,
		Workflow:  "e2e-fixture",
		Gaggle:    gaggle,
		Trigger:   journal.Trigger{Kind: journal.TriggerManual},
		StartedAt: startedAt,
	}, nil)
	if err != nil {
		t.Fatalf("create run: %v", err)
	}

	at := startedAt
	tick := func() time.Time {
		at = at.Add(time.Second)
		return at
	}

	for _, stage := range stages {
		if err := jr.Append(journal.Event{
			Type: journal.EventStageStarted, Stage: stage.name, Attempt: stage.number, Time: tick(),
		}); err != nil {
			t.Fatalf("append stage.started: %v", err)
		}
		if err := jr.Append(journal.PlacementEvent(stage.name, stage.number, "", journal.Placement{
			Runner: "runner-a", Pod: stage.pod, Node: stage.node, OS: stage.os,
		})); err != nil {
			t.Fatalf("append runner.placement: %v", err)
		}
		if err := jr.Append(journal.Event{
			Type: journal.EventStageFinished, Stage: stage.name, Attempt: stage.number,
			Status: "success", Time: tick(),
		}); err != nil {
			t.Fatalf("append stage.finished: %v", err)
		}
	}
	if err := jr.Append(journal.Event{Type: journal.EventRunFinished, Status: string(finishedStatus), Time: tick()}); err != nil {
		t.Fatalf("append run.finished: %v", err)
	}
	if err := jr.Close(); err != nil {
		t.Fatalf("close run: %v", err)
	}
}

func TestE2EVerifyPassesFromRecordedRunDataAlone(t *testing.T) {
	root := t.TempDir()
	const runID = "e2e-verify-pass-run"
	newE2ETestRun(t, root, runID, "goobers", []e2eTestStage{
		{name: "implement", number: 1, pod: "pod-implement-1", node: "node-a", os: "linux"},
		{name: "windows-build", number: 1, pod: "pod-windows-build-1", node: "node-b", os: "windows"},
	}, journal.PhaseCompleted)

	code, stdout, stderr := runArgs(t, "e2e", "verify", "--run", runID, root)
	if code != 0 {
		t.Fatalf("code = %d, want 0; stderr=%s", code, stderr)
	}

	var bundle e2e.Bundle
	if err := json.Unmarshal([]byte(stdout), &bundle); err != nil {
		t.Fatalf("stdout is not a valid evidence bundle: %v\n%s", err, stdout)
	}
	if bundle.Overall() != e2e.VerdictPass {
		t.Fatalf("bundle.Overall() = %v, want pass; items=%#v", bundle.Overall(), bundle.Items)
	}
	got := make(map[e2e.SmokeItemID]e2e.Verdict, len(bundle.Items))
	for _, item := range bundle.Items {
		got[item.Item] = item.Verdict
	}
	for _, want := range []e2e.SmokeItemID{e2e.ItemS1FreshPod, e2e.ItemS2OSHop} {
		if got[want] != e2e.VerdictPass {
			t.Errorf("item %s verdict = %v, want pass", want, got[want])
		}
	}
	// The optional items — including S8, which needs a REAL live-capture
	// (--expected "liveVisibility"), never a journal-derived substitute —
	// were never asked (no --expected), so they carry no verdict at all:
	// never a silent pass.
	for _, absent := range []e2e.SmokeItemID{
		e2e.ItemS8LiveVisibility, "ARCH11-7", e2e.ItemArch11Item8CapabilityGap, e2e.ItemS9NegativeControl,
	} {
		if _, ok := got[absent]; ok {
			t.Errorf("item %s was recorded without --expected data supplying it", absent)
		}
	}
	if !strings.Contains(stderr, "skipped") || !strings.Contains(stderr, "ARCH11-7") || !strings.Contains(stderr, "S8") {
		t.Errorf("stderr should note the skipped optional items, including S8: %q", stderr)
	}
}

// TestE2EVerifyS8NeverPassesWithoutLiveCapture is the regression guard for
// the false-green a journal-timestamp proxy produced: a normal, cleanly
// completed multi-stage run's stage.started/stage.finished events are ALWAYS
// timestamped before the run's terminal event — a closed-run journal cannot
// be told apart from a genuine live capture by timestamp alone. S8 must stay
// unrecorded (never pass, never fail) unless --expected supplies a real
// portal/SSE observation.
func TestE2EVerifyS8NeverPassesWithoutLiveCapture(t *testing.T) {
	root := t.TempDir()
	const runID = "e2e-verify-s8-no-capture-run"
	newE2ETestRun(t, root, runID, "goobers", []e2eTestStage{
		{name: "implement", number: 1, pod: "pod-implement-1", node: "node-a", os: "linux"},
		{name: "windows-build", number: 1, pod: "pod-windows-build-1", node: "node-b", os: "windows"},
	}, journal.PhaseCompleted)

	code, stdout, stderr := runArgs(t, "e2e", "verify", "--run", runID, root)
	if code != 0 {
		t.Fatalf("code = %d, want 0; stderr=%s", code, stderr)
	}
	var bundle e2e.Bundle
	if err := json.Unmarshal([]byte(stdout), &bundle); err != nil {
		t.Fatalf("stdout is not a valid evidence bundle: %v\n%s", err, stdout)
	}
	for _, item := range bundle.Items {
		if item.Item == e2e.ItemS8LiveVisibility {
			t.Fatalf("S8 was recorded (verdict=%v) despite no --expected liveVisibility capture — a journal-timestamp false green", item.Verdict)
		}
	}
	_, skippedLine, ok := strings.Cut(stderr, "skipped")
	if !ok || !strings.Contains(skippedLine, "S8") {
		t.Errorf("stderr should report S8 as skipped without a capture: %q", stderr)
	}
}

// TestE2EVerifyS8PassesWithSuppliedLiveCapture is S8's real-observer path:
// with --expected "liveVisibility" supplying an observation timestamped
// before the run's terminal event, S8 is checked as the genuine capture
// internal/e2e.AssertLiveVisibility expects, not the journal.
func TestE2EVerifyS8PassesWithSuppliedLiveCapture(t *testing.T) {
	root := t.TempDir()
	const runID = "e2e-verify-s8-capture-run"
	newE2ETestRun(t, root, runID, "goobers", []e2eTestStage{
		{name: "implement", number: 1, pod: "pod-implement-1", node: "node-a", os: "linux"},
		{name: "windows-build", number: 1, pod: "pod-windows-build-1", node: "node-b", os: "windows"},
	}, journal.PhaseCompleted)

	// The fixture's terminal event lands at startedAt+5s (12:00:05Z); this
	// observation is captured mid-run, well before it.
	expectedPath := filepath.Join(t.TempDir(), "topology.json")
	expected := `{
		"liveVisibility": {
			"source": "portal",
			"observations": [
				{"stage": "implement", "transition": "started", "observedAt": "2026-08-01T12:00:01Z"}
			]
		}
	}`
	if err := os.WriteFile(expectedPath, []byte(expected), 0o644); err != nil {
		t.Fatal(err)
	}

	code, stdout, stderr := runArgs(t, "e2e", "verify", "--run", runID, "--expected", expectedPath, root)
	if code != 0 {
		t.Fatalf("code = %d, want 0; stderr=%s", code, stderr)
	}
	var bundle e2e.Bundle
	if err := json.Unmarshal([]byte(stdout), &bundle); err != nil {
		t.Fatalf("stdout is not a valid evidence bundle: %v\n%s", err, stdout)
	}
	var found bool
	for _, item := range bundle.Items {
		if item.Item == e2e.ItemS8LiveVisibility {
			found = true
			if item.Verdict != e2e.VerdictPass {
				t.Errorf("S8 verdict = %v, want pass; reason=%q", item.Verdict, item.Reason)
			}
			if item.Observer != e2e.LiveVisibilityObserver {
				t.Errorf("S8 observer = %q, want the real internal/e2e.LiveVisibilityObserver, not a journal-derived substitute", item.Observer)
			}
		}
	}
	if !found {
		t.Fatal("S8 was not recorded despite --expected supplying a liveVisibility capture")
	}
}

func TestE2EVerifyFailsOnReusedPod(t *testing.T) {
	root := t.TempDir()
	const runID = "e2e-verify-reused-pod-run"
	newE2ETestRun(t, root, runID, "goobers", []e2eTestStage{
		{name: "implement", number: 1, pod: "pod-shared", node: "node-a", os: "linux"},
		{name: "windows-build", number: 1, pod: "pod-shared", node: "node-b", os: "windows"},
	}, journal.PhaseCompleted)

	code, stdout, _ := runArgs(t, "e2e", "verify", "--run", runID, root)
	if code != 1 {
		t.Fatalf("code = %d, want 1; stdout=%s", code, stdout)
	}
	var bundle e2e.Bundle
	if err := json.Unmarshal([]byte(stdout), &bundle); err != nil {
		t.Fatalf("stdout is not a valid evidence bundle: %v\n%s", err, stdout)
	}
	for _, item := range bundle.Items {
		if item.Item == e2e.ItemS1FreshPod && item.Verdict != e2e.VerdictFail {
			t.Errorf("S1 verdict = %v, want fail (reused pod); reason=%q", item.Verdict, item.Reason)
		}
	}
}

func TestE2EVerifyRunsOptionalItemsFromExpectedTopology(t *testing.T) {
	root := t.TempDir()
	const runID = "e2e-verify-topology-run"
	newE2ETestRun(t, root, runID, "goobers", []e2eTestStage{
		{name: "implement", number: 1, pod: "pod-implement-1", node: "node-a", os: "linux"},
		{name: "windows-build", number: 1, pod: "pod-windows-build-1", node: "node-b", os: "windows"},
	}, journal.PhaseCompleted)

	expectedPath := filepath.Join(t.TempDir(), "topology.json")
	expected := `{
		"liveVisibility": {
			"source": "portal",
			"observations": [
				{"stage": "implement", "transition": "started", "observedAt": "2026-08-01T12:00:01Z"}
			]
		},
		"ledgerTouchingStages": ["implement"],
		"capabilityGap": {
			"wantUnsatStage": "windows-only-stage",
			"unsatisfiableStages": [
				{"stage": "windows-only-stage", "kind": "requirement", "diagnostic": "no runner provides os=windows"}
			]
		},
		"negativeControl": {
			"denial":          {"endpoint": "blocked.example.com:443", "exitCode": 28},
			"positiveControl": {"endpoint": "allowed.example.com:443", "exitCode": 0},
			"modelEndpoints":  ["api.anthropic.com"],
			"controlVantage":  {"endpoint": "blocked.example.com:443", "exitCode": 0}
		}
	}`
	if err := os.WriteFile(expectedPath, []byte(expected), 0o644); err != nil {
		t.Fatal(err)
	}

	outPath := filepath.Join(t.TempDir(), "bundle.json")
	code, _, stderr := runArgs(t, "e2e", "verify", "--run", runID, "--expected", expectedPath, "--out", outPath, root)
	if code != 0 {
		t.Fatalf("code = %d, want 0; stderr=%s", code, stderr)
	}

	data, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("read --out bundle: %v", err)
	}
	var bundle e2e.Bundle
	if err := json.Unmarshal(data, &bundle); err != nil {
		t.Fatalf("--out file is not a valid evidence bundle: %v\n%s", err, data)
	}
	if bundle.Overall() != e2e.VerdictPass {
		t.Fatalf("bundle.Overall() = %v, want pass; items=%#v", bundle.Overall(), bundle.Items)
	}
	got := make(map[e2e.SmokeItemID]e2e.Verdict, len(bundle.Items))
	for _, item := range bundle.Items {
		got[item.Item] = item.Verdict
	}
	for _, want := range []e2e.SmokeItemID{
		e2e.ItemS1FreshPod, e2e.ItemS2OSHop, e2e.ItemS8LiveVisibility,
		"ARCH11-7", e2e.ItemArch11Item8CapabilityGap, e2e.ItemS9NegativeControl,
	} {
		if got[want] != e2e.VerdictPass {
			t.Errorf("item %s verdict = %v, want pass", want, got[want])
		}
	}
	// S3-S7 always need a live topology-driving orchestration this command
	// never does, so they are still reported skipped — but S8 and the ARCH11
	// items --expected supplied data for must not be. Scope the check to
	// the "skipped" line alone: "checked:" legitimately names all of them.
	_, skippedLine, ok := strings.Cut(stderr, "skipped")
	if !ok || !strings.Contains(skippedLine, "S3") {
		t.Errorf("stderr should still report S3-S7 as skipped (no live orchestration): %q", stderr)
	}
	if strings.Contains(skippedLine, "S8") || strings.Contains(skippedLine, "ARCH11-7") || strings.Contains(skippedLine, "ARCH11-8") {
		t.Errorf("stderr's skipped line should not name S8/ARCH11-7/ARCH11-8 when --expected supplies their data: %q", skippedLine)
	}

	// §5 rule 4 (re-runnable from the bundle alone): the bundle's own
	// Collateral must point back at --expected (where the S8/S9 captures
	// live) and the run's journal directory, so a reader can reproduce
	// every verdict offline without this invocation's context.
	if bundle.Collateral.S8CapturePath != expectedPath {
		t.Errorf("Collateral.S8CapturePath = %q, want %q", bundle.Collateral.S8CapturePath, expectedPath)
	}
	if bundle.Collateral.S9ProbeOutputPath != expectedPath {
		t.Errorf("Collateral.S9ProbeOutputPath = %q, want %q", bundle.Collateral.S9ProbeOutputPath, expectedPath)
	}
	if len(bundle.Collateral.RunJournalPaths) == 0 {
		t.Error("Collateral.RunJournalPaths is empty, want the run's journal directory recorded")
	}
}

func TestE2EVerifyRequiresRunFlag(t *testing.T) {
	code, _, stderr := runArgs(t, "e2e", "verify", t.TempDir())
	if code != 2 || !strings.Contains(stderr, "--run is required") {
		t.Fatalf("code = %d, stderr = %q, want 2 and a --run required message", code, stderr)
	}
}

func TestE2EVerifyRejectsUnknownRunID(t *testing.T) {
	root := t.TempDir()
	// Exit 2 ("the command itself could not run") — a run that never
	// resolved produced no bundle at all, so it can never be exit 1 or 3
	// (those are reserved for what a PRODUCED bundle's items say).
	code, _, stderr := runArgs(t, "e2e", "verify", "--run", "no-such-run", root)
	if code != 2 || !strings.Contains(stderr, "no run") {
		t.Fatalf("code = %d, stderr = %q, want 2 and a not-found message", code, stderr)
	}
}

func TestE2EVerifyRejectsGaggleMismatch(t *testing.T) {
	root := t.TempDir()
	const runID = "e2e-verify-gaggle-mismatch-run"
	newE2ETestRun(t, root, runID, "goobers", []e2eTestStage{
		{name: "implement", number: 1, pod: "pod-implement-1", node: "node-a", os: "linux"},
	}, journal.PhaseCompleted)

	// Exit 2, same reasoning as TestE2EVerifyRejectsUnknownRunID: no bundle
	// was ever produced.
	code, _, stderr := runArgs(t, "e2e", "verify", "--run", runID, "--gaggle", "some-other-gaggle", root)
	if code != 2 || !strings.Contains(stderr, "gaggle") {
		t.Fatalf("code = %d, stderr = %q, want 2 and a gaggle-mismatch message", code, stderr)
	}
}

// TestE2EVerifyExitCodeInvalidWhenNoFail is exit 3's guard: an item that
// came back INVALID (the observer machinery couldn't establish evidence —
// here, capabilityGap.unsatisfiableStages is empty, so
// AssertCapabilityGapEnforced never even ran a real solve) with nothing
// else FAILING must exit 3, distinct from both a clean pass (0) and a real
// fail (1) — "fix the instrumentation and re-run" is a different response
// than "the design is broken."
func TestE2EVerifyExitCodeInvalidWhenNoFail(t *testing.T) {
	root := t.TempDir()
	const runID = "e2e-verify-invalid-run"
	newE2ETestRun(t, root, runID, "goobers", []e2eTestStage{
		{name: "implement", number: 1, pod: "pod-implement-1", node: "node-a", os: "linux"},
		{name: "windows-build", number: 1, pod: "pod-windows-build-1", node: "node-b", os: "windows"},
	}, journal.PhaseCompleted)

	expectedPath := filepath.Join(t.TempDir(), "topology.json")
	if err := os.WriteFile(expectedPath, []byte(`{"capabilityGap": {"wantUnsatStage": "x", "unsatisfiableStages": []}}`), 0o644); err != nil {
		t.Fatal(err)
	}

	code, stdout, _ := runArgs(t, "e2e", "verify", "--run", runID, "--expected", expectedPath, root)
	if code != 3 {
		t.Fatalf("code = %d, want 3; stdout=%s", code, stdout)
	}
	var bundle e2e.Bundle
	if err := json.Unmarshal([]byte(stdout), &bundle); err != nil {
		t.Fatalf("stdout is not a valid evidence bundle: %v\n%s", err, stdout)
	}
	for _, item := range bundle.Items {
		if item.Item == e2e.ItemArch11Item8CapabilityGap && item.Verdict != e2e.VerdictInvalid {
			t.Errorf("ARCH11-8 verdict = %v, want invalid", item.Verdict)
		}
		if item.Verdict == e2e.VerdictFail {
			t.Fatalf("no item should have FAILED in this scenario, got a FAIL at %s", item.Item)
		}
	}
}

// TestE2EVerifyExitCodePreferesFailOverInvalid confirms the driver-facing
// precedence: a real FAIL (S1's reused pod) alongside an INVALID item
// (capabilityGap's empty unsatisfiableStages) must still exit 1, not 3 — a
// real fail is actionable now, and the invalid item stays visible in the
// bundle for the re-run.
func TestE2EVerifyExitCodePreferesFailOverInvalid(t *testing.T) {
	root := t.TempDir()
	const runID = "e2e-verify-fail-and-invalid-run"
	newE2ETestRun(t, root, runID, "goobers", []e2eTestStage{
		{name: "implement", number: 1, pod: "pod-shared", node: "node-a", os: "linux"},
		{name: "windows-build", number: 1, pod: "pod-shared", node: "node-b", os: "windows"},
	}, journal.PhaseCompleted)

	expectedPath := filepath.Join(t.TempDir(), "topology.json")
	if err := os.WriteFile(expectedPath, []byte(`{"capabilityGap": {"wantUnsatStage": "x", "unsatisfiableStages": []}}`), 0o644); err != nil {
		t.Fatal(err)
	}

	code, stdout, _ := runArgs(t, "e2e", "verify", "--run", runID, "--expected", expectedPath, root)
	if code != 1 {
		t.Fatalf("code = %d, want 1 (FAIL must win over INVALID); stdout=%s", code, stdout)
	}
	var bundle e2e.Bundle
	if err := json.Unmarshal([]byte(stdout), &bundle); err != nil {
		t.Fatalf("stdout is not a valid evidence bundle: %v\n%s", err, stdout)
	}
	var sawFail, sawInvalid bool
	for _, item := range bundle.Items {
		switch item.Verdict {
		case e2e.VerdictFail:
			sawFail = true
		case e2e.VerdictInvalid:
			sawInvalid = true
		}
	}
	if !sawFail || !sawInvalid {
		t.Fatalf("expected both a FAIL and an INVALID item recorded, got items=%#v", bundle.Items)
	}
}

// TestE2EVerifyPrintRunnerClass is --print-runner-class's contract test: it
// mirrors netpol-render --print-blob-endpoint — no --run, no instance root,
// no cluster — and generates the exact value
// internal/runnercap.RunnerClassValue derives, so a topology file's
// negativeControl restriction fields never have to be hand-transcribed.
func TestE2EVerifyPrintRunnerClass(t *testing.T) {
	code, stdout, stderr := runArgs(t, "e2e", "verify", "--print-runner-class", "network:allowlist")
	if code != 0 {
		t.Fatalf("code = %d, want 0; stderr=%s", code, stderr)
	}
	var got struct {
		Restrictions []string `json:"restrictions"`
		RunnerClass  string   `json:"runnerClass"`
	}
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("stdout is not valid JSON: %v\n%s", err, stdout)
	}
	want := runnercap.RunnerClassValue([]string{"network:allowlist"})
	if got.RunnerClass != want {
		t.Errorf("runnerClass = %q, want %q (from internal/runnercap.RunnerClassValue, the dispatcher's own producer)", got.RunnerClass, want)
	}
	if len(got.Restrictions) != 1 || got.Restrictions[0] != "network:allowlist" {
		t.Errorf("restrictions = %#v, want [\"network:allowlist\"]", got.Restrictions)
	}
}

// TestE2EVerifyNegativeControlGeneratesRunnerClass confirms S9's evidence
// records the runner-class value THIS COMMAND generated from --expected's
// declared restrictions (internal/runnercap.RunnerClassValue), never a
// class string the topology file would have had to hand-transcribe.
func TestE2EVerifyNegativeControlGeneratesRunnerClass(t *testing.T) {
	root := t.TempDir()
	const runID = "e2e-verify-runner-class-run"
	newE2ETestRun(t, root, runID, "goobers", []e2eTestStage{
		{name: "implement", number: 1, pod: "pod-implement-1", node: "node-a", os: "linux"},
		{name: "windows-build", number: 1, pod: "pod-windows-build-1", node: "node-b", os: "windows"},
	}, journal.PhaseCompleted)

	expectedPath := filepath.Join(t.TempDir(), "topology.json")
	expected := `{
		"negativeControl": {
			"denial":                           {"endpoint": "blocked.example.com:443", "exitCode": 28},
			"positiveControl":                  {"endpoint": "allowed.example.com:443", "exitCode": 0},
			"modelEndpoints":                   ["api.anthropic.com"],
			"controlVantage":                   {"endpoint": "blocked.example.com:443", "exitCode": 0},
			"restrictedRunnerRestrictions":     ["network:allowlist"],
			"controlVantageRunnerRestrictions": []
		}
	}`
	if err := os.WriteFile(expectedPath, []byte(expected), 0o644); err != nil {
		t.Fatal(err)
	}

	code, stdout, stderr := runArgs(t, "e2e", "verify", "--run", runID, "--expected", expectedPath, root)
	if code != 0 {
		t.Fatalf("code = %d, want 0; stderr=%s", code, stderr)
	}
	wantRestricted := runnercap.RunnerClassValue([]string{"network:allowlist"})
	wantControlVantage := runnercap.RunnerClassValue(nil) // explicit empty array -> unrestricted
	if !strings.Contains(stdout, `"restrictedRunnerClass":"`+wantRestricted+`"`) &&
		!strings.Contains(stdout, `"restrictedRunnerClass": "`+wantRestricted+`"`) {
		t.Errorf("bundle evidence missing the generated restrictedRunnerClass %q:\n%s", wantRestricted, stdout)
	}
	if !strings.Contains(stdout, `"controlVantageRunnerClass":"`+wantControlVantage+`"`) &&
		!strings.Contains(stdout, `"controlVantageRunnerClass": "`+wantControlVantage+`"`) {
		t.Errorf("bundle evidence missing the generated controlVantageRunnerClass %q:\n%s", wantControlVantage, stdout)
	}
}
