package apicontract

import "github.com/goobers/goobers/internal/workbench"

// Workbench suggestion routes select retained artifacts and record human review.
const (
	WorkbenchSuggestionsPath                  = V1Prefix + "/gaggles/{gaggle}/workbench/suggestions"
	WorkbenchSuggestionArtifactsPath          = WorkbenchSuggestionsPath + "/artifacts/{run}"
	WorkbenchSuggestionLoadPath               = WorkbenchSuggestionsPath + "/artifacts/{run}/{sequence}"
	WorkbenchSuggestionPreviewPath            = WorkbenchSuggestionsPath + "/preview"
	WorkbenchSuggestionDecidePath             = WorkbenchSuggestionsPath + "/decisions"
	WorkbenchSuggestionReviewPath             = WorkbenchSuggestionsPath + "/reviews/{review}"
	RouteWorkbenchSuggestionArtifacts RouteID = "workbenchSuggestionArtifacts"
	RouteWorkbenchSuggestionLoad      RouteID = "workbenchSuggestionLoad"
	RouteWorkbenchSuggestionPreview   RouteID = "workbenchSuggestionPreview"
	RouteWorkbenchSuggestionDecide    RouteID = "workbenchSuggestionDecide"
	RouteWorkbenchSuggestionReview    RouteID = "workbenchSuggestionReview"
)

// SuggestionSelection identifies one host-retained artifact event.
type SuggestionSelection = workbench.SuggestionSelection

// SuggestionInventory is a bounded provenance-only artifact window.
type SuggestionInventory = workbench.SuggestionInventory

// SuggestionBatch contains visible candidates, never accepted source edges.
type SuggestionBatch = workbench.SuggestionBatch

// SuggestionPreviewRequest chooses one bounded candidate.
type SuggestionPreviewRequest = workbench.SuggestionPreviewRequest

// SuggestionPreview is a source-owned metadata PR diff.
type SuggestionPreview = workbench.SuggestionPreview

// SuggestionDecisionRequest cannot supply authority or replacement source content.
type SuggestionDecisionRequest = workbench.SuggestionDecisionRequest

// SuggestionReview exposes attributed custody and the linked ordinary proposal.
type SuggestionReview = workbench.SuggestionReview

func workbenchSuggestionRoute(id RouteID) bool {
	switch id {
	case RouteWorkbenchSuggestionArtifacts, RouteWorkbenchSuggestionLoad, RouteWorkbenchSuggestionPreview, RouteWorkbenchSuggestionDecide, RouteWorkbenchSuggestionReview:
		return true
	default:
		return false
	}
}

func withSuggestionFixtures(f wireFixtures) wireFixtures {
	run := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	ref := workbench.NodeRef{GaggleID: "web", SourceBindingID: "strategy", Kind: "objective-document", SourceID: "obj-00000000-0000-0000-0000-000000000001"}
	to := ref
	to.SourceID = "obj-00000000-0000-0000-0000-000000000002"
	pins := workbench.SuggestionRepositoryRevision(f.MetadataPreview.Expected)
	evidence := &workbench.SuggestionEvidence{SourceTargetDigest: f.MetadataPreview.TargetDigest, Path: "plan.md", RepositoryRevision: &pins}
	bound := workbench.BoundSuggestion{Key: f.MetadataPreview.OperationDigest, Proposal: workbench.RelationshipSuggestion{Kind: "references", From: workbench.SuggestionEndpoint{Ref: &ref, Evidence: evidence}, To: workbench.SuggestionEndpoint{Ref: &to, Evidence: evidence}, Rationale: "Review this relationship."}, Origin: workbench.SuggestionOrigin{RunID: run, StageID: "curate", Attempt: 1, ArtifactPath: "artifacts/suggestions.json", ArtifactDigest: f.MetadataPreview.TargetDigest}}
	artifact := workbench.SuggestionArtifact{Sequence: 3, StageSequence: 2, StageID: "curate", Attempt: 1, Name: "suggestions.json", Digest: f.MetadataPreview.TargetDigest, Bytes: 1024}
	f.SuggestionSelection = SuggestionSelection{RunID: run, Sequence: 3}
	f.SuggestionInventory = SuggestionInventory{RunID: run, Artifacts: []workbench.SuggestionArtifact{artifact}}
	f.SuggestionBatch = SuggestionBatch{Selection: f.SuggestionSelection, Artifact: artifact, Candidates: []workbench.SuggestionCandidate{{Suggestion: bound, Supported: true}}}
	f.SuggestionPreviewRequest = SuggestionPreviewRequest{Selection: f.SuggestionSelection, Key: bound.Key}
	f.SuggestionPreview = SuggestionPreview{SourceBindingID: "strategy", Preview: f.MetadataPreview}
	f.SuggestionDecision = SuggestionDecisionRequest{Selection: f.SuggestionSelection, Key: bound.Key, Decision: "accept", ExpectedOwner: &f.MetadataPreview.Expected, ExpectedOperationDigest: f.MetadataPreview.OperationDigest}
	f.SuggestionReview = SuggestionReview{ID: f.MetadataProposal.ID, Suggestion: bound, State: "linked", Decision: "accept", AcceptedAt: f.MetadataProposal.AcceptedAt, Proposal: &f.MetadataProposal}
	return f
}
