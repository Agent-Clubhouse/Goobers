package main

import (
	"context"
	"encoding/json"
	"io"
	"time"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/providers"
)

// priorNoWorkVerdictField is the claimed-item field through which
// query-backlog hands an item's recorded no-work verdict to the run that just
// claimed it (#5643). The claimed item is the context the implement stage
// reads, so a later run can agree or disagree with the earlier conclusion
// instead of restarting from nothing.
const priorNoWorkVerdictField = "priorNoWorkVerdict"

// priorNoWorkVerdict is the claimed-item projection of a noWorkStreakRecord.
type priorNoWorkVerdict struct {
	// Count is how many no-work verdicts the item has had with no productive
	// run in between; the item parks at noWorkStreakThreshold.
	Count      int       `json:"count"`
	Stage      string    `json:"stage,omitempty"`
	Verdict    string    `json:"verdict,omitempty"`
	Reason     string    `json:"reason,omitempty"`
	Evidence   string    `json:"evidence,omitempty"`
	RunID      string    `json:"runId,omitempty"`
	RecordedAt time.Time `json:"recordedAt"`
}

// withPriorNoWorkVerdict adds the item's recorded no-work verdict to a
// single claimed item's JSON. Best-effort by design: the verdict is advisory
// context, so a state-plane read failure is reported on stderr and the claim
// proceeds with the item exactly as it was, never failing the selection.
func withPriorNoWorkVerdict(
	ctx context.Context,
	l instance.Layout,
	repo providers.RepositoryRef,
	itemID string,
	data []byte,
	stderr io.Writer,
) []byte {
	record, err := loadNoWorkStreakRecord(ctx, l, repo, itemID)
	if err != nil {
		pf(stderr, "warning: read recorded no-work verdict for item %s: %v\n", itemID, err)
		return data
	}
	if record.Count == 0 {
		return data
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return data
	}
	verdict, err := json.Marshal(priorNoWorkVerdict{
		Count: record.Count, Stage: record.Stage, Verdict: record.Verdict, Reason: record.Reason,
		Evidence: record.Evidence, RunID: record.RunID, RecordedAt: record.UpdatedAt,
	})
	if err != nil {
		return data
	}
	fields[priorNoWorkVerdictField] = verdict
	enriched, err := json.Marshal(fields)
	if err != nil {
		return data
	}
	return enriched
}
