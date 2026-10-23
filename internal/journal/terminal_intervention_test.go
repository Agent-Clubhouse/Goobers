package journal

import (
	"errors"
	"fmt"
	"path/filepath"
	"testing"
)

func terminalRequest(id string, seq, generation uint64) TerminalIntervention {
	fingerprint := Digest([]byte("resume:" + id))
	return TerminalIntervention{RequestID: id, Fingerprint: fingerprint, ExpectedTerminalSeq: seq, ExpectedTerminalGeneration: generation, Events: []Event{
		{Type: EventRunnerAnnotation, Runner: map[string]any{"kind": "intervention.idempotency", "idempotencyKey": id, "fingerprint": fingerprint, "action": "resume", "stage": "work", "emitKey": id + "-marker"}},
		{Type: EventRunResumed, Target: "work", Actor: "operator", Action: "resume", Runner: map[string]any{"emitKey": id + "-resume"}},
	}}
}
func terminalHistory() []Event {
	return []Event{{Seq: 1, Type: EventRunStarted}, {Seq: 2, Type: EventRunFinished, Status: string(PhaseFailed)}}
}
func stampedPrefix(req TerminalIntervention, n int) []Event {
	events := append([]Event(nil), req.Events[:n]...)
	for i := range events {
		events[i].Seq = req.ExpectedTerminalSeq + uint64(i) + 1
	}
	return events
}

func TestTerminalInterventionChecksGenerationAndExactPrefix(t *testing.T) {
	for _, test := range []struct {
		name string
		edit func(*TerminalIntervention, *[]Event)
		want error
		skip int
	}{
		{name: "new"},
		{name: "partial", edit: func(r *TerminalIntervention, h *[]Event) { *h = append(*h, stampedPrefix(*r, 1)...) }, skip: 1},
		{name: "duplicate", edit: func(r *TerminalIntervention, h *[]Event) { *h = append(*h, stampedPrefix(*r, 2)...) }, skip: 2},
		{name: "duplicate after next terminal", edit: func(r *TerminalIntervention, h *[]Event) {
			*h = append(*h, stampedPrefix(*r, 2)...)
			*h = append(*h, Event{Seq: 5, Type: EventRunFinished, Status: string(PhaseFailed)})
		}, skip: 2},
		{name: "wrong seq", edit: func(r *TerminalIntervention, _ *[]Event) { r.ExpectedTerminalSeq++ }, want: ErrInterventionStale},
		{name: "wrong generation", edit: func(r *TerminalIntervention, _ *[]Event) { r.ExpectedTerminalGeneration++ }, want: ErrInterventionStale},
		{name: "other pending intent", edit: func(_ *TerminalIntervention, h *[]Event) {
			*h = append(*h, stampedPrefix(terminalRequest("other", 2, 1), 1)...)
		}, want: ErrInterventionPending},
		{name: "changed resume", edit: func(r *TerminalIntervention, h *[]Event) {
			*h = append(*h, stampedPrefix(*r, 2)...)
			r.Events[1].Target = "elsewhere"
		}, want: ErrInterventionConflict},
		{name: "key collision", edit: func(r *TerminalIntervention, h *[]Event) {
			(*h)[0].Runner = map[string]any{"emitKey": r.Events[0].Runner["emitKey"]}
		}, want: ErrInterventionConflict},
		{name: "stray op", edit: func(r *TerminalIntervention, _ *[]Event) { r.Events[1].Type = EventStageStarted }, want: ErrInterventionInvalid},
	} {
		t.Run(test.name, func(t *testing.T) {
			req, history := terminalRequest("request", 2, 1), terminalHistory()
			if test.edit != nil {
				test.edit(&req, &history)
			}
			check, err := NewTerminalInterventionCheck(req, history)
			var n int
			var receipt TerminalInterventionReceipt
			if err == nil {
				n, receipt, err = check.Remaining()
			}
			if !errors.Is(err, test.want) {
				t.Fatalf("err=%v want %v", err, test.want)
			}
			if err == nil && (n != test.skip || receipt.Segment != 2 || !receipt.Resumed || receipt.Duplicate != (test.skip == 2)) {
				t.Fatalf("skip=%d receipt=%+v", n, receipt)
			}
		})
	}
}

func TestTerminalInterventionFileRecoveryAndDenial(t *testing.T) {
	for _, partial := range []bool{false, true} {
		t.Run(fmt.Sprint(partial), func(t *testing.T) {
			root := t.TempDir()
			run, err := Create(root, RunIdentity{RunID: "intervention", Workflow: "wf", WorkflowVersion: 1}, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = run.Close() }()
			if err := run.Append(Event{Type: EventRunFinished, Status: string(PhaseFailed)}); err != nil {
				t.Fatal(err)
			}
			req := terminalRequest("first", run.Seq(), 1)
			if partial {
				if err := run.Append(req.Events[0]); err != nil {
					t.Fatal(err)
				}
			}
			if err := run.Close(); err != nil {
				t.Fatal(err)
			}
			run, _, err = Recover(filepath.Join(root, "intervention"))
			if err != nil {
				t.Fatal(err)
			}
			receipt, err := run.CommitTerminalIntervention(req)
			if err != nil || receipt.Segment != 2 || receipt.Duplicate || run.Phase() != PhaseRunning {
				t.Fatalf("resume=%+v err=%v phase=%v", receipt, err, run.Phase())
			}
			replay, err := run.CommitTerminalIntervention(req)
			if err != nil || !replay.Duplicate || replay.Seq != receipt.Seq {
				t.Fatalf("replay=%+v err=%v", replay, err)
			}
			if err := run.Append(Event{Type: EventRunFinished, Status: string(PhaseEscalated)}); err != nil {
				t.Fatal(err)
			}
			deny := terminalRequest("deny", run.Seq(), 2)
			deny.Events = deny.Events[:1]
			deny.Events[0].Runner["kind"] = "escalation.resolution"
			deny.Events[0].Runner["resolution"] = "deny"
			denied, err := run.CommitTerminalIntervention(deny)
			if err != nil || denied.Resumed || denied.Segment != 2 || run.Phase() != PhaseEscalated {
				t.Fatalf("deny=%+v err=%v phase=%v", denied, err, run.Phase())
			}
		})
	}
}
