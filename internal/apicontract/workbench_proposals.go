package apicontract

import (
	"strings"
	"time"

	"github.com/goobers/goobers/internal/workbench"
)

// Governed metadata routes cannot select provider identity or publication targets.
const (
	WorkbenchProposalPreviewPath           = WorkbenchSourcesPath + "/{source}/proposal-preview"
	WorkbenchProposalsPath                 = WorkbenchSourcesPath + "/{source}/proposals"
	WorkbenchProposalPath                  = WorkbenchProposalsPath + "/{command}"
	WorkbenchProposalCheckPath             = WorkbenchProposalPath + "/check"
	WorkbenchProposalContinuePath          = WorkbenchProposalPath + "/continue"
	RouteWorkbenchProposalPreview  RouteID = "workbenchProposalPreview"
	RouteWorkbenchProposalSubmit   RouteID = "workbenchProposalSubmit"
	RouteWorkbenchProposal         RouteID = "workbenchProposal"
	RouteWorkbenchProposalCheck    RouteID = "workbenchProposalCheck"
	RouteWorkbenchProposalContinue RouteID = "workbenchProposalContinue"
)

// MetadataChangeRequest is one typed edit to an existing declared source file.
type MetadataChangeRequest = workbench.MetadataChangeRequest

// MetadataPreview contains exact before/after bytes under a4MiB encoded ceiling.
type MetadataPreview = workbench.MetadataPreview

// MetadataProposalCommand exposes immutable attempts and separate observations.
type MetadataProposalCommand = workbench.MetadataProposalCommand

func workbenchProposalRoute(id RouteID) bool {
	switch id {
	case RouteWorkbenchProposalPreview, RouteWorkbenchProposalSubmit, RouteWorkbenchProposal, RouteWorkbenchProposalCheck, RouteWorkbenchProposalContinue:
		return true
	default:
		return false
	}
}
func withWorkbenchProposalFixtures(f wireFixtures) wireFixtures {
	at := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	value := "# Revised strategy\n"
	revision := workbench.MetadataRevision{Commit: strings.Repeat("a", 40), BlobID: strings.Repeat("b", 40), ContentDigest: strings.Repeat("c", 64)}
	f.MetadataChange = MetadataChangeRequest{Path: "plan.md", Expected: revision, Field: "description", Value: &value}
	f.MetadataObjective = MetadataChangeRequest{Path: "plan.md", Expected: revision, Objective: &workbench.MetadataObjectiveAssignment{ObjectiveID: "obj-00000000-0000-0000-0000-000000000001", Title: "Assigned objective"}}
	f.MetadataAlias = MetadataChangeRequest{Path: "relationships.yaml", Expected: revision, Alias: &workbench.MetadataAliasEdit{Action: "add", Alias: workbench.Alias{Name: "delivery", Target: workbench.NodeRef{GaggleID: "web", SourceBindingID: "strategy", Kind: "objective-document", SourceID: "obj-00000000-0000-0000-0000-000000000001"}}}}
	f.MetadataPreview = MetadataPreview{Path: "plan.md", Expected: revision, TargetDigest: strings.Repeat("d", 64), OperationDigest: strings.Repeat("e", 64), ProposedContentDigest: strings.Repeat("f", 64), Changed: true, Before: "# Strategy\n", After: value}
	f.MetadataProposal = MetadataProposalCommand{ID: "workbench-" + strings.Repeat("a", 32), Gaggle: "web", SourceBindingID: "strategy", Actor: workbench.CommandActor{Issuer: "https://identity.example", Subject: "alice"}, Path: "plan.md", State: "unknown", RequestDigest: strings.Repeat("b", 64), OperationDigest: strings.Repeat("c", 64), Expected: revision, ProposedContentDigest: strings.Repeat("d", 64), Branch: "goobers/workbench/" + strings.Repeat("a", 32), AcceptedAt: at, Phases: []workbench.MetadataProposalPhase{{Name: "tree", Outcome: "unknown", ClaimedAt: at, FinishedAt: &at}}, Observations: []workbench.MetadataProposalObservation{}, NextAction: "Inspect the exact retained effect; do not retry the write."}
	return f
}
