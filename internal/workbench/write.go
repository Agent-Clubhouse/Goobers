package workbench

import apiv1 "github.com/goobers/goobers/api/v1alpha1"

// BacklogPatchRequest changes exactly one native field. SourceID and revision
// are mandatory; ID remains the mutable provider locator. List fields replace
// the complete visible set. Empty Values deliberately clears that set.
type BacklogPatchRequest struct {
	ID               string               `json:"id"`
	SourceID         string               `json:"sourceId"`
	ExpectedRevision string               `json:"expectedRevision"`
	Field            apiv1.WorkbenchField `json:"field"`
	Value            *string              `json:"value,omitempty"`
	Values           []string             `json:"values,omitempty"`
}

// BacklogWriteCapabilities declares actually implemented native operations,
// intersected with the source's write allowlist. It does not grant human access.
type BacklogWriteCapabilities struct {
	Fields              []apiv1.WorkbenchField        `json:"fields"`
	Relationships       []apiv1.WorkbenchRelationship `json:"relationships"`
	RevisionSemantics   string                        `json:"revisionSemantics"`
	MaxAssignees        int                           `json:"maxAssignees"`
	ControlLabelChanges bool                          `json:"controlLabelChanges"`
}

// BacklogPatchReceipt separates a desired command from the observed source.
// Outcome is not-applied, confirmed or unknown. A matching readback alone never
// proves this command authored that state. Unknown commands must be inspected;
// this adapter does not replay them or invent provider idempotency.
type BacklogPatchReceipt struct {
	OperationDigest      string       `json:"operationDigest"`
	Outcome              string       `json:"outcome"`
	RevisionSemantics    string       `json:"revisionSemantics"`
	ProviderAcknowledged bool         `json:"providerAcknowledged"`
	ObservedMatches      bool         `json:"observedMatches"`
	Observed             *BacklogItem `json:"observed,omitempty"`
}
