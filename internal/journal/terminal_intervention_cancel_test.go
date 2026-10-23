package journal

import (
	"errors"
	"fmt"
	"path/filepath"
	"strconv"
	"testing"
)

func cancelTerminalRequest(req TerminalIntervention) TerminalIntervention {
	req.Cancellation = []Event{
		{Type: EventRunnerAnnotation, Runner: map[string]any{"kind": "intervention.cancelled", "idempotencyKey": req.RequestID, "fingerprint": req.Fingerprint, "terminalGeneration": strconv.FormatUint(req.ExpectedTerminalGeneration, 10), "terminalSeq": strconv.FormatUint(req.ExpectedTerminalSeq, 10), "emitKey": req.RequestID + "-cancel"}},
		{Type: EventRunFinished, Status: string(PhaseAborted), Reason: "operator cancellation", Runner: map[string]any{"emitKey": req.RequestID + "-abort"}},
	}
	return req
}

func TestTerminalInterventionCancellationRecoversFilePrefix(t *testing.T) {
	for prefix := 0; prefix <= 2; prefix++ {
		for _, cancelPrefix := range []bool{false, true} {
			t.Run(fmt.Sprintf("original=%d/cancel=%v", prefix, cancelPrefix), func(t *testing.T) {
				root := t.TempDir()
				const id = "cancelled-prefix"
				run, err := Create(root, RunIdentity{RunID: id, Workflow: "wf"}, nil)
				if err != nil {
					t.Fatal(err)
				}
				if err := run.Append(Event{Type: EventRunFinished, Status: "failed"}); err != nil {
					t.Fatal(err)
				}
				original := terminalRequest("intent", 2, 1)
				cancel := cancelTerminalRequest(original)
				for _, event := range original.Events[:prefix] {
					if err := run.Append(event); err != nil {
						t.Fatal(err)
					}
				}
				if cancelPrefix {
					if err := run.Append(cancel.Cancellation[0]); err != nil {
						t.Fatal(err)
					}
				}
				if err := run.Close(); err != nil {
					t.Fatal(err)
				}
				run, _, err = Recover(filepath.Join(root, id))
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = run.Close() }()
				if cancelPrefix {
					if _, err := run.CommitTerminalIntervention(original); !errors.Is(err, ErrInterventionCancelled) {
						t.Fatalf("retry past tombstone=%v", err)
					}
				}
				receipt, err := run.CommitTerminalIntervention(cancel)
				if err != nil || !receipt.Cancelled || receipt.Seq != uint64(prefix+4) || run.Phase() != PhaseAborted {
					t.Fatalf("cancel=%+v phase=%s err=%v", receipt, run.Phase(), err)
				}
				if _, err := run.CommitTerminalIntervention(original); !errors.Is(err, ErrInterventionCancelled) {
					t.Fatalf("late retry reopened: %v", err)
				}
				duplicate, err := run.CommitTerminalIntervention(cancel)
				if err != nil || !duplicate.Duplicate || !duplicate.Cancelled || duplicate.Seq != receipt.Seq {
					t.Fatalf("cancel retry=%+v err=%v", duplicate, err)
				}
				reader, err := OpenReadOnly(filepath.Join(root, id))
				if err != nil {
					t.Fatal(err)
				}
				events, err := reader.Events()
				if err != nil {
					t.Fatal(err)
				}
				reopened := 0
				for _, event := range events {
					if event.Type == EventRunResumed {
						reopened++
					}
				}
				want := 0
				if prefix == 2 {
					want = 1
				}
				if reopened != want {
					t.Fatalf("cancel added a resume: %d, want %d", reopened, want)
				}
			})
		}
	}
}

func TestTerminalInterventionTombstoneClosesOtherIntents(t *testing.T) {
	original := terminalRequest("original", 2, 1)
	cancel := cancelTerminalRequest(original)
	history := append(terminalHistory(), cancel.Cancellation[0])
	history[2].Seq = 3
	other := terminalRequest("other", 2, 1)
	check, err := NewTerminalInterventionCheck(other, history)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := check.Remaining(); err == nil {
		t.Fatal("a different request reopened past incomplete cancellation")
	}
}
