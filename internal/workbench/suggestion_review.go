package workbench

import "time"

// SuggestionSelection names an actual artifact event in a same-gaggle run.
type SuggestionSelection struct {
	RunID    string `json:"runId"`
	Sequence uint64 `json:"sequence"`
}

// SuggestionArtifact describes committed provenance, never model authority.
type SuggestionArtifact struct {
	Sequence      uint64 `json:"sequence"`
	StageSequence uint64 `json:"stageSequence"`
	StageID       string `json:"stageId"`
	Attempt       int    `json:"attempt"`
	Branch        int    `json:"branch"`
	Name          string `json:"name"`
	Digest        string `json:"digest"`
	Bytes         int64  `json:"bytes"`
}

// SuggestionInventory is an explicit bounded artifact window with no body reads.
type SuggestionInventory struct {
	RunID        string               `json:"runId"`
	Artifacts    []SuggestionArtifact `json:"artifacts"`
	NextSequence uint64               `json:"nextSequence,omitempty"`
	Partial      bool                 `json:"partial"`
}

// SuggestionCandidate remains outside the accepted source graph.
type SuggestionCandidate struct {
	Suggestion  BoundSuggestion `json:"suggestion"`
	Supported   bool            `json:"supported"`
	Reason      string          `json:"reason,omitempty"`
	ReviewID    string          `json:"reviewId,omitempty"`
	ReviewState string          `json:"reviewState,omitempty"`
}

// SuggestionBatch omits candidates without current endpoint visibility.
type SuggestionBatch struct {
	Selection  SuggestionSelection   `json:"selection"`
	Artifact   SuggestionArtifact    `json:"artifact"`
	Candidates []SuggestionCandidate `json:"candidates"`
	Omitted    int                   `json:"omitted"`
}

// SuggestionPreviewRequest chooses one candidate without sending source content.
type SuggestionPreviewRequest struct {
	Selection SuggestionSelection `json:"selection"`
	Key       string              `json:"key"`
}

// SuggestionPreview contains the same bounded diff as an ordinary metadata PR.
type SuggestionPreview struct {
	SourceBindingID string          `json:"sourceBindingId"`
	Preview         MetadataPreview `json:"preview"`
}

// SuggestionDecisionRequest contains no destination or edge override. Acceptance
// must name the exact owner revision and operation digest shown by Preview.
type SuggestionDecisionRequest struct {
	Selection               SuggestionSelection `json:"selection"`
	Key                     string              `json:"key"`
	Decision                string              `json:"decision"`
	Reason                  string              `json:"reason,omitempty"`
	ExpectedOwner           *MetadataRevision   `json:"expectedOwner,omitempty"`
	ExpectedOperationDigest string              `json:"expectedOperationDigest,omitempty"`
}

// SuggestionReview is operational attribution and a normal proposal receipt.
// Accepted/linked review states do not imply a source relationship exists.
type SuggestionReview struct {
	ID         string                   `json:"id"`
	Suggestion BoundSuggestion          `json:"suggestion"`
	State      string                   `json:"state"`
	Decision   string                   `json:"decision"`
	Reason     string                   `json:"reason,omitempty"`
	AcceptedAt time.Time                `json:"acceptedAt"`
	Proposal   *MetadataProposalCommand `json:"proposal,omitempty"`
	Duplicate  bool                     `json:"duplicate"`
}
