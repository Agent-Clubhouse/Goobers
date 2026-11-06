package workflowcluster

import (
	"fmt"
	"math"
	"sort"
	"strings"
)

const dominantAblationChange = 0.20

// AblationReport measures how clustering changes when each feature or feature
// group is removed.
type AblationReport struct {
	BaselineStability float64         `json:"baselineStability"`
	DominanceCutoff   float64         `json:"dominanceCutoff"`
	Features          []FeatureImpact `json:"features"`
}

// FeatureImpact is one leave-one-feature-out fit. WL histogram dimensions are
// treated as one feature group.
type FeatureImpact struct {
	Feature          string  `json:"feature"`
	ClusterStability float64 `json:"clusterStability"`
	StabilityChange  float64 `json:"stabilityChange"`
	AssignmentChange float64 `json:"assignmentChange"`
	Dominant         bool    `json:"dominant"`
}

// Ablate refits the population after dropping each feature (or the WL group).
func Ablate(featureVersion string, points []Point, opts Options, baseline Model) (AblationReport, error) {
	report := AblationReport{
		BaselineStability: meanStability(baseline),
		DominanceCutoff:   dominantAblationChange,
	}
	for _, feature := range ablationFeatures(baseline.Schema) {
		reduced := dropFeature(points, feature)
		model, err := Fit(featureVersion, reduced, opts)
		if err != nil {
			return AblationReport{}, fmt.Errorf("ablate %s: %w", feature, err)
		}
		stability := meanStability(model)
		change := assignmentChange(baseline, model)
		delta := math.Abs(stability - report.BaselineStability)
		report.Features = append(report.Features, FeatureImpact{
			Feature: feature, ClusterStability: stability, StabilityChange: delta,
			AssignmentChange: change, Dominant: change >= dominantAblationChange || delta >= dominantAblationChange,
		})
	}
	return report, nil
}

func ablationFeatures(schema Schema) []string {
	features := make([]string, 0, len(schema.Features))
	hasWL := false
	for _, feature := range schema.Features {
		if strings.HasPrefix(feature.Name, wlFeaturePrefix) {
			hasWL = true
			continue
		}
		features = append(features, feature.Name)
	}
	if hasWL {
		features = append(features, "wl")
	}
	sort.Strings(features)
	return features
}

func dropFeature(points []Point, feature string) []Point {
	out := make([]Point, len(points))
	for i, point := range points {
		out[i] = Point{ID: point.ID, Numeric: map[string]float64{}, Categorical: map[string][]string{}}
		for name, value := range point.Numeric {
			if name != feature && !(feature == "wl" && strings.HasPrefix(name, wlFeaturePrefix)) {
				out[i].Numeric[name] = value
			}
		}
		for name, values := range point.Categorical {
			if name != feature {
				out[i].Categorical[name] = append([]string(nil), values...)
			}
		}
	}
	return out
}

func meanStability(model Model) float64 {
	if len(model.Clusters) == 0 {
		return 0
	}
	var total float64
	for _, cluster := range model.Clusters {
		total += cluster.Stability
	}
	return total / float64(len(model.Clusters))
}

// assignmentChange matches each ablated cluster to the baseline cluster with
// greatest member Jaccard, avoiding dependence on generated cluster IDs.
func assignmentChange(baseline, ablated Model) float64 {
	base := memberLabels(baseline)
	other := memberLabels(ablated)
	matches := map[string]string{}
	for _, cluster := range ablated.Clusters {
		bestID, best := "", -1.0
		for _, candidate := range baseline.Clusters {
			score := memberJaccard(cluster.Members, candidate.Members)
			if score > best {
				bestID, best = candidate.ID, score
			}
		}
		matches[cluster.ID] = bestID
	}
	changed := 0
	for id, expected := range base {
		actual := other[id]
		if actual != "" {
			actual = matches[actual]
		}
		if actual != expected {
			changed++
		}
	}
	if len(base) == 0 {
		return 0
	}
	return float64(changed) / float64(len(base))
}

func memberLabels(model Model) map[string]string {
	out := make(map[string]string, model.Population)
	for _, id := range model.Noise {
		out[id] = ""
	}
	for _, cluster := range model.Clusters {
		for _, id := range cluster.Members {
			out[id] = cluster.ID
		}
	}
	return out
}

func memberJaccard(a, b []string) float64 {
	return 1 - jaccardDistance(a, b)
}
