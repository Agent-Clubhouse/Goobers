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
type AttributionEvidenceLink struct {
	RunID  string `json:"runId,omitempty"`
	NodeID string `json:"nodeId,omitempty"`
	Stage  string `json:"stage,omitempty"`
	Detail string `json:"detail,omitempty"`
	Source string `json:"source,omitempty"`
}

// ContributingPath is the aggregate attribution for a single node/path in one cohort.
type ContributingPath struct {
	Nodes      []string                  `json:"nodes,omitempty"`
	Share      float64                   `json:"share"`
	Confidence float64                   `json:"confidence"`
	Evidence   []AttributionEvidenceLink `json:"evidence,omitempty"`
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
	RunID            string      `json:"runId"`
	EffectiveVersion string      `json:"effectiveVersion,omitempty"`
	Workload         string      `json:"workload,omitempty"`
	Attribution      Attribution `json:"attribution"`
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
		byNode := map[string]*ContributingPath{}
		for _, observation := range group {
			for _, contribution := range observation.Attribution.Contributions {
				path := byNode[contribution.NodeID]
				if path == nil {
					path = &ContributingPath{Nodes: []string{contribution.NodeID}, Evidence: make([]AttributionEvidenceLink, 0, 1)}
					byNode[contribution.NodeID] = path
				}
				path.Share += contribution.Share
				path.Confidence += contribution.Confidence
				path.Evidence = append(path.Evidence, AttributionEvidenceLink{
					RunID:  observation.RunID,
					NodeID: contribution.NodeID,
					Stage:  contribution.Stage,
					Detail: fmt.Sprintf("share=%s, confidence=%s", formatFloat(contribution.Share), formatFloat(contribution.Confidence)),
					Source: "contribution",
				})
			}
			for _, cause := range observation.Attribution.Causes {
				for _, evidence := range cause.Evidence {
					if strings.TrimSpace(evidence) == "" {
						continue
					}
					aggregation.CounterEvidence = append(aggregation.CounterEvidence, AttributionEvidenceLink{
						RunID:  observation.RunID,
						NodeID: cause.NodeID,
						Stage:  cause.Stage,
						Detail: evidence,
						Source: string(cause.Class),
					})
				}
			}
		}

		paths := make([]ContributingPath, 0, len(byNode))
		for _, path := range byNode {
			if len(path.Evidence) == 0 {
				continue
			}
			count := float64(len(path.Evidence))
			path.Share /= count
			path.Confidence /= count
			paths = append(paths, *path)
		}
		sort.Slice(paths, func(i, j int) bool {
			if paths[i].Share == paths[j].Share {
				if paths[i].Confidence == paths[j].Confidence {
					return strings.Join(paths[i].Nodes, "/") < strings.Join(paths[j].Nodes, "/")
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
