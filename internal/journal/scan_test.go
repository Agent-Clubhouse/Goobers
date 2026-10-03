package journal

import (
	"reflect"
	"testing"
)

func TestLastEvent(t *testing.T) {
	events := []Event{
		{Type: EventRunStarted},
		{Type: EventStageFinished, Stage: "first"},
		{Type: EventStageStarted, Stage: "second"},
		{Type: EventStageFinished, Stage: "second"},
	}

	event, ok := LastEvent(events, func(event Event) bool {
		return event.Type == EventStageFinished
	})
	if !ok || event.Stage != "second" {
		t.Fatalf("LastEvent() = (%+v, %v), want second stage finish", event, ok)
	}
	if event, ok := LastEvent(events, func(event Event) bool {
		return event.Type == EventRunFinished
	}); ok || !reflect.DeepEqual(event, Event{}) {
		t.Fatalf("LastEvent() = (%+v, %v), want zero event and false", event, ok)
	}
	if event, ok := LastEvent(nil, func(Event) bool { return true }); ok || !reflect.DeepEqual(event, Event{}) {
		t.Fatalf("LastEvent(nil) = (%+v, %v), want zero event and false", event, ok)
	}
}

func TestLastIndex(t *testing.T) {
	events := []Event{
		{Type: EventStageStarted, Stage: "first"},
		{Type: EventStageFinished, Stage: "first"},
		{Type: EventStageStarted, Stage: "second"},
	}

	index, ok := LastIndex(events, func(event Event) bool {
		return event.Type == EventStageStarted
	})
	if !ok || index != 2 {
		t.Fatalf("LastIndex() = (%d, %v), want (2, true)", index, ok)
	}
	if index, ok := LastIndex(events, func(event Event) bool {
		return event.Type == EventRunFinished
	}); ok || index != 0 {
		t.Fatalf("LastIndex() = (%d, %v), want (0, false)", index, ok)
	}
	if index, ok := LastIndex(nil, func(Event) bool { return true }); ok || index != 0 {
		t.Fatalf("LastIndex(nil) = (%d, %v), want (0, false)", index, ok)
	}
}
