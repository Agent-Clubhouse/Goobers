package gate

import (
	"encoding/json"
	"slices"
	"strings"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/learning"
)

type episodeHistory struct {
	latestSeq    uint64
	active       map[string]apiv1.Finding
	known        map[string]apiv1.Finding
	bySignature  map[string]apiv1.Finding
	evidenceByID map[string]map[string]bool
	resolved     map[string]bool
}

func reconcileLearningFindings(
	verdict apiv1.Verdict,
	pointers []apiv1.ContextPointer,
	resolve ArtifactBytes,
	gateName, diffDigest string,
) (apiv1.Verdict, findingResolution) {
	history := readEpisodeHistory(pointers, resolve, gateName)
	originalCount := len(verdict.Findings)
	remaining := make([]apiv1.Finding, 0, originalCount)
	current := map[string]bool{}
	var resolution findingResolution

	for _, finding := range verdict.Findings {
		if finding.ID == "" {
			if finding.LearningSignature == "" {
				finding.LearningSignature = learning.FindingSignature(gateName, finding)
			}
			if prior, ok := history.bySignature[finding.LearningSignature]; ok {
				finding.ID = prior.ID
				if finding.LearningClassification == "" {
					finding.LearningClassification = prior.LearningClassification
				}
			}
		}
		if prior, ok := history.known[finding.ID]; ok {
			if finding.LearningSignature == "" {
				finding.LearningSignature = prior.LearningSignature
			}
			if finding.LearningClassification == "" {
				finding.LearningClassification = prior.LearningClassification
			}
		}
		// The reviewed diff is the effective evidence when the reviewer does
		// not provide a finding-specific digest. Seed it before suppression so
		// a resolved identity can reopen on genuinely changed evidence.
		learning.NormalizeFinding(&finding, gateName, diffDigest)

		if history.resolved[finding.ID] && history.active[finding.ID].ID == "" {
			if finding.EvidenceDigest == "" || history.evidenceByID[finding.ID][finding.EvidenceDigest] {
				resolution.Suppressed = append(resolution.Suppressed, finding.ID)
				continue
			}
			resolution.Reopened = append(resolution.Reopened, finding.ID)
		}
		current[finding.ID] = true
		remaining = append(remaining, finding)
	}

	for id := range history.active {
		if !current[id] {
			resolution.Resolved = append(resolution.Resolved, id)
		}
	}
	verdict.Findings = remaining
	if verdict.Decision == apiv1.VerdictNeedsChanges && originalCount > 0 && len(remaining) == 0 && len(resolution.Suppressed) > 0 {
		verdict.Decision = apiv1.VerdictPass
		resolution.AllSuppressed = true
		note := ReasonFindingResolved + ": suppressed previously resolved finding identities without new evidence: " +
			strings.Join(resolution.Suppressed, ", ")
		if verdict.Rationale == "" {
			verdict.Rationale = note
		} else {
			verdict.Rationale += "\n\n" + note
		}
	}
	slices.Sort(resolution.Resolved)
	slices.Sort(resolution.Suppressed)
	slices.Sort(resolution.Reopened)
	slices.Sort(resolution.Disproven)
	return verdict, resolution
}

func readEpisodeHistory(pointers []apiv1.ContextPointer, resolve ArtifactBytes, gateName string) episodeHistory {
	history := episodeHistory{
		active:       map[string]apiv1.Finding{},
		known:        map[string]apiv1.Finding{},
		bySignature:  map[string]apiv1.Finding{},
		evidenceByID: map[string]map[string]bool{},
		resolved:     map[string]bool{},
	}
	if resolve == nil {
		return history
	}
	var episodes []learning.Episode
	for _, pointer := range pointers {
		// The SAME classifier contextFrom selection uses (#3928), so the set
		// of names that reach a stage and the set this reads back cannot
		// drift: a pointer selected as an episode is read as one, and a
		// malformed name is neither.
		class, _ := apiv1.ClassifyContextPointer(pointer.Name)
		if class != apiv1.ContextPointerLearningEpisode || pointer.Artifact == nil {
			continue
		}
		data, err := resolve(*pointer.Artifact)
		if err != nil {
			continue
		}
		var episode learning.Episode
		if json.Unmarshal(data, &episode) != nil || episode.Schema != learning.EpisodeSchema || episode.Gate != gateName {
			continue
		}
		episodes = append(episodes, episode)
	}
	for _, episode := range episodes {
		if episode.SourceSeq > history.latestSeq {
			history.latestSeq = episode.SourceSeq
		}
		for _, finding := range episode.Findings {
			history.known[finding.ID] = finding
			history.bySignature[finding.LearningSignature] = finding
			if history.evidenceByID[finding.ID] == nil {
				history.evidenceByID[finding.ID] = map[string]bool{}
			}
			if finding.EvidenceDigest != "" {
				history.evidenceByID[finding.ID][finding.EvidenceDigest] = true
			}
		}
	}
	for _, episode := range episodes {
		for _, finding := range episode.Findings {
			if episode.SourceSeq == history.latestSeq {
				history.active[finding.ID] = finding
			} else {
				history.resolved[finding.ID] = true
			}
		}
	}
	for id := range history.active {
		delete(history.resolved, id)
	}
	return history
}

func findingIDs(findings []apiv1.Finding) []string {
	ids := make([]string, 0, len(findings))
	for _, finding := range findings {
		if finding.ID != "" {
			ids = append(ids, finding.ID)
		}
	}
	return ids
}

func removedFindingIDs(before []string, remaining []apiv1.Finding) []string {
	kept := map[string]bool{}
	for _, finding := range remaining {
		kept[finding.ID] = true
	}
	var removed []string
	for _, id := range before {
		if id != "" && !kept[id] {
			removed = append(removed, id)
		}
	}
	return removed
}

func removedFindings(before, remaining []apiv1.Finding) []apiv1.Finding {
	kept := map[string]bool{}
	for _, finding := range remaining {
		kept[finding.ID] = true
	}
	var removed []apiv1.Finding
	for _, finding := range before {
		if finding.ID != "" && !kept[finding.ID] {
			removed = append(removed, finding)
		}
	}
	return removed
}

func learningFindingRecords(findings []apiv1.Finding) []map[string]any {
	records := make([]map[string]any, 0, len(findings))
	for _, finding := range findings {
		records = append(records, map[string]any{
			"id":             finding.ID,
			"signature":      finding.LearningSignature,
			"classification": finding.LearningClassification,
			"evidenceDigest": finding.EvidenceDigest,
			"message":        finding.Message,
			"location":       finding.Location,
			"class":          finding.Class,
			"severity":       finding.Severity,
		})
	}
	return records
}
