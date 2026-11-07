package workflowcluster

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/goobers/goobers/internal/fleet"
	"github.com/goobers/goobers/internal/journal"
)

// ErrFleetRetentionDisabled is returned when a fleet has not opted in to
// retained snapshots.
var ErrFleetRetentionDisabled = errors.New("workflowcluster: retained fleet snapshots are disabled")

// FleetRetentionPolicy is the explicit retention contract for one fleet.
type FleetRetentionPolicy struct {
	Enabled                 bool
	MaxAge                  time.Duration
	MaxSnapshotsPerInstance int
}

// RetainedSnapshotStore retains the projection needed for fleet drift.
type RetainedSnapshotStore interface {
	Write(context.Context, string, FleetSnapshot) error
	Snapshots(context.Context, string) ([]FleetSnapshot, error)
	PurgeInstance(context.Context, string, string) error
}

// ReconcileRetainedFleet purges every retained snapshot whose source is no
// longer an active member of fleetID that grants labs.workflowFeatures in the
// fleet's authoritative association set: the instance left (its association is
// absent), was revoked, or removed the scope. It returns the purged instance
// IDs in sorted order.
func ReconcileRetainedFleet(ctx context.Context, store RetainedSnapshotStore, fleetID string, associations []fleet.Association) ([]string, error) {
	if store == nil {
		return nil, errors.New("workflowcluster: retained snapshot store is required")
	}
	active := retainableFleetMembers(fleetID, associations)
	snapshots, err := store.Snapshots(ctx, fleetID)
	if err != nil {
		return nil, err
	}
	retained := make(map[string]bool, len(snapshots))
	for _, snapshot := range snapshots {
		retained[snapshot.InstanceID] = true
	}
	purged := []string{}
	for instanceID := range retained {
		if active[instanceID] {
			continue
		}
		if err := store.PurgeInstance(ctx, fleetID, instanceID); err != nil {
			return nil, err
		}
		purged = append(purged, instanceID)
	}
	sort.Strings(purged)
	return purged, nil
}

func retainableFleetMembers(fleetID string, associations []fleet.Association) map[string]bool {
	active := make(map[string]bool, len(associations))
	for _, association := range associations {
		if association.FleetID != fleetID || association.InstanceID == "" {
			continue
		}
		if slices.Contains(association.ActiveProjections(), fleet.ProjectionLabsWorkflowFeatures) {
			active[association.InstanceID] = true
		}
	}
	return active
}

// RetainedFleetCollection is the outcome of one retained collection pass.
type RetainedFleetCollection struct {
	Retained []FleetSnapshot  `json:"retained"`
	Excluded []FleetExclusion `json:"excluded"`
	// Purged lists instances whose retained snapshots were deleted because
	// they left, were revoked, or removed the labs.workflowFeatures scope.
	Purged []string `json:"purged"`
}

// CollectRetainedFleet is the retained counterpart of ClusterFleetOnDemand.
// It refuses to run unless fleetID has opted in to retention, purges every
// instance that is no longer an active scoped member of the authoritative
// association set, fetches only from active scoped members, purges any
// instance whose own scope gate now refuses the projection (it left, was
// revoked, or removed the scope since the association set was read), and
// retains the remaining snapshots under the store's bounds. Offline, timed-out,
// and invalid responses are excluded without purging, so their earlier
// snapshots remain until they age out.
func CollectRetainedFleet(ctx context.Context, store RetainedSnapshotStore, fleetID string, associations []fleet.Association, targets []FleetTarget, fetch SnapshotFetcher, opts FleetCollectionOptions) (RetainedFleetCollection, error) {
	purged, err := ReconcileRetainedFleet(ctx, store, fleetID, associations)
	if err != nil {
		return RetainedFleetCollection{}, err
	}
	active := retainableFleetMembers(fleetID, associations)
	result := RetainedFleetCollection{Retained: []FleetSnapshot{}, Excluded: []FleetExclusion{}}
	scoped := make([]FleetTarget, 0, len(targets))
	for _, target := range targets {
		if active[target.InstanceID] {
			scoped = append(scoped, target)
			continue
		}
		result.Excluded = append(result.Excluded, FleetExclusion{InstanceID: target.InstanceID, Reason: FleetExclusionNotGranted})
	}
	snapshots, excluded, err := CollectFleetSnapshots(ctx, scoped, fetch, opts)
	if err != nil {
		return RetainedFleetCollection{}, err
	}
	purgedSet := make(map[string]bool, len(purged))
	for _, instanceID := range purged {
		purgedSet[instanceID] = true
	}
	for _, exclusion := range excluded {
		result.Excluded = append(result.Excluded, exclusion)
		if exclusion.Reason != FleetExclusionNotGranted || purgedSet[exclusion.InstanceID] {
			continue
		}
		if err := store.PurgeInstance(ctx, fleetID, exclusion.InstanceID); err != nil {
			return RetainedFleetCollection{}, err
		}
		purgedSet[exclusion.InstanceID] = true
	}
	for _, snapshot := range snapshots {
		if err := store.Write(ctx, fleetID, snapshot); err != nil {
			return RetainedFleetCollection{}, err
		}
		result.Retained = append(result.Retained, snapshot)
	}
	for instanceID := range purgedSet {
		result.Purged = append(result.Purged, instanceID)
	}
	sort.Strings(result.Purged)
	if result.Purged == nil {
		result.Purged = []string{}
	}
	sortFleetExclusions(result.Excluded)
	return result, nil
}

// FileRetainedSnapshotStore stores snapshots as private JSON files.
type FileRetainedSnapshotStore struct {
	root     string
	policies map[string]FleetRetentionPolicy
	now      func() time.Time
	mu       sync.Mutex
}

// NewFileRetainedSnapshotStore creates a store with explicit per-fleet
// policies. Enabled fleets must set both retention bounds.
func NewFileRetainedSnapshotStore(root string, policies map[string]FleetRetentionPolicy) (*FileRetainedSnapshotStore, error) {
	if strings.TrimSpace(root) == "" {
		return nil, errors.New("workflowcluster: retained snapshot root is required")
	}
	copied := make(map[string]FleetRetentionPolicy, len(policies))
	for fleetID, policy := range policies {
		if strings.TrimSpace(fleetID) == "" {
			return nil, errors.New("workflowcluster: retained snapshot policy needs a fleet ID")
		}
		if policy.MaxAge < 0 || policy.MaxSnapshotsPerInstance < 0 {
			return nil, fmt.Errorf("workflowcluster: fleet %q retention bounds must not be negative", fleetID)
		}
		if policy.Enabled && (policy.MaxAge == 0 || policy.MaxSnapshotsPerInstance == 0) {
			return nil, fmt.Errorf("workflowcluster: fleet %q retention requires max age and max snapshots per instance", fleetID)
		}
		copied[fleetID] = policy
	}
	return &FileRetainedSnapshotStore{root: root, policies: copied, now: time.Now}, nil
}

// Write validates and retains a snapshot, then enforces both policy bounds.
func (s *FileRetainedSnapshotStore) Write(ctx context.Context, fleetID string, snapshot FleetSnapshot) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	policy, ok := s.policies[fleetID]
	if !ok || !policy.Enabled {
		return fmt.Errorf("%w for fleet %q", ErrFleetRetentionDisabled, fleetID)
	}
	if snapshot.GeneratedAt.IsZero() {
		return errors.New("workflowcluster: retained snapshot generation time is required")
	}
	if err := validateFleetSnapshot(snapshot); err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	dir := s.instanceDir(fleetID, snapshot.InstanceID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("workflowcluster: create retained snapshot directory: %w", err)
	}
	data, err := json.Marshal(snapshot)
	if err != nil {
		return fmt.Errorf("workflowcluster: encode retained snapshot: %w", err)
	}
	path := filepath.Join(dir, snapshot.GeneratedAt.UTC().Format("20060102T150405.000000000Z")+".json")
	if err := journal.WriteFileAtomic(path, data, 0o600); err != nil {
		return fmt.Errorf("workflowcluster: write retained snapshot: %w", err)
	}
	if err := s.pruneInstance(ctx, dir, policy); err != nil {
		return err
	}
	return nil
}

// Snapshots returns all retained snapshots for a fleet in generation order
// after enforcing the policy bounds. A fleet without enabled retention retains
// nothing: any snapshots left from when it was enabled are deleted and
// ErrFleetRetentionDisabled is returned.
func (s *FileRetainedSnapshotStore) Snapshots(ctx context.Context, fleetID string) ([]FleetSnapshot, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	policy, ok := s.policies[fleetID]
	s.mu.Lock()
	defer s.mu.Unlock()
	if !ok || !policy.Enabled {
		if strings.TrimSpace(fleetID) != "" {
			if err := os.RemoveAll(s.fleetDir(fleetID)); err != nil {
				return nil, fmt.Errorf("workflowcluster: delete retained snapshots for disabled fleet %q: %w", fleetID, err)
			}
		}
		return nil, fmt.Errorf("%w for fleet %q", ErrFleetRetentionDisabled, fleetID)
	}

	root := s.fleetDir(fleetID)
	instanceDirs, err := os.ReadDir(root)
	if errors.Is(err, os.ErrNotExist) {
		return []FleetSnapshot{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("workflowcluster: read retained fleet %q: %w", fleetID, err)
	}
	var snapshots []FleetSnapshot
	for _, instanceDir := range instanceDirs {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if !instanceDir.IsDir() {
			continue
		}
		dir := filepath.Join(root, instanceDir.Name())
		if err := s.pruneInstance(ctx, dir, policy); err != nil {
			return nil, err
		}
		values, err := readRetainedSnapshots(dir)
		if err != nil {
			return nil, err
		}
		snapshots = append(snapshots, values...)
	}
	sort.Slice(snapshots, func(i, j int) bool {
		if !snapshots[i].GeneratedAt.Equal(snapshots[j].GeneratedAt) {
			return snapshots[i].GeneratedAt.Before(snapshots[j].GeneratedAt)
		}
		return snapshots[i].InstanceID < snapshots[j].InstanceID
	})
	return snapshots, nil
}

// PurgeInstance removes every retained snapshot when an instance leaves, is
// revoked, or removes the workflow-features projection.
func (s *FileRetainedSnapshotStore) PurgeInstance(ctx context.Context, fleetID, instanceID string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if strings.TrimSpace(fleetID) == "" || strings.TrimSpace(instanceID) == "" {
		return errors.New("workflowcluster: fleet and instance IDs are required for retained snapshot purge")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.RemoveAll(s.instanceDir(fleetID, instanceID)); err != nil {
		return fmt.Errorf("workflowcluster: purge retained snapshots for %q: %w", instanceID, err)
	}
	return nil
}

func (s *FileRetainedSnapshotStore) pruneInstance(ctx context.Context, dir string, policy FleetRetentionPolicy) error {
	snapshots, err := readRetainedSnapshotFiles(dir)
	if err != nil {
		return err
	}
	cutoff := s.now().UTC().Add(-policy.MaxAge)
	keep := make([]retainedSnapshotFile, 0, len(snapshots))
	for _, snapshot := range snapshots {
		if err := ctx.Err(); err != nil {
			return err
		}
		if snapshot.snapshot.GeneratedAt.Before(cutoff) {
			if err := os.Remove(snapshot.path); err != nil && !errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("workflowcluster: prune expired retained snapshot %q: %w", snapshot.path, err)
			}
			continue
		}
		keep = append(keep, snapshot)
	}
	for len(keep) > policy.MaxSnapshotsPerInstance {
		if err := os.Remove(keep[0].path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("workflowcluster: prune excess retained snapshot %q: %w", keep[0].path, err)
		}
		keep = keep[1:]
	}
	return nil
}

type retainedSnapshotFile struct {
	path     string
	snapshot FleetSnapshot
}

func readRetainedSnapshots(dir string) ([]FleetSnapshot, error) {
	files, err := readRetainedSnapshotFiles(dir)
	if err != nil {
		return nil, err
	}
	snapshots := make([]FleetSnapshot, 0, len(files))
	for _, file := range files {
		snapshots = append(snapshots, file.snapshot)
	}
	return snapshots, nil
}

func readRetainedSnapshotFiles(dir string) ([]retainedSnapshotFile, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return []retainedSnapshotFile{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("workflowcluster: read retained snapshot directory: %w", err)
	}
	files := make([]retainedSnapshotFile, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		info, err := entry.Info()
		if err != nil {
			return nil, fmt.Errorf("workflowcluster: inspect retained snapshot %q: %w", path, err)
		}
		if info.Size() > maxFleetSnapshotBytes {
			return nil, fmt.Errorf("workflowcluster: retained snapshot %q exceeds %d bytes", path, maxFleetSnapshotBytes)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("workflowcluster: read retained snapshot %q: %w", path, err)
		}
		var snapshot FleetSnapshot
		if err := json.Unmarshal(data, &snapshot); err != nil {
			return nil, fmt.Errorf("workflowcluster: decode retained snapshot %q: %w", path, err)
		}
		if err := validateFleetSnapshot(snapshot); err != nil {
			return nil, fmt.Errorf("workflowcluster: invalid retained snapshot %q: %w", path, err)
		}
		if snapshot.GeneratedAt.IsZero() {
			return nil, fmt.Errorf("workflowcluster: retained snapshot %q has no generation time", path)
		}
		files = append(files, retainedSnapshotFile{path: path, snapshot: snapshot})
	}
	sort.Slice(files, func(i, j int) bool {
		return files[i].snapshot.GeneratedAt.Before(files[j].snapshot.GeneratedAt)
	})
	return files, nil
}

func (s *FileRetainedSnapshotStore) fleetDir(fleetID string) string {
	return filepath.Join(s.root, retainedPathKey(fleetID))
}

func (s *FileRetainedSnapshotStore) instanceDir(fleetID, instanceID string) string {
	return filepath.Join(s.fleetDir(fleetID), retainedPathKey(instanceID))
}

func retainedPathKey(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}
