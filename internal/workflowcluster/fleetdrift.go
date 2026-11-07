package workflowcluster

import (
	"context"
	"errors"
	"fmt"
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

// maxFleetDriftWindows bounds how many populated windows one drift report
// clusters, which bounds its memory and work regardless of the window size.
const maxFleetDriftWindows = 1024

// FleetDriftChange compares a window with the most recent earlier window that
// has comparable occupancy. From is that earlier window's start.
type FleetDriftChange struct {
	From              time.Time `json:"from"`
	OccupancyDistance float64   `json:"occupancyDistance"`
	DiversityDelta    float64   `json:"diversityDelta"`
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

// FleetDrift clusters each populated retained window and compares cluster
// occupancy and Shannon diversity with the most recent earlier window that has
// comparable occupancy. Windows without retained snapshots are omitted, and a
// window without comparable occupancy (no workflows on the reference feature
// version) reports no Change, so missed collections and feature-version
// changes do not show up as drift. Each window's View is its own independent
// convergence; Occupancy and Change use the shared reference model's stable
// cluster IDs. More than maxFleetDriftWindows populated windows is an error.
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
	for _, snapshot := range snapshots {
		start := snapshot.GeneratedAt.UTC().Truncate(window)
		grouped[start] = append(grouped[start], snapshot)
	}
	if len(grouped) > maxFleetDriftWindows {
		return FleetDriftReport{}, fmt.Errorf("workflowcluster: fleet drift spans %d windows of %s; use a window of at least %s", len(grouped), window, minFleetDriftWindow(grouped))
	}
	starts := make([]time.Time, 0, len(grouped))
	for start := range grouped {
		starts = append(starts, start)
	}
	sort.Slice(starts, func(i, j int) bool { return starts[i].Before(starts[j]) })
	windows := make([]fleetDriftWindowInput, 0, len(starts))
	versions := map[string]int{}
	for _, start := range starts {
		if err := ctx.Err(); err != nil {
			return FleetDriftReport{}, err
		}
		input, err := clusterFleetDriftWindow(start, grouped[start], opts)
		if err != nil {
			return FleetDriftReport{}, err
		}
		if input.view.FeatureVersion != "" {
			versions[input.view.FeatureVersion]++
		}
		windows = append(windows, input)
	}
	reference, err := fitFleetDriftReference(windows, mostCommonFeatureVersion(versions), opts)
	if err != nil {
		return FleetDriftReport{}, err
	}
	report.FeatureVersion, report.ModelID = reference.featureVersion, reference.modelID
	var previous *FleetDriftWindow
	for _, input := range windows {
		current := FleetDriftWindow{Start: input.start, End: input.start.Add(window), View: input.view}
		current.Occupancy, current.Diversity = reference.occupancy(input)
		comparable := len(current.Occupancy) > 0
		if comparable && previous != nil {
			current.Change = &FleetDriftChange{
				From:              previous.Start,
				OccupancyDistance: occupancyDistance(previous.Occupancy, current.Occupancy),
				DiversityDelta:    current.Diversity - previous.Diversity,
			}
		}
		report.Windows = append(report.Windows, current)
		if comparable {
			previous = &report.Windows[len(report.Windows)-1]
		}
	}
	return report, nil
}

func clusterFleetDriftWindow(start time.Time, snapshots []FleetSnapshot, opts Options) (fleetDriftWindowInput, error) {
	view, err := ClusterFleet(snapshots, opts)
	if err != nil {
		return fleetDriftWindowInput{}, err
	}
	latest, _, err := latestFleetSnapshots(snapshots)
	if err != nil {
		return fleetDriftWindowInput{}, err
	}
	var workflows []FleetWorkflow
	for _, snapshot := range latest {
		if snapshot.FeatureVersion == view.FeatureVersion {
			workflows = append(workflows, snapshot.Workflows...)
		}
	}
	return fleetDriftWindowInput{start: start, view: view, workflows: workflows}, nil
}

// minFleetDriftWindow suggests a window size that would fit the retained span
// into maxFleetDriftWindows windows.
func minFleetDriftWindow(grouped map[time.Time][]FleetSnapshot) time.Duration {
	var first, last time.Time
	for start := range grouped {
		if first.IsZero() || start.Before(first) {
			first = start
		}
		if start.After(last) {
			last = start
		}
	}
	return (last.Sub(first)/maxFleetDriftWindows + time.Second).Truncate(time.Second)
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
