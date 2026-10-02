package clustercheck

import (
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/journal"
)

func TestResultFreshnessAndRendering(t *testing.T) {
	at := time.Date(2026, 10, 2, 10, 0, 0, 0, time.UTC)
	for _, outcome := range []string{"pass", "warn", "fail"} {
		event := journal.Event{Type: journal.EventClusterCheckCompleted, Time: at, Runner: map[string]any{"check": "apiserver-ipblock-drift", "outcome": outcome, "expiresAt": at.Add(time.Hour).Format(time.RFC3339Nano)}}
		result, ok := FromEvent(event)
		if !ok {
			t.Fatal("valid event rejected")
		}
		for _, expired := range []bool{false, true} {
			now := at
			if expired {
				now = at.Add(time.Hour)
			}
			results := Snapshot(map[string]Result{result.Check: result}, now)
			want := map[string]string{"pass": "healthy", "warn": "warning", "fail": "degraded"}[outcome]
			if expired && outcome != "fail" {
				want = "stale"
			}
			if results[0].State != want || results[0].Stale != expired {
				t.Fatalf("result = %+v", results[0])
			}
			var text strings.Builder
			WriteStatus(&text, results)
			if !strings.Contains(text.String(), ": "+want) || !strings.Contains(text.String(), "last="+outcome) || !strings.Contains(text.String(), "expires=") {
				t.Fatalf("status = %s", text.String())
			}
		}
	}
}

func TestResultRejectsMalformedEvent(t *testing.T) {
	at := time.Now()
	for _, event := range []journal.Event{
		{},
		{Type: journal.EventClusterCheckCompleted, Time: at},
		{Type: journal.EventClusterCheckCompleted, Time: at, Runner: map[string]any{"check": "a", "outcome": "unknown", "expiresAt": at.Add(time.Hour).Format(time.RFC3339Nano)}},
		{Type: journal.EventClusterCheckCompleted, Time: at, Runner: map[string]any{"check": "a", "outcome": "pass", "expiresAt": at.Add(-time.Hour).Format(time.RFC3339Nano)}},
	} {
		if _, ok := FromEvent(event); ok {
			t.Fatalf("accepted %+v", event)
		}
	}
}
