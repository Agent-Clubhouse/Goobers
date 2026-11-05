package workflowcluster

import (
	"math"
	"math/rand/v2"
	"sort"
)

// Evidence labels for a ConfoundReport. Results are observational unless the
// caller compared runs on an identical gym task suite.
const (
	EvidenceObservational = "observational"
	EvidenceGymControlled = "gym-controlled"
)

// NoiseLabel is the assignment label for points HDBSCAN left unclustered.
// Noise counts as its own label so a confound that pushes points out of every
// dense group is still detected.
const NoiseLabel = "noise"

// Strata maps a point ID to its stratum key, such as model, harness version,
// or task cohort joined by the caller. Points without an entry are not tested.
type Strata map[string]string

// ConfoundOptions tunes ConfoundCheck. Zero values select the defaults.
type ConfoundOptions struct {
	// MinStratumSize excludes smaller strata from the test; they are reported
	// as undersized and never pooled into a combined stratum.
	MinStratumSize int `json:"minStratumSize"`
	// Threshold is the Cramér's V at or above which an association counts.
	Threshold float64 `json:"threshold"`
	// Alpha is the permutation p-value at or below which it is significant.
	Alpha float64 `json:"alpha"`
	// Permutations is the size of the label-permutation null.
	Permutations int `json:"permutations"`
	// Seed makes the permutation null deterministic.
	Seed uint64 `json:"seed"`
	// Evidence is EvidenceGymControlled only when strata come from an
	// identical gym task suite; anything else is reported as observational.
	Evidence string `json:"evidence"`
}

func (o ConfoundOptions) withDefaults() ConfoundOptions {
	if o.MinStratumSize <= 0 {
		o.MinStratumSize = 5
	}
	if o.Threshold <= 0 {
		o.Threshold = 0.5
	}
	if o.Alpha <= 0 || o.Alpha >= 1 {
		o.Alpha = 0.05
	}
	if o.Permutations <= 0 {
		o.Permutations = 999
	}
	if o.Evidence != EvidenceGymControlled {
		o.Evidence = EvidenceObservational
	}
	return o
}

// StratumCount is the number of model points in one stratum.
type StratumCount struct {
	Key  string `json:"key"`
	Size int    `json:"size"`
}

// ConfoundReport states whether cluster assignment tracks a stratum label.
type ConfoundReport struct {
	Evidence string          `json:"evidence"`
	Options  ConfoundOptions `json:"options"`
	// Statistic names the association measure; Association is its value.
	Statistic   string  `json:"statistic"`
	Association float64 `json:"association"`
	// PValue is the share of permuted labelings at least as associated,
	// with the add-one correction.
	PValue     float64 `json:"pValue"`
	Confounded bool    `json:"confounded"`
	// Tested counts points in strata large enough to test.
	Tested int `json:"tested"`
	// Unlabeled counts model points with no stratum.
	Unlabeled  int            `json:"unlabeled"`
	Strata     []StratumCount `json:"strata"`
	Undersized []string       `json:"undersized"`
}

// ConfoundCheck measures the association between m's cluster assignment and
// the strata labels with Cramér's V and a seeded permutation null. It flags
// Confounded when V reaches the threshold and is significant at Alpha.
func ConfoundCheck(m Model, strata Strata, opts ConfoundOptions) ConfoundReport {
	opts = opts.withDefaults()
	report := ConfoundReport{
		Evidence: opts.Evidence, Options: opts, Statistic: "cramersV", PValue: 1,
		Strata: []StratumCount{}, Undersized: []string{},
	}

	assignment := map[string]string{}
	for _, c := range m.Clusters {
		for _, id := range c.Members {
			assignment[id] = c.ID
		}
	}
	for _, id := range m.Noise {
		assignment[id] = NoiseLabel
	}
	ids := make([]string, 0, len(assignment))
	for id := range assignment {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	sizes := map[string]int{}
	for _, id := range ids {
		key, ok := strata[id]
		if !ok {
			report.Unlabeled++
			continue
		}
		sizes[key]++
	}
	for key, size := range sizes {
		report.Strata = append(report.Strata, StratumCount{Key: key, Size: size})
		if size < opts.MinStratumSize {
			report.Undersized = append(report.Undersized, key)
		}
	}
	sort.Slice(report.Strata, func(i, j int) bool { return report.Strata[i].Key < report.Strata[j].Key })
	sort.Strings(report.Undersized)

	var clusters, keys []string
	for _, id := range ids {
		key, ok := strata[id]
		if !ok || sizes[key] < opts.MinStratumSize {
			continue
		}
		clusters = append(clusters, assignment[id])
		keys = append(keys, key)
	}
	report.Tested = len(keys)
	rows, cols := indexLabels(clusters), indexLabels(keys)
	if distinct(rows) < 2 || distinct(cols) < 2 {
		return report
	}

	observed := cramersV(rows, cols)
	report.Association = observed
	rng := rand.New(rand.NewPCG(opts.Seed, opts.Seed^0x9e3779b97f4a7c15))
	shuffled := append([]int(nil), cols...)
	atLeast := 0
	for range opts.Permutations {
		rng.Shuffle(len(shuffled), func(i, j int) { shuffled[i], shuffled[j] = shuffled[j], shuffled[i] })
		if cramersV(rows, shuffled) >= observed-1e-12 {
			atLeast++
		}
	}
	report.PValue = float64(atLeast+1) / float64(opts.Permutations+1)
	report.Confounded = observed >= opts.Threshold && report.PValue <= opts.Alpha
	return report
}

// indexLabels maps labels to dense indices in sorted label order.
func indexLabels(labels []string) []int {
	unique := append([]string(nil), labels...)
	sort.Strings(unique)
	index := map[string]int{}
	for _, label := range unique {
		if _, ok := index[label]; !ok {
			index[label] = len(index)
		}
	}
	out := make([]int, len(labels))
	for i, label := range labels {
		out[i] = index[label]
	}
	return out
}

func distinct(indices []int) int {
	most := -1
	for _, i := range indices {
		most = max(most, i)
	}
	return most + 1
}

// cramersV is the bias-uncorrected Cramér's V of two paired label vectors.
func cramersV(rows, cols []int) float64 {
	r, c, n := distinct(rows), distinct(cols), len(rows)
	table := make([]float64, r*c)
	rowSum := make([]float64, r)
	colSum := make([]float64, c)
	for i := range rows {
		table[rows[i]*c+cols[i]]++
		rowSum[rows[i]]++
		colSum[cols[i]]++
	}
	var chi2 float64
	for i := range r {
		for j := range c {
			expected := rowSum[i] * colSum[j] / float64(n)
			diff := table[i*c+j] - expected
			chi2 += diff * diff / expected
		}
	}
	return math.Sqrt(chi2 / (float64(n) * float64(min(r, c)-1)))
}
