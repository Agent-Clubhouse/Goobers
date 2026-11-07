package workflowcluster

import (
	"context"
	"errors"
	"math"
	"sort"
	"time"
)

// FleetDriftOccupancy is one cluster's share of workflows in a window.
type FleetDriftOccupancy struct {
	Cluster   string  `json:"cluster"`
	Workflows int     `json:"workflows"`
	Share     float64 `json:"share"`
}

// FleetDriftChange compares a window with its chronological predecessor.
type FleetDriftChange struct {
	OccupancyDistance float64 `json:"occupancyDistance"`
	DiversityDelta    float64 `json:"diversityDelta"`
}

// FleetDriftWindow is one window's fleet convergence and change.
type FleetDriftWindow struct {
	Start     time.Time             `json:"start"`
	End       time.Time             `json:"end"`
	View      FleetView             `json:"view"`
	Occupancy []FleetDriftOccupancy `json:"occupancy"`
	Diversity float64               `json:"diversity"`
	Change    *FleetDriftChange     `json:"change,omitempty"`
}

// FleetDriftReport summarizes retained convergence over fixed UTC windows.
// Occupancy in every window is measured against one reference model fitted to
// the distinct shapes of all windows, so a cluster keeps the same ID across
// windows even when an independent per-window refit would pick a different
// medoid, renumber clusters, or see a slightly different member set.
type FleetDriftReport struct {
	FleetID        string             `json:"fleetId"`
	Window         time.Duration      `json:"window"`
	FeatureVersion string             `json:"featureVersion,omitempty"`
	ModelID        string             `json:"modelId,omitempty"`
	Windows        []FleetDriftWindow `json:"windows"`
}

// FleetDrift clusters each retained window and compares cluster occupancy and
// Shannon diversity with the previous window. Each window's View is its own
// independent convergence; Occupancy and Change use the shared reference
// model's stable cluster IDs.
func FleetDrift(ctx context.Context, store RetainedSnapshotStore, fleetID string, window time.Duration, opts Options) (FleetDriftReport, error) {
	if store == nil {
		return FleetDriftReport{}, errors.New("workflowcluster: retained snapshot store is required")
	}
	if fleetID == "" {
		return FleetDriftReport{}, errors.New("workflowcluster: fleet ID is required")
	}
	if window <= 0 {
		return FleetDriftReport{}, errors.New("workflowcluster: fleet drift window must be positive")
	}
	snapshots, err := store.Snapshots(ctx, fleetID)
	if err != nil {
		return FleetDriftReport{}, err
	}
	report := FleetDriftReport{FleetID: fleetID, Window: window, Windows: []FleetDriftWindow{}}
	if len(snapshots) == 0 {
		return report, nil
	}
	grouped := map[time.Time][]FleetSnapshot{}
	var first, last time.Time
	for _, snapshot := range snapshots {
		start := snapshot.GeneratedAt.UTC().Truncate(window)
		grouped[start] = append(grouped[start], snapshot)
		if first.IsZero() || start.Before(first) {
			first = start
		}
		if last.IsZero() || start.After(last) {
			last = start
		}
	}
	var windows []fleetDriftWindowInput
	versions := map[string]int{}
	for start := first; !start.After(last); start = start.Add(window) {
		if err := ctx.Err(); err != nil {
			return FleetDriftReport{}, err
		}
		view, err := ClusterFleet(grouped[start], opts)
		if err != nil {
			return FleetDriftReport{}, err
		}
		latest, _, err := latestFleetSnapshots(grouped[start])
		if err != nil {
			return FleetDriftReport{}, err
		}
		var workflows []FleetWorkflow
		for _, snapshot := range latest {
			if snapshot.FeatureVersion == view.FeatureVersion {
				workflows = append(workflows, snapshot.Workflows...)
			}
		}
		if view.FeatureVersion != "" {
			versions[view.FeatureVersion]++
		}
		windows = append(windows, fleetDriftWindowInput{start: start, view: view, workflows: workflows})
	}
	reference, err := fitFleetDriftReference(windows, mostCommonFeatureVersion(versions), opts)
	if err != nil {
		return FleetDriftReport{}, err
	}
	report.FeatureVersion, report.ModelID = reference.featureVersion, reference.modelID
	for _, input := range windows {
		current := FleetDriftWindow{Start: input.start, End: input.start.Add(window), View: input.view}
		current.Occupancy, current.Diversity = reference.occupancy(input)
		if len(report.Windows) > 0 {
			previous := report.Windows[len(report.Windows)-1]
			current.Change = &FleetDriftChange{
				OccupancyDistance: occupancyDistance(previous.Occupancy, current.Occupancy),
				DiversityDelta:    current.Diversity - previous.Diversity,
			}
		}
		report.Windows = append(report.Windows, current)
	}
	return report, nil
}

type fleetDriftWindowInput struct {
	start     time.Time
	view      FleetView
	workflows []FleetWorkflow
}

// fleetDriftReference maps every shape seen in any window to one cluster of a
// single model, which gives clusters a stable identity across windows.
type fleetDriftReference struct {
	featureVersion string
	modelID        string
	clusterOf      map[string]string
}

func fitFleetDriftReference(windows []fleetDriftWindowInput, featureVersion string, opts Options) (fleetDriftReference, error) {
	reference := fleetDriftReference{featureVersion: featureVersion, clusterOf: map[string]string{}}
	shapes := map[string]FleetWorkflow{}
	for _, input := range windows {
		if input.view.FeatureVersion != featureVersion {
			continue
		}
		for _, workflow := range input.workflows {
			shapes[workflow.Shape] = workflow
		}
	}
	points := make([]Point, 0, len(shapes))
	for shape, workflow := range shapes {
		points = append(points, Point{ID: shape, Numeric: workflow.Numeric, Categorical: workflow.Categorical})
	}
	sort.Slice(points, func(i, j int) bool { return points[i].ID < points[j].ID })
	if len(points) < opts.withDefaults().MinClusterSize {
		return reference, nil
	}
	model, err := Fit(featureVersion, points, opts)
	if err != nil {
		return fleetDriftReference{}, err
	}
	reference.modelID = model.ID
	for _, cluster := range model.Clusters {
		for _, member := range cluster.Members {
			reference.clusterOf[member] = cluster.ID
		}
	}
	return reference, nil
}

// occupancy counts a window's workflows per reference cluster; shapes outside
// every reference cluster are noise. Windows clustered on a different feature
// version have no comparable occupancy.
func (r fleetDriftReference) occupancy(input fleetDriftWindowInput) ([]FleetDriftOccupancy, float64) {
	occupancy := []FleetDriftOccupancy{}
	if input.view.FeatureVersion != r.featureVersion || len(input.workflows) == 0 {
		return occupancy, 0
	}
	counts := map[string]int{}
	for _, workflow := range input.workflows {
		cluster, ok := r.clusterOf[workflow.Shape]
		if !ok {
			cluster = trendNoiseCluster
		}
		counts[cluster]++
	}
	diversity := 0.0
	for cluster, workflows := range counts {
		share := float64(workflows) / float64(len(input.workflows))
		occupancy = append(occupancy, FleetDriftOccupancy{Cluster: cluster, Workflows: workflows, Share: share})
		diversity -= share * math.Log2(share)
	}
	sort.Slice(occupancy, func(i, j int) bool { return occupancy[i].Cluster < occupancy[j].Cluster })
	return occupancy, diversity
}

func occupancyDistance(before, after []FleetDriftOccupancy) float64 {
	shares := map[string]float64{}
	for _, value := range before {
		shares[value.Cluster] = value.Share
	}
	distance := 0.0
	for _, value := range after {
		distance += math.Abs(shares[value.Cluster] - value.Share)
		delete(shares, value.Cluster)
	}
	for _, share := range shares {
		distance += share
	}
	return distance / 2
}
