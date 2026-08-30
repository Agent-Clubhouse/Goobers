package localscheduler

import (
	"fmt"
	"sync"
	"time"

	"github.com/goobers/goobers/internal/platform/memstat"
)

// MemoryGate reports whether the daemon's own memory cgroup is close enough to
// its limit that starting another run risks an OOM kill.
//
// It exists because of #3949, where the daemon was killed with exit 137 while
// its own heap held 4.7Mi. The memory that filled the cgroup was not the
// daemon's: it was the page cache and the anonymous memory of the CI child
// processes the daemon itself starts on-pod. No per-workflow readiness
// condition can see that, because it is not a property of any workflow — it is
// a property of the container every admitted run will share.
//
// Like OpenPRCounter and ProviderQuotaGate, an unwired gate is not enforced.
type MemoryGate interface {
	// UnderPressure reports whether admission should be refused, and if so a
	// human-readable measurement to append to the skip reason. It must be
	// cheap and non-blocking: Conditions calls it while holding its own lock.
	UnderPressure() (bool, string)
}

// defaultMemoryHighWater is the fraction of the cgroup's memory limit above
// which CgroupMemoryGate refuses new runs.
//
// 0.90 is chosen from the #3949 measurements rather than by taste. That pod
// sat at 0.76 during ordinary operation, so a lower bound would refuse runs in
// the steady state and turn a memory problem into an availability one. Its
// fatal burst crossed 0.99. 0.90 is above the noise and below the cliff, and
// it leaves ~1Gi of headroom on the 10Gi limit — roughly one CI child's
// anonymous footprint, which is the allocation this gate exists to survive.
const defaultMemoryHighWater = 0.90

// memoryGateSampleTTL bounds how often the gate re-reads the cgroup. Admission
// runs per dispatchable entry per tick, and this read happens under
// Conditions' lock, so a tick with many entries must not turn into many file
// reads. One reading per TTL is ample: the quantity is a slow-moving
// aggregate, not a per-run value.
const memoryGateSampleTTL = time.Second

// CgroupMemoryGate is the MemoryGate backed by the process's own memory
// cgroup. Outside a container — a developer machine, most CI — there is no
// cgroup to read and the gate never refuses anything.
type CgroupMemoryGate struct {
	highWater float64
	// read is the sampling function, replaced in tests. It returns the same
	// footprint the heartbeat prints, so the gate and the log an operator
	// reads to explain it cannot disagree.
	read func() memstat.Footprint
	now  func() time.Time

	mu         sync.Mutex
	cached     memstat.Footprint
	cachedAt   time.Time
	haveCached bool
}

// NewCgroupMemoryGate returns a gate refusing admission above highWater, a
// fraction of the cgroup's memory limit. A highWater outside (0, 1] is
// replaced by defaultMemoryHighWater — a zero or negative threshold would
// refuse every run, which is a worse failure than the one this guards against.
func NewCgroupMemoryGate(highWater float64) *CgroupMemoryGate {
	if highWater <= 0 || highWater > 1 {
		highWater = defaultMemoryHighWater
	}
	return &CgroupMemoryGate{highWater: highWater, read: memstat.Read, now: time.Now}
}

// UnderPressure implements MemoryGate.
func (g *CgroupMemoryGate) UnderPressure() (bool, string) {
	if g == nil {
		return false, ""
	}
	footprint := g.sample()
	used, ok := footprint.Cgroup.UsedFraction()
	// No cgroup, or one with no limit set: there is no ceiling to be near, so
	// there is nothing to refuse. Fail open, like every other condition does
	// on missing wiring.
	if !ok || used < g.highWater {
		return false, ""
	}
	return true, fmt.Sprintf("%s at %.0f%% of limit (threshold %.0f%%); %s",
		memstat.FormatBytes(footprint.Cgroup.Current), used*100, g.highWater*100, footprint.Cgroup.Breakdown())
}

func (g *CgroupMemoryGate) sample() memstat.Footprint {
	g.mu.Lock()
	defer g.mu.Unlock()
	now := g.now()
	if g.haveCached && now.Sub(g.cachedAt) < memoryGateSampleTTL {
		return g.cached
	}
	g.cached = g.read()
	g.cachedAt = now
	g.haveCached = true
	return g.cached
}
