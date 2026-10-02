package nowork

import (
	"encoding/json"
	"time"
)

// PriorVerdictField is the claimed-item field through which
// query-backlog hands an item's recorded no-work verdict to the run that just
// claimed it (#5643). The claimed item is the context the implement stage
// reads, so a later run can agree or disagree with the earlier conclusion
// instead of restarting from nothing.
const PriorVerdictField = "priorNoWorkVerdict"

// PriorVerdict is the claimed-item projection of a Record.
type PriorVerdict struct {
	// Count is how many no-work verdicts the item has had with no productive
	// run in between; the item parks at StreakThreshold.
	Count      int       `json:"count"`
	Stage      string    `json:"stage,omitempty"`
	Verdict    string    `json:"verdict,omitempty"`
	Reason     string    `json:"reason,omitempty"`
	Evidence   string    `json:"evidence,omitempty"`
	RunID      string    `json:"runId,omitempty"`
	RecordedAt time.Time `json:"recordedAt"`
}

// WithPriorVerdict adds the recorded verdict to a claimed item's JSON. A missing
// record or an encoding failure leaves the original item unchanged.
func WithPriorVerdict(data []byte, record Record) []byte {
	if record.Count == 0 {
		return data
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return data
	}
	verdict, err := json.Marshal(PriorVerdict{
		Count: record.Count, Stage: record.Stage, Verdict: record.Verdict, Reason: record.Reason,
		Evidence: record.Evidence, RunID: record.RunID, RecordedAt: record.UpdatedAt,
	})
	if err != nil {
		return data
	}
	fields[PriorVerdictField] = verdict
	enriched, err := json.Marshal(fields)
	if err != nil {
		return data
	}
	return enriched
}
