package tracefollow

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/readservice"
)

type scriptedReader struct {
	readservice.OfflineRuns
	ledgers   [][]readservice.RunEvent
	reads     int
	ledgerErr error
	failAfter int
	contents  map[uint64]string
	readErr   error
}

func (r *scriptedReader) RunEvents(context.Context, string) (readservice.EventList, error) {
	if r.ledgerErr != nil && r.reads >= r.failAfter {
		return readservice.EventList{}, r.ledgerErr
	}
	i := min(r.reads, len(r.ledgers)-1)
	r.reads++
	return readservice.EventList{Events: r.ledgers[i]}, nil
}

func (r *scriptedReader) Transcript(_ context.Context, _ string, seq uint64) (readservice.TranscriptContent, error) {
	content, ok := r.contents[seq]
	if !ok {
		return readservice.TranscriptContent{}, r.readErr
	}
	return readservice.TranscriptContent{Seq: seq, Stage: "implement", Bytes: []byte(content)}, nil
}

func transcriptEvent(seq uint64, partial bool, capture string) readservice.RunEvent {
	event := readservice.RunEvent{Seq: seq, KnownSchema: true, Type: journal.EventSpanRecorded,
		Stage: "run:implement", Name: "agent.transcript", Runner: map[string]any{}}
	if partial {
		event.Name += ".partial"
		event.Runner = map[string]any{"partial": true, "transcriptCapture": capture, "transcriptStream": "output/1", "reason": "checkpoint"}
	} else if capture != "" {
		event.Runner["transcriptCaptureComplete"] = capture
	}
	return event
}

func TestFollowSelectionAndCanonicalReplacement(t *testing.T) {
	partial, final := transcriptEvent(1, true, "capture"), transcriptEvent(2, false, "capture")
	other := transcriptEvent(3, false, "")
	other.Stage = "review"
	for _, tc := range []struct {
		name  string
		after uint64
		json  bool
		want  string
	}{
		{"text", 0, false, "--- stage=\"implement\" name=\"agent.transcript\" seq=2 replaces-capture=\"capture\" ---\nfinal\n"},
		{"json", 0, true, "{\"runId\":\"run\",\"seq\":2,\"stage\":\"implement\",\"name\":\"agent.transcript\",\"content\":\"final\",\"replacesCapture\":\"capture\"}\n"},
		{"resume", 1, false, "--- stage=\"implement\" name=\"agent.transcript\" seq=2 replaces-capture=\"capture\" ---\nfinal\n"},
		{"already consumed", 3, false, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reads := &scriptedReader{ledgers: [][]readservice.RunEvent{{partial, final, other}}, contents: map[uint64]string{2: "final", 3: "other"}, readErr: io.ErrUnexpectedEOF}
			var out bytes.Buffer
			if err := FollowTranscripts(t.Context(), reads, "run", "implement", tc.after, true, tc.json, time.Millisecond, &out); err != nil {
				t.Fatal(err)
			}
			if out.String() != tc.want {
				t.Fatalf("output = %q, want %q", out.String(), tc.want)
			}
		})
	}
}

func TestFollowLiveCheckpointThenFinal(t *testing.T) {
	partial, final := transcriptEvent(1, true, "capture"), transcriptEvent(2, false, "capture")
	finished := readservice.RunEvent{Seq: 3, Type: journal.EventRunFinished}
	reads := &scriptedReader{ledgers: [][]readservice.RunEvent{{partial}, {partial, final, finished}}, contents: map[uint64]string{1: "partial\n", 2: "final\n"}}
	var out bytes.Buffer
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if err := FollowTranscripts(ctx, reads, "run", "", 0, false, false, time.Millisecond, &out); err != nil {
		t.Fatal(err)
	}
	want := "--- stage=\"implement\" name=\"agent.transcript.partial\" seq=1 capture=\"capture\" stream=\"output/1\" reason=\"checkpoint\" ---\npartial\n--- stage=\"implement\" name=\"agent.transcript\" seq=2 replaces-capture=\"capture\" ---\nfinal\n"
	if out.String() != want {
		t.Fatalf("output = %q, want %q", out.String(), want)
	}
}

func TestFollowFinalizationRaceRequiresVerifiedSameCapture(t *testing.T) {
	broken, refreshErr := errors.New("corrupt transcript"), errors.New("refresh failed")
	partial := transcriptEvent(1, true, "capture")
	for _, tc := range []struct {
		name, capture string
		verified      bool
		refreshErr    error
		want          error
	}{
		{"same capture", "capture", true, nil, nil},
		{"other capture", "unrelated", true, nil, broken},
		{"unverified final", "capture", false, nil, broken},
		{"refresh error", "capture", true, refreshErr, refreshErr},
	} {
		t.Run(tc.name, func(t *testing.T) {
			final := transcriptEvent(2, false, tc.capture)
			reads := &scriptedReader{ledgers: [][]readservice.RunEvent{{partial}, {partial, final, {Seq: 3, Type: journal.EventRunFinished}}}, contents: map[uint64]string{}, readErr: broken, ledgerErr: tc.refreshErr, failAfter: 1}
			if tc.verified {
				reads.contents[2] = "final\n"
			}
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()
			var out bytes.Buffer
			err := FollowTranscripts(ctx, reads, "run", "", 0, false, true, time.Millisecond, &out)
			if !errors.Is(err, tc.want) {
				t.Fatalf("error = %v, want %v", err, tc.want)
			}
			if tc.want == nil && (!strings.Contains(out.String(), "final") || strings.Contains(out.String(), "partial")) {
				t.Fatalf("lost final or exposed retired partial: %s", out.String())
			}
		})
	}
}

func TestFollowCancellationAndLedgerFailure(t *testing.T) {
	want := errors.New("ledger failed")
	reads := &scriptedReader{ledgerErr: want}
	if err := FollowTranscripts(t.Context(), reads, "run", "", 0, false, false, time.Millisecond, io.Discard); !errors.Is(err, want) {
		t.Fatalf("error = %v", err)
	}
	for _, events := range [][]readservice.RunEvent{nil, {transcriptEvent(1, false, "")}} {
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		reads := &scriptedReader{ledgers: [][]readservice.RunEvent{events}}
		if err := FollowTranscripts(ctx, reads, "run", "", 0, false, false, time.Millisecond, io.Discard); !errors.Is(err, context.Canceled) {
			t.Fatalf("error = %v", err)
		}
	}
}

func TestTranscriptEventSelection(t *testing.T) {
	for _, tc := range []struct {
		name, stage          string
		known, partial, want bool
		kind                 journal.EventType
	}{
		{"transcript", "implement", true, false, true, journal.EventSpanRecorded},
		{"agent.transcript", "run:implement", true, false, true, journal.EventSpanRecorded},
		{"transcript.partial", "implement", true, true, true, journal.EventSpanRecorded},
		{"transcript.partial", "implement", true, false, false, journal.EventSpanRecorded},
		{"agent.transcript", "review", true, false, false, journal.EventSpanRecorded},
		{"agent.transcript", "runx:implement", true, false, false, journal.EventSpanRecorded},
		{"agent.transcript", "implement", false, false, false, journal.EventSpanRecorded},
		{"agent.transcript", "implement", true, false, false, journal.EventArtifactRecorded},
	} {
		event := readservice.RunEvent{Name: tc.name, Stage: tc.stage, KnownSchema: tc.known, Type: tc.kind, Runner: map[string]any{"partial": tc.partial}}
		if got := traceTranscriptEvent(event, "run", "implement"); got != tc.want {
			t.Errorf("%+v: got %v", tc, got)
		}
	}
}

func TestEventsTerminalUsesLatestLifecycle(t *testing.T) {
	for _, tc := range []struct {
		events []readservice.RunEvent
		want   bool
	}{
		{nil, false},
		{[]readservice.RunEvent{{Type: journal.EventRunStarted}}, false},
		{[]readservice.RunEvent{{Type: journal.EventRunFinished}}, true},
		{[]readservice.RunEvent{{Type: journal.EventRunFinished}, {Type: journal.EventRunResumed}}, false},
		{[]readservice.RunEvent{{Type: journal.EventRunFinished}, {Type: journal.EventRunResumed}, {Type: journal.EventRunFinished}}, true},
	} {
		if got := EventsTerminal(tc.events); got != tc.want {
			t.Errorf("events=%v: terminal=%v, want %v", tc.events, got, tc.want)
		}
	}
}
