package harness

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

type windowRecorder struct {
	windows  []string
	finished []bool
}

func (r *windowRecorder) ObserveWindow(_ apiv1.InvocationEnvelope, _ string, w []byte) {
	r.windows = append(r.windows, string(w))
}
func (r *windowRecorder) FinishStage(_ apiv1.InvocationEnvelope, hit bool) {
	r.finished = append(r.finished, hit)
}

type secretScrubber struct{}

func (secretScrubber) Scrub(b []byte) []byte {
	return bytes.ReplaceAll(b, []byte("SECRET"), []byte("[redacted]"))
}

func TestTranscriptWindowsRedactAcrossDeltasAndBoundTail(t *testing.T) {
	rec := &windowRecorder{}
	e := &Executor{scrubber: secretScrubber{}, windows: rec}
	var sunk int
	req := RunRequest{TranscriptCheckpoint: func(InvocationTranscriptDelta) error { sunk++; return nil }}
	e.teeTranscriptWindows(apiv1.InvocationEnvelope{TaskID: "run:stage"}, &req)

	for _, part := range []string{"token SEC", "RET end\n"} {
		if err := req.TranscriptCheckpoint(InvocationTranscriptDelta{Source: "process-output", Invocation: 1, TranscriptDelta: TranscriptDelta{Data: []byte(part)}}); err != nil {
			t.Fatal(err)
		}
	}
	if sunk != 2 || len(rec.windows) != 2 {
		t.Fatalf("sunk=%d windows=%d", sunk, len(rec.windows))
	}
	last := rec.windows[len(rec.windows)-1]
	if strings.Contains(last, "SECRET") || !strings.Contains(last, "[redacted]") {
		t.Fatalf("split secret reached the observer: %q", last)
	}

	big := strings.Repeat("x", maxTranscriptWindow) + "tail"
	_ = req.TranscriptCheckpoint(InvocationTranscriptDelta{Source: "process-output", Invocation: 1, TranscriptDelta: TranscriptDelta{Data: []byte(big)}})
	if w := rec.windows[len(rec.windows)-1]; len(w) != maxTranscriptWindow || !strings.HasSuffix(w, "tail") {
		t.Fatalf("window not bounded to the tail: len=%d", len(w))
	}
}

func TestTranscriptWindowsFinishLabelsBudgetHit(t *testing.T) {
	rec := &windowRecorder{}
	e := &Executor{windows: rec}
	e.finishTranscriptWindows(apiv1.InvocationEnvelope{}, fmt.Errorf("x: %w", ErrTimeout))
	e.finishTranscriptWindows(apiv1.InvocationEnvelope{}, errors.New("other"))
	e.finishTranscriptWindows(apiv1.InvocationEnvelope{}, nil)
	if len(rec.finished) != 3 || !rec.finished[0] || rec.finished[1] || rec.finished[2] {
		t.Fatalf("finished = %v", rec.finished)
	}
	(&Executor{}).finishTranscriptWindows(apiv1.InvocationEnvelope{}, nil)
}
