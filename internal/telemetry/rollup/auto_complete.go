package rollup

import "sort"

// ADO acknowledges auto-complete without a forge queue-entry identity. Use
// the persisted intent identity instead; never manufacture a queue entry or
// infer completed-merge ownership from an accepted auto-complete setting.
type autoCompleteEvidence struct {
	entries   map[string]RecordedLandingIntent
	conflicts map[string]bool
}

func newAutoCompleteEvidence() *autoCompleteEvidence {
	return &autoCompleteEvidence{entries: map[string]RecordedLandingIntent{}, conflicts: map[string]bool{}}
}

func (e *autoCompleteEvidence) observe(entry RecordedLandingIntent) {
	if previous, exists := e.entries[entry.ID]; exists {
		if previous.LandingIntent != entry.LandingIntent || previous.Provider != entry.Provider || previous.InstanceID != entry.InstanceID || previous.Gaggle != entry.Gaggle || previous.RunID != entry.RunID {
			e.conflicts[entry.ID] = true
		}
		return
	}
	e.entries[entry.ID] = entry
}

func (e *autoCompleteEvidence) publish(query MergeReportQuery, report *MergeReport) {
	report.AutoCompleteAcknowledgements = []RecordedLandingIntent{}
	report.ConflictingAutoCompleteIntents = len(e.conflicts)
	for id, entry := range e.entries {
		if e.conflicts[id] || !matchesMergeQuery(ConfirmedMerge{InstanceID: entry.InstanceID, Gaggle: entry.Gaggle, RepositoryAPIURL: entry.RepositoryAPIURL}, query) {
			continue
		}
		report.AutoCompleteAcknowledgements = append(report.AutoCompleteAcknowledgements, entry)
	}
	sort.Slice(report.AutoCompleteAcknowledgements, func(i, j int) bool {
		a, b := report.AutoCompleteAcknowledgements[i], report.AutoCompleteAcknowledgements[j]
		if !a.OccurredAt.Equal(b.OccurredAt) {
			return a.OccurredAt.Before(b.OccurredAt)
		}
		return a.ID < b.ID
	})
}
