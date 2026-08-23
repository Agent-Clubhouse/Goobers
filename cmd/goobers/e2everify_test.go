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
)

// e2eTestStage is one fabricated stage attempt for the fixture runs below:
// enough to drive S1 (fresh pod), S2 (OS hop), and S8's journal proxy.
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
	for _, want := range []e2e.SmokeItemID{e2e.ItemS1FreshPod, e2e.ItemS2OSHop, e2e.ItemS8LiveVisibility} {
		if got[want] != e2e.VerdictPass {
			t.Errorf("item %s verdict = %v, want pass", want, got[want])
		}
	}
	// The optional items were never asked (no --expected), so they carry no
	// verdict at all — never a silent pass.
	for _, absent := range []e2e.SmokeItemID{"ARCH11-7", e2e.ItemArch11Item8CapabilityGap, e2e.ItemS9NegativeControl} {
		if _, ok := got[absent]; ok {
			t.Errorf("item %s was recorded without --expected data supplying it", absent)
		}
	}
	if !strings.Contains(stderr, "skipped") || !strings.Contains(stderr, "ARCH11-7") {
		t.Errorf("stderr should note the skipped optional items: %q", stderr)
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
	// never does, so they are still reported skipped — but the ARCH11 items
	// --expected supplied data for must not be. Scope the check to the
	// "skipped" line alone: "checked:" legitimately names ARCH11-7/ARCH11-8.
	_, skippedLine, ok := strings.Cut(stderr, "skipped")
	if !ok || !strings.Contains(skippedLine, "S3") {
		t.Errorf("stderr should still report S3-S7 as skipped (no live orchestration): %q", stderr)
	}
	if strings.Contains(skippedLine, "ARCH11-7") || strings.Contains(skippedLine, "ARCH11-8") {
		t.Errorf("stderr's skipped line should not name ARCH11-7/ARCH11-8 when --expected supplies their data: %q", skippedLine)
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
	code, _, stderr := runArgs(t, "e2e", "verify", "--run", "no-such-run", root)
	if code != 1 || !strings.Contains(stderr, "no run") {
		t.Fatalf("code = %d, stderr = %q, want 1 and a not-found message", code, stderr)
	}
}

func TestE2EVerifyRejectsGaggleMismatch(t *testing.T) {
	root := t.TempDir()
	const runID = "e2e-verify-gaggle-mismatch-run"
	newE2ETestRun(t, root, runID, "goobers", []e2eTestStage{
		{name: "implement", number: 1, pod: "pod-implement-1", node: "node-a", os: "linux"},
	}, journal.PhaseCompleted)

	code, _, stderr := runArgs(t, "e2e", "verify", "--run", runID, "--gaggle", "some-other-gaggle", root)
	if code != 1 || !strings.Contains(stderr, "gaggle") {
		t.Fatalf("code = %d, stderr = %q, want 1 and a gaggle-mismatch message", code, stderr)
	}
}
