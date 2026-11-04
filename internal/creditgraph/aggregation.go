package creditgraph

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/goobers/goobers/internal/journal"
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
	Score      *float64                  `json:"score,omitempty"`
	Confidence float64                   `json:"confidence"`
	Evidence   []AttributionEvidenceLink `json:"evidence,omitempty"`
	samples    int
	weight     float64
}

// CohortAggregation summarizes repeated attribution evidence across runs in one cohort.
type CohortAggregation struct {
	EffectiveVersion     string                    `json:"effectiveVersion,omitempty"`
	Workload             string                    `json:"workload,omitempty"`
	RunCount             int                       `json:"runCount"`
	TopContributingPaths []ContributingPath        `json:"topContributingPaths,omitempty"`
	CounterEvidence      []AttributionEvidenceLink `json:"counterEvidence,omitempty"`
	GroundTruth          *GroundTruthSummary       `json:"groundTruth,omitempty"`
}

// GroundTruthSummary counts a cohort's runs by their effective user-defined
// ground-truth label. Unlabeled runs are counted only in RunCount, so
// LabeledRunCount shows how much of the cohort the verdicts cover.
type GroundTruthSummary struct {
	LabeledRunCount   int `json:"labeledRunCount"`
	CorrectRunCount   int `json:"correctRunCount"`
	IncorrectRunCount int `json:"incorrectRunCount"`
}

// AttributionObservation is one run's attribution record placed in a cohort.
type AttributionObservation struct {
	RunID            string                    `json:"runId"`
	Workflow         string                    `json:"workflow,omitempty"`
	WorkflowDigest   string                    `json:"workflowDigest,omitempty"`
	GooberDigest     string                    `json:"gooberDigest,omitempty"`
	EffectiveVersion string                    `json:"effectiveVersion,omitempty"`
	Workload         string                    `json:"workload,omitempty"`
	Environments     []string                  `json:"environments,omitempty"`
	ObservedAt       time.Time                 `json:"observedAt,omitempty"`
	Status           RecordStatus              `json:"status,omitempty"`
	RunPhase         journal.RunPhase          `json:"runPhase,omitempty"`
	Failure          string                    `json:"failure,omitempty"`
	Attribution      Attribution               `json:"attribution"`
	Evidence         []AttributionEvidenceLink `json:"evidence,omitempty"`
	GroundTruth      *GroundTruthLabel         `json:"groundTruth,omitempty"`
}

// AggregateAttributionEvidence summarizes repeated attribution evidence by cohort.
func AggregateAttributionEvidence(observations []AttributionObservation) []CohortAggregation {
	if len(observations) == 0 {
		return nil
	}
	byCohort := map[CohortKey][]AttributionObservation{}
	keys := make([]CohortKey, 0, len(observations))
	for _, observation := range observations {
		if observation.Status == RecordFailed || strings.TrimSpace(observation.EffectiveVersion) == "" {
			continue
		}
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
		aggregation.GroundTruth = summarizeGroundTruth(group)
		byPath := map[string]*ContributingPath{}
		for _, observation := range group {
			nodeIdentities := contributionNodeIdentities(observation.Attribution.Contributions)
			// Several occurrences in one run (repeated attempts, invocations, or
			// tool calls) can share a structural path. Consolidate them first so
			// each run contributes its weight to a path exactly once.
			runPaths := map[string]*runPathTotal{}
			runOrder := make([]string, 0)
			for _, contribution := range observation.Attribution.Contributions {
				if !surfaceContributionPath(contribution) {
					continue
				}
				rawPathNodes := contributionPath(contribution)
				pathNodes := structuralContributionPath(rawPathNodes, nodeIdentities)
				if len(pathNodes) == 0 {
					continue
				}
				key := strings.Join(pathNodes, "/")
				total := runPaths[key]
				if total == nil {
					total = &runPathTotal{}
					runPaths[key] = total
					runOrder = append(runOrder, key)
				}
				total.add(contribution)
				path := byPath[key]
				if path == nil {
					path = &ContributingPath{Nodes: append([]string(nil), pathNodes...), Score: new(float64)}
					byPath[key] = path
				}
				detail := fmt.Sprintf("share=%s, confidence=%s", formatFloat(contribution.Share), formatFloat(contribution.Confidence))
				path.Evidence = append(path.Evidence, observationEvidence(
					observation,
					lastNode(rawPathNodes),
					contribution.Stage,
					detail,
					"contribution",
				)...)
			}
			weight := groundTruthWeight(observation.GroundTruth)
			for _, key := range runOrder {
				total := runPaths[key]
				path := byPath[key]
				path.samples++
				path.weight += weight
				path.Share += weight * total.share
				*path.Score += weight * total.score
				path.Confidence += weight * groundTruthConfidence(total.confidence(), observation.GroundTruth)
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
			if path.samples == 0 || path.weight == 0 {
				continue
			}
			path.Share /= path.weight
			*path.Score /= path.weight
			path.Confidence /= path.weight
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

func summarizeGroundTruth(group []AttributionObservation) *GroundTruthSummary {
	var summary GroundTruthSummary
	for _, observation := range group {
		if observation.GroundTruth == nil {
			continue
		}
		switch observation.GroundTruth.Outcome {
		case LabelCorrect:
			summary.CorrectRunCount++
		case LabelIncorrect:
			summary.IncorrectRunCount++
		default:
			continue
		}
		summary.LabeledRunCount++
	}
	if summary.LabeledRunCount == 0 {
		return nil
	}
	return &summary
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

func contributionNodeIdentities(contributions []Contribution) map[string]string {
	identities := make(map[string]string, len(contributions))
	for _, contribution := range contributions {
		if contribution.NodeID == "" {
			continue
		}
		identities[contribution.NodeID] = structuralNodeIdentity(contribution)
	}
	return identities
}

func structuralContributionPath(path []string, identities map[string]string) []string {
	structural := make([]string, 0, len(path))
	for _, nodeID := range path {
		if identity := identities[nodeID]; identity != "" {
			structural = append(structural, identity)
			continue
		}
		if strings.HasPrefix(nodeID, "run:") {
			structural = append(structural, "run")
			continue
		}
		structural = append(structural, nodeID)
	}
	return structural
}

func structuralNodeIdentity(contribution Contribution) string {
	label := strings.TrimSpace(contribution.Label)
	switch contribution.Kind {
	case KindOutcome:
		return "outcome"
	case KindRun:
		return "run"
	case KindStage:
		if label == "" {
			label = strings.TrimSpace(contribution.Stage)
		}
		return structuralLabel("stage", label)
	case KindSubagent:
		return structuralLabel("subagent", structuralAgentLabel(label))
	case KindModelInvocation:
		return structuralLabel("model", label)
	case KindToolCall:
		return structuralLabel("toolcall", label)
	case KindToolResult:
		return structuralLabel("toolresult", label)
	case KindTool:
		return structuralLabel("tool", label)
	case KindEvidence:
		return structuralLabel("evidence", label)
	case KindEvaluator:
		return structuralLabel("evaluator", label)
	case KindRuntime:
		return structuralLabel("runtime", label)
	case KindEnvironment:
		return structuralLabel("environment", label)
	default:
		return contribution.NodeID
	}
}

// structuralAgentLabel drops the occurrence suffix from the synthetic agent
// that owns unattributed spans ("unknown/stage:verify#2"), whose label is
// otherwise derived from a per-attempt stage node ID.
func structuralAgentLabel(label string) string {
	if !strings.HasPrefix(label, unknownAgentPrefix) {
		return label
	}
	if hash := strings.LastIndex(label, "#"); hash > len(unknownAgentPrefix) {
		if _, err := strconv.Atoi(label[hash+1:]); err == nil {
			return label[:hash]
		}
	}
	return label
}

const unknownAgentPrefix = "unknown/stage:"

// runPathTotal consolidates one run's contributions that share a structural
// path. Share and score are additive responsibility mass; confidence is the
// share-weighted mean, falling back to a plain mean when every share is zero.
type runPathTotal struct {
	share, score       float64
	weightedConfidence float64
	confidenceSum      float64
	count              int
}

func (t *runPathTotal) add(contribution Contribution) {
	t.share += contribution.Share
	t.score += contribution.Score
	t.weightedConfidence += contribution.Share * contribution.Confidence
	t.confidenceSum += contribution.Confidence
	t.count++
}

func (t *runPathTotal) confidence() float64 {
	if t.share > 0 {
		return t.weightedConfidence / t.share
	}
	if t.count == 0 {
		return 0
	}
	return t.confidenceSum / float64(t.count)
}

func structuralLabel(kind, label string) string {
	identity := kind
	if label != "" {
		identity += ":" + label
	}
	return identity
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

func formatFloat(value float64) string {
	return fmt.Sprintf("%.6f", round(value))
}
