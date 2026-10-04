package apicontract

import (
	"strings"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/workbench"
)

func workbenchDocumentFixture() workbench.DocumentPage {
	ref := workbench.NodeRef{GaggleID: "web", SourceBindingID: "strategy", Kind: "objective-document", SourceID: "obj-00000000-0000-0000-0000-000000000001"}
	edge := workbench.Edge{EdgeID: "edge-00000000-0000-0000-0000-000000000001", Kind: "contributes-to", From: workbench.NodeRef{GaggleID: "web", SourceBindingID: "backlog", Kind: "work-item", SourceID: "987654"}, To: ref, Rationale: "Explicit team objective"}
	commit := strings.Repeat("a", 40)
	return workbench.DocumentPage{SourceBindingID: "strategy", Repository: apiv1.InteractiveRepositoryIdentity{Provider: "github", Owner: "acme", Name: "plans"}, Branch: "main", Commit: commit, SourceTargetDigest: strings.Repeat("b", 64), TotalPaths: 1, Exhausted: true, Coverage: "complete", Files: []workbench.DocumentFileRead{{Path: "objectives/reliability.md", Status: "available", Provenance: &workbench.SourceProvenance{Commit: commit, BlobID: strings.Repeat("c", 40), ContentDigest: strings.Repeat("d", 64)}, Ref: &ref, Body: "# Reliability\nKeep work recoverable.\n", Objective: &workbench.ObjectiveMetadata{SchemaVersion: "objectives/v1", ObjectiveID: ref.SourceID, Title: "Reliable delivery", Edges: []workbench.Edge{edge}}}}}
}
