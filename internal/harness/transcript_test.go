package harness

import (
	"bufio"
	"bytes"
	"encoding/json"
	"testing"

	"github.com/goobers/goobers/internal/telemetry"
)

func canonicalTranscriptFixture(t *testing.T, legacy []byte) []byte {
	t.Helper()
	events := decodeLegacyTranscriptEvents(t, legacy)
	canonical, err := marshalTranscriptEvents(events...)
	if err != nil {
		t.Fatalf("marshal canonical transcript fixture: %v", err)
	}
	return canonical
}

func decodeLegacyTranscriptEvents(t *testing.T, data []byte) []transcriptEvent {
	t.Helper()
	scanner := bufio.NewScanner(bytes.NewReader(data))
	var events []transcriptEvent
	for scanner.Scan() {
		var event transcriptEvent
		if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
			t.Fatalf("decode transcript event: %v", err)
		}
		if event.Schema != "" {
			t.Fatalf("legacy fixture unexpectedly has schema %q", event.Schema)
		}
		events = append(events, event)
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("scan transcript fixture: %v", err)
	}
	return events
}

func decodeTranscriptEvents(t *testing.T, data []byte) []transcriptEvent {
	t.Helper()
	scanner := bufio.NewScanner(bytes.NewReader(data))
	var events []transcriptEvent
	for scanner.Scan() {
		var event transcriptEvent
		if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
			t.Fatalf("decode canonical transcript event: %v", err)
		}
		if event.Schema != telemetry.GenAIEventSchema {
			t.Fatalf("transcript event schema = %q, want %q", event.Schema, telemetry.GenAIEventSchema)
		}
		events = append(events, event)
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("scan canonical transcript: %v", err)
	}
	return events
}
