package workflowcluster

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/rand/v2"
	"sort"
	"strings"
)

// ModelVersion identifies the fitted-model format and algorithm. Bump it when
// distance, clustering, or stability semantics change.
const ModelVersion = "workflowcluster.model/v2"

// Feature kinds in a Schema.
const (
	KindNumeric = "numeric"
	KindSet     = "set"
)

// wlFeaturePrefix names flattened Weisfeiler-Lehman buckets in a Point.
const wlFeaturePrefix = "wl."

// wlGroupWeight preserves the aggregate weight of the original 32-bucket WL
// histogram after widening it, so the structural signal cannot dominate.
const wlGroupWeight = 8.0

const (
	// DefaultApproximateAbove is the largest population fitted exactly.
	DefaultApproximateAbove = 2000
	// DefaultApproximateSample is the deterministic sample clustered above
	// DefaultApproximateAbove.
	DefaultApproximateSample = 1000
)

// Point is one member of the population being clustered, such as a child
// workflow proposal or an EffectiveVersion.
type Point struct {
	ID          string              `json:"id"`
	Numeric     map[string]float64  `json:"numeric,omitempty"`
	Categorical map[string][]string `json:"categorical,omitempty"`
}

// StructuralPoint flattens structural features into a Point keyed by id.
func StructuralPoint(id string, f StructuralFeatures) Point {
	numeric := make(map[string]float64, len(f.Numeric)+len(f.WL))
	for name, value := range f.Numeric {
		numeric[name] = value
	}
	for i, value := range f.WL {
		numeric[fmt.Sprintf("%s%02d", wlFeaturePrefix, i)] = value
	}
	categorical := make(map[string][]string, len(f.Categorical))
	for name, values := range f.Categorical {
		categorical[name] = append([]string(nil), values...)
	}
	return Point{ID: id, Numeric: numeric, Categorical: categorical}
}

// SchemaFeature is one frozen dimension of the Gower distance. Min and Max are
// the fit population's range and act as the scaler for later assignments.
type SchemaFeature struct {
	Name   string  `json:"name"`
	Kind   string  `json:"kind"`
	Weight float64 `json:"weight"`
	Min    float64 `json:"min,omitempty"`
	Max    float64 `json:"max,omitempty"`
}

// Schema is the frozen feature space of a fitted model.
type Schema struct {
	Features []SchemaFeature `json:"features"`
}

// Distance is the weighted Gower distance in [0, 1]. Numeric features are
// range-scaled and clipped, sets use Jaccard distance, and features that were
// constant in the fit population are ignored.
func (s Schema) Distance(a, b Point) float64 {
	var sum, weight float64
	for _, f := range s.Features {
		switch f.Kind {
		case KindNumeric:
			span := f.Max - f.Min
			if span <= 0 {
				continue
			}
			sum += f.Weight * math.Min(1, math.Abs(a.Numeric[f.Name]-b.Numeric[f.Name])/span)
		case KindSet:
			sum += f.Weight * jaccardDistance(a.Categorical[f.Name], b.Categorical[f.Name])
		default:
			continue
		}
		weight += f.Weight
	}
	if weight == 0 {
		return 0
	}
	return sum / weight
}

func jaccardDistance(a, b []string) float64 {
	if len(a) == 0 && len(b) == 0 {
		return 0
	}
	inA := make(map[string]bool, len(a))
	for _, v := range a {
		inA[v] = true
	}
	union := len(inA)
	shared := 0
	seen := map[string]bool{}
	for _, v := range b {
		if seen[v] {
			continue
		}
		seen[v] = true
		if inA[v] {
			shared++
		} else {
			union++
		}
	}
	return 1 - float64(shared)/float64(union)
}

// Options configures Fit. Zero values select the documented defaults.
type Options struct {
	// MinClusterSize is the smallest group reported as a cluster. Default 5.
	MinClusterSize int `json:"minClusterSize"`
	// MinSamples sets core-distance smoothing. Default MinClusterSize.
	MinSamples int `json:"minSamples"`
	// AllowSingleCluster lets the whole population form one cluster.
	AllowSingleCluster bool `json:"allowSingleCluster,omitempty"`
	// Resamples is the number of subsample refits for stability. Default 50.
	Resamples int `json:"resamples"`
	// SubsampleFraction is the share of points kept per refit. Default 0.8.
	SubsampleFraction float64 `json:"subsampleFraction"`
	// StabilityThreshold is the mean Jaccard below which a cluster is
	// reported unstable. Default 0.6.
	StabilityThreshold float64 `json:"stabilityThreshold"`
	// TopFeatures is the number of distinguishing features kept. Default 5.
	TopFeatures int `json:"topFeatures"`
	// Seed makes subsampling reproducible.
	Seed uint64 `json:"seed"`
	// ApproximateAbove switches larger populations to deterministic sampled
	// fitting. Default 2000; exact fitting is guaranteed at or below it.
	ApproximateAbove int `json:"approximateAbove"`
	// ApproximateSample is the sample clustered by HDBSCAN. Default 1000.
	ApproximateSample int `json:"approximateSample"`
}

func (o Options) withDefaults() Options {
	if o.MinClusterSize <= 0 {
		o.MinClusterSize = 5
	}
	if o.MinSamples <= 0 {
		o.MinSamples = o.MinClusterSize
	}
	if o.Resamples <= 0 {
		o.Resamples = 50
	}
	if o.SubsampleFraction <= 0 || o.SubsampleFraction > 1 {
		o.SubsampleFraction = 0.8
	}
	if o.StabilityThreshold <= 0 {
		o.StabilityThreshold = 0.6
	}
	if o.TopFeatures <= 0 {
		o.TopFeatures = 5
	}
	if o.ApproximateAbove <= 0 {
		o.ApproximateAbove = DefaultApproximateAbove
	}
	if o.ApproximateSample <= 0 {
		o.ApproximateSample = DefaultApproximateSample
	}
	o.ApproximateSample = max(o.ApproximateSample, o.MinClusterSize)
	return o
}

// Attribution explains how a cluster differs from the rest of the population.
// For numeric features Effect is the range-scaled mean difference; for sets it
// is the prevalence difference of Value.
type Attribution struct {
	Feature string  `json:"feature"`
	Value   string  `json:"value,omitempty"`
	Effect  float64 `json:"effect"`
}

// Cluster is one discovered group. Medoid is a real member, never a synthetic
// centroid, so it can be inspected or promoted as-is.
type Cluster struct {
	ID      string   `json:"id"`
	Medoid  Point    `json:"medoid"`
	Members []string `json:"members"`
	// Radius is the largest member-to-medoid distance and bounds Assign.
	Radius float64 `json:"radius"`
	// Stability is the mean best-match Jaccard across subsample refits.
	Stability      float64       `json:"stability"`
	Stable         bool          `json:"stable"`
	Distinguishing []Attribution `json:"distinguishing,omitempty"`
}

// Model is a frozen, versioned clustering. Later populations are assigned to
// it rather than refit so clusters stay comparable over time.
type Model struct {
	Version        string    `json:"version"`
	ID             string    `json:"id"`
	FeatureVersion string    `json:"featureVersion"`
	Options        Options   `json:"options"`
	Schema         Schema    `json:"schema"`
	Population     int       `json:"population"`
	Clusters       []Cluster `json:"clusters"`
	Noise          []string  `json:"noise"`
}

// ErrFeatureVersionMismatch reports a point from a different feature schema.
var ErrFeatureVersionMismatch = errors.New("workflowcluster: feature version mismatch")

// Fit clusters points with HDBSCAN over Gower distance and freezes the result.
// The output is a pure function of the point set, featureVersion, and opts;
// input order does not matter.
func Fit(featureVersion string, points []Point, opts Options) (Model, error) {
	opts = opts.withDefaults()
	if featureVersion == "" {
		return Model{}, errors.New("workflowcluster: feature version is required")
	}
	sorted, err := validatePoints(points)
	if err != nil {
		return Model{}, err
	}
	if len(sorted) < opts.MinClusterSize {
		return Model{}, fmt.Errorf("workflowcluster: population %d is below minimum cluster size %d", len(sorted), opts.MinClusterSize)
	}

	schema := buildSchema(sorted)
	if len(sorted) > opts.ApproximateAbove && opts.ApproximateSample < len(sorted) {
		return fitSampled(featureVersion, sorted, schema, opts)
	}
	return fitExact(featureVersion, sorted, schema, opts)
}

func fitExact(featureVersion string, sorted []Point, schema Schema, opts Options) (Model, error) {
	dist := distanceMatrix(schema, sorted)
	labels := hdbscan(dist, opts.MinClusterSize, opts.MinSamples, opts.AllowSingleCluster)
	groups := groupLabels(labels)
	stability := subsampleStability(dist, groups, opts)
	return buildModel(featureVersion, sorted, schema, opts, dist, labels, groups, stability)
}

func buildModel(featureVersion string, sorted []Point, schema Schema, opts Options, dist [][]float64, labels []int, groups [][]int, stability []float64) (Model, error) {
	model := Model{
		Version: ModelVersion, FeatureVersion: featureVersion, Options: opts,
		Schema: schema, Population: len(sorted), Noise: []string{},
	}
	for g, members := range groups {
		medoid, radius := medoidOf(dist, members)
		ids := make([]string, len(members))
		for i, m := range members {
			ids[i] = sorted[m].ID
		}
		model.Clusters = append(model.Clusters, Cluster{
			Medoid: sorted[medoid], Members: ids, Radius: radius,
			Stability: stability[g], Stable: stability[g] >= opts.StabilityThreshold,
			Distinguishing: distinguishing(schema, sorted, members, opts.TopFeatures),
		})
	}
	sort.Slice(model.Clusters, func(i, j int) bool { return model.Clusters[i].Medoid.ID < model.Clusters[j].Medoid.ID })
	for i := range model.Clusters {
		model.Clusters[i].ID = fmt.Sprintf("c%d", i)
	}
	for i, label := range labels {
		if label < 0 {
			model.Noise = append(model.Noise, sorted[i].ID)
		}
	}
	var err error
	model.ID, err = modelID(model)
	if err != nil {
		return Model{}, err
	}
	return model, nil
}

// fitSampled clusters an evenly spaced deterministic sample, then assigns
// remaining points to a sampled cluster only when they fall within its radius.
// It bounds distance-matrix memory while preserving exact fitting through the
// configured threshold.
func fitSampled(featureVersion string, sorted []Point, schema Schema, opts Options) (Model, error) {
	sampleSize := min(opts.ApproximateSample, len(sorted))
	sample := make([]Point, sampleSize)
	sampleOriginal := make([]int, sampleSize)
	sampled := make([]bool, len(sorted))
	for i := range sampleSize {
		index := i * len(sorted) / sampleSize
		sample[i], sampleOriginal[i], sampled[index] = sorted[index], index, true
	}
	dist := distanceMatrix(schema, sample)
	labels := hdbscan(dist, opts.MinClusterSize, opts.MinSamples, opts.AllowSingleCluster)
	sampleGroups := groupLabels(labels)
	stability := subsampleStability(dist, sampleGroups, opts)

	groups := make([][]int, len(sampleGroups))
	medoids := make([]Point, len(sampleGroups))
	radii := make([]float64, len(sampleGroups))
	for g, members := range sampleGroups {
		medoid, radius := medoidOf(dist, members)
		medoids[g], radii[g] = sample[medoid], radius
		for _, member := range members {
			groups[g] = append(groups[g], sampleOriginal[member])
		}
	}
	fullLabels := make([]int, len(sorted))
	for i := range fullLabels {
		fullLabels[i] = -1
	}
	for g, members := range groups {
		for _, member := range members {
			fullLabels[member] = g
		}
	}
	for i, point := range sorted {
		if sampled[i] {
			continue
		}
		best, nearest := math.Inf(1), -1
		for g, medoid := range medoids {
			if d := schema.Distance(point, medoid); d < best {
				best, nearest = d, g
			}
		}
		if nearest >= 0 && best <= radii[nearest] {
			fullLabels[i] = nearest
			groups[nearest] = append(groups[nearest], i)
		}
	}

	model := Model{
		Version: ModelVersion, FeatureVersion: featureVersion, Options: opts,
		Schema: schema, Population: len(sorted), Noise: []string{},
	}
	for g, members := range groups {
		ids := make([]string, len(members))
		for i, member := range members {
			ids[i] = sorted[member].ID
		}
		sort.Strings(ids)
		model.Clusters = append(model.Clusters, Cluster{
			Medoid: medoids[g], Members: ids, Radius: radii[g],
			Stability: stability[g], Stable: stability[g] >= opts.StabilityThreshold,
			Distinguishing: distinguishing(schema, sorted, members, opts.TopFeatures),
		})
	}
	sort.Slice(model.Clusters, func(i, j int) bool { return model.Clusters[i].Medoid.ID < model.Clusters[j].Medoid.ID })
	for i := range model.Clusters {
		model.Clusters[i].ID = fmt.Sprintf("c%d", i)
	}
	for i, label := range fullLabels {
		if label < 0 {
			model.Noise = append(model.Noise, sorted[i].ID)
		}
	}
	var err error
	model.ID, err = modelID(model)
	return model, err
}

func validatePoints(points []Point) ([]Point, error) {
	sorted := append([]Point(nil), points...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].ID < sorted[j].ID })
	for i, p := range sorted {
		if p.ID == "" {
			return nil, errors.New("workflowcluster: point id is required")
		}
		if i > 0 && sorted[i-1].ID == p.ID {
			return nil, fmt.Errorf("workflowcluster: duplicate point id %q", p.ID)
		}
		for name, value := range p.Numeric {
			if math.IsNaN(value) || math.IsInf(value, 0) {
				return nil, fmt.Errorf("workflowcluster: point %q feature %q is not finite", p.ID, name)
			}
		}
	}
	return sorted, nil
}

func buildSchema(points []Point) Schema {
	numeric := map[string][2]float64{}
	sets := map[string]bool{}
	for _, p := range points {
		for name := range p.Numeric {
			numeric[name] = [2]float64{}
		}
		for name := range p.Categorical {
			sets[name] = true
		}
	}
	for name := range numeric {
		lo, hi := math.Inf(1), math.Inf(-1)
		for _, p := range points {
			v := p.Numeric[name]
			lo, hi = math.Min(lo, v), math.Max(hi, v)
		}
		numeric[name] = [2]float64{lo, hi}
	}
	wlFeatures := 0
	for name := range numeric {
		if strings.HasPrefix(name, wlFeaturePrefix) {
			wlFeatures++
		}
	}
	var schema Schema
	for name, bounds := range numeric {
		weight := 1.0
		if strings.HasPrefix(name, wlFeaturePrefix) {
			weight = wlGroupWeight / float64(wlFeatures)
		}
		schema.Features = append(schema.Features, SchemaFeature{Name: name, Kind: KindNumeric, Weight: weight, Min: bounds[0], Max: bounds[1]})
	}
	for name := range sets {
		schema.Features = append(schema.Features, SchemaFeature{Name: name, Kind: KindSet, Weight: 1})
	}
	sort.Slice(schema.Features, func(i, j int) bool {
		if schema.Features[i].Kind != schema.Features[j].Kind {
			return schema.Features[i].Kind < schema.Features[j].Kind
		}
		return schema.Features[i].Name < schema.Features[j].Name
	})
	return schema
}

func distanceMatrix(schema Schema, points []Point) [][]float64 {
	d := make([][]float64, len(points))
	for i := range d {
		d[i] = make([]float64, len(points))
	}
	for i := range points {
		for j := i + 1; j < len(points); j++ {
			d[i][j] = schema.Distance(points[i], points[j])
			d[j][i] = d[i][j]
		}
	}
	return d
}

func groupLabels(labels []int) [][]int {
	var groups [][]int
	for i, label := range labels {
		if label < 0 {
			continue
		}
		for len(groups) <= label {
			groups = append(groups, nil)
		}
		groups[label] = append(groups[label], i)
	}
	return groups
}

// medoidOf returns the member minimizing total distance to the others (lowest
// index on ties) and the largest member-to-medoid distance.
func medoidOf(d [][]float64, members []int) (int, float64) {
	best, bestSum := members[0], math.Inf(1)
	for _, m := range members {
		var sum float64
		for _, o := range members {
			sum += d[m][o]
		}
		if sum < bestSum {
			best, bestSum = m, sum
		}
	}
	var radius float64
	for _, m := range members {
		radius = math.Max(radius, d[best][m])
	}
	return best, radius
}

// subsampleStability refits on seeded subsamples and returns, per cluster, the
// mean best-match Jaccard between the cluster (restricted to the subsample) and
// any subsample cluster (Hennig, 2007).
func subsampleStability(d [][]float64, groups [][]int, opts Options) []float64 {
	n := len(d)
	stability := make([]float64, len(groups))
	if len(groups) == 0 {
		return stability
	}
	size := int(math.Ceil(opts.SubsampleFraction * float64(n)))
	rng := rand.New(rand.NewPCG(opts.Seed, opts.Seed^0x9e3779b97f4a7c15))
	counted := make([]int, len(groups))
	for range opts.Resamples {
		sample := rng.Perm(n)[:size]
		sort.Ints(sample)
		sub := make([][]float64, size)
		for i, a := range sample {
			sub[i] = make([]float64, size)
			for j, b := range sample {
				sub[i][j] = d[a][b]
			}
		}
		refit := groupLabels(hdbscan(sub, opts.MinClusterSize, opts.MinSamples, opts.AllowSingleCluster))
		position := make(map[int]int, size)
		for i, a := range sample {
			position[a] = i
		}
		for g, members := range groups {
			inSample := map[int]bool{}
			for _, m := range members {
				if i, ok := position[m]; ok {
					inSample[i] = true
				}
			}
			if len(inSample) == 0 {
				continue
			}
			counted[g]++
			var best float64
			for _, other := range refit {
				shared := 0
				for _, i := range other {
					if inSample[i] {
						shared++
					}
				}
				best = math.Max(best, float64(shared)/float64(len(inSample)+len(other)-shared))
			}
			stability[g] += best
		}
	}
	for g := range stability {
		if counted[g] > 0 {
			stability[g] /= float64(counted[g])
		}
	}
	return stability
}

// distinguishing ranks features by how strongly the cluster differs from every
// other point (including noise).
func distinguishing(schema Schema, points []Point, members []int, top int) []Attribution {
	in := make([]bool, len(points))
	for _, m := range members {
		in[m] = true
	}
	outCount := len(points) - len(members)
	if outCount == 0 {
		return nil
	}
	var out []Attribution
	for _, f := range schema.Features {
		switch f.Kind {
		case KindNumeric:
			span := f.Max - f.Min
			if span <= 0 {
				continue
			}
			var inSum, outSum float64
			for i, p := range points {
				if in[i] {
					inSum += p.Numeric[f.Name]
				} else {
					outSum += p.Numeric[f.Name]
				}
			}
			out = append(out, Attribution{Feature: f.Name, Effect: (inSum/float64(len(members)) - outSum/float64(outCount)) / span})
		case KindSet:
			inFreq, outFreq := map[string]float64{}, map[string]float64{}
			for i, p := range points {
				for _, v := range p.Categorical[f.Name] {
					if in[i] {
						inFreq[v]++
					} else {
						outFreq[v]++
					}
				}
			}
			values := map[string]bool{}
			for v := range inFreq {
				values[v] = true
			}
			for v := range outFreq {
				values[v] = true
			}
			for v := range values {
				out = append(out, Attribution{Feature: f.Name, Value: v, Effect: inFreq[v]/float64(len(members)) - outFreq[v]/float64(outCount)})
			}
		}
	}
	sort.Slice(out, func(i, j int) bool {
		ai, aj := math.Abs(out[i].Effect), math.Abs(out[j].Effect)
		if ai != aj {
			return ai > aj
		}
		if out[i].Feature != out[j].Feature {
			return out[i].Feature < out[j].Feature
		}
		return out[i].Value < out[j].Value
	})
	var kept []Attribution
	for _, a := range out {
		if len(kept) == top || a.Effect == 0 {
			break
		}
		kept = append(kept, a)
	}
	return kept
}

func modelID(m Model) (string, error) {
	m.ID = ""
	raw, err := json.Marshal(m)
	if err != nil {
		return "", fmt.Errorf("workflowcluster: encode model: %w", err)
	}
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

// Assignment places one point against a frozen model.
type Assignment struct {
	// ClusterID is set only when the point falls within a cluster's radius.
	ClusterID string `json:"clusterId,omitempty"`
	// Nearest is the closest cluster regardless of radius.
	Nearest string `json:"nearest,omitempty"`
	// Distance to the nearest medoid; a novelty signal in [0, 1].
	Distance float64 `json:"distance"`
}

// Assign maps p to the frozen model without refitting: O(clusters x features)
// and deterministic. A model with no clusters yields an empty Assignment.
func (m Model) Assign(featureVersion string, p Point) (Assignment, error) {
	if featureVersion != m.FeatureVersion {
		return Assignment{}, fmt.Errorf("%w: model %q, point %q", ErrFeatureVersionMismatch, m.FeatureVersion, featureVersion)
	}
	var result Assignment
	best := math.Inf(1)
	for _, c := range m.Clusters {
		d := m.Schema.Distance(p, c.Medoid)
		if d < best {
			best = d
			result.Nearest = c.ID
			result.ClusterID = ""
			if d <= c.Radius {
				result.ClusterID = c.ID
			}
		}
	}
	if result.Nearest != "" {
		result.Distance = best
	}
	return result, nil
}
