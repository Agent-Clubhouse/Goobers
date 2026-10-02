package journal

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestTerminalCauseDurableScrubbedRecovery(t *testing.T) {
	scrubber := NewRegistryScrubber()
	scrubber.Register([]byte("sensitive-terminal-message"))
	run, err := Create(t.TempDir(), testIdentity(), nil, WithScrubber(scrubber))
	if err != nil {
		t.Fatal(err)
	}
	cause := &TerminalCause{Schema: TerminalCauseSchema, Phase: PhaseEscalated, Classification: TerminalEscalation, Code: "REVIEW_STOP", Message: "reason sensitive-terminal-message", CausalEventSeq: 1, Target: TargetEscalate}
	if err := run.Append(Event{Type: EventRunFinished, Status: string(PhaseEscalated), TerminalCause: cause}); err != nil {
		t.Fatal(err)
	}
	dir := run.Dir()
	if err := run.Close(); err != nil {
		t.Fatal(err)
	}
	simulateCrashInTerminalWindow(t, dir, PhaseRunning, "review")
	recovered, _, err := Recover(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := recovered.Close(); err != nil {
		t.Fatal(err)
	}
	reader, err := OpenRead(dir)
	if err != nil {
		t.Fatal(err)
	}
	got, err := reader.TerminalCause()
	if err != nil {
		t.Fatal(err)
	}
	want := *cause
	want.Message = "reason " + Redacted
	if !reflect.DeepEqual(*got, want) {
		t.Fatalf("cause = %+v, want %+v", got, want)
	}
	events, err := reader.Events()
	if err != nil || len(events) != 2 {
		t.Fatalf("events = %v, %v", events, err)
	}
	data, err := os.ReadFile(filepath.Join(dir, fileEvents))
	if err != nil || strings.Contains(string(data), "sensitive-terminal-message") {
		t.Fatalf("redaction failed: %v", err)
	}
}

func TestTerminalCauseExplicitUnavailableAndGenerations(t *testing.T) {
	terminal := Event{Schema: EventSchema, Type: EventRunFinished, Status: string(PhaseFailed)}
	for _, events := range [][]Event{nil, {terminal}, {{Schema: EventSchema, Type: EventGateEvaluated, Target: TargetEscalate}}} {
		if _, err := TerminalCauseFromEvents(events); !errors.Is(err, ErrTerminalCauseUnavailable) {
			t.Fatalf("error = %v", err)
		}
	}
	terminal.TerminalCause = &TerminalCause{Schema: TerminalCauseSchema, Code: "first", Retry: &TerminalBudget{Consumed: 1, Allowed: 2}}
	for _, reset := range []EventType{EventRunResumed, EventStageRerunRequested, EventGateOverridden} {
		events := []Event{terminal, {Schema: EventSchema, Type: reset}}
		if _, err := TerminalCauseFromEvents(events); !errors.Is(err, ErrTerminalCauseUnavailable) {
			t.Fatalf("%s: error = %v", reset, err)
		}
		events = append(events, Event{Schema: EventSchema, Type: EventRunFinished, TerminalCause: &TerminalCause{Schema: TerminalCauseSchema, Code: "second"}})
		got, err := TerminalCauseFromEvents(events)
		if err != nil || got.Code != "second" {
			t.Fatalf("%s: cause=%+v error=%v", reset, got, err)
		}
	}
	got, err := TerminalCauseFromEvents([]Event{terminal})
	if err != nil {
		t.Fatal(err)
	}
	got.Retry.Consumed = 7
	if terminal.TerminalCause.Retry.Consumed != 1 {
		t.Fatal("projection aliased source")
	}
	terminal.TerminalCause.Schema = "future"
	if _, err := TerminalCauseFromEvents([]Event{terminal}); !errors.Is(err, ErrTerminalCauseUnavailable) {
		t.Fatal(err)
	}
}
