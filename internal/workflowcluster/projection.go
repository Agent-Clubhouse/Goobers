package workflowcluster

import (
	"math"
	"sort"
)

// MapPoint is one workflow placed on the Labs cluster map. X and Y come from
// classical multidimensional scaling of the model's own distance matrix, so
// nearby points are workflows the model considers similar. Cluster is empty
// for noise.
type MapPoint struct {
	ID      string  `json:"id"`
	X       float64 `json:"x"`
	Y       float64 `json:"y"`
	Cluster string  `json:"cluster,omitempty"`
}

const (
	projectionIterations = 200
	projectionTolerance  = 1e-10
)

// Project lays the model's population out in two dimensions for display. The
// layout is deterministic for a given model and point set and is advisory:
// it is a lossy view of the clustering, not an input to it.
func Project(m Model, points []Point) []MapPoint {
	sorted, err := validatePoints(points)
	if err != nil || len(sorted) == 0 {
		return nil
	}
	cluster := map[string]string{}
	for _, c := range m.Clusters {
		for _, id := range c.Members {
			cluster[id] = c.ID
		}
	}
	n := len(sorted)
	b := doubleCenteredGram(distanceMatrix(m.Schema, sorted))
	axes := [2][]float64{}
	for k := range axes {
		value, vector := dominantEigen(b)
		scale := 0.0
		if value > projectionTolerance {
			scale = math.Sqrt(value)
		}
		axes[k] = make([]float64, n)
		for i := range vector {
			axes[k][i] = vector[i] * scale
		}
		for i := range b {
			for j := range b[i] {
				b[i][j] -= value * vector[i] * vector[j]
			}
		}
	}
	out := make([]MapPoint, n)
	for i, p := range sorted {
		out[i] = MapPoint{ID: p.ID, X: round6(axes[0][i]), Y: round6(axes[1][i]), Cluster: cluster[p.ID]}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// doubleCenteredGram returns B = -1/2 J D² J, the Gram matrix classical MDS
// factors.
func doubleCenteredGram(d [][]float64) [][]float64 {
	n := len(d)
	rowMean := make([]float64, n)
	total := 0.0
	for i := range d {
		for j := range d[i] {
			sq := d[i][j] * d[i][j]
			rowMean[i] += sq
			total += sq
		}
		rowMean[i] /= float64(n)
	}
	total /= float64(n * n)
	b := make([][]float64, n)
	for i := range b {
		b[i] = make([]float64, n)
		for j := range b[i] {
			b[i][j] = -0.5 * (d[i][j]*d[i][j] - rowMean[i] - rowMean[j] + total)
		}
	}
	return b
}

// dominantEigen finds the largest eigenpair of symmetric a by power iteration
// from a fixed start, and orients the vector so its largest-magnitude
// component is positive, which keeps the layout stable across refits.
func dominantEigen(a [][]float64) (float64, []float64) {
	n := len(a)
	v := make([]float64, n)
	for i := range v {
		v[i] = 1 + float64(i)/float64(n)
	}
	normalize(v)
	value := 0.0
	next := make([]float64, n)
	for iter := 0; iter < projectionIterations; iter++ {
		for i := range a {
			next[i] = 0
			for j := range a[i] {
				next[i] += a[i][j] * v[j]
			}
		}
		norm := normalize(next)
		if norm < projectionTolerance {
			return 0, make([]float64, n)
		}
		delta := 0.0
		for i := range v {
			delta = math.Max(delta, math.Abs(next[i]-v[i]))
		}
		copy(v, next)
		value = 0
		for i := range a {
			for j := range a[i] {
				value += v[i] * a[i][j] * v[j]
			}
		}
		if delta < projectionTolerance {
			break
		}
	}
	largest := 0
	for i := range v {
		if math.Abs(v[i]) > math.Abs(v[largest])+projectionTolerance {
			largest = i
		}
	}
	if v[largest] < 0 {
		for i := range v {
			v[i] = -v[i]
		}
	}
	return value, v
}

func normalize(v []float64) float64 {
	norm := 0.0
	for _, x := range v {
		norm += x * x
	}
	norm = math.Sqrt(norm)
	if norm < projectionTolerance {
		return norm
	}
	for i := range v {
		v[i] /= norm
	}
	return norm
}

func round6(x float64) float64 {
	r := math.Round(x*1e6) / 1e6
	if r == 0 {
		return 0
	}
	return r
}
