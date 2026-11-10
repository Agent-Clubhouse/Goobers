package workflowcluster

import (
	"slices"
	"strings"
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

func TestAblationKeysFeaturesByKind(t *testing.T) {
	def := reviewLoop([4]string{"implement", "review", "verdict", "publish"}, "fix")
	other := reviewLoop([4]string{"implement", "review", "verdict", "publish"}, "fix")
	other.Spec.Tasks[0].Capabilities = []string{"repo.write"}
	points := []Point{
		StructuralPoint("a", extract(t, def)),
		StructuralPoint("b", extract(t, other)),
	}
	targets := ablationFeatures(buildSchema(points))
	seen := map[string]ablationTarget{}
	for _, target := range targets {
		if _, dup := seen[target.label]; dup {
			t.Fatalf("duplicate ablation target %q", target.label)
		}
		seen[target.label] = target
	}
	numeric, okNumeric := seen["capabilities"]
	set, okSet := seen["capabilities[set]"]
	if !okNumeric || !okSet {
		t.Fatalf("targets = %v, want capabilities and capabilities[set]", targets)
	}

	for _, p := range dropFeature(points, numeric) {
		if _, ok := p.Numeric["capabilities"]; ok {
			t.Errorf("%s: numeric capabilities kept after numeric drop", p.ID)
		}
		if _, ok := p.Categorical["capabilities"]; !ok {
			t.Errorf("%s: set capabilities dropped by numeric drop", p.ID)
		}
	}
	for _, p := range dropFeature(points, set) {
		if _, ok := p.Numeric["capabilities"]; !ok {
			t.Errorf("%s: numeric capabilities dropped by set drop", p.ID)
		}
		if _, ok := p.Categorical["capabilities"]; ok {
			t.Errorf("%s: set capabilities kept after set drop", p.ID)
		}
	}
	for _, p := range dropFeature(points, seen["wl"]) {
		for name := range p.Numeric {
			if strings.HasPrefix(name, wlFeaturePrefix) {
				t.Fatalf("%s: %s kept after wl drop", p.ID, name)
			}
		}
		if _, ok := p.Numeric["capabilities"]; !ok {
			t.Errorf("%s: wl drop removed capabilities", p.ID)
		}
	}
}
