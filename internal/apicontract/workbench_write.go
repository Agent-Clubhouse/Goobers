package apicontract

import (
	"strings"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/workbench"
)

// Native write routes require a verified human and current per-source authority.
const (
	WorkbenchWriteCapabilitiesPath          = WorkbenchSourcesPath + "/{source}/write-capabilities"
	WorkbenchCommandPath                    = WorkbenchSourcesPath + "/{source}/commands/{command}"
	RouteWorkbenchWriteCapabilities RouteID = "workbenchWriteCapabilities"
	RouteWorkbenchPatch             RouteID = "workbenchPatch"
	RouteWorkbenchCommand           RouteID = "workbenchCommand"
)

// BacklogPatchInput contains one field change. Native locator, actor, source
// binding, credentials and idempotency key belong to verified transport context.
type BacklogPatchInput struct {
	SourceID         string               `json:"sourceId"`
	ExpectedRevision string               `json:"expectedRevision"`
	Field            apiv1.WorkbenchField `json:"field"`
	Value            *string              `json:"value,omitempty"`
	Values           []string             `json:"values,omitempty"`
}

// BacklogWriteCapabilities is a configured native-operation intersection.
type BacklogWriteCapabilities = workbench.BacklogWriteCapabilities

// BacklogEditCommand exposes bounded evidence, not internal custody or authority.
type BacklogEditCommand = workbench.BacklogEditCommand

func workbenchWriteRoute(id RouteID) bool {
	return id == RouteWorkbenchWriteCapabilities || id == RouteWorkbenchPatch || id == RouteWorkbenchCommand
}
func withWorkbenchWriteFixtures(fixtures wireFixtures) wireFixtures {
	at := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	value := "Clarify the human surface"
	fixtures.WorkbenchPatch = BacklogPatchInput{SourceID: "987654", ExpectedRevision: "2026-10-04T11:59:00Z", Field: "title", Value: &value}
	fixtures.WorkbenchWriteCapabilities = BacklogWriteCapabilities{Fields: []apiv1.WorkbenchField{"title", "description"}, Relationships: []apiv1.WorkbenchRelationship{}, RevisionSemantics: "timestamp-preflight", MaxAssignees: 10}
	fixtures.WorkbenchCommand = BacklogEditCommand{ID: "workbench-" + strings.Repeat("a", 32), Gaggle: "web", SourceBindingID: "backlog", Actor: workbench.CommandActor{Issuer: "https://identity.example", Subject: "alice"}, ItemID: "42", SourceID: "987654", Field: "title", State: "unknown", RequestDigest: strings.Repeat("b", 64), OperationDigest: strings.Repeat("c", 64), AcceptedAt: at, AttemptedAt: &at, CompletedAt: &at, Receipt: &workbench.BacklogPatchReceipt{OperationDigest: strings.Repeat("c", 64), Outcome: "unknown", RevisionSemantics: "timestamp-preflight", ObservedMatches: true, Observed: &fixtures.WorkbenchItem}, NextAction: "Inspect the source. Do not retry the write."}
	return fixtures
}
