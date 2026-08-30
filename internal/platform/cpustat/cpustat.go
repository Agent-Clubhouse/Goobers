package cpustat

import (
	"fmt"
	"math"
	"runtime"
	"strings"
)

// Budget is one point-in-time CPU reading.
//
// Cgroup is nil whenever no container CPU cgroup could be read — a developer
// laptop, a non-Linux host, a cgroup layout this package does not recognize, or
// a restricted mount. That is an ordinary, expected outcome, never an error:
// the host half is always available, so a caller degrades to less detail rather
// than losing the reading.
type Budget struct {
	// HostCPUs is runtime.NumCPU: the logical CPU count of the machine, which
	// is what nproc, os.cpus() and every unaware child process see. It is the
	// number this package exists to stop a container from believing.
	HostCPUs int
	// GOMAXPROCS is the daemon's own scheduler width. Go 1.25+ derives its
	// default from the cgroup quota, so on a constrained pod this reads below
	// HostCPUs — and the gap between the two is precisely what a child process
	// that does not read the cgroup gets wrong.
	GOMAXPROCS int
	// Cgroup is the container CPU cgroup reading, or nil if unavailable.
	Cgroup *Cgroup
}

// Cgroup is the CPU quota the process is scheduled against — the accounting the
// kernel's CFS throttler actually enforces.
type Cgroup struct {
	// QuotaUSec is the microseconds of CPU time the cgroup may consume per
	// period. Zero means unlimited (cgroup v2 writes "max", v1 writes -1); a
	// zero quota must not be used as a numerator.
	QuotaUSec uint64
	// PeriodUSec is the length of the enforcement period, conventionally
	// 100000 (100 ms). Never zero in a reading this package returns.
	PeriodUSec uint64
	// Periods is nr_periods: how many enforcement periods have elapsed.
	Periods uint64
	// ThrottledPeriods is nr_throttled: how many of those periods ended with
	// the cgroup's runnable threads stopped because the quota was spent.
	//
	// This is the term that reads as "the pod is CPU-hungry" on a dashboard
	// while actually meaning the opposite: the work asked for more parallelism
	// than the pod declared, so the kernel stopped it. On the prod AKS instance
	// it was 26328 of 33103 periods (#3963).
	ThrottledPeriods uint64
	// ThrottledUSec is the accumulated CPU time lost to throttling, normalized
	// to microseconds across both cgroup generations.
	ThrottledUSec uint64
}

// Read samples the current budget. It never fails: an unreadable cgroup leaves
// Budget.Cgroup nil, because a diagnostic that can error is a diagnostic a
// caller is tempted to drop from the path that needs it most.
func Read() Budget {
	return read(defaultCgroupRoot)
}

func read(cgroupRoot string) Budget {
	return Budget{
		HostCPUs:   runtime.NumCPU(),
		GOMAXPROCS: runtime.GOMAXPROCS(0),
		Cgroup:     readCgroup(cgroupRoot),
	}
}

// CPUs is the quota expressed as a number of whole-CPU-equivalents, and whether
// that number means anything — it does not when the cgroup is unlimited.
func (c *Cgroup) CPUs() (float64, bool) {
	if c == nil || c.QuotaUSec == 0 || c.PeriodUSec == 0 {
		return 0, false
	}
	return float64(c.QuotaUSec) / float64(c.PeriodUSec), true
}

// ThrottledFraction is the share of elapsed enforcement periods in which the
// cgroup was throttled, and whether any period has elapsed to divide by.
func (c *Cgroup) ThrottledFraction() (float64, bool) {
	if c == nil || c.Periods == 0 {
		return 0, false
	}
	return float64(c.ThrottledPeriods) / float64(c.Periods), true
}

// ThrottledSeconds is ThrottledUSec in seconds — the unit an operator reads.
func (c *Cgroup) ThrottledSeconds() float64 {
	if c == nil {
		return 0
	}
	return float64(c.ThrottledUSec) / 1_000_000
}

// Procs returns the CPU parallelism budget a stage or harness child process
// tree should be given, and whether the cgroup constrains it below the host's
// logical CPU count at all.
//
// The false result is load-bearing, not a convenience: the caller must inject
// nothing when it is false. Setting GOMAXPROCS in a child's environment
// permanently disables the Go runtime's automatic re-detection of the limit
// (runtime.GOMAXPROCS, "Updates"), so pinning a number onto an unconstrained
// host would take capability away in exchange for nothing.
func Procs() (int, bool) {
	return procs(defaultCgroupRoot)
}

func procs(cgroupRoot string) (int, bool) {
	return budgetProcs(runtime.NumCPU(), readCgroup(cgroupRoot))
}

// budgetProcs applies runtime.GOMAXPROCS's own documented default rule to a
// cgroup reading: the average CPU throughput limit is quota/period, rounded UP
// to a whole number, clamped to the logical CPU count, and never below 2 unless
// the host itself is.
//
// Matching that rule exactly is the point. The injected value is then the same
// number a Go 1.25+ runtime would have chosen for itself, so this never fights
// the runtime — it extends the runtime's answer to every child that cannot
// compute it: a Go binary built from a module declaring go < 1.25 (where
// GODEBUG containermaxprocs=0 is still the default), a tool that reads
// GOMAXPROCS without being a Go program's own runtime, and `go build -p` /
// `go test -p`, whose default process fan-out is GOMAXPROCS.
func budgetProcs(hostCPUs int, cgroup *Cgroup) (int, bool) {
	if hostCPUs < 1 {
		return 0, false
	}
	cpus, ok := cgroup.CPUs()
	if !ok {
		return 0, false
	}
	limit := int(math.Ceil(cpus))
	if limit < 1 {
		limit = 1
	}
	if limit > hostCPUs {
		limit = hostCPUs
	}
	if limit < 2 && hostCPUs >= 2 {
		limit = 2
	}
	if limit >= hostCPUs {
		// The quota is at or above what the machine can deliver anyway, so
		// there is nothing to correct and nothing to pin.
		return 0, false
	}
	return limit, true
}

// String renders the budget as one operator-facing clause, in the order a
// responder reads it: the quota against the host count (is this container
// narrower than the machine, and does the daemon know it?), then the throttling
// counters (has that difference already cost anything?).
//
// The throttling term is the reason this clause exists. A container at its CPU
// quota looks identical to a busy one in every point-in-time metric; only
// nr_throttled distinguishes "doing work" from "being stopped".
func (b Budget) String() string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "cpu %d host", b.HostCPUs)
	if b.Cgroup == nil {
		fmt.Fprintf(&sb, ", GOMAXPROCS %d", b.GOMAXPROCS)
		return sb.String()
	}
	if cpus, ok := b.Cgroup.CPUs(); ok {
		fmt.Fprintf(&sb, ", quota %s", formatCPUs(cpus))
	} else {
		sb.WriteString(", quota unlimited")
	}
	fmt.Fprintf(&sb, ", GOMAXPROCS %d", b.GOMAXPROCS)
	if fraction, ok := b.Cgroup.ThrottledFraction(); ok {
		fmt.Fprintf(&sb, ", throttled %.1f%% of %d period(s), %.0fs lost",
			fraction*100, b.Cgroup.Periods, b.Cgroup.ThrottledSeconds())
	}
	return sb.String()
}

// formatCPUs keeps one decimal place so a fractional quota (500m, 2500m) stays
// visible instead of being rounded into a whole CPU it is not.
func formatCPUs(cpus float64) string {
	return fmt.Sprintf("%.1f", cpus)
}
