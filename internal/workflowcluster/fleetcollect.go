package workflowcluster

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"
)

const (
	// FleetExclusionOffline means the instance could not be reached.
	FleetExclusionOffline = "offline"
	// FleetExclusionNotGranted means the instance denied the projection.
	FleetExclusionNotGranted = "not_granted"
	// FleetExclusionTimeout means the instance did not respond within its budget.
	FleetExclusionTimeout = "timeout"
	// FleetExclusionInvalidSnapshot means the instance returned an invalid snapshot.
	FleetExclusionInvalidSnapshot = "invalid_snapshot"
	// FleetExclusionFetchFailed means the fetch failed for an unclassified
	// reason, such as a server error from a reachable instance.
	FleetExclusionFetchFailed = "fetch_failed"
)

var (
	// ErrFleetTargetOffline is returned by a SnapshotFetcher when its target
	// cannot be reached.
	ErrFleetTargetOffline = errors.New("workflowcluster: fleet target is offline")
	// ErrFleetInvalidSnapshot is returned by a SnapshotFetcher when its target
	// responds with a body that is not a usable snapshot.
	ErrFleetInvalidSnapshot = errors.New("workflowcluster: fleet target returned an invalid snapshot")
)

// FleetTarget identifies an enrolled instance to query.
type FleetTarget struct {
	InstanceID string
	Endpoint   string
}

// SnapshotFetcher fetches one in-memory snapshot from an enrolled instance.
// Implementations map an unreachable instance to ErrFleetTargetOffline, a
// projection_not_granted response to ErrFleetProjectionNotGranted, and an
// undecodable or malformed body to ErrFleetInvalidSnapshot. Context errors
// become timeout exclusions and any other error becomes a fetch_failed
// exclusion; no fetcher error fails the whole collection.
type SnapshotFetcher func(context.Context, FleetTarget) (FleetSnapshot, error)

// FleetCollectionOptions bounds an on-demand collection.
type FleetCollectionOptions struct {
	MaxConcurrency     int
	PerInstanceTimeout time.Duration
	OverallTimeout     time.Duration
}

func (o FleetCollectionOptions) withDefaults() FleetCollectionOptions {
	if o.MaxConcurrency <= 0 {
		o.MaxConcurrency = 4
	}
	if o.PerInstanceTimeout <= 0 {
		o.PerInstanceTimeout = 10 * time.Second
	}
	if o.OverallTimeout <= 0 {
		o.OverallTimeout = 30 * time.Second
	}
	return o
}

type fleetCollectionResult struct {
	snapshot  FleetSnapshot
	exclusion FleetExclusion
}

// CollectFleetSnapshots fetches snapshots with bounded concurrency. Transport
// outcomes, including unclassified fetcher errors, are returned as
// per-instance exclusions; only invalid inputs and caller cancellation fail
// the collection itself.
func CollectFleetSnapshots(ctx context.Context, targets []FleetTarget, fetch SnapshotFetcher, opts FleetCollectionOptions) ([]FleetSnapshot, []FleetExclusion, error) {
	if fetch == nil {
		return nil, nil, errors.New("workflowcluster: snapshot fetcher is required")
	}
	seen := make(map[string]bool, len(targets))
	for _, target := range targets {
		if target.InstanceID == "" {
			return nil, nil, errors.New("workflowcluster: fleet target instance ID is required")
		}
		if seen[target.InstanceID] {
			return nil, nil, fmt.Errorf("workflowcluster: duplicate fleet target %q", target.InstanceID)
		}
		seen[target.InstanceID] = true
	}

	opts = opts.withDefaults()
	collectionCtx, cancel := context.WithTimeout(ctx, opts.OverallTimeout)
	defer cancel()

	jobs := make(chan FleetTarget, len(targets))
	results := make(chan fleetCollectionResult, len(targets))
	for _, target := range targets {
		jobs <- target
	}
	close(jobs)

	workers := min(opts.MaxConcurrency, len(targets))
	var group sync.WaitGroup
	group.Add(workers)
	for range workers {
		go func() {
			defer group.Done()
			for target := range jobs {
				if collectionCtx.Err() != nil {
					// Queued targets are not fetched once the collection is
					// cancelled or past its deadline; they time out unqueried.
					results <- fleetCollectionResult{exclusion: FleetExclusion{InstanceID: target.InstanceID, Reason: FleetExclusionTimeout}}
					continue
				}
				results <- fetchFleetSnapshot(collectionCtx, target, fetch, opts.PerInstanceTimeout)
			}
		}()
	}
	group.Wait()
	close(results)

	snapshots := make([]FleetSnapshot, 0, len(targets))
	exclusions := make([]FleetExclusion, 0, len(targets))
	for result := range results {
		if result.exclusion.InstanceID != "" {
			exclusions = append(exclusions, result.exclusion)
			continue
		}
		snapshots = append(snapshots, result.snapshot)
	}
	sort.Slice(snapshots, func(i, j int) bool { return snapshots[i].InstanceID < snapshots[j].InstanceID })
	sortFleetExclusions(exclusions)
	if err := ctx.Err(); err != nil {
		return snapshots, exclusions, err
	}
	return snapshots, exclusions, nil
}

func fetchFleetSnapshot(ctx context.Context, target FleetTarget, fetch SnapshotFetcher, timeout time.Duration) fleetCollectionResult {
	fetchCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	snapshot, err := fetch(fetchCtx, target)
	if err != nil {
		return fleetCollectionResult{exclusion: FleetExclusion{InstanceID: target.InstanceID, Reason: fleetFetchErrorReason(err)}}
	}
	if snapshot.InstanceID != target.InstanceID {
		return fleetCollectionResult{exclusion: FleetExclusion{InstanceID: target.InstanceID, Reason: FleetExclusionInvalidSnapshot}}
	}
	if err := validateFleetSnapshot(snapshot); err != nil {
		return fleetCollectionResult{exclusion: FleetExclusion{InstanceID: target.InstanceID, Reason: FleetExclusionInvalidSnapshot}}
	}
	return fleetCollectionResult{snapshot: snapshot}
}

func fleetFetchErrorReason(err error) string {
	switch {
	case errors.Is(err, ErrFleetProjectionNotGranted):
		return FleetExclusionNotGranted
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		return FleetExclusionTimeout
	case errors.Is(err, ErrFleetTargetOffline):
		return FleetExclusionOffline
	case errors.Is(err, ErrFleetInvalidSnapshot):
		return FleetExclusionInvalidSnapshot
	default:
		return FleetExclusionFetchFailed
	}
}

// ClusterFleetOnDemand collects ephemeral snapshots and clusters the valid
// responses without writing them to disk.
func ClusterFleetOnDemand(ctx context.Context, targets []FleetTarget, fetch SnapshotFetcher, collectionOpts FleetCollectionOptions, clusterOpts Options) (FleetView, error) {
	snapshots, excluded, err := CollectFleetSnapshots(ctx, targets, fetch, collectionOpts)
	if err != nil {
		return FleetView{}, err
	}
	view, err := ClusterFleet(snapshots, clusterOpts)
	if err != nil {
		return FleetView{}, err
	}
	view.Excluded = append(view.Excluded, excluded...)
	sortFleetExclusions(view.Excluded)
	return view, nil
}

func sortFleetExclusions(exclusions []FleetExclusion) {
	sort.Slice(exclusions, func(i, j int) bool {
		if exclusions[i].InstanceID != exclusions[j].InstanceID {
			return exclusions[i].InstanceID < exclusions[j].InstanceID
		}
		return exclusions[i].Reason < exclusions[j].Reason
	})
}
