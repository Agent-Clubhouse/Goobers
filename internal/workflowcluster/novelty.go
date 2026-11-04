package workflowcluster

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/workflow"
)

// MaxModelBytes bounds a frozen model file read at admission time.
const MaxModelBytes = 8 << 20

// Reasons a proposal is left unscored. Unscored never changes admission.
const (
	UnscoredInvalidModel = "invalid_model"
	UnscoredStaleModel   = "stale_model"
	UnscoredFeatures     = "features_unavailable"
)

// Novelty is advisory evidence about how a workflow's structure compares with
// a frozen cluster model. It never relaxes or tightens an admission gate.
type Novelty struct {
	Scored         bool   `json:"scored"`
	Reason         string `json:"reason,omitempty"`
	ModelID        string `json:"modelId,omitempty"`
	FeatureVersion string `json:"featureVersion,omitempty"`
	// ClusterID is set when the workflow falls within a cluster's radius.
	ClusterID string `json:"clusterId,omitempty"`
	// Nearest describes the closest cluster even when outside its radius.
	Nearest       string  `json:"nearest,omitempty"`
	NearestMedoid string  `json:"nearestMedoid,omitempty"`
	NearestSize   int     `json:"nearestSize,omitempty"`
	NearestStable bool    `json:"nearestStable,omitempty"`
	Distance      float64 `json:"distance"`
	// OutOfDistribution is true when the workflow lies outside every cluster.
	OutOfDistribution bool `json:"outOfDistribution"`
}

// LoadModel reads a frozen model written by `goobers workflow clusters --json`
// and verifies that its content address still matches, so an edited or
// truncated file is rejected rather than silently trusted.
func LoadModel(path string) (Model, error) {
	file, err := os.Open(path)
	if err != nil {
		return Model{}, err
	}
	defer func() { _ = file.Close() }()
	raw, err := io.ReadAll(io.LimitReader(file, MaxModelBytes+1))
	if err != nil {
		return Model{}, err
	}
	if len(raw) > MaxModelBytes {
		return Model{}, fmt.Errorf("workflowcluster: model exceeds %d bytes", MaxModelBytes)
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var model Model
	if err := decoder.Decode(&model); err != nil {
		return Model{}, fmt.Errorf("workflowcluster: decode model: %w", err)
	}
	if model.Version != ModelVersion {
		return Model{}, fmt.Errorf("workflowcluster: model version %q, want %q", model.Version, ModelVersion)
	}
	id, err := modelID(model)
	if err != nil {
		return Model{}, err
	}
	if id != model.ID {
		return Model{}, errors.New("workflowcluster: model content does not match its id")
	}
	return model, nil
}

// ScoreWorkflow assigns wf's structural features to model. A nil model or an
// invalid wf yields an unscored result with a reason instead of an error, so
// callers can degrade without changing their own outcome.
func ScoreWorkflow(model *Model, wf apiv1.Workflow) Novelty {
	if model == nil {
		return Novelty{Reason: UnscoredInvalidModel}
	}
	result := Novelty{ModelID: model.ID, FeatureVersion: model.FeatureVersion}
	if model.FeatureVersion != StructuralFeatureVersion {
		result.Reason = UnscoredStaleModel
		return result
	}
	features, err := ExtractDefinition(definitionOf(wf))
	if err != nil {
		result.Reason = UnscoredFeatures
		return result
	}
	assignment, err := model.Assign(StructuralFeatureVersion, StructuralPoint(wf.Name, features))
	if err != nil {
		result.Reason = UnscoredStaleModel
		return result
	}
	result.Scored = true
	result.ClusterID = assignment.ClusterID
	result.Nearest = assignment.Nearest
	result.Distance = assignment.Distance
	result.OutOfDistribution = assignment.ClusterID == ""
	for _, c := range model.Clusters {
		if c.ID == assignment.Nearest {
			result.NearestMedoid, result.NearestSize, result.NearestStable = c.Medoid.ID, len(c.Members), c.Stable
		}
	}
	return result
}

// Summary renders n as one human-readable advisory line.
func (n Novelty) Summary() string {
	switch {
	case !n.Scored:
		return "unscored (" + n.Reason + ")"
	case n.Nearest == "":
		return "novel shape: the model has no clusters to compare against"
	case n.OutOfDistribution:
		return fmt.Sprintf("novel shape: outside every cluster (nearest %s, medoid %s, distance %.2f)", n.Nearest, n.NearestMedoid, n.Distance)
	default:
		stability := "stable"
		if !n.NearestStable {
			stability = "unstable"
		}
		return fmt.Sprintf("similar to cluster %s (medoid %s, %d members, %s), distance %.2f", n.ClusterID, n.NearestMedoid, n.NearestSize, stability, n.Distance)
	}
}

func definitionOf(wf apiv1.Workflow) workflow.Definition {
	return workflow.Definition{Name: wf.Name, Version: 1, DSLVersion: wf.DSLVersion, Spec: wf.Spec, Annotations: wf.Annotations}
}
