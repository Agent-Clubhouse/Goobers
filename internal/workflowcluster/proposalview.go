package workflowcluster

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/runsemantics"
)

// ChildProposalInsight is the privacy-safe read model for one retained,
// agent-authored child workflow. It intentionally contains no proposal text.
type ChildProposalInsight struct {
	ID               string           `json:"id"`
	Gaggle           string           `json:"gaggle"`
	SourceDigest     string           `json:"sourceDigest"`
	ParentRunID      string           `json:"parentRunId"`
	StageOccurrence  string           `json:"stageOccurrence"`
	EffectiveVersion string           `json:"effectiveVersion"`
	Outcome          journal.RunPhase `json:"outcome"`
	ClusterID        string           `json:"clusterId,omitempty"`
	// StableClusterMedoid marks the medoid of a stable cluster. It is a
	// structural observation only, not a reusable-template candidacy claim;
	// candidacy requires the joined evidence bar in ProposeTemplates.
	StableClusterMedoid bool                  `json:"stableClusterMedoid"`
	Novelty             *journal.ChildNovelty `json:"novelty,omitempty"`
}

// ChildProposalGaggle is one independently fitted, per-gaggle proposal map.
type ChildProposalGaggle struct {
	Gaggle         string                 `json:"gaggle"`
	Status         string                 `json:"status"`
	Population     int                    `json:"population"`
	MinClusterSize int                    `json:"minClusterSize"`
	Model          *Model                 `json:"model,omitempty"`
	Map            []MapPoint             `json:"map"`
	Proposals      []ChildProposalInsight `json:"proposals"`
}

// ChildProposalView is shared by the Labs HTTP read and CLI JSON output.
type ChildProposalView struct {
	Candidates int                   `json:"candidates"`
	OverLimit  int                   `json:"overLimit"`
	Excluded   int                   `json:"excluded"`
	Gaggles    []ChildProposalGaggle `json:"gaggles"`
}

type childProposalObservation struct {
	insight ChildProposalInsight
	point   Point
}

// ScanChildProposalInsights reconstructs retained child proposal shapes and
// partitions them before fitting so no cluster crosses a gaggle boundary.
func ScanChildProposalInsights(ctx context.Context, root string, maxRuns int) (ChildProposalView, error) {
	if maxRuns <= 0 {
		maxRuns = DefaultTrendMaxRuns
	}
	candidates, err := listRunCandidates(ctx, root)
	if err != nil {
		return ChildProposalView{}, fmt.Errorf("list child proposal journals: %w", err)
	}
	view := ChildProposalView{Candidates: len(candidates), Gaggles: []ChildProposalGaggle{}}
	observations := make([]childProposalObservation, 0)
	limits := BehaviorScanOptions{}.withDefaults()
	for i, candidate := range candidates {
		if err := ctx.Err(); err != nil {
			return ChildProposalView{}, err
		}
		if i >= maxRuns {
			view.OverLimit = len(candidates) - i
			break
		}
		observation, child, err := readChildProposalObservation(candidate.dir, limits)
		if !child {
			continue
		}
		if err != nil {
			view.Excluded++
			continue
		}
		observations = append(observations, observation)
	}
	view.Gaggles, err = analyzeChildProposalObservations(observations, cliMinClusterSize)
	if err != nil {
		return ChildProposalView{}, err
	}
	return view, nil
}

func readChildProposalObservation(dir string, limits BehaviorScanOptions) (childProposalObservation, bool, error) {
	in, err := readBehaviorInput(dir, limits)
	if err != nil {
		return childProposalObservation{}, false, err
	}
	if in.Identity.Child == nil {
		return childProposalObservation{}, false, nil
	}
	descriptor, err := ExtractBehavior(in)
	if err != nil {
		return childProposalObservation{}, true, err
	}
	reader, err := journal.OpenReadOnly(dir)
	if err != nil {
		return childProposalObservation{}, true, err
	}
	machine, err := runsemantics.PinnedWorkflowMachine(reader, in.Identity)
	if err != nil {
		return childProposalObservation{}, true, err
	}
	features, err := ExtractDefinition(machine.Def)
	if err != nil {
		return childProposalObservation{}, true, err
	}
	lineage := in.Identity.Child
	insight := ChildProposalInsight{
		ID: in.Identity.RunID, Gaggle: in.Identity.Gaggle, SourceDigest: lineage.SourceDigest,
		ParentRunID: lineage.ParentRunID, StageOccurrence: lineage.StageOccurrence,
		EffectiveVersion: descriptor.EffectiveVersion, Outcome: descriptor.Outcome,
	}
	if lineage.Novelty != nil {
		novelty := *lineage.Novelty
		insight.Novelty = &novelty
	}
	return childProposalObservation{insight: insight, point: StructuralPoint(insight.ID, features)}, true, nil
}

func analyzeChildProposalObservations(observations []childProposalObservation, minClusterSize int) ([]ChildProposalGaggle, error) {
	if minClusterSize < 2 {
		return nil, errors.New("workflowcluster: child proposal minimum cluster size must be at least 2")
	}
	grouped := map[string][]childProposalObservation{}
	for _, observation := range observations {
		if observation.insight.ID == "" || observation.insight.Gaggle == "" || observation.insight.ParentRunID == "" {
			return nil, errors.New("workflowcluster: child proposal insight requires run, gaggle, and parent")
		}
		grouped[observation.insight.Gaggle] = append(grouped[observation.insight.Gaggle], observation)
	}
	gaggles := make([]ChildProposalGaggle, 0, len(grouped))
	for _, gaggle := range sortedKeys(grouped) {
		members := grouped[gaggle]
		sort.Slice(members, func(i, j int) bool { return members[i].insight.ID < members[j].insight.ID })
		group := ChildProposalGaggle{
			Gaggle: gaggle, Status: ViewInsufficientData, Population: len(members),
			MinClusterSize: minClusterSize, Map: []MapPoint{}, Proposals: make([]ChildProposalInsight, len(members)),
		}
		points := make([]Point, len(members))
		for i, member := range members {
			points[i] = member.point
			group.Proposals[i] = member.insight
		}
		if len(points) >= minClusterSize {
			model, err := Fit(StructuralFeatureVersion, points, Options{MinClusterSize: minClusterSize})
			if err != nil {
				return nil, err
			}
			group.Status, group.Model, group.Map = ViewClustered, &model, Project(model, points)
			clusters := map[string]Cluster{}
			for _, cluster := range model.Clusters {
				clusters[cluster.ID] = cluster
			}
			for i := range group.Proposals {
				assignment, err := model.Assign(StructuralFeatureVersion, points[i])
				if err != nil {
					return nil, err
				}
				group.Proposals[i].ClusterID = assignment.ClusterID
				cluster, found := clusters[assignment.ClusterID]
				group.Proposals[i].StableClusterMedoid = found && cluster.Stable && cluster.Medoid.ID == points[i].ID
			}
		}
		gaggles = append(gaggles, group)
	}
	return gaggles, nil
}
