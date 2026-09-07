package validate

import (
	"strings"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/investigation"
)

func completeInvestigationEvidence() investigation.Evidence {
	p := completeArtifactPointer("artifacts/investigation/evidence")
	digest := p.Digest
	ref := investigation.EvidenceRef{Kind: "profile", Artifact: p, ProducerStage: "instrument", Description: "CPU profile", CaptureContext: map[string]any{"workers": 4}}
	return investigation.Evidence{
		SchemaVersion: investigation.SchemaVersion,
		Subject:       investigation.Subject{Item: apiv1.ExternalRef{Kind: "issue", URI: "https://example.com/issues/1", Description: "reproduction issue"}, Repository: "owner/repo", BaseRevision: strings.Repeat("a", 40), FixRevision: strings.Repeat("b", 40)},
		Environment:   investigation.Environment{Platform: "linux/amd64", Dimensions: map[string]any{"workers": 4}, ConfigDigest: digest},
		Reproduction:  investigation.Reproduction{Harness: p, Baseline: p, SourceSnapshotDigest: digest, OracleDigest: digest, Symptom: "unexpected stall"},
		Diagnosis:     investigation.Diagnosis{Report: p, Confidence: "confirmed", Evidence: []investigation.EvidenceRef{ref}},
		Fix:           investigation.Fix{Report: p, DiffDigest: digest},
		Validation:    investigation.Validation{Result: p, SourceSnapshotDigest: digest, HarnessDigest: digest, OracleDigest: digest, CompletedAttempts: 3, SymptomObservationsBefore: 1, SymptomObservationsAfter: 0, Passed: true},
		Attachments:   []investigation.EvidenceRef{ref},
	}
}
