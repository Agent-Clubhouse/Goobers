package childworkflow

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/triggerqueue"
)

// characterizationRun is a child journal with recorded artifacts, read back
// read-only as the terminal observer reads it.
func characterizationRun(t *testing.T) (*journal.Run, func() *journal.Reader) {
	t.Helper()
	run, err := journal.Create(t.TempDir(), journal.RunIdentity{RunID: "child-run", Workflow: "generated", WorkflowVersion: 1, Gaggle: "web"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = run.Close() })
	return run, func() *journal.Reader {
		rd, err := journal.OpenReadOnly(run.Dir())
		if err != nil {
			t.Fatal(err)
		}
		return rd
	}
}

func recordArtifact(t *testing.T, run *journal.Run, name string) journal.Ref {
	t.Helper()
	ref, err := run.RecordArtifact(name, []byte("artifact "+name))
	if err != nil {
		t.Fatal(err)
	}
	return ref
}

// The terminal input a child's journal yields: the state from its phase, the
// finish time from its terminal event, the latest stage summary (bounded), and
// at most 16 distinct readable artifact references, newest event first.
// Pinned before these moved out of cmd/goobers (childTerminalInput and
// childResultEvidence), and unchanged since, so the move is byte-identical.
func TestTerminalInputFromJournalCharacterization(t *testing.T) {
	run, open := characterizationRun(t)
	finished := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	first, second := recordArtifact(t, run, "first.txt"), recordArtifact(t, run, "second.txt")
	oversized := journal.Ref{Path: "artifacts/huge.bin", Digest: "sha256:" + strings.Repeat("f", 64), Size: MaxChildResultArtifactBytes + 1}
	many := make([]journal.Ref, 0, 20)
	for i := range 20 {
		many = append(many, recordArtifact(t, run, fmt.Sprintf("many-%02d.txt", i)))
	}
	events := []journal.Event{
		{Type: journal.EventRunStarted},
		{Type: journal.EventStageFinished, Stage: "early", Outputs: map[string]any{"summary": "early summary"}, Artifacts: []journal.Ref{first}},
		{Type: journal.EventStageFinished, Stage: "late", Outputs: map[string]any{"summary": "late summary"}, Artifacts: []journal.Ref{second, first, oversized}},
		{Type: journal.EventRunFinished, Status: string(journal.PhaseCompleted), Time: finished},
	}
	input, found, err := TerminalInputFromJournal(open(), events, journal.PhaseCompleted)
	want := TerminalResultInput{State: triggerqueue.ChildCompleted, FinishedAt: finished, Summary: "late summary",
		References: []string{second.Digest, first.Digest}}
	if err != nil || !found || fmt.Sprint(input) != fmt.Sprint(want) {
		t.Fatalf("completed child = %+v, %v, %v; want %+v", input, found, err, want)
	}

	for phase, state := range map[journal.RunPhase]triggerqueue.ChildState{journal.PhaseFailed: triggerqueue.ChildFailed, journal.PhaseAborted: triggerqueue.ChildCancelled} {
		terminal := []journal.Event{{Type: journal.EventRunFinished, Status: string(phase), Time: finished}}
		input, found, err := TerminalInputFromJournal(open(), terminal, phase)
		if err != nil || !found || input.State != state || input.Summary != "Child workflow "+string(phase) || len(input.References) != 0 {
			t.Fatalf("%s child = %+v, %v, %v", phase, input, found, err)
		}
	}
	// Evidence recorded after run.finished is not the child's result.
	late := recordArtifact(t, run, "after-finish.txt")
	after := append(append([]journal.Event{}, events...), journal.Event{Type: journal.EventStageFinished, Stage: "after", Outputs: map[string]any{"summary": "after finish"}, Artifacts: []journal.Ref{late}})
	if input, found, err := TerminalInputFromJournal(open(), after, journal.PhaseCompleted); err != nil || !found || fmt.Sprint(input) != fmt.Sprint(want) {
		t.Fatalf("child with evidence after run.finished = %+v, %v, %v; want %+v", input, found, err, want)
	}
	if _, found, err := TerminalInputFromJournal(open(), events, journal.PhaseRunning); found || err != nil {
		t.Fatalf("running child = found %v, %v; want not terminal", found, err)
	}
	if _, found, err := TerminalInputFromJournal(open(), events[:3], journal.PhaseCompleted); found || err != nil {
		t.Fatalf("no terminal event = found %v, %v; want not found", found, err)
	}
	for name, terminal := range map[string]journal.Event{
		"phase mismatch": {Type: journal.EventRunFinished, Status: string(journal.PhaseFailed), Time: finished},
		"no finish time": {Type: journal.EventRunFinished, Status: string(journal.PhaseCompleted)},
	} {
		if _, _, err := TerminalInputFromJournal(open(), []journal.Event{terminal}, journal.PhaseCompleted); err == nil {
			t.Fatalf("%s: accepted", name)
		}
	}

	// References: at most 16, de-duplicated, newest event first.
	capped, summary, err := ResultEvidence(open(), []journal.Event{{Type: journal.EventStageFinished, Artifacts: many}})
	if err != nil || summary != "" || len(capped) != 16 || capped[0] != many[0].Digest || capped[15] != many[15].Digest {
		t.Fatalf("capped references = %d (%v), %q", len(capped), err, summary)
	}
	// An unreadable referenced artifact fails capture rather than being dropped.
	missing := journal.Ref{Path: "artifacts/missing.txt", Digest: "sha256:" + strings.Repeat("e", 64), Size: 10}
	if _, _, err := ResultEvidence(open(), []journal.Event{{Type: journal.EventStageFinished, Artifacts: []journal.Ref{missing}}}); err == nil {
		t.Fatal("an unreadable artifact reference was accepted")
	}
	// A summary is bounded to 8 KiB on a UTF-8 boundary; a non-string or
	// invalid one is ignored.
	long := strings.Repeat("a", 8191) + "é" + "tail"
	_, summary, err = ResultEvidence(open(), []journal.Event{{Type: journal.EventStageFinished, Outputs: map[string]any{"summary": long}}})
	if err != nil || summary != strings.Repeat("a", 8191) {
		t.Fatalf("bounded summary = %d bytes (%v), want 8191", len(summary), err)
	}
	// An ASCII summary one byte over the bound keeps exactly 8 KiB.
	_, summary, err = ResultEvidence(open(), []journal.Event{{Type: journal.EventStageFinished, Outputs: map[string]any{"summary": strings.Repeat("b", 8193)}}})
	if err != nil || summary != strings.Repeat("b", 8192) {
		t.Fatalf("bounded ASCII summary = %d bytes (%v), want 8192", len(summary), err)
	}
	for name, value := range map[string]any{"not a string": 42, "invalid UTF-8": string([]byte{0xff, 0xfe})} {
		if _, summary, err := ResultEvidence(open(), []journal.Event{{Type: journal.EventStageFinished, Outputs: map[string]any{"summary": value}}}); err != nil || summary != "" {
			t.Fatalf("%s summary = %q, %v; want ignored", name, summary, err)
		}
	}
}
