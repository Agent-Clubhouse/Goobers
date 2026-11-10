package workflowcluster

import (
	"math"
	"reflect"
	"slices"
	"testing"
)

func TestProjectKeepsClustersApartAndIsDeterministic(t *testing.T) {
	points := blobs()
	model := fit(t, points)
	got := Project(model, points)
	if len(got) != len(points) {
		t.Fatalf("map has %d points, want %d", len(got), len(points))
	}

	centroids := map[string][2]float64{}
	counts := map[string]float64{}
	for _, p := range got {
		if p.ID == "outlier" {
			if p.Cluster != "" {
				t.Errorf("noise point assigned cluster %q", p.Cluster)
			}
			continue
		}
		c := centroids[p.Cluster]
		centroids[p.Cluster] = [2]float64{c[0] + p.X, c[1] + p.Y}
		counts[p.Cluster]++
	}
	for id, c := range centroids {
		centroids[id] = [2]float64{c[0] / counts[id], c[1] / counts[id]}
	}
	if len(centroids) != 3 {
		t.Fatalf("centroids = %v, want 3 clusters", centroids)
	}
	for _, p := range got {
		if p.Cluster == "" {
			continue
		}
		own := math.Hypot(p.X-centroids[p.Cluster][0], p.Y-centroids[p.Cluster][1])
		for id, c := range centroids {
			if id != p.Cluster && math.Hypot(p.X-c[0], p.Y-c[1]) <= own {
				t.Errorf("%s (cluster %s) is no closer to its own cluster than to %s", p.ID, p.Cluster, id)
			}
		}
	}

	reversed := slices.Clone(points)
	slices.Reverse(reversed)
	if again := Project(model, reversed); !reflect.DeepEqual(again, got) {
		t.Errorf("projection depends on input order")
	}
}
