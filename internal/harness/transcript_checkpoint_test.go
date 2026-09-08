package harness

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"
)

func TestTranscriptCheckpointAdvancesOnlyAfterAcknowledgement(t *testing.T) {
	buffer := newTranscriptBuffer(10)
	_, _ = buffer.Write([]byte("hello"))
	failure := errors.New("checkpoint storage unavailable")
	var got []TranscriptDelta
	state := transcriptCheckpointState{buffer: buffer, sink: func(delta TranscriptDelta) error {
		got = append(got, delta)
		return failure
	}}
	if err := state.capture("checkpoint"); !errors.Is(err, failure) || state.offset != 0 {
		t.Fatalf("unacknowledged bytes advanced offset: %d %v", state.offset, err)
	}
	failure = nil
	if err := state.capture("checkpoint"); err != nil || state.offset != 5 || !bytes.Equal(got[1].Data, []byte("hello")) {
		t.Fatalf("acknowledged delta lost: %+v %v", got, err)
	}
	if err := state.capture("checkpoint"); err != nil || len(got) != 2 {
		t.Fatal("unchanged transcript created checkpoint")
	}
	if err := state.capture("canceled"); err != nil || len(got) != 3 || len(got[2].Data) != 0 || got[2].Offset != 5 || got[2].Reason != "canceled" {
		t.Fatalf("termination metadata not recorded: %+v %v", got, err)
	}
}

func TestTranscriptCheckpointWorkerDoesNotRetryFailedWrite(t *testing.T) {
	buffer := newTranscriptBuffer(10)
	_, _ = buffer.Write([]byte("hello"))
	failure := errors.New("uncertain write")
	called := make(chan struct{})
	calls := 0
	worker := startTranscriptCheckpoints(buffer, time.Millisecond, func(TranscriptDelta) error {
		calls++
		close(called)
		return failure
	})
	select {
	case <-called:
	case <-time.After(5 * time.Second):
		_ = worker.finish("timeout")
		t.Fatal("checkpoint did not run")
	}
	if err := worker.finish("process-exit"); !errors.Is(err, failure) || calls != 1 {
		t.Fatalf("failed write retried or lost: calls=%d err=%v", calls, err)
	}
}

func TestTranscriptCheckpointProcessHelper(t *testing.T) {
	if os.Getenv("GOOBERS_CHECKPOINT_HELPER") != "1" {
		return
	}
	if _, err := fmt.Fprint(os.Stdout, "checkpoint-before-exit"); err != nil {
		t.Fatal(err)
	}
	timer := time.NewTimer(30 * time.Second)
	defer timer.Stop()
	<-timer.C
}

func TestTranscriptCheckpointRunsBeforeProcessExitAndFlushesCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var deltas []TranscriptDelta
	var once sync.Once
	result, err := (ExecProcessRunner{}).Run(ctx, ProcessRequest{
		Command: []string{os.Args[0], "-test.run=^TestTranscriptCheckpointProcessHelper$"},
		Env:     []string{"GOOBERS_CHECKPOINT_HELPER=1"}, Timeout: 10 * time.Second,
		TranscriptCheckpointInterval: time.Millisecond,
		TranscriptCheckpoint: func(delta TranscriptDelta) error {
			deltas = append(deltas, delta)
			if delta.Reason == "checkpoint" && len(delta.Data) > 0 {
				once.Do(cancel)
			}
			return nil
		},
	})
	if !errors.Is(err, ErrCanceled) || len(deltas) < 2 || deltas[0].Reason != "checkpoint" || deltas[len(deltas)-1].Reason != "canceled" {
		t.Fatalf("missing pre-exit checkpoint/cancel reason: %+v %v", deltas, err)
	}
	var reconstructed []byte
	for _, delta := range deltas {
		if delta.Offset != len(reconstructed) {
			t.Fatalf("noncontiguous delta: %+v", delta)
		}
		reconstructed = append(reconstructed, delta.Data...)
	}
	if !bytes.Equal(reconstructed, result.Transcript) {
		t.Fatalf("delta chain differs from process capture: %q / %q", reconstructed, result.Transcript)
	}
}

func TestTranscriptCheckpointFailureSurvivesSuccessfulProcess(t *testing.T) {
	failure := errors.New("durable capture failed")
	result, err := (ExecProcessRunner{}).Run(context.Background(), ProcessRequest{
		Command:                      []string{os.Args[0], "-test.run=^TestTranscriptCheckpointProcessHelper$"},
		Timeout:                      10 * time.Second,
		TranscriptCheckpointInterval: time.Hour,
		TranscriptCheckpoint: func(delta TranscriptDelta) error {
			if delta.Reason != "process-exit" {
				t.Errorf("terminal reason=%q", delta.Reason)
			}
			return failure
		},
	})
	if result.ExitCode != 0 || !errors.Is(err, failure) {
		t.Fatalf("capture failure lost or exit code falsified: %+v %v", result, err)
	}
}
