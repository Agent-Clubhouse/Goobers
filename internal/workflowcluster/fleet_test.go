package workflowcluster

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

var fleetT0 = time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)

func snapshotOf(t *testing.T, featureVersion, instance, salt string, at time.Time, points ...[]Point) FleetSnapshot {
	t.Helper()
	s, err := NewFleetSnapshot(featureVersion, instance, "test", salt, slices.Concat(points...), at)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestFleetSnapshotCarriesNoNamesAndSaltsGoobers(t *testing.T) {
	features := extract(t, reviewLoop([4]string{"draft-zz", "inspect-zz", "decide-zz", "ship-zz"}, "secret goal text"))
	point := StructuralPoint("secret-gaggle/secret-workflow", features)
	a := snapshotOf(t, StructuralFeatureVersion, "inst-a", "fleet-1", fleetT0, []Point{point})
	data, err := json.Marshal(a)
	if err != nil {
		t.Fatal(err)
	}
	for _, leaked := range []string{"secret-gaggle", "secret-workflow", "secret goal", "-zz", "coder", "reviewer"} {
		if strings.Contains(string(data), leaked) {
			t.Fatalf("snapshot leaks %q: %s", leaked, data)
		}
	}
	if got := a.Workflows[0].Categorical["capabilities"]; !slices.Equal(got, features.Categorical["capabilities"]) {
		t.Fatalf("capabilities = %v, want unhashed %v", got, features.Categorical["capabilities"])
	}

	point.ID = "other-gaggle/clone"
	b := snapshotOf(t, StructuralFeatureVersion, "inst-b", "fleet-1", fleetT0, []Point{point})
	if a.Workflows[0].Shape != b.Workflows[0].Shape || a.Workflows[0].Key == b.Workflows[0].Key {
		t.Fatalf("same fleet salt: want equal shapes and distinct keys, got %+v %+v", a.Workflows[0], b.Workflows[0])
	}
	other := snapshotOf(t, StructuralFeatureVersion, "inst-b", "fleet-2", fleetT0, []Point{point})
	if other.Workflows[0].Shape == a.Workflows[0].Shape {
		t.Fatal("a different fleet salt must not reproduce goober hashes")
	}
}

func fleetSnapshots(t *testing.T) []FleetSnapshot {
	outlier := []Point{{ID: "o", Numeric: map[string]float64{"x": 30, "y": 30}}}
	return []FleetSnapshot{
		snapshotOf(t, "test/v1", "inst-1", "s", fleetT0, blob("a", 0, 0, 8), blob("b", 10, 0, 8)),
		snapshotOf(t, "test/v1", "inst-2", "s", fleetT0.Add(-time.Hour), blob("old", 50, 50, 2)),
		snapshotOf(t, "test/v1", "inst-2", "s", fleetT0, blob("clone", 0, 0, 8), blob("c", 0, 10, 8)),
		snapshotOf(t, "test/v1", "inst-3", "s", fleetT0, outlier),
		snapshotOf(t, "test/v2", "inst-4", "s", fleetT0, blob("d", 0, 0, 8)),
	}
}

func TestClusterFleetCountsClonesOnceAndScoresInstances(t *testing.T) {
	view, err := ClusterFleet(fleetSnapshots(t), Options{MinClusterSize: 4, Resamples: 20, Seed: 7})
	if err != nil {
		t.Fatal(err)
	}
	if view.Status != FleetClustered || view.FeatureVersion != "test/v1" {
		t.Fatalf("status %q version %q", view.Status, view.FeatureVersion)
	}
	if view.Workflows != 33 || view.Population != 25 {
		t.Fatalf("workflows %d population %d, want 33 deployments of 25 distinct shapes", view.Workflows, view.Population)
	}
	if len(view.Clusters) != 3 {
		t.Fatalf("clusters = %+v", view.Clusters)
	}
	shared := 0
	for _, c := range view.Clusters {
		if c.Instances == 2 {
			shared++
			if c.Shapes != 8 || c.Workflows != 16 {
				t.Fatalf("shared cluster %+v, want 8 shapes deployed 16 times", c)
			}
		}
	}
	if shared != 1 {
		t.Fatalf("want one cluster shared by two instances, got %+v", view.Clusters)
	}
	want := map[string]FleetInstance{
		"inst-1": {InstanceID: "inst-1", GoobersVersion: "test", Workflows: 16, Shapes: 16, Aligned: 8, Private: 8},
		"inst-2": {InstanceID: "inst-2", GoobersVersion: "test", Workflows: 16, Shapes: 16, Aligned: 8, Private: 8},
		"inst-3": {InstanceID: "inst-3", GoobersVersion: "test", Workflows: 1, Shapes: 1, Outliers: 1},
	}
	if len(view.Instances) != len(want) {
		t.Fatalf("instances = %+v", view.Instances)
	}
	for _, got := range view.Instances {
		if got != want[got.InstanceID] {
			t.Fatalf("instance %+v, want %+v", got, want[got.InstanceID])
		}
	}
	reasons := map[string]string{}
	for _, e := range view.Excluded {
		reasons[e.InstanceID] = e.Reason
	}
	if reasons["inst-2"] != fleetReasonSuperseded || !strings.Contains(reasons["inst-4"], "test/v2") || len(view.Excluded) != 2 {
		t.Fatalf("excluded = %+v", view.Excluded)
	}
	if view.Confounds == nil || len(view.Map) != 25 {
		t.Fatalf("want an instance confound check and a 25-point map, got %v / %d", view.Confounds, len(view.Map))
	}
}

func TestClusterFleetRejectsTamperedShapesAndUnknownVersions(t *testing.T) {
	s := snapshotOf(t, "test/v1", "inst-1", "s", fleetT0, blob("a", 0, 0, 4))
	s.Workflows[0].Numeric = map[string]float64{"x": 99, "y": 99}
	if _, err := ClusterFleet([]FleetSnapshot{s}, Options{}); err == nil {
		t.Fatal("want an error for a workflow whose shape does not match its features")
	}
	s = snapshotOf(t, "test/v1", "inst-1", "s", fleetT0, blob("a", 0, 0, 4))
	s.Version = "workflowcluster.fleetsnapshot/v0"
	if _, err := ClusterFleet([]FleetSnapshot{s}, Options{}); err == nil {
		t.Fatal("want an error for an unsupported snapshot version")
	}
}

func TestRunCLIClustersFleetSnapshotDirectory(t *testing.T) {
	dir := t.TempDir()
	for i, s := range fleetSnapshots(t) {
		data, err := json.Marshal(s)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, s.InstanceID+"-"+string(rune('a'+i))+".json"), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	var stdout, stderr bytes.Buffer
	code := RunCLI([]string{"--fleet", dir, "--min-cluster-size", "4", "--seed", "7"}, flag.NewFlagSet("t", flag.ContinueOnError), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	for _, want := range []string{"across 3 instances", "33 workflows, 25 distinct shapes", "on 2/3 instances", "inst-4  feature version test/v2", "Confounds (stratified by instance"} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("output missing %q:\n%s", want, stdout.String())
		}
	}
}

func TestRunCLIRejectsSnapshotFlagsWithoutSnapshot(t *testing.T) {
	for _, args := range [][]string{
		{"--salt", "s"},
		{"--instance-id", "i"},
		{"--snapshot", "--fleet", "d"},
		{"--snapshot", "--json"},
		{"--fleet", "d", "--stratify", "gaggle"},
	} {
		var stdout, stderr bytes.Buffer
		if code := RunCLI(args, flag.NewFlagSet("t", flag.ContinueOnError), &stdout, &stderr); code != 2 {
			t.Fatalf("%v: exit %d, want 2", args, code)
		}
	}
}
