package workflowcluster

import (
	"slices"
	"testing"
)

func TestAblateReportsFeaturesAndWLAsOneGroup(t *testing.T) {
	points := blobs()
	for i := range points {
		points[i].Numeric["wl.00"] = float64(i % 2)
		points[i].Numeric["wl.01"] = float64(i % 3)
	}
	opts := Options{MinClusterSize: 4, Resamples: 5, Seed: 7}
	baseline, err := Fit("test/v1", points, opts)
	if err != nil {
		t.Fatal(err)
	}
	report, err := Ablate("test/v1", points, opts, baseline)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, feature := range report.Features {
		names = append(names, feature.Feature)
		if feature.AssignmentChange < 0 || feature.AssignmentChange > 1 {
			t.Errorf("%s assignment change = %v", feature.Feature, feature.AssignmentChange)
		}
	}
	if !slices.Equal(names, []string{"wl", "x", "y"}) {
		t.Fatalf("features = %v", names)
	}
}

func TestAssignmentChangeIgnoresClusterLabelRenumbering(t *testing.T) {
	baseline := Model{Population: 4, Clusters: []Cluster{
		{ID: "c0", Members: []string{"a", "b"}},
		{ID: "c1", Members: []string{"c", "d"}},
	}}
	renumbered := Model{Population: 4, Clusters: []Cluster{
		{ID: "c0", Members: []string{"c", "d"}},
		{ID: "c1", Members: []string{"a", "b"}},
	}}
	if change := assignmentChange(baseline, renumbered); change != 0 {
		t.Fatalf("assignment change = %v, want 0", change)
	}
}

func TestAssignmentChangeDetectsClusterSplitsAndMerges(t *testing.T) {
	oneCluster := Model{Population: 4, Clusters: []Cluster{
		{ID: "c0", Members: []string{"a", "b", "c", "d"}},
	}}
	twoClusters := Model{Population: 4, Clusters: []Cluster{
		{ID: "c0", Members: []string{"a", "b"}},
		{ID: "c1", Members: []string{"c", "d"}},
	}}
	if change := assignmentChange(oneCluster, twoClusters); change != 0.5 {
		t.Errorf("split assignment change = %v, want 0.5", change)
	}
	if change := assignmentChange(twoClusters, oneCluster); change != 0.5 {
		t.Errorf("merge assignment change = %v, want 0.5", change)
	}
}
