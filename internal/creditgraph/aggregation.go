package creditgraph

import (
	"fmt"
	"sort"
	"strings"
)

// CohortKey identifies one cohort of repeated attribution evidence.
type CohortKey struct {
	EffectiveVersion string `json:"effectiveVersion"`
	Workload         string `json:"workload"`
}

// AttributionEvidenceLink is a concrete journal-backed reference to one piece of evidence.
// It expands the prior run/node/stage shorthand with the pointer fields a Tutor or
// operator needs to retrace a finding to the exact journal entry and, when present,
// the exact artifact that carried the evidence.
type AttributionEvidenceLink struct {
	RunID             string `json:"runId,omitempty"`
	NodeID            string `json:"nodeId,omitempty"`
	Stage             string `json:"stage,omitempty"`
	Detail            string `json:"detail,omitempty"`
	Source            string `json:"source,omitempty"`
	JournalSequence   int64  `json:"journalSequence,omitempty"`
	JournalPath       string `json:"journalPath,omitempty"`
	ArtifactPath      string `json:"artifactPath,omitempty"`
	ArtifactDigest    string `json:"artifactDigest,omitempty"`
	ArtifactMediaType string `json:"artifactMediaType,omitempty"`
}

// ContributingPath is the aggregate attribution for a single node/path in one cohort.
type ContributingPath struct {
	Nodes      []string                  `json:"nodes,omitempty"`
	Share      float64                   `json:"share"`
	Confidence float64                   `json:"confidence"`
	Evidence   []AttributionEvidenceLink `json:"evidence,omitempty"`
	samples    int
}

// CohortAggregation summarizes repeated attribution evidence across runs in one cohort.
type CohortAggregation struct {
	EffectiveVersion     string                    `json:"effectiveVersion,omitempty"`
	Workload             string                    `json:"workload,omitempty"`
	RunCount             int                       `json:"runCount"`
	TopContributingPaths []ContributingPath        `json:"topContributingPaths,omitempty"`
	CounterEvidence      []AttributionEvidenceLink `json:"counterEvidence,omitempty"`
}

// AttributionObservation is one run's attribution record placed in a cohort.
type AttributionObservation struct {
	RunID            string                    `json:"runId"`
	EffectiveVersion string                    `json:"effectiveVersion,omitempty"`
	Workload         string                    `json:"workload,omitempty"`
	Attribution      Attribution               `json:"attribution"`
	Evidence         []AttributionEvidenceLink `json:"evidence,omitempty"`
}

// AggregateAttributionEvidence summarizes repeated attribution evidence by cohort.
func AggregateAttributionEvidence(observations []AttributionObservation) []CohortAggregation {
	if len(observations) == 0 {
		return nil
	}
	byCohort := map[CohortKey][]AttributionObservation{}
	keys := make([]CohortKey, 0, len(observations))
	for _, observation := range observations {
		key := CohortKey{EffectiveVersion: observation.EffectiveVersion, Workload: observation.Workload}
		if _, exists := byCohort[key]; !exists {
			keys = append(keys, key)
		}
		byCohort[key] = append(byCohort[key], observation)
	}

	out := make([]CohortAggregation, 0, len(keys))
	for _, key := range keys {
		group := byCohort[key]
		aggregation := CohortAggregation{
			EffectiveVersion: key.EffectiveVersion,
			Workload:         key.Workload,
			RunCount:         len(group),
		}
		byPath := map[string]*ContributingPath{}
		for _, observation := range group {
			for _, contribution := range observation.Attribution.Contributions {
				if !surfaceContributionPath(contribution) {
					continue
				}
				pathNodes := contributionPath(contribution)
				if len(pathNodes) == 0 {
					continue
				}
				key := strings.Join(pathNodes, "/")
				path := byPath[key]
				if path == nil {
					path = &ContributingPath{Nodes: append([]string(nil), pathNodes...)}
					byPath[key] = path
				}
				path.samples++
				path.Share += contribution.Share
				path.Confidence += contribution.Confidence
				detail := fmt.Sprintf("share=%s, confidence=%s", formatFloat(contribution.Share), formatFloat(contribution.Confidence))
				path.Evidence = append(path.Evidence, observationEvidence(
					observation,
					lastNode(pathNodes),
					contribution.Stage,
					detail,
					"contribution",
				)...)
			}
			for _, cause := range observation.Attribution.Causes {
				for _, evidence := range cause.Evidence {
					if strings.TrimSpace(evidence) == "" {
						continue
					}
					aggregation.CounterEvidence = append(aggregation.CounterEvidence, observationEvidence(
						observation,
						cause.NodeID,
						cause.Stage,
						evidence,
						string(cause.Class),
					)...)
				}
			}
		}

		paths := make([]ContributingPath, 0, len(byPath))
		for _, path := range byPath {
			if path.samples == 0 {
				continue
			}
			count := float64(path.samples)
			path.Share /= count
			path.Confidence /= count
			paths = append(paths, *path)
		}
		sort.Slice(paths, func(i, j int) bool {
			if paths[i].Share == paths[j].Share {
				if paths[i].Confidence == paths[j].Confidence {
					if len(paths[i].Nodes) == len(paths[j].Nodes) {
						return strings.Join(paths[i].Nodes, "/") < strings.Join(paths[j].Nodes, "/")
					}
					return len(paths[i].Nodes) > len(paths[j].Nodes)
				}
				return paths[i].Confidence > paths[j].Confidence
			}
			return paths[i].Share > paths[j].Share
		})
		if len(paths) > 5 {
			paths = paths[:5]
		}
		aggregation.TopContributingPaths = paths

		sort.Slice(aggregation.CounterEvidence, func(i, j int) bool {
			if aggregation.CounterEvidence[i].RunID == aggregation.CounterEvidence[j].RunID {
				return aggregation.CounterEvidence[i].Detail < aggregation.CounterEvidence[j].Detail
			}
			return aggregation.CounterEvidence[i].RunID < aggregation.CounterEvidence[j].RunID
		})
		out = append(out, aggregation)
	}

	sort.Slice(out, func(i, j int) bool {
		if out[i].EffectiveVersion == out[j].EffectiveVersion {
			return out[i].Workload < out[j].Workload
		}
		return out[i].EffectiveVersion < out[j].EffectiveVersion
	})
	return out
}

func surfaceContributionPath(contribution Contribution) bool {
	switch contribution.Kind {
	case KindOutcome, KindRun, KindStage, KindSubagent, KindTool:
		return false
	default:
		return true
	}
}

func contributionPath(contribution Contribution) []string {
	if len(contribution.Path) > 0 {
		path := make([]string, 0, len(contribution.Path))
		for _, nodeID := range contribution.Path {
			if strings.TrimSpace(nodeID) == "" {
				continue
			}
			path = append(path, nodeID)
		}
		if len(path) > 0 {
			return path
		}
	}
	if strings.TrimSpace(contribution.NodeID) == "" {
		return nil
	}
	return []string{contribution.NodeID}
}

func observationEvidence(observation AttributionObservation, nodeID, stage, detail, source string) []AttributionEvidenceLink {
	var matches []AttributionEvidenceLink
	for _, link := range observation.Evidence {
		if link.Source != source || link.Detail != detail {
			continue
		}
		if strings.TrimSpace(nodeID) != "" && link.NodeID != nodeID {
			continue
		}
		if strings.TrimSpace(stage) != "" && link.Stage != stage {
			continue
		}
		matches = append(matches, link)
	}
	if len(matches) > 0 {
		return matches
	}
	return nil
}

func lastNode(path []string) string {
	if len(path) == 0 {
		return ""
	}
	return path[len(path)-1]
}

// AggregateAttributions is a compatibility alias for cohort-level aggregation.
func AggregateAttributions(observations []AttributionObservation) []CohortAggregation {
	return AggregateAttributionEvidence(observations)
}

// AggregateByCohort is a compatibility alias for cohort-level aggregation.
func AggregateByCohort(observations []AttributionObservation) []CohortAggregation {
	return AggregateAttributionEvidence(observations)
}

// AggregateByEffectiveVersionAndWorkload is a compatibility alias.
func AggregateByEffectiveVersionAndWorkload(observations []AttributionObservation) []CohortAggregation {
	return AggregateAttributionEvidence(observations)
}

func formatFloat(value float64) string {
	return fmt.Sprintf("%.6f", round(value))
}
