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
// treated as one feature group, and set features are labeled "name[set]".
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
	for _, target := range ablationFeatures(baseline.Schema) {
		reduced := dropFeature(points, target)
		model, err := Fit(featureVersion, reduced, opts)
		if err != nil {
			return AblationReport{}, fmt.Errorf("ablate %s: %w", target.label, err)
		}
		stability := meanStability(model)
		change := assignmentChange(baseline, model)
		delta := math.Abs(stability - report.BaselineStability)
		report.Features = append(report.Features, FeatureImpact{
			Feature: target.label, ClusterStability: stability, StabilityChange: delta,
			AssignmentChange: change, Dominant: change >= dominantAblationChange || delta >= dominantAblationChange,
		})
	}
	return report, nil
}

// ablationTarget is one leave-out unit. A name can appear as both a numeric
// and a set feature (for example "capabilities"), so targets are keyed by kind.
type ablationTarget struct {
	label string
	kind  string
	name  string
}

const wlAblationLabel = "wl"

func ablationFeatures(schema Schema) []ablationTarget {
	targets := make([]ablationTarget, 0, len(schema.Features))
	seen := map[string]bool{}
	for _, feature := range schema.Features {
		target := ablationTarget{label: feature.Name, kind: feature.Kind, name: feature.Name}
		switch {
		case feature.Kind == KindNumeric && strings.HasPrefix(feature.Name, wlFeaturePrefix):
			target = ablationTarget{label: wlAblationLabel, kind: KindNumeric, name: wlAblationLabel}
		case feature.Kind == KindSet:
			target.label = feature.Name + "[set]"
		}
		if seen[target.label] {
			continue
		}
		seen[target.label] = true
		targets = append(targets, target)
	}
	sort.Slice(targets, func(i, j int) bool { return targets[i].label < targets[j].label })
	return targets
}

func dropFeature(points []Point, target ablationTarget) []Point {
	out := make([]Point, len(points))
	for i, point := range points {
		out[i] = Point{ID: point.ID, Numeric: map[string]float64{}, Categorical: map[string][]string{}}
		for name, value := range point.Numeric {
			if target.kind != KindNumeric || !target.matchesNumeric(name) {
				out[i].Numeric[name] = value
			}
		}
		for name, values := range point.Categorical {
			if target.kind != KindSet || name != target.name {
				out[i].Categorical[name] = append([]string(nil), values...)
			}
		}
	}
	return out
}

func (t ablationTarget) matchesNumeric(name string) bool {
	if t.name == wlAblationLabel {
		return strings.HasPrefix(name, wlFeaturePrefix)
	}
	return name == t.name
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

// assignmentChange uses maximum-overlap one-to-one cluster matching, avoiding
// dependence on generated cluster IDs while still detecting splits and merges.
func assignmentChange(baseline, ablated Model) float64 {
	otherCluster := make(map[string]int, ablated.Population)
	for i, cluster := range ablated.Clusters {
		for _, id := range cluster.Members {
			otherCluster[id] = i
		}
	}
	overlap := make([][]int, len(baseline.Clusters))
	total := len(baseline.Noise)
	for i, cluster := range baseline.Clusters {
		overlap[i] = make([]int, len(ablated.Clusters))
		total += len(cluster.Members)
		for _, id := range cluster.Members {
			if j, ok := otherCluster[id]; ok {
				overlap[i][j]++
			}
		}
	}
	otherNoise := make(map[string]bool, len(ablated.Noise))
	for _, id := range ablated.Noise {
		otherNoise[id] = true
	}
	unchanged := maximumOverlap(overlap)
	for _, id := range baseline.Noise {
		if otherNoise[id] {
			unchanged++
		}
	}
	if total == 0 {
		return 0
	}
	return float64(total-unchanged) / float64(total)
}

func maximumOverlap(weights [][]int) int {
	rows := len(weights)
	cols := 0
	if rows > 0 {
		cols = len(weights[0])
	}
	n := max(rows, cols)
	if n == 0 {
		return 0
	}

	u, v := make([]int, n+1), make([]int, n+1)
	matchedRow, previousColumn := make([]int, n+1), make([]int, n+1)
	for row := 1; row <= n; row++ {
		matchedRow[0] = row
		minCost := make([]int, n+1)
		used := make([]bool, n+1)
		for column := 1; column <= n; column++ {
			minCost[column] = int(^uint(0) >> 1)
		}
		column := 0
		for {
			used[column] = true
			currentRow := matchedRow[column]
			delta, nextColumn := int(^uint(0)>>1), 0
			for candidate := 1; candidate <= n; candidate++ {
				if used[candidate] {
					continue
				}
				weight := 0
				if currentRow <= rows && candidate <= cols {
					weight = weights[currentRow-1][candidate-1]
				}
				cost := -weight - u[currentRow] - v[candidate]
				if cost < minCost[candidate] {
					minCost[candidate], previousColumn[candidate] = cost, column
				}
				if minCost[candidate] < delta {
					delta, nextColumn = minCost[candidate], candidate
				}
			}
			for candidate := 0; candidate <= n; candidate++ {
				if used[candidate] {
					u[matchedRow[candidate]] += delta
					v[candidate] -= delta
				} else {
					minCost[candidate] -= delta
				}
			}
			column = nextColumn
			if matchedRow[column] == 0 {
				break
			}
		}
		for {
			nextColumn := previousColumn[column]
			matchedRow[column] = matchedRow[nextColumn]
			column = nextColumn
			if column == 0 {
				break
			}
		}
	}

	total := 0
	for column := 1; column <= cols; column++ {
		if row := matchedRow[column]; row > 0 && row <= rows {
			total += weights[row-1][column-1]
		}
	}
	return total
}
