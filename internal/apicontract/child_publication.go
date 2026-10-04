package apicontract

import "time"

// ChildPublicationSummary exposes verified desired/observed publication status.
// It never includes credentials, filesystem locations or authored PR text.
type ChildPublicationSummary struct {
	CheckedAt         *time.Time `json:"checkedAt,omitempty"`
	Observation       string     `json:"observation"`
	Action            string     `json:"action"`
	IntentDigest      string     `json:"intentDigest"`
	State             string     `json:"state"`
	Head              string     `json:"head"`
	Base              string     `json:"base"`
	Commit            string     `json:"commit"`
	PullRequestURL    string     `json:"pullRequestUrl,omitempty"`
	PullRequestNumber int        `json:"pullRequestNumber,omitempty"`
	NeedsHuman        bool       `json:"needsHuman"`
	CreatedAt         time.Time  `json:"createdAt"`
	UpdatedAt         time.Time  `json:"updatedAt"`
}

// ChildPublicationCheckRequest selects an existing immutable intent for a
// read-only provider observation. Human identity is supplied by authentication.
type ChildPublicationCheckRequest struct {
	Action               string `json:"action"`
	ExpectedIntentDigest string `json:"expectedIntentDigest"`
}

// ChildPublicationCheckResult distinguishes a completed observation from proof
// that the external effect is confirmed. A pending publication stays actionable.
type ChildPublicationCheckResult struct {
	RunID       string                  `json:"runId"`
	RequestID   string                  `json:"requestId"`
	Publication ChildPublicationSummary `json:"publication"`
}
