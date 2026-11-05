package journalpg

import (
	"bytes"
	"testing"

	"github.com/goobers/goobers/internal/journal"
)

func TestPrepareEventRejectsInvalidAndOwnsScrubbedPayload(t *testing.T) {
	scrubber := journal.NewRegistryScrubber()
	scrubber.Register([]byte("fixture-secret"))
	r := &runOwner{options: normalizedOptions(journal.RunWriterOptions{Scrubber: scrubber})}
	input := journal.Event{Type: journal.EventRunnerAnnotation, Runner: map[string]any{"text": "fixture-secret"}}
	clean, err := prepareEvent(r, input)
	if err != nil {
		t.Fatal(err)
	}
	input.Runner["text"] = "mutated"
	if clean.Runner["text"] != "[REDACTED]" {
		t.Fatalf("prepared payload = %v", clean.Runner)
	}
	if _, err := prepareEvent(r, journal.Event{Type: journal.EventNotificationRequested}); err == nil {
		t.Fatal("accepted invalid shared envelope")
	}
	input.Runner["text"] = string(bytes.Repeat([]byte("x"), maxEventBytes))
	if _, err := prepareEvent(r, input); err == nil {
		t.Fatal("accepted oversized envelope")
	}
}
