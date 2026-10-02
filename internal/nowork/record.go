package nowork

import "time"

// Record is one item's authoritative repeated-no-work state.
//
// Reason carries the LAST recorded no-work rationale so the park comment can
// quote why the implementer kept declining, rather than parking an item with
// no explanation attached — the second half of #5379, which observed that the
// agentic stage journaled `status: no-work` with no outputs at all, leaving an
// operator unable to tell an unactionable item from a misread one.
//
// Verdict and Evidence (#5643) carry the stage's own classification of its
// no-work answer (for example "already-fixed") and the evidence it cited (for
// example an existing commit). The record is the durable per-item verdict
// artifact: query-backlog hands it to the next run on the item, and a later
// verdict that disagrees with it is flagged on the issue.
type Record struct {
	Count     int       `json:"count"`
	Reason    string    `json:"reason,omitempty"`
	Verdict   string    `json:"verdict,omitempty"`
	Evidence  string    `json:"evidence,omitempty"`
	Stage     string    `json:"stage,omitempty"`
	RunID     string    `json:"runId,omitempty"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// Advance records a completed no-work verdict, retaining an earlier rationale
// when the current stage supplies none. The caller supplies the CAS attempt's time.
func Advance(current Record, terminal Terminal, runID string, now time.Time) Record {
	next := Record{
		Count:     current.Count + 1,
		Reason:    terminal.Reason,
		Verdict:   terminal.Verdict,
		Evidence:  terminal.Evidence,
		Stage:     terminal.Stage,
		RunID:     runID,
		UpdatedAt: now,
	}
	// An absent reason on this iteration must not erase a reason an
	// earlier iteration did record: the park comment is more useful
	// quoting a stale rationale than quoting nothing.
	if next.Reason == "" {
		next.Reason = current.Reason
	}
	return next
}
