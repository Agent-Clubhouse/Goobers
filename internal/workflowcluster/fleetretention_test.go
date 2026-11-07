package workflowcluster

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/goobers/goobers/internal/fleet"
)

func newRetentionStore(t *testing.T, root string, policy FleetRetentionPolicy, now time.Time) *FileRetainedSnapshotStore {
	t.Helper()
	store, err := NewFileRetainedSnapshotStore(root, map[string]FleetRetentionPolicy{"fleet-1": policy})
	if err != nil {
		t.Fatal(err)
	}
	store.now = func() time.Time { return now }
	return store
}

func TestRetainedSnapshotStoreRefusesWritesWhenDisabled(t *testing.T) {
	store := newRetentionStore(t, t.TempDir(), FleetRetentionPolicy{}, fleetT0)
	err := store.Write(context.Background(), "fleet-1", snapshotOf(t, "test/v1", "inst-1", "fleet-1", fleetT0, blob("a", 0, 0, 2)))
	if !errors.Is(err, ErrFleetRetentionDisabled) {
		t.Fatalf("Write error = %v, want retention disabled", err)
	}
}

func TestRetainedSnapshotStorePrunesAgeAndPerInstanceCount(t *testing.T) {
	store := newRetentionStore(t, t.TempDir(), FleetRetentionPolicy{
		Enabled: true, MaxAge: 2 * time.Hour, MaxSnapshotsPerInstance: 2,
	}, fleetT0)
	for i, at := range []time.Time{
		fleetT0.Add(-3 * time.Hour), fleetT0.Add(-90 * time.Minute),
		fleetT0.Add(-time.Hour), fleetT0,
	} {
		snapshot := snapshotOf(t, "test/v1", "inst-1", "fleet-1", at, blob(string(rune('a'+i)), float64(i), 0, 2))
		if err := store.Write(context.Background(), "fleet-1", snapshot); err != nil {
			t.Fatal(err)
		}
	}
	snapshots, err := store.Snapshots(context.Background(), "fleet-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshots) != 2 || !snapshots[0].GeneratedAt.Equal(fleetT0.Add(-time.Hour)) || !snapshots[1].GeneratedAt.Equal(fleetT0) {
		t.Fatalf("retained snapshots = %+v", snapshots)
	}
}

func TestRetainedSnapshotStoreDisabledFleetRetainsNothing(t *testing.T) {
	root := t.TempDir()
	enabled := newRetentionStore(t, root, FleetRetentionPolicy{
		Enabled: true, MaxAge: 24 * time.Hour, MaxSnapshotsPerInstance: 2,
	}, fleetT0)
	if err := enabled.Write(context.Background(), "fleet-1", snapshotOf(t, "test/v1", "inst-1", "fleet-1", fleetT0, blob("a", 0, 0, 2))); err != nil {
		t.Fatal(err)
	}
	for name, policies := range map[string]map[string]FleetRetentionPolicy{
		"unconfigured": {},
		"disabled":     {"fleet-1": {}},
	} {
		t.Run(name, func(t *testing.T) {
			store, err := NewFileRetainedSnapshotStore(root, policies)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := store.Snapshots(context.Background(), "fleet-1"); !errors.Is(err, ErrFleetRetentionDisabled) {
				t.Fatalf("Snapshots error = %v, want retention disabled", err)
			}
			fetched := false
			_, err = CollectRetainedFleet(context.Background(), store, "fleet-1",
				[]fleet.Association{scopedAssociation("inst-1")}, []FleetTarget{{InstanceID: "inst-1"}},
				func(context.Context, FleetTarget) (FleetSnapshot, error) {
					fetched = true
					return FleetSnapshot{}, nil
				}, FleetCollectionOptions{})
			if !errors.Is(err, ErrFleetRetentionDisabled) || fetched {
				t.Fatalf("CollectRetainedFleet error = %v fetched = %v, want refusal before any fetch", err, fetched)
			}
		})
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("disabled fleet left retained data: %v", entries)
	}
}

func scopedAssociation(instanceID string) fleet.Association {
	return fleet.Association{
		FleetID: "fleet-1", InstanceID: instanceID,
		Projections: []string{fleet.ProjectionLabsWorkflowFeatures},
	}
}

// retentionTestInstance is an enrolled instance whose snapshots are served by
// the real scope-gated FleetSnapshotSource over durable fleet association
// storage, so lifecycle transitions are the production state changes.
type retentionTestInstance struct {
	root    string
	storage *fleet.FileStorage
	source  *FleetSnapshotSource
}

func joinRetentionTestInstance(t *testing.T, instanceID string, now func() time.Time) retentionTestInstance {
	t.Helper()
	storage, err := fleet.NewFileStorage(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	key, err := fleet.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	privateKey, err := fleet.MarshalPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	if err := storage.Save(root, fleet.Record{
		Association: fleet.Association{
			InstanceID: instanceID, DisplayName: instanceID, FleetID: "fleet-1",
			RegistrationID: "registration-" + instanceID, RegistrationGeneration: 1,
			CanonicalURI: "https://fleet.example", ConnectionEndpoint: "wss://fleet.example/connections",
			ProtocolVersion: fleet.ProtocolVersion,
			ACL:             fleet.ACL{PolicyVersion: fleet.DefaultACLPolicyVersion, Grants: []fleet.Grant{}},
			Projections:     []string{fleet.ProjectionLabsWorkflowFeatures},
			JoinedAt:        fleetT0,
		},
		PrivateKey: privateKey,
		Credential: "credential-" + instanceID,
	}); err != nil {
		t.Fatal(err)
	}
	source, err := NewFleetSnapshotSource(root, storage)
	if err != nil {
		t.Fatal(err)
	}
	source.now = now
	source.configuredPoints = func(string) ([]Point, error) { return blob(instanceID, 0, 0, 3), nil }
	return retentionTestInstance{root: root, storage: storage, source: source}
}

// retentionTestSocket replays queued fleet messages, then blocks until closed.
type retentionTestSocket struct {
	reads  chan []byte
	closed chan struct{}
	once   sync.Once
}

func (s *retentionTestSocket) Read(ctx context.Context) (websocket.MessageType, []byte, error) {
	select {
	case data := <-s.reads:
		return websocket.MessageText, data, nil
	case <-ctx.Done():
		return 0, nil, ctx.Err()
	case <-s.closed:
		return 0, nil, errors.New("closed")
	}
}

func (s *retentionTestSocket) Write(context.Context, websocket.MessageType, []byte) error { return nil }

func (s *retentionTestSocket) Close(websocket.StatusCode, string) error {
	s.once.Do(func() { close(s.closed) })
	return nil
}

// revokeViaConnector delivers a service-side revoke message to the instance's
// real fleet connector, which records the revocation durably.
func revokeViaConnector(t *testing.T, instance retentionTestInstance) {
	t.Helper()
	socket := &retentionTestSocket{reads: make(chan []byte, 3), closed: make(chan struct{})}
	for _, message := range []any{
		fleet.Challenge{Type: "challenge", ProtocolVersion: fleet.ProtocolVersion, FleetID: "fleet-1",
			ConnectionID: "connection-1", Nonce: "nonce-1", SentAt: fleetT0},
		fleet.HelloAck{Type: "hello-ack", ConnectionID: "connection-1", HeartbeatSeconds: 30},
		fleet.Revoke{Type: "revoke", Reason: "operator removed registration"},
	} {
		data, err := json.Marshal(message)
		if err != nil {
			t.Fatal(err)
		}
		socket.reads <- data
	}
	connector := fleet.NewConnector(instance.storage, instance.root, "test")
	connector.Dial = func(context.Context, string, *websocket.DialOptions) (fleet.Socket, *http.Response, error) {
		return socket, nil, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := connector.Run(ctx); err != nil {
		t.Fatal(err)
	}
	association, err := instance.storage.LoadAssociation(instance.root)
	if err != nil {
		t.Fatal(err)
	}
	if !association.Revoked {
		t.Fatalf("connector did not record revocation: %+v", association)
	}
}

func TestCollectRetainedFleetPurgesWhenInstanceLeavesIsRevokedOrRemovesScope(t *testing.T) {
	transitions := map[string]func(*testing.T, retentionTestInstance){
		// goobers fleet leave deletes the durable association.
		"leave": func(t *testing.T, instance retentionTestInstance) {
			if err := instance.storage.Delete(instance.root); err != nil {
				t.Fatal(err)
			}
		},
		"revoke": revokeViaConnector,
		"scope-removal": func(t *testing.T, instance retentionTestInstance) {
			if err := instance.storage.Update(instance.root, func(association *fleet.Association) error {
				association.Projections = slices.DeleteFunc(association.Projections, func(projection string) bool {
					return projection == fleet.ProjectionLabsWorkflowFeatures
				})
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		},
	}
	for name, transition := range transitions {
		t.Run(name, func(t *testing.T) {
			now := fleetT0
			clock := func() time.Time { return now }
			store := newRetentionStore(t, t.TempDir(), FleetRetentionPolicy{
				Enabled: true, MaxAge: 24 * time.Hour, MaxSnapshotsPerInstance: 10,
			}, fleetT0.Add(time.Hour))
			instances := map[string]retentionTestInstance{
				"gone": joinRetentionTestInstance(t, "gone", clock),
				"kept": joinRetentionTestInstance(t, "kept", clock),
			}
			// The instance API maps every scope-gate refusal to 403
			// projection_not_granted, which the fetcher surfaces as
			// ErrFleetProjectionNotGranted.
			fetch := func(ctx context.Context, target FleetTarget) (FleetSnapshot, error) {
				snapshot, err := instances[target.InstanceID].source.WorkflowFeatures(ctx, FleetFeatureStructural)
				if errors.Is(err, ErrFleetNotAssociated) || errors.Is(err, ErrFleetAssociationRevoked) ||
					errors.Is(err, ErrFleetProjectionNotGranted) {
					return FleetSnapshot{}, ErrFleetProjectionNotGranted
				}
				return snapshot, err
			}
			// The fleet's association set is read before the transition, so
			// only the instance's own scope gate can reveal it.
			associations := []fleet.Association{scopedAssociation("gone"), scopedAssociation("kept")}
			targets := []FleetTarget{{InstanceID: "gone"}, {InstanceID: "kept"}}
			collect := func() RetainedFleetCollection {
				t.Helper()
				result, err := CollectRetainedFleet(context.Background(), store, "fleet-1", associations, targets, fetch, FleetCollectionOptions{})
				if err != nil {
					t.Fatal(err)
				}
				return result
			}
			for range 2 {
				if result := collect(); len(result.Retained) != 2 || len(result.Purged) != 0 {
					t.Fatalf("collection before %s = %+v", name, result)
				}
				now = now.Add(time.Minute)
			}
			if snapshots, err := store.Snapshots(context.Background(), "fleet-1"); err != nil || len(snapshots) != 4 {
				t.Fatalf("snapshots before %s = %+v, %v", name, snapshots, err)
			}

			transition(t, instances["gone"])
			result := collect()
			if !slices.Equal(result.Purged, []string{"gone"}) ||
				!slices.Equal(result.Excluded, []FleetExclusion{{InstanceID: "gone", Reason: FleetExclusionNotGranted}}) {
				t.Fatalf("collection after %s = %+v", name, result)
			}
			snapshots, err := store.Snapshots(context.Background(), "fleet-1")
			if err != nil {
				t.Fatal(err)
			}
			if len(snapshots) != 3 {
				t.Fatalf("snapshots after %s = %+v", name, snapshots)
			}
			for _, snapshot := range snapshots {
				if snapshot.InstanceID != "kept" {
					t.Fatalf("snapshot for %s instance survived: %+v", name, snapshot)
				}
			}
		})
	}
}

func TestCollectRetainedFleetPurgesMembersTheAssociationSetNoLongerScopes(t *testing.T) {
	revoked := scopedAssociation("revoked")
	revoked.Revoked = true
	otherFleet := scopedAssociation("other-fleet")
	otherFleet.FleetID = "fleet-2"
	associations := []fleet.Association{
		scopedAssociation("kept"),
		revoked,
		{FleetID: "fleet-1", InstanceID: "unscoped"},
		otherFleet,
		// "left" has no association at all.
	}
	store := newRetentionStore(t, t.TempDir(), FleetRetentionPolicy{
		Enabled: true, MaxAge: 24 * time.Hour, MaxSnapshotsPerInstance: 10,
	}, fleetT0)
	instances := []string{"kept", "left", "other-fleet", "revoked", "unscoped"}
	for _, instance := range instances {
		if err := store.Write(context.Background(), "fleet-1", snapshotOf(t, "test/v1", instance, "fleet-1", fleetT0, blob(instance, 0, 0, 2))); err != nil {
			t.Fatal(err)
		}
	}
	var fetched []string
	targets := make([]FleetTarget, 0, len(instances))
	for _, instance := range instances {
		targets = append(targets, FleetTarget{InstanceID: instance})
	}
	result, err := CollectRetainedFleet(context.Background(), store, "fleet-1", associations, targets,
		func(_ context.Context, target FleetTarget) (FleetSnapshot, error) {
			fetched = append(fetched, target.InstanceID)
			return snapshotOf(t, "test/v1", target.InstanceID, "fleet-1", fleetT0.Add(time.Minute), blob(target.InstanceID, 0, 0, 2)), nil
		}, FleetCollectionOptions{MaxConcurrency: 1})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(fetched, []string{"kept"}) {
		t.Fatalf("fetched unscoped targets: %v", fetched)
	}
	if want := []string{"left", "other-fleet", "revoked", "unscoped"}; !slices.Equal(result.Purged, want) {
		t.Fatalf("purged = %v, want %v", result.Purged, want)
	}
	snapshots, err := store.Snapshots(context.Background(), "fleet-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshots) != 2 || snapshots[0].InstanceID != "kept" || snapshots[1].InstanceID != "kept" {
		t.Fatalf("retained snapshots = %+v", snapshots)
	}
}

func TestCollectRetainedFleetKeepsSnapshotsOfOfflineInstances(t *testing.T) {
	store := newRetentionStore(t, t.TempDir(), FleetRetentionPolicy{
		Enabled: true, MaxAge: 24 * time.Hour, MaxSnapshotsPerInstance: 10,
	}, fleetT0)
	if err := store.Write(context.Background(), "fleet-1", snapshotOf(t, "test/v1", "inst-1", "fleet-1", fleetT0, blob("a", 0, 0, 2))); err != nil {
		t.Fatal(err)
	}
	result, err := CollectRetainedFleet(context.Background(), store, "fleet-1",
		[]fleet.Association{scopedAssociation("inst-1")}, []FleetTarget{{InstanceID: "inst-1"}},
		func(context.Context, FleetTarget) (FleetSnapshot, error) {
			return FleetSnapshot{}, ErrFleetTargetOffline
		},
		FleetCollectionOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Purged) != 0 || !slices.Equal(result.Excluded, []FleetExclusion{{InstanceID: "inst-1", Reason: FleetExclusionOffline}}) {
		t.Fatalf("offline collection = %+v", result)
	}
	if snapshots, err := store.Snapshots(context.Background(), "fleet-1"); err != nil || len(snapshots) != 1 {
		t.Fatalf("offline instance snapshots = %+v, %v", snapshots, err)
	}
}

func TestRetainedSnapshotFilesContainOnlyFleetSnapshotFields(t *testing.T) {
	root := t.TempDir()
	store := newRetentionStore(t, root, FleetRetentionPolicy{
		Enabled: true, MaxAge: 24 * time.Hour, MaxSnapshotsPerInstance: 2,
	}, fleetT0)
	features := extract(t, reviewLoop([4]string{"draft-zz", "inspect-zz", "decide-zz", "ship-zz"}, "secret goal text"))
	snapshot := snapshotOf(t, StructuralFeatureVersion, "inst-1", "fleet-1", fleetT0, []Point{
		StructuralPoint("secret-gaggle/secret-workflow", features),
	})
	if err := store.Write(context.Background(), "fleet-1", snapshot); err != nil {
		t.Fatal(err)
	}
	var path string
	if err := filepath.WalkDir(root, func(candidate string, entry os.DirEntry, err error) error {
		if err == nil && !entry.IsDir() && filepath.Ext(candidate) == ".json" {
			path = candidate
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var stored map[string]json.RawMessage
	if err := json.Unmarshal(data, &stored); err != nil {
		t.Fatal(err)
	}
	if len(stored) != 6 {
		t.Fatalf("stored fields = %v", stored)
	}
	for _, leaked := range []string{"secret-gaggle", "secret-workflow", "secret goal", "-zz", "coder", "reviewer"} {
		if strings.Contains(string(data), leaked) {
			t.Fatalf("retained snapshot leaks %q: %s", leaked, data)
		}
	}
}

func TestFleetDriftReportsThreeWindowsOfOccupancyChange(t *testing.T) {
	store := newRetentionStore(t, t.TempDir(), FleetRetentionPolicy{
		Enabled: true, MaxAge: 24 * time.Hour, MaxSnapshotsPerInstance: 10,
	}, fleetT0.Add(3*time.Hour))
	write := func(instance string, at time.Time, points []Point) {
		t.Helper()
		if err := store.Write(context.Background(), "fleet-1", snapshotOf(t, "test/v1", instance, "fleet-1", at, points)); err != nil {
			t.Fatal(err)
		}
	}
	write("inst-1", fleetT0, blob("a", 0, 0, 6))
	write("inst-2", fleetT0, blob("b", 0, 0, 6))
	write("inst-1", fleetT0.Add(time.Hour), blob("c", 0, 0, 6))
	write("inst-2", fleetT0.Add(time.Hour), blob("d", 10, 10, 6))
	write("inst-1", fleetT0.Add(2*time.Hour), blob("e", 10, 10, 6))
	write("inst-2", fleetT0.Add(2*time.Hour), blob("f", 10, 10, 6))

	report, err := FleetDrift(context.Background(), store, "fleet-1", time.Hour, Options{
		MinClusterSize: 4, MinSamples: 2, Resamples: 10, Seed: 7,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Windows) != 3 || report.ModelID == "" || report.FeatureVersion != "test/v1" {
		t.Fatalf("report = %+v", report)
	}
	for i, window := range report.Windows {
		if window.View.Workflows != 12 {
			t.Fatalf("window %d workflows = %d, want 12", i, window.View.Workflows)
		}
	}
	first, second, third := report.Windows[0].Occupancy, report.Windows[1].Occupancy, report.Windows[2].Occupancy
	if len(first) != 1 || first[0].Workflows != 12 || len(third) != 1 || third[0].Workflows != 12 ||
		first[0].Cluster == third[0].Cluster || first[0].Cluster == trendNoiseCluster || third[0].Cluster == trendNoiseCluster {
		t.Fatalf("first and last occupancy = %+v, %+v", first, third)
	}
	want := []FleetDriftOccupancy{
		{Cluster: first[0].Cluster, Workflows: 6, Share: 0.5},
		{Cluster: third[0].Cluster, Workflows: 6, Share: 0.5},
	}
	slices.SortFunc(want, func(a, b FleetDriftOccupancy) int { return strings.Compare(a.Cluster, b.Cluster) })
	if !slices.Equal(second, want) {
		t.Fatalf("middle occupancy = %+v, want %+v", second, want)
	}
	if report.Windows[0].Change != nil || report.Windows[1].Change == nil || report.Windows[2].Change == nil {
		t.Fatalf("changes = %+v", report.Windows)
	}
	if math.Abs(report.Windows[1].Change.OccupancyDistance-0.5) > 1e-9 ||
		math.Abs(report.Windows[1].Change.DiversityDelta-1) > 1e-9 ||
		math.Abs(report.Windows[2].Change.OccupancyDistance-0.5) > 1e-9 ||
		math.Abs(report.Windows[2].Change.DiversityDelta+1) > 1e-9 {
		t.Fatalf("unexpected occupancy changes: %+v, %+v", report.Windows[1].Change, report.Windows[2].Change)
	}
}

func driftPoint(id string, x, y float64) Point {
	return Point{ID: id, Numeric: map[string]float64{"x": x, "y": y}}
}

// driftMembership is the five-shape cluster whose medoid depends on how the
// rest of a window scales x against y: the x-median is m2, the y-median m0.
func driftMembership() []Point {
	return []Point{
		driftPoint("m0", 0, 2), driftPoint("m1", 1, 0), driftPoint("m2", 2, 4),
		driftPoint("m3", 3, 1), driftPoint("m4", 4, 3),
	}
}

func driftViewClusterOf(t *testing.T, view FleetView, shapes map[string]bool) FleetCluster {
	t.Helper()
	var clusterID string
	for _, shape := range view.Shapes {
		if !shapes[shape.Shape] {
			continue
		}
		if shape.Cluster == "" || (clusterID != "" && shape.Cluster != clusterID) {
			t.Fatalf("window split or dropped the unchanged membership: %+v", view.Shapes)
		}
		clusterID = shape.Cluster
	}
	for _, cluster := range view.Clusters {
		if cluster.ID == clusterID {
			return cluster
		}
	}
	t.Fatalf("membership cluster %q not found in %+v", clusterID, view.Clusters)
	return FleetCluster{}
}

func TestFleetDriftKeepsClusterIdentityWhenWindowMedoidChanges(t *testing.T) {
	store := newRetentionStore(t, t.TempDir(), FleetRetentionPolicy{
		Enabled: true, MaxAge: 24 * time.Hour, MaxSnapshotsPerInstance: 10,
	}, fleetT0.Add(2*time.Hour))
	write := func(instance string, at time.Time, points ...[]Point) {
		t.Helper()
		if err := store.Write(context.Background(), "fleet-1", snapshotOf(t, "test/v1", instance, "fleet-1", at, points...)); err != nil {
			t.Fatal(err)
		}
	}
	// The same workflows are deployed in both windows. Only the unrelated
	// cluster moves, which rescales the features and moves the per-window
	// medoid of the unchanged cluster.
	write("inst-1", fleetT0, driftMembership())
	write("inst-2", fleetT0, blob("far-y", 0, 100, 5))
	write("inst-1", fleetT0.Add(time.Hour), driftMembership())
	write("inst-2", fleetT0.Add(time.Hour), blob("far-x", 100, 0, 5))

	report, err := FleetDrift(context.Background(), store, "fleet-1", time.Hour, Options{
		MinClusterSize: 5, MinSamples: 2, Resamples: 10, Seed: 7,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Windows) != 2 {
		t.Fatalf("windows = %+v", report.Windows)
	}
	membership := snapshotOf(t, "test/v1", "inst-1", "fleet-1", fleetT0, driftMembership())
	shapes := map[string]bool{}
	for _, workflow := range membership.Workflows {
		shapes[workflow.Shape] = true
	}
	before := driftViewClusterOf(t, report.Windows[0].View, shapes)
	after := driftViewClusterOf(t, report.Windows[1].View, shapes)
	if before.MedoidShape == after.MedoidShape {
		t.Fatalf("fixture must move the per-window medoid; both windows chose %s", before.MedoidShape)
	}

	shared := map[string][2]int{}
	for i, window := range report.Windows {
		for _, occupancy := range window.Occupancy {
			counts := shared[occupancy.Cluster]
			counts[i] = occupancy.Workflows
			shared[occupancy.Cluster] = counts
		}
	}
	stable := 0
	for cluster, counts := range shared {
		if counts[0] > 0 && counts[1] > 0 {
			stable++
			if cluster == trendNoiseCluster || counts != [2]int{5, 5} {
				t.Fatalf("unchanged membership occupancy = %v in %s; report = %+v", counts, cluster, report.Windows)
			}
		}
	}
	if stable != 1 {
		t.Fatalf("unchanged membership was not one stable cluster across windows: %+v", shared)
	}
	// Only the unrelated cluster changed, so half the occupancy moved.
	if change := report.Windows[1].Change; change == nil || math.Abs(change.OccupancyDistance-0.5) > 1e-9 {
		t.Fatalf("change = %+v, want occupancy distance 0.5", change)
	}
}

func TestFleetDriftKeepsClusterIdentityWhenMembershipGrows(t *testing.T) {
	store := newRetentionStore(t, t.TempDir(), FleetRetentionPolicy{
		Enabled: true, MaxAge: 24 * time.Hour, MaxSnapshotsPerInstance: 10,
	}, fleetT0.Add(2*time.Hour))
	write := func(instance string, at time.Time, points ...[]Point) {
		t.Helper()
		if err := store.Write(context.Background(), "fleet-1", snapshotOf(t, "test/v1", instance, "fleet-1", at, points...)); err != nil {
			t.Fatal(err)
		}
	}
	write("inst-1", fleetT0, blob("a", 0, 0, 6), blob("b", 10, 10, 6))
	write("inst-1", fleetT0.Add(time.Hour), blob("a", 0, 0, 7), blob("b", 10, 10, 6))

	report, err := FleetDrift(context.Background(), store, "fleet-1", time.Hour, Options{
		MinClusterSize: 3, MinSamples: 2, Resamples: 10, Seed: 7,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Windows) != 2 {
		t.Fatalf("windows = %+v", report.Windows)
	}
	before, after := report.Windows[0].Occupancy, report.Windows[1].Occupancy
	if len(before) != 2 || len(after) != 2 || before[0].Cluster != after[0].Cluster || before[1].Cluster != after[1].Cluster {
		t.Fatalf("one new workflow renamed clusters: before %+v after %+v", before, after)
	}
	if change := report.Windows[1].Change; change == nil || change.OccupancyDistance > 0.05 {
		t.Fatalf("change = %+v, want only the one new workflow's share to move", change)
	}
}
