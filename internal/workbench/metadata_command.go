package workbench

import "time"

// MetadataProposalCommand is bounded source-edit custody, never graph truth.
// Immutable attempt outcomes and later read observations remain distinguishable.
// Before/after source bytes are returned only by the separate preview operation.
type MetadataProposalCommand struct {
	ID                    string                        `json:"id"`
	Gaggle                string                        `json:"gaggle"`
	SourceBindingID       string                        `json:"sourceBindingId"`
	Actor                 CommandActor                  `json:"actor"`
	Path                  string                        `json:"path"`
	State                 string                        `json:"state"`
	Duplicate             bool                          `json:"duplicate"`
	RequestDigest         string                        `json:"requestDigest"`
	OperationDigest       string                        `json:"operationDigest"`
	Expected              MetadataRevision              `json:"expected"`
	ProposedContentDigest string                        `json:"proposedContentDigest,omitempty"`
	Branch                string                        `json:"branch,omitempty"`
	AcceptedAt            time.Time                     `json:"acceptedAt"`
	CompletedAt           *time.Time                    `json:"completedAt,omitempty"`
	Phases                []MetadataProposalPhase       `json:"phases"`
	Observations          []MetadataProposalObservation `json:"observations"`
	OmittedObservations   int64                         `json:"omittedObservations"`
	NextAction            string                        `json:"nextAction"`
}

// MetadataProposalPhase describes one immutable provider call, never a retry.
type MetadataProposalPhase struct {
	Name        string              `json:"name"`
	Outcome     string              `json:"outcome"`
	ClaimedAt   time.Time           `json:"claimedAt"`
	FinishedAt  *time.Time          `json:"finishedAt,omitempty"`
	TreeID      string              `json:"treeId,omitempty"`
	CommitID    string              `json:"commitId,omitempty"`
	PullRequest *MetadataProposalPR `json:"pullRequest,omitempty"`
}

// MetadataProposalObservation records exact read evidence independently of a call.
type MetadataProposalObservation struct {
	Phase       int                 `json:"phase"`
	At          time.Time           `json:"at"`
	Found       bool                `json:"found"`
	Matches     bool                `json:"matches"`
	TreeID      string              `json:"treeId,omitempty"`
	CommitID    string              `json:"commitId,omitempty"`
	PullRequest *MetadataProposalPR `json:"pullRequest,omitempty"`
}

// MetadataProposalPR identifies an exact provider PR, without arbitrary content.
type MetadataProposalPR struct {
	ID     string `json:"id"`
	Number int    `json:"number"`
	URL    string `json:"url"`
}
