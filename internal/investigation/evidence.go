// Package investigation defines the durable evidence manifest shared by the
// investigation workflow and its deterministic evidence consumers.
package investigation

import apiv1 "github.com/goobers/goobers/api/v1alpha1"

// SchemaVersion identifies the immutable investigation evidence wire contract.
const SchemaVersion = "goobers.dev/investigation-evidence/v1alpha1"

// Evidence is an index over independently retained journal artifacts, not a
// container for raw diagnostic payloads or an attestation of repository state.
type Evidence struct {
	SchemaVersion string        `json:"schemaVersion"`
	Subject       Subject       `json:"subject"`
	Environment   Environment   `json:"environment"`
	Reproduction  Reproduction  `json:"reproduction"`
	Diagnosis     Diagnosis     `json:"diagnosis"`
	Fix           Fix           `json:"fix"`
	Validation    Validation    `json:"validation"`
	Attachments   []EvidenceRef `json:"attachments,omitempty"`
}

// Subject identifies the issue and the pinned revisions under investigation.
type Subject struct {
	Item         apiv1.ExternalRef `json:"item"`
	Repository   string            `json:"repository"`
	BaseRevision string            `json:"baseRevision"`
	FixRevision  string            `json:"fixRevision"`
}

// Environment contains sanitized, bounded scalar comparison dimensions.
type Environment struct {
	Platform     string         `json:"platform"`
	Dimensions   map[string]any `json:"dimensions"`
	ConfigDigest string         `json:"configDigest"`
}

// Reproduction references the harness and the runner-authored baseline.
type Reproduction struct {
	Harness              apiv1.ArtifactPointer `json:"harness"`
	Baseline             apiv1.ArtifactPointer `json:"baseline"`
	SourceSnapshotDigest string                `json:"sourceSnapshotDigest"`
	OracleDigest         string                `json:"oracleDigest"`
	Symptom              string                `json:"symptom"`
}

// Diagnosis identifies the causal report and at least one supporting artifact.
type Diagnosis struct {
	Report     apiv1.ArtifactPointer `json:"report"`
	Confidence string                `json:"confidence"`
	Evidence   []EvidenceRef         `json:"evidence"`
}

// Fix links the implementation report to the canonical diff digest.
type Fix struct {
	Report     apiv1.ArtifactPointer `json:"report"`
	DiffDigest string                `json:"diffDigest"`
}

// Validation references the runner-authored result. These fields describe
// evidence; #1482's admission checks and revision verifier must attest it.
type Validation struct {
	Result                    apiv1.ArtifactPointer `json:"result"`
	SourceSnapshotDigest      string                `json:"sourceSnapshotDigest"`
	HarnessDigest             string                `json:"harnessDigest"`
	OracleDigest              string                `json:"oracleDigest"`
	CompletedAttempts         int                   `json:"completedAttempts"`
	SymptomObservationsBefore int                   `json:"symptomObservationsBefore"`
	SymptomObservationsAfter  int                   `json:"symptomObservationsAfter"`
	Passed                    bool                  `json:"passed"`
}

// EvidenceRef describes one optional-kind diagnostic artifact. No particular
// kind is mandatory; every reference that is present must resolve and verify.
type EvidenceRef struct {
	Kind           string                `json:"kind"`
	Artifact       apiv1.ArtifactPointer `json:"artifact"`
	ProducerStage  string                `json:"producerStage"`
	Description    string                `json:"description"`
	CaptureContext map[string]any        `json:"captureContext"`
}
