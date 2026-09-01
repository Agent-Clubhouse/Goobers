package cpustat

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeCgroupFiles(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// The numbers here are the ones measured inside the throttled goobers-api pod
// in #3963: a 3 CPU limit enforced over the conventional 100 ms period, against
// which 26328 of 33103 CFS periods ended throttled for a cumulative 2111 CPU-
// seconds. The whole point of the reading is that those terms are reported at
// all — the pod's own metrics showed only "CPU near the limit" — so the case
// that motivated the package is the case the test asserts.
func TestReadCgroupV2ReportsTheQuotaAndItsThrottling(t *testing.T) {
	root := t.TempDir()
	writeCgroupFiles(t, root, map[string]string{
		"cpu.max":  "300000 100000\n",
		"cpu.stat": "usage_usec 9550117762\nnr_periods 33103\nnr_throttled 26328\nthrottled_usec 2111356121\n",
	})

	cgroup := readCgroup(root)
	if cgroup == nil {
		t.Fatal("readCgroup = nil, want a v2 reading")
	}
	cpus, ok := cgroup.CPUs()
	if !ok || cpus != 3 {
		t.Fatalf("CPUs = %v (ok=%v), want 3", cpus, ok)
	}
	if cgroup.Periods != 33103 || cgroup.ThrottledPeriods != 26328 {
		t.Fatalf("periods = %d throttled = %d, want 33103/26328", cgroup.Periods, cgroup.ThrottledPeriods)
	}
	fraction, ok := cgroup.ThrottledFraction()
	if !ok || fraction < 0.795 || fraction > 0.796 {
		t.Fatalf("ThrottledFraction = %v (ok=%v), want ~0.795", fraction, ok)
	}
	if seconds := cgroup.ThrottledSeconds(); seconds < 2111 || seconds > 2112 {
		t.Fatalf("ThrottledSeconds = %v, want ~2111", seconds)
	}
}

// A v1 host reports the identical quota through two files instead of one and
// accumulates throttled time in NANOseconds under a different key. An operator
// reading a heartbeat must not have to know which generation produced it, so
// the same incident must read the same on both.
func TestReadCgroupV1MatchesTheV2ReadingOfTheSameLimit(t *testing.T) {
	v1Files := map[string]string{
		"cpu.cfs_quota_us":  "300000\n",
		"cpu.cfs_period_us": "100000\n",
		"cpu.stat":          "nr_periods 33103\nnr_throttled 26328\nthrottled_time 2111356121000\n",
	}
	for _, layout := range []string{"cpu", "cpu,cpuacct", "."} {
		t.Run(layout, func(t *testing.T) {
			root := t.TempDir()
			writeCgroupFiles(t, filepath.Join(root, layout), v1Files)

			cgroup := readCgroup(root)
			if cgroup == nil {
				t.Fatalf("readCgroup = nil for the %q layout, want a v1 reading", layout)
			}
			cpus, ok := cgroup.CPUs()
			if !ok || cpus != 3 {
				t.Fatalf("CPUs = %v (ok=%v), want 3", cpus, ok)
			}
			if cgroup.Periods != 33103 || cgroup.ThrottledPeriods != 26328 {
				t.Fatalf("periods = %d throttled = %d, want 33103/26328", cgroup.Periods, cgroup.ThrottledPeriods)
			}
			if seconds := cgroup.ThrottledSeconds(); seconds < 2111 || seconds > 2112 {
				t.Fatalf("ThrottledSeconds = %v, want ~2111 — v1 throttled_time is nanoseconds", seconds)
			}
		})
	}
}

// "max" (v2) and -1 (v1) are the two spellings of "no quota". Both must read as
// a present cgroup with an unlimited quota, not as an absent one and not as a
// quota of zero, which would divide the budget to nothing.
func TestReadCgroupTreatsTheUnlimitedSentinelsAsUnlimited(t *testing.T) {
	for _, tc := range []struct {
		name  string
		dir   string
		files map[string]string
	}{
		{
			name:  "v2 max",
			files: map[string]string{"cpu.max": "max 100000\n", "cpu.stat": "nr_periods 0\nnr_throttled 0\n"},
		},
		{
			name:  "v1 -1",
			dir:   "cpu",
			files: map[string]string{"cpu.cfs_quota_us": "-1\n", "cpu.cfs_period_us": "100000\n"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			writeCgroupFiles(t, filepath.Join(root, tc.dir), tc.files)

			cgroup := readCgroup(root)
			if cgroup == nil {
				t.Fatal("readCgroup = nil, want a reading with an unlimited quota")
			}
			if cpus, ok := cgroup.CPUs(); ok {
				t.Fatalf("CPUs = %v (ok=true), want the unlimited sentinel to report not-ok", cpus)
			}
			if procs, ok := budgetProcs(8, cgroup); ok {
				t.Fatalf("budgetProcs = %d (ok=true), want nothing derived from an unlimited quota", procs)
			}
		})
	}
}

// A developer laptop and a non-Linux host have no cgroup at all. That is the
// common case, and it must degrade to "no reading", never to an error and never
// to a wrong number.
func TestReadCgroupIsNilWhereNoneIsReadable(t *testing.T) {
	for _, tc := range []struct {
		name string
		root string
	}{
		{name: "empty root path", root: ""},
		{name: "no cgroup files", root: t.TempDir()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if cgroup := readCgroup(tc.root); cgroup != nil {
				t.Fatalf("readCgroup = %+v, want nil", cgroup)
			}
		})
	}
}

// A cpu.max whose period is zero or unparseable cannot yield a quota, and a
// wrong quota is worse than no reading: it would be injected as GOMAXPROCS.
func TestReadCgroupV2RefusesAnUnusablePeriod(t *testing.T) {
	for _, body := range []string{"300000 0\n", "300000\n", "300000 notanumber\n", "\n"} {
		root := t.TempDir()
		writeCgroupFiles(t, root, map[string]string{"cpu.max": body})
		if cgroup := readCgroup(root); cgroup != nil {
			t.Fatalf("readCgroup(cpu.max=%q) = %+v, want nil", body, cgroup)
		}
	}
}

// budgetProcs must reproduce runtime.GOMAXPROCS's own documented default rule,
// because the value it produces is handed to children that would otherwise
// compute it themselves. Any divergence would mean two Go processes in one pod
// disagreeing about the same quota.
func TestBudgetProcsMatchesTheRuntimeDefaultRule(t *testing.T) {
	cgroup := func(quotaUSec uint64) *Cgroup {
		return &Cgroup{QuotaUSec: quotaUSec, PeriodUSec: 100000}
	}
	for _, tc := range []struct {
		name     string
		hostCPUs int
		cgroup   *Cgroup
		want     int
		wantOK   bool
	}{
		// The measured incident: a 3 CPU quota on a 4 CPU node.
		{name: "quota below host", hostCPUs: 4, cgroup: cgroup(300000), want: 3, wantOK: true},
		// "If the cgroup CPU throughput limit is not a whole number, the Go
		// runtime rounds up to the next whole number."
		{name: "fractional quota rounds up", hostCPUs: 4, cgroup: cgroup(250000), want: 3, wantOK: true},
		// "it will never set GOMAXPROCS less than 2 unless the logical CPU
		// count ... [is] below 2."
		{name: "sub-CPU quota floors at two", hostCPUs: 4, cgroup: cgroup(50000), want: 2, wantOK: true},
		{name: "sub-CPU quota on a single-CPU host", hostCPUs: 1, cgroup: cgroup(50000), wantOK: false},
		// At or above the host count there is nothing to correct, and pinning
		// GOMAXPROCS would only cost the runtime its automatic re-detection.
		{name: "quota equal to host", hostCPUs: 4, cgroup: cgroup(400000), wantOK: false},
		{name: "quota above host", hostCPUs: 4, cgroup: cgroup(800000), wantOK: false},
		{name: "quota above a smaller host", hostCPUs: 2, cgroup: cgroup(300000), wantOK: false},
		{name: "unlimited quota", hostCPUs: 4, cgroup: cgroup(0), wantOK: false},
		{name: "no cgroup", hostCPUs: 4, wantOK: false},
		{name: "unknown host count", hostCPUs: 0, cgroup: cgroup(300000), wantOK: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := budgetProcs(tc.hostCPUs, tc.cgroup)
			if ok != tc.wantOK || (ok && got != tc.want) {
				t.Fatalf("budgetProcs(%d, %+v) = %d, %v; want %d, %v", tc.hostCPUs, tc.cgroup, got, ok, tc.want, tc.wantOK)
			}
		})
	}
}

// Read must never fail and never invent a cgroup, whatever host it runs on.
func TestReadAlwaysCarriesTheHostHalf(t *testing.T) {
	budget := read(t.TempDir())
	if budget.HostCPUs < 1 || budget.GOMAXPROCS < 1 {
		t.Fatalf("read = %+v, want a usable host half", budget)
	}
	if budget.Cgroup != nil {
		t.Fatalf("read(empty root).Cgroup = %+v, want nil", budget.Cgroup)
	}
	if procs, ok := budgetProcs(budget.HostCPUs, budget.Cgroup); ok {
		t.Fatalf("budgetProcs = %d (ok=true) with no cgroup, want not-ok", procs)
	}
}

// The heartbeat clause is the operator's only view of this; assert the terms
// that make it actionable are actually on the line, with the incident's own
// numbers.
func TestBudgetStringCarriesTheQuotaAndTheThrottlingTerm(t *testing.T) {
	budget := Budget{
		HostCPUs:   4,
		GOMAXPROCS: 3,
		Cgroup: &Cgroup{
			QuotaUSec:        300000,
			PeriodUSec:       100000,
			Periods:          33103,
			ThrottledPeriods: 26328,
			ThrottledUSec:    2111356121,
		},
	}

	line := budget.String()
	for _, want := range []string{"cpu 4 host", "quota 3.0", "GOMAXPROCS 3", "throttled 79.5%", "33103 period(s)", "2111s lost"} {
		if !strings.Contains(line, want) {
			t.Fatalf("Budget.String() = %q, want it to contain %q", line, want)
		}
	}
}

// Without a cgroup the clause still has to say something — a heartbeat with a
// hole in it reads as a bug, and the host count alone is the honest answer.
func TestBudgetStringDegradesWithoutACgroup(t *testing.T) {
	line := Budget{HostCPUs: 8, GOMAXPROCS: 8}.String()
	for _, want := range []string{"cpu 8 host", "GOMAXPROCS 8"} {
		if !strings.Contains(line, want) {
			t.Fatalf("Budget.String() = %q, want it to contain %q", line, want)
		}
	}
	if strings.Contains(line, "throttled") {
		t.Fatalf("Budget.String() = %q, want no throttling term without a cgroup", line)
	}
}

// An unlimited quota is a real reading, not a missing one: say so rather than
// printing nothing or a zero that reads as "no CPU".
func TestBudgetStringNamesAnUnlimitedQuota(t *testing.T) {
	line := Budget{HostCPUs: 8, GOMAXPROCS: 8, Cgroup: &Cgroup{PeriodUSec: 100000}}.String()
	if !strings.Contains(line, "quota unlimited") {
		t.Fatalf("Budget.String() = %q, want it to name the unlimited quota", line)
	}
}
