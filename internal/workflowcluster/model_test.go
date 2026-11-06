package workflowcluster

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
)

func blob(prefix string, cx, cy float64, n int) []Point {
	points := make([]Point, n)
	for i := range n {
		points[i] = Point{
			ID:      fmt.Sprintf("%s%02d", prefix, i),
			Numeric: map[string]float64{"x": cx + float64(i%3)*0.3, "y": cy + float64(i/3)*0.3},
		}
	}
	return points
}

func blobs() []Point {
	points := slices.Concat(blob("a", 0, 0, 8), blob("b", 10, 0, 8), blob("c", 0, 10, 8))
	return append(points, Point{ID: "outlier", Numeric: map[string]float64{"x": 30, "y": 30}})
}

func fit(t *testing.T, points []Point) Model {
	t.Helper()
	model, err := Fit("test/v1", points, Options{MinClusterSize: 4, Resamples: 20, Seed: 7})
	if err != nil {
		t.Fatal(err)
	}
	return model
}

func TestFitFindsDensityClustersAndNoise(t *testing.T) {
	model := fit(t, blobs())

	if len(model.Clusters) != 3 {
		t.Fatalf("clusters = %d, want 3: %+v", len(model.Clusters), model.Clusters)
	}
	for i, prefix := range []string{"a", "b", "c"} {
		c := model.Clusters[i]
		if len(c.Members) != 8 || !strings.HasPrefix(c.Medoid.ID, prefix) {
			t.Errorf("cluster %d: medoid %s, members %v", i, c.Medoid.ID, c.Members)
		}
		for _, id := range c.Members {
			if !strings.HasPrefix(id, prefix) {
				t.Errorf("cluster %d mixes %s", i, id)
			}
		}
		if !c.Stable || c.Stability < 0.9 {
			t.Errorf("cluster %d stability = %v, stable = %v", i, c.Stability, c.Stable)
		}
		if len(c.Distinguishing) == 0 {
			t.Errorf("cluster %d has no distinguishing features", i)
		}
	}
	if !slices.Equal(model.Noise, []string{"outlier"}) {
		t.Errorf("noise = %v", model.Noise)
	}
}

func TestFitIsReproducibleAndOrderIndependent(t *testing.T) {
	points := blobs()
	first := fit(t, points)
	slices.Reverse(points)
	second := fit(t, points)
	if first.ID != second.ID || !strings.HasPrefix(first.ID, "sha256:") {
		t.Fatalf("model ids differ: %s vs %s", first.ID, second.ID)
	}
}

func TestFitRejectsInvalidPopulations(t *testing.T) {
	if _, err := Fit("test/v1", blob("a", 0, 0, 3), Options{MinClusterSize: 4}); err == nil {
		t.Error("population below minimum cluster size was accepted")
	}
	dup := append(blob("a", 0, 0, 5), Point{ID: "a00"})
	if _, err := Fit("test/v1", dup, Options{MinClusterSize: 4}); err == nil {
		t.Error("duplicate ids were accepted")
	}
}

func TestFitHandlesIdenticalPopulation(t *testing.T) {
	var points []Point
	for i := range 6 {
		points = append(points, Point{ID: fmt.Sprintf("p%d", i), Numeric: map[string]float64{"x": 1}})
	}
	single, err := Fit("test/v1", points, Options{MinClusterSize: 3, AllowSingleCluster: true, Resamples: 5})
	if err != nil {
		t.Fatal(err)
	}
	if len(single.Clusters) != 1 || len(single.Clusters[0].Members) != 6 || single.Clusters[0].Radius != 0 {
		t.Fatalf("identical population with single cluster allowed = %+v", single.Clusters)
	}
	split, err := Fit("test/v1", points, Options{MinClusterSize: 3, Resamples: 5})
	if err != nil {
		t.Fatal(err)
	}
	if len(split.Clusters)+len(split.Noise) == 0 {
		t.Fatal("identical population lost every point")
	}
}

func TestAssignUsesFrozenModel(t *testing.T) {
	model := fit(t, blobs())

	near, err := model.Assign("test/v1", Point{ID: "new", Numeric: map[string]float64{"x": 10.2, "y": 0.2}})
	if err != nil {
		t.Fatal(err)
	}
	if near.ClusterID != "c1" || near.Nearest != "c1" {
		t.Errorf("near assignment = %+v, want c1", near)
	}

	// Values beyond the frozen range are clipped, never rescaled.
	far, err := model.Assign("test/v1", Point{ID: "novel", Numeric: map[string]float64{"x": 500, "y": -500}})
	if err != nil {
		t.Fatal(err)
	}
	if far.ClusterID != "" || far.Nearest == "" || far.Distance <= near.Distance {
		t.Errorf("novel assignment = %+v", far)
	}

	if _, err := model.Assign("test/v2", Point{ID: "x"}); !errors.Is(err, ErrFeatureVersionMismatch) {
		t.Errorf("version mismatch err = %v", err)
	}
}

func TestFitSeparatesWorkflowShapes(t *testing.T) {
	var points []Point
	for i := range 6 {
		names := [4]string{"implement", "review", "verdict", "publish"}
		loop := reviewLoop(names, strings.Repeat("x", i))
		loop.Spec.Tasks[0].Retry.MaxAttempts = int32(2 + i%2)
		points = append(points, StructuralPoint(fmt.Sprintf("loop-%d", i), extract(t, loop)))

		linear := reviewLoop(names, strings.Repeat("y", i))
		linear.Spec.Gates[0].Branches = map[string]string{"approved": "publish", "changes": ""}
		linear.Spec.Tasks[0].Retry = nil
		points = append(points, StructuralPoint(fmt.Sprintf("linear-%d", i), extract(t, linear)))
	}

	model, err := Fit(StructuralFeatureVersion, points, Options{MinClusterSize: 3, Resamples: 20, Seed: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(model.Clusters) != 2 {
		t.Fatalf("clusters = %d, want 2: %+v", len(model.Clusters), model.Clusters)
	}
	for _, c := range model.Clusters {
		kind := strings.SplitN(c.Medoid.ID, "-", 2)[0]
		for _, id := range c.Members {
			if !strings.HasPrefix(id, kind+"-") {
				t.Errorf("cluster %s mixes %s into %s", c.ID, id, kind)
			}
		}
	}
}

func TestFitUsesSampledPathAboveExactnessBoundary(t *testing.T) {
	points := blobs()
	model, err := Fit("test/v1", points, Options{
		MinClusterSize: 4, Resamples: 5, Seed: 7,
		ApproximateAbove: 20, ApproximateSample: 20,
	})
	if err != nil {
		t.Fatal(err)
	}
	assigned := len(model.Noise)
	for _, cluster := range model.Clusters {
		assigned += len(cluster.Members)
	}
	if model.Population != len(points) || assigned != len(points) {
		t.Fatalf("population = %d, assigned = %d, want %d", model.Population, assigned, len(points))
	}
	if len(model.Clusters) == 0 || !slices.Contains(model.Noise, "outlier") {
		t.Fatalf("sampled clusters = %d, noise = %v", len(model.Clusters), model.Noise)
	}
	if model.Options.ApproximateAbove != 20 || model.Options.ApproximateSample != 20 {
		t.Fatalf("options = %+v", model.Options)
	}
}

func BenchmarkFitPopulationScale(b *testing.B) {
	for _, tc := range []struct {
		name string
		size int
	}{
		{"1k", 1000},
		{"5k", 5000},
		{"20k", 20000},
	} {
		b.Run(tc.name, func(b *testing.B) {
			points := make([]Point, tc.size)
			for i := range points {
				points[i] = Point{
					ID: fmt.Sprintf("p%05d", i),
					Numeric: map[string]float64{
						"x": float64((i%4)*100) + float64(i%10),
						"y": float64((i / 4) % 10),
					},
				}
			}
			b.ResetTimer()
			for range b.N {
				if _, err := Fit("benchmark/v1", points, Options{
					MinClusterSize: 5, Resamples: 1, ApproximateSample: 500,
				}); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
