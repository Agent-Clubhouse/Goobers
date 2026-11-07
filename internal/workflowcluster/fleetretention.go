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

// ErrFleetSnapshotFromFuture is returned when a snapshot claims a generation
// time beyond the store clock plus MaxRetainedClockSkew. Generation times come
// from instances, so a future-dated snapshot would otherwise evade age pruning
// and evict genuine snapshots from the per-instance count bound.
var ErrFleetSnapshotFromFuture = errors.New("workflowcluster: retained snapshot is dated in the future")

// MaxRetainedClockSkew is how far ahead of the store clock an instance's
// snapshot generation time may be.
const MaxRetainedClockSkew = 5 * time.Minute

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
	// RetainOnly deletes every instance's retained snapshots except those of
	// the listed instances, without reading snapshot contents, and returns how
	// many instances it purged.
	RetainOnly(context.Context, string, []string) (int, error)
}

// ReconcileRetainedFleet purges every retained snapshot whose source is no
// longer an active member of fleetID that grants labs.workflowFeatures in the
// fleet's authoritative association set: the instance left (its association is
// absent), was revoked, or removed the scope. Purging works from the store's
// instance listing, so unreadable snapshot files cannot block it. It returns
// the number of instances purged.
func ReconcileRetainedFleet(ctx context.Context, store RetainedSnapshotStore, fleetID string, associations []fleet.Association) (int, error) {
	if store == nil {
		return 0, errors.New("workflowcluster: retained snapshot store is required")
	}
	active := retainableFleetMembers(fleetID, associations)
	keep := make([]string, 0, len(active))
	for instanceID := range active {
		keep = append(keep, instanceID)
	}
	sort.Strings(keep)
	return store.RetainOnly(ctx, fleetID, keep)
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
	// Reconciled counts instances purged because the association set no
	// longer lists them as active scoped members.
	Reconciled int `json:"reconciled"`
	// Purged lists instances purged because their own scope gate refused the
	// projection: they left, were revoked, or removed the scope.
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
	reconciled, err := ReconcileRetainedFleet(ctx, store, fleetID, associations)
	if err != nil {
		return RetainedFleetCollection{}, err
	}
	active := retainableFleetMembers(fleetID, associations)
	result := RetainedFleetCollection{Retained: []FleetSnapshot{}, Excluded: []FleetExclusion{}, Reconciled: reconciled, Purged: []string{}}
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
	for _, exclusion := range excluded {
		result.Excluded = append(result.Excluded, exclusion)
		if exclusion.Reason != FleetExclusionNotGranted {
			continue
		}
		if err := store.PurgeInstance(ctx, fleetID, exclusion.InstanceID); err != nil {
			return RetainedFleetCollection{}, err
		}
		result.Purged = append(result.Purged, exclusion.InstanceID)
	}
	for _, snapshot := range snapshots {
		err := store.Write(ctx, fleetID, snapshot)
		if errors.Is(err, ErrFleetSnapshotFromFuture) {
			result.Excluded = append(result.Excluded, FleetExclusion{InstanceID: snapshot.InstanceID, Reason: FleetExclusionInvalidSnapshot})
			continue
		}
		if err != nil {
			return RetainedFleetCollection{}, err
		}
		result.Retained = append(result.Retained, snapshot)
	}
	sort.Strings(result.Purged)
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
	if snapshot.GeneratedAt.After(s.now().Add(MaxRetainedClockSkew)) {
		return fmt.Errorf("%w: %s from %q", ErrFleetSnapshotFromFuture, snapshot.GeneratedAt.UTC().Format(time.RFC3339), snapshot.InstanceID)
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
	_, err = s.pruneInstance(ctx, dir, policy)
	return err
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
		values, err := s.pruneInstance(ctx, filepath.Join(root, instanceDir.Name()), policy)
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

// RetainOnly deletes the snapshot directory of every instance not in keep. It
// matches hashed directory names from the listing and never reads snapshot
// contents, so a corrupt or incompatible file cannot block a lifecycle purge.
// A fleet without enabled retention keeps nothing and returns
// ErrFleetRetentionDisabled after deleting its snapshots.
func (s *FileRetainedSnapshotStore) RetainOnly(ctx context.Context, fleetID string, keep []string) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if strings.TrimSpace(fleetID) == "" {
		return 0, errors.New("workflowcluster: fleet ID is required for retained snapshot purge")
	}
	policy, ok := s.policies[fleetID]
	s.mu.Lock()
	defer s.mu.Unlock()
	if !ok || !policy.Enabled {
		if err := os.RemoveAll(s.fleetDir(fleetID)); err != nil {
			return 0, fmt.Errorf("workflowcluster: delete retained snapshots for disabled fleet %q: %w", fleetID, err)
		}
		return 0, fmt.Errorf("%w for fleet %q", ErrFleetRetentionDisabled, fleetID)
	}
	kept := make(map[string]bool, len(keep))
	for _, instanceID := range keep {
		kept[retainedPathKey(instanceID)] = true
	}
	root := s.fleetDir(fleetID)
	entries, err := os.ReadDir(root)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("workflowcluster: read retained fleet %q: %w", fleetID, err)
	}
	purged := 0
	for _, entry := range entries {
		if kept[entry.Name()] {
			continue
		}
		if err := os.RemoveAll(filepath.Join(root, entry.Name())); err != nil {
			return purged, fmt.Errorf("workflowcluster: purge retained snapshots %q: %w", entry.Name(), err)
		}
		if entry.IsDir() {
			purged++
		}
	}
	return purged, nil
}

// pruneInstance enforces the policy bounds on one instance directory and
// returns the surviving snapshots in generation order. Files that cannot be
// trusted, whether unreadable, oversized, undecodable, invalid, filed under
// another instance, or dated beyond the allowed clock skew, are deleted rather
// than failing the read.
func (s *FileRetainedSnapshotStore) pruneInstance(ctx context.Context, dir string, policy FleetRetentionPolicy) ([]FleetSnapshot, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return []FleetSnapshot{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("workflowcluster: read retained snapshot directory: %w", err)
	}
	now := s.now().UTC()
	cutoff, latest := now.Add(-policy.MaxAge), now.Add(MaxRetainedClockSkew)
	keep := make([]retainedSnapshotFile, 0, len(entries))
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		snapshot, ok := readRetainedSnapshotFile(path, filepath.Base(dir))
		if ok && !snapshot.GeneratedAt.Before(cutoff) && !snapshot.GeneratedAt.After(latest) {
			keep = append(keep, retainedSnapshotFile{path: path, snapshot: snapshot})
			continue
		}
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("workflowcluster: prune retained snapshot %q: %w", path, err)
		}
	}
	sort.Slice(keep, func(i, j int) bool {
		return keep[i].snapshot.GeneratedAt.Before(keep[j].snapshot.GeneratedAt)
	})
	for len(keep) > policy.MaxSnapshotsPerInstance {
		if err := os.Remove(keep[0].path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("workflowcluster: prune excess retained snapshot %q: %w", keep[0].path, err)
		}
		keep = keep[1:]
	}
	snapshots := make([]FleetSnapshot, 0, len(keep))
	for _, file := range keep {
		snapshots = append(snapshots, file.snapshot)
	}
	return snapshots, nil
}

type retainedSnapshotFile struct {
	path     string
	snapshot FleetSnapshot
}

// readRetainedSnapshotFile decodes one retained file and reports whether it is
// a valid snapshot belonging to the instance directory instanceKey.
func readRetainedSnapshotFile(path, instanceKey string) (FleetSnapshot, bool) {
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() > maxFleetSnapshotBytes {
		return FleetSnapshot{}, false
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return FleetSnapshot{}, false
	}
	var snapshot FleetSnapshot
	if json.Unmarshal(data, &snapshot) != nil || validateFleetSnapshot(snapshot) != nil ||
		snapshot.GeneratedAt.IsZero() || retainedPathKey(snapshot.InstanceID) != instanceKey {
		return FleetSnapshot{}, false
	}
	return snapshot, true
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
