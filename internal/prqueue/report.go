package prqueue

import "time"

// MaxReportItems bounds each persisted selection observation. OmittedItems
// makes truncation explicit; CompleteSnapshot describes provider coverage,
// not whether every observed item fits in this report.
const MaxReportItems = 1000

// Item is one policy decision plus an independent claim observation.
type Item struct {
	Number   int              `json:"number"`
	Eligible bool             `json:"eligible"`
	Reason   string           `json:"reason,omitempty"`
	NextStep string           `json:"nextStep"`
	Claim    ClaimObservation `json:"claim"`
}

// Report is a bounded, scoped observation from one PR selection cycle.
// It is historical evidence, not authorization to claim or merge a PR.
type Report struct {
	Version          int       `json:"version"`
	RepositoryKey    string    `json:"repositoryKey"`
	Gaggle           string    `json:"gaggle"`
	Workflow         string    `json:"workflow"`
	RunID            string    `json:"runId"`
	ObservedAt       time.Time `json:"observedAt"`
	CompleteSnapshot bool      `json:"completeSnapshot"`
	MatchingItems    int       `json:"matchingItems"`
	OmittedItems     int       `json:"omittedItems"`
	Items            []Item    `json:"items"`
}

// Add records the selector's decisive reason, not a second evaluation of
// policy. An eligible PR may still be unavailable because of a live claim.
func (r *Report) Add(number int, reason string) {
	r.MatchingItems++
	if len(r.Items) >= MaxReportItems {
		r.OmittedItems++
		return
	}
	step := "Re-evaluate eligibility and claim availability before selecting this pull request."
	if reason != "" {
		step = NextStep(reason)
	}
	r.Items = append(r.Items, Item{
		Number: number, Eligible: reason == "", Reason: reason, NextStep: step,
		Claim: ObserveClaim(false, "", "", time.Time{}, r.ObservedAt, false),
	})
}
