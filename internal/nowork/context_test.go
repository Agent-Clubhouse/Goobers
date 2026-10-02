package nowork

import (
	"encoding/json"
	"testing"
	"time"
)

func TestWithPriorVerdictPreservesItemAndProjection(t *testing.T) {
	item := []byte(`{"id":"5629","title":"flake","extra":{"keep":true}}`)
	record := Record{Count: 1, Stage: "implement", Verdict: "already-fixed", Reason: "fixed by an earlier commit", Evidence: "02642a86a", RunID: "run-a", UpdatedAt: time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)}
	got := WithPriorVerdict(item, record)
	var decoded struct {
		ID    string          `json:"id"`
		Title string          `json:"title"`
		Extra json.RawMessage `json:"extra"`
		Prior *PriorVerdict   `json:"priorNoWorkVerdict"`
	}
	if err := json.Unmarshal(got, &decoded); err != nil {
		t.Fatal(err)
	}
	want := PriorVerdict{Count: 1, Stage: "implement", Verdict: "already-fixed", Reason: "fixed by an earlier commit", Evidence: "02642a86a", RunID: "run-a", RecordedAt: record.UpdatedAt}
	if decoded.ID != "5629" || decoded.Title != "flake" || string(decoded.Extra) != `{"keep":true}` || decoded.Prior == nil || *decoded.Prior != want {
		t.Fatalf("claimed item = %s, want preserved item and %+v", got, want)
	}
}

func TestWithPriorVerdictFallsBackToOriginalItem(t *testing.T) {
	for _, tc := range []struct {
		name   string
		data   string
		record Record
	}{
		{name: "no verdict", data: `{ "id": "5629" }`},
		{name: "invalid JSON", data: `{`, record: Record{Count: 1}},
		{name: "non-object", data: `[]`, record: Record{Count: 1}},
		{name: "unencodable time", data: `{"id":"5629"}`, record: Record{Count: 1, UpdatedAt: time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := WithPriorVerdict([]byte(tc.data), tc.record); string(got) != tc.data {
				t.Fatalf("item changed to %s, want %s", got, tc.data)
			}
		})
	}
}
