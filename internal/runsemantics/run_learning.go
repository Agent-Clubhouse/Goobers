package runsemantics

import (
	"slices"
	"strings"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/learning"
)

func learningFindingsForRepass(gateName, stage string, verdict *apiv1.Verdict, result apiv1.ResultEnvelope) []apiv1.Finding {
	if verdict != nil && len(verdict.Findings) > 0 {
		findings := append([]apiv1.Finding(nil), verdict.Findings...)
		for i := range findings {
			learning.NormalizeFinding(&findings[i], gateName, findings[i].EvidenceDigest)
		}
		return findings
	}
	message := strings.TrimSpace(result.Summary)
	if result.Error != nil {
		if result.Error.Message != "" {
			message = result.Error.Message
		}
		if message == "" {
			message = result.Error.Code
		}
	}
	if message == "" {
		message = "validation failed"
	}
	finding := apiv1.Finding{
		Severity:               apiv1.SeverityError,
		Message:                message,
		Location:               stage,
		LearningClassification: apiv1.LearningValidation,
	}
	learning.NormalizeFinding(&finding, stage, learningEvidenceDigest(result.Artifacts))
	return []apiv1.Finding{finding}
}

func learningEvidence(pointer *apiv1.ContextPointer, verdict *apiv1.Verdict, result apiv1.ResultEnvelope) []apiv1.ArtifactPointer {
	var evidence []apiv1.ArtifactPointer
	if pointer != nil && pointer.Artifact != nil {
		evidence = append(evidence, *pointer.Artifact)
	}
	if verdict != nil {
		evidence = append(evidence, verdict.Evidence...)
	}
	evidence = append(evidence, result.Artifacts...)
	seen := map[string]bool{}
	out := evidence[:0]
	for _, artifact := range evidence {
		key := artifact.Digest + "\x00" + artifact.Path
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, artifact)
	}
	return out
}

func learningEvidenceDigest(artifacts []apiv1.ArtifactPointer) string {
	digests := make([]string, 0, len(artifacts))
	for _, artifact := range artifacts {
		if artifact.Digest != "" {
			digests = append(digests, artifact.Digest)
		}
	}
	slices.Sort(digests)
	if len(digests) == 0 {
		return ""
	}
	return apiv1.Digest([]byte(strings.Join(digests, "\n")))
}
