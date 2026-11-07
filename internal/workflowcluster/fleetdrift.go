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
type FleetDriftReport struct {
	FleetID string             `json:"fleetId"`
	Window  time.Duration      `json:"window"`
	Windows []FleetDriftWindow `json:"windows"`
}

// FleetDrift clusters each retained window and compares cluster occupancy and
// Shannon diversity with the previous window.
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
	for start := first; !start.After(last); start = start.Add(window) {
		if err := ctx.Err(); err != nil {
			return FleetDriftReport{}, err
		}
		view, err := ClusterFleet(grouped[start], opts)
		if err != nil {
			return FleetDriftReport{}, err
		}
		current := FleetDriftWindow{Start: start, End: start.Add(window), View: view}
		current.Occupancy, current.Diversity = fleetWindowOccupancy(view)
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

func fleetWindowOccupancy(view FleetView) ([]FleetDriftOccupancy, float64) {
	counts := make(map[string]int, len(view.Clusters)+1)
	clustered := 0
	for _, cluster := range view.Clusters {
		counts[fleetDriftClusterID(view, cluster.ID)] += cluster.Workflows
		clustered += cluster.Workflows
	}
	if noise := view.Workflows - clustered; noise > 0 {
		counts[trendNoiseCluster] = noise
	}
	occupancy := make([]FleetDriftOccupancy, 0, len(counts))
	diversity := 0.0
	for cluster, workflows := range counts {
		share := float64(workflows) / float64(view.Workflows)
		occupancy = append(occupancy, FleetDriftOccupancy{Cluster: cluster, Workflows: workflows, Share: share})
		diversity -= share * math.Log2(share)
	}
	sort.Slice(occupancy, func(i, j int) bool { return occupancy[i].Cluster < occupancy[j].Cluster })
	return occupancy, diversity
}

func fleetDriftClusterID(view FleetView, clusterID string) string {
	parts := []string{"drift-cluster"}
	for _, shape := range view.Shapes {
		if shape.Cluster == clusterID {
			parts = append(parts, shape.Shape)
		}
	}
	sort.Strings(parts[1:])
	return fleetHash(parts...)
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
