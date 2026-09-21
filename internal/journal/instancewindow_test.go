package journal

import (
	"testing"
	"time"
)

func TestInstanceLogWindowBounds(t *testing.T) {
	log, _, err := OpenInstanceLog(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = log.Close() }()
	for i := 0; i < 12; i++ {
		if err := log.Append(Event{Time: time.Now(), Type: EventRunnerAnnotation, Runner: map[string]any{"index": i}}); err != nil {
			t.Fatal(err)
		}
	}
	events, truncated, err := ReadInstanceLogWindow(log.Dir(), 4096, 3)
	if err != nil || !truncated || len(events) != 3 {
		t.Fatalf("events=%d truncated=%v err=%v", len(events), truncated, err)
	}
	for i, event := range events {
		if event.Runner["index"] != float64(i+9) {
			t.Fatal(event)
		}
	}
	events, truncated, err = ReadInstanceLogWindow(log.Dir(), 1, 3)
	if err != nil || !truncated || len(events) != 0 {
		t.Fatalf("byte bound: %d %v %v", len(events), truncated, err)
	}
}
func TestInstanceWindowCorruptionIsNotEmptyHistory(t *testing.T) {
	if _, _, err := decodeInstanceWindow([]byte("not-json\n"), 10, false); err == nil {
		t.Fatal("corruption hidden")
	}
	if _, _, err := ReadInstanceLogWindow(t.TempDir(), 9<<20, 10); err == nil {
		t.Fatal("unbounded window allowed")
	}
}
