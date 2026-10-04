package apicontract

import apiv1 "github.com/goobers/goobers/api/v1alpha1"

// InteractiveRunView is an explicitly authorized shared view of one run's
// actionable gate occurrences and recorded guidance.
type InteractiveRunView struct {
	RunID         string                        `json:"runId"`
	Gaggle        string                        `json:"gaggle"`
	Phase         string                        `json:"phase"`
	Actions       []InteractiveRunAction        `json:"actions"`
	Guidance      []apiv1.OperatorMessageRecord `json:"guidance"`
	RestartReason string                        `json:"restartReason"`
}

// InteractiveRunAction binds an offered command to an observed gate occurrence.
type InteractiveRunAction struct {
	Kind            string   `json:"kind"`
	Stage           string   `json:"stage"`
	SubjectSequence uint64   `json:"subjectSequence"`
	Decisions       []string `json:"decisions"`
	Available       bool     `json:"available"`
	Reason          string   `json:"reason"`
}

// InteractiveRunCommand contains human data only. Identity and gaggle are
// resolved from the authenticated request and retained run.
type InteractiveRunCommand struct {
	Kind                    string   `json:"kind"`
	Stage                   string   `json:"stage"`
	ExpectedSubjectSequence uint64   `json:"expectedSubjectSequence"`
	Decision                string   `json:"decision,omitempty"`
	Rationale               string   `json:"rationale,omitempty"`
	Guidance                string   `json:"guidance,omitempty"`
	GuidanceIDs             []string `json:"guidanceIds,omitempty"`
}

// InteractiveRunCommandResult identifies durable evidence of acceptance.
// Saved guidance is not evidence that an agent received it or work resumed.
type InteractiveRunCommandResult struct {
	Status            string                       `json:"status"`
	Accepted          bool                         `json:"accepted"`
	RunID             string                       `json:"runId"`
	JournalSequence   uint64                       `json:"journalSequence"`
	Phase             string                       `json:"phase"`
	Guidance          *apiv1.OperatorMessageRecord `json:"guidance,omitempty"`
	ContinuationRunID string                       `json:"continuationRunId,omitempty"`
}
