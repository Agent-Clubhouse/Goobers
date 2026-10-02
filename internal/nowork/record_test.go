package nowork

import (
	"encoding/json"
	"testing"
	"time"
)

func TestAdvanceRetainsOnlyPriorReason(t *testing.T) {
	now := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	current := Record{Count: 2, Reason: "earlier rationale", Verdict: "already-fixed", Evidence: "old-commit", Stage: "old-stage", RunID: "old-run"}
	for _, reason := range []string{"", "new rationale"} {
		t.Run(reason, func(t *testing.T) {
			got := Advance(current, Terminal{Stage: "implement", Reason: reason}, "run-b", now)
			wantReason := reason
			if wantReason == "" {
				wantReason = current.Reason
			}
			want := Record{Count: 3, Reason: wantReason, Stage: "implement", RunID: "run-b", UpdatedAt: now}
			if got != want {
				t.Fatalf("record = %+v, want %+v", got, want)
			}
		})
	}
	got := Advance(Record{}, Terminal{Stage: "implement", Verdict: "already-fixed", Evidence: "abc123"}, "run-a", now)
	data, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"count":1,"verdict":"already-fixed","evidence":"abc123","stage":"implement","runId":"run-a","updatedAt":"2026-10-02T12:00:00Z"}`
	if string(data) != want {
		t.Fatalf("record JSON = %s, want %s", data, want)
	}
}
