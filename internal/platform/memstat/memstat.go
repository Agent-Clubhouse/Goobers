package memstat

import (
	"fmt"
	"runtime/metrics"
	"strings"
)

// Footprint is one point-in-time memory reading.
//
// Cgroup is nil whenever no container memory cgroup could be read — a developer
// laptop, a non-Linux host, a cgroup layout this package does not recognize, or
// a restricted mount. That is an ordinary, expected outcome, never an error:
// the runtime half is always available, so a caller degrades to less detail
// rather than losing the reading.
type Footprint struct {
	// HeapBytes is live heap objects: the term that grows under a classic Go
	// leak (retained references) and the first thing to look at.
	HeapBytes uint64
	// RetainedBytes is all memory the Go runtime holds from the OS minus what
	// it has released back — heap plus stacks, plus the runtime's own
	// metadata. It is the runtime-side ceiling HeapBytes lives inside, and the
	// gap between the two is fragmentation and unreturned spans.
	RetainedBytes uint64
	// Goroutines counts live goroutines. It separates a leak of *work* (a
	// worker or watcher started per run and never stopped, each pinning its
	// captured state) from a leak of *data*, which HeapBytes alone cannot do.
	Goroutines uint64
	// Cgroup is the container memory cgroup reading, or nil if unavailable.
	Cgroup *Cgroup
}

// Cgroup is the memory cgroup the process is charged against — the accounting
// the kernel OOM killer actually enforces.
type Cgroup struct {
	// Current is total charge against the limit: anonymous memory, page
	// cache, kernel memory, and every child process in the same cgroup.
	Current uint64
	// Limit is the hard limit. Zero means unlimited (cgroup v2 writes "max",
	// v1 writes a sentinel near the word size); a zero Limit must not be
	// used as a divisor.
	Limit uint64
	// Anon is anonymous memory: heap, stacks and private mappings, summed
	// across every process in the cgroup. This is the term an application
	// leak grows, and the one to compare against Footprint.HeapBytes.
	Anon uint64
	// File is page cache: the kernel's cache of file contents read or
	// written by anything in the cgroup, charged to it in full.
	//
	// It is reclaimable, so it is easy to dismiss — but reclaim is not
	// instant. A cgroup whose page cache already sits near the limit will
	// be OOM-killed by an allocation burst that arrives faster than the
	// kernel can write back and evict, which is exactly the shape of #3949.
	File uint64
}

// runtime metric names. Sampled through runtime/metrics rather than
// runtime.ReadMemStats because ReadMemStats stops the world; this runs on a
// timer inside a live daemon, where a periodic pause is not acceptable for a
// diagnostic that must be cheap enough to always be on.
const (
	metricHeapObjects  = "/memory/classes/heap/objects:bytes"
	metricTotal        = "/memory/classes/total:bytes"
	metricHeapReleased = "/memory/classes/heap/released:bytes"
	metricGoroutines   = "/sched/goroutines:goroutines"
)

// Read samples the current footprint. It never fails: an unreadable cgroup
// leaves Footprint.Cgroup nil, because a diagnostic that can error is a
// diagnostic a caller is tempted to drop from the path that needs it most.
func Read() Footprint {
	return read(defaultCgroupRoot)
}

func read(cgroupRoot string) Footprint {
	samples := []metrics.Sample{
		{Name: metricHeapObjects},
		{Name: metricTotal},
		{Name: metricHeapReleased},
		{Name: metricGoroutines},
	}
	metrics.Read(samples)

	footprint := Footprint{
		HeapBytes:  uint64Value(samples[0]),
		Goroutines: uint64Value(samples[3]),
	}
	// Released memory is still mapped but handed back to the OS, so it is not
	// part of what the runtime retains. Guard the subtraction: the two samples
	// are read together but the arithmetic must not underflow if a future
	// runtime reports them inconsistently.
	total, released := uint64Value(samples[1]), uint64Value(samples[2])
	if total >= released {
		footprint.RetainedBytes = total - released
	} else {
		footprint.RetainedBytes = total
	}
	footprint.Cgroup = readCgroup(cgroupRoot)
	return footprint
}

func uint64Value(sample metrics.Sample) uint64 {
	if sample.Value.Kind() != metrics.KindUint64 {
		return 0
	}
	return sample.Value.Uint64()
}

// UsedFraction is Current as a fraction of Limit, and whether that fraction
// means anything — it does not when the cgroup is unlimited or unread.
func (c *Cgroup) UsedFraction() (float64, bool) {
	if c == nil || c.Limit == 0 {
		return 0, false
	}
	return float64(c.Current) / float64(c.Limit), true
}

// String renders the footprint as one operator-facing clause.
//
// The field order is the order a responder reads them in: heap and goroutines
// first (is the daemon itself growing?), then the cgroup total against the
// limit (how close is the kill?), then the anon/cache split that says which of
// the two the pressure is actually made of.
func (f Footprint) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "heap %s, retained %s, %d goroutine(s)",
		FormatBytes(f.HeapBytes), FormatBytes(f.RetainedBytes), f.Goroutines)
	if f.Cgroup == nil {
		return b.String()
	}
	b.WriteString(", cgroup " + FormatBytes(f.Cgroup.Current))
	if fraction, ok := f.Cgroup.UsedFraction(); ok {
		fmt.Fprintf(&b, "/%s (%.0f%%)", FormatBytes(f.Cgroup.Limit), fraction*100)
	} else {
		b.WriteString("/unlimited")
	}
	fmt.Fprintf(&b, " = anon %s + cache %s",
		FormatBytes(f.Cgroup.Anon), FormatBytes(f.Cgroup.File))
	return b.String()
}

// FormatBytes renders a byte count in binary units, keeping one decimal place
// below 10 units so a slow climb stays visible between two consecutive
// heartbeats instead of being rounded flat.
func FormatBytes(n uint64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%dB", n)
	}
	value := float64(n)
	suffixes := []string{"Ki", "Mi", "Gi", "Ti", "Pi"}
	var suffix string
	for _, candidate := range suffixes {
		value /= unit
		suffix = candidate
		if value < unit {
			break
		}
	}
	if value < 10 {
		return fmt.Sprintf("%.1f%s", value, suffix)
	}
	return fmt.Sprintf("%.0f%s", value, suffix)
}
