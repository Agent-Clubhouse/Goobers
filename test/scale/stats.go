package main

import (
	"fmt"
	"runtime"
	"sort"
	"time"
)

// Stat summarizes repeated samples of one measured operation.
//
// Cold and warm are reported separately and deliberately never pooled: design
// §14.12 states separate cold and warm p99.9 targets because in a hosted shape a
// cold read is a deploy, and averaging the first sample into the rest hides
// exactly the cost that matters there.
type Stat struct {
	Op string `json:"op"`
	// Cold is the first sample — the one that pays for cache population,
	// index reconciliation, and page-cache misses.
	Cold time.Duration `json:"coldNanos"`
	// Samples counts the warm samples only (all but the first).
	Samples int           `json:"warmSamples"`
	P50     time.Duration `json:"warmP50Nanos"`
	P99     time.Duration `json:"warmP99Nanos"`
	// P999 is populated only when Samples is large enough for the figure to
	// mean anything (see minSamplesForP999). WarmP999Valid says whether it is.
	P999      time.Duration `json:"warmP999Nanos"`
	P999Valid bool          `json:"warmP999Valid"`
	Max       time.Duration `json:"warmMaxNanos"`
}

// minSamplesForP999 is the smallest warm sample count from which a p99.9 can be
// read without inventing precision.
//
// This is a correctness constraint on the acceptance bar, not a style
// preference. A p99.9 is the 999th of 1000 ordered observations; from 20 samples
// the nearest rank to 0.999 is simply the maximum, so reporting "p99.9" there
// reports the max under a name that implies a tail estimate. §14.12's targets
// are stated as p99.9 and are meant to be falsifiable, so the harness either
// takes enough samples to support the claim or says it did not.
const minSamplesForP999 = 1000

// summarize builds a Stat from an ordered-by-collection slice of samples. The
// first element is treated as the cold sample; the remainder are warm.
func summarize(op string, samples []time.Duration) Stat {
	if len(samples) == 0 {
		return Stat{Op: op}
	}
	st := Stat{Op: op, Cold: samples[0]}
	warm := samples[1:]
	if len(warm) == 0 {
		return st
	}
	sorted := make([]time.Duration, len(warm))
	copy(sorted, warm)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })

	st.Samples = len(sorted)
	st.P50 = percentile(sorted, 0.50)
	st.P99 = percentile(sorted, 0.99)
	st.Max = sorted[len(sorted)-1]
	if len(sorted) >= minSamplesForP999 {
		st.P999 = percentile(sorted, 0.999)
		st.P999Valid = true
	}
	return st
}

// percentile returns the nearest-rank percentile of an ascending slice.
func percentile(sorted []time.Duration, q float64) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	// Nearest-rank: ceil(q*N) clamped into range, 1-indexed.
	rank := int(q*float64(len(sorted)) + 0.9999999)
	if rank < 1 {
		rank = 1
	}
	if rank > len(sorted) {
		rank = len(sorted)
	}
	return sorted[rank-1]
}

// Host records where a measurement was taken.
//
// §14.12's targets are declared "on the reference benchmark host" and no such
// host was ever defined, which made every number in the acceptance bar
// unattributable — a p99.9 from a contended three-way-sharded CI runner and one
// from a quiet workstation are not comparable, and neither can defend a target.
// Recording the host with the numbers is the minimum that makes the comparison
// legitimate; declaring which host is the reference is a separate, human act.
type Host struct {
	GOOS    string `json:"goos"`
	GOARCH  string `json:"goarch"`
	NumCPU  int    `json:"numCPU"`
	GoVer   string `json:"goVersion"`
	MemMiB  uint64 `json:"memAvailableMiB"`
	CI      bool   `json:"ci"`
	NoFsync bool   `json:"fsyncDisabled"`
}

// describeHost captures the attribution fields for a measurement report.
func describeHost(ci, noFsync bool) Host {
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	return Host{
		GOOS:   runtime.GOOS,
		GOARCH: runtime.GOARCH,
		NumCPU: runtime.NumCPU(),
		GoVer:  runtime.Version(),
		MemMiB: ms.Sys / (1 << 20),
		CI:     ci,
		// NoFsync is load-bearing attribution, not trivia: generating with
		// fsync disabled is ~18× faster (measured 124 vs 6.9 runs/s) and
		// changes the durability behavior some fixtures exist to test, so a
		// corpus built that way must say so wherever its numbers are quoted.
		NoFsync: noFsync,
	}
}

// String renders a one-line host attribution for the text report.
func (h Host) String() string {
	s := fmt.Sprintf("%s/%s cpu=%d %s", h.GOOS, h.GOARCH, h.NumCPU, h.GoVer)
	if h.CI {
		s += " ci=true"
	}
	if h.NoFsync {
		s += " fsync=disabled"
	}
	return s
}
