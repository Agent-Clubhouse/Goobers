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

// memoryPressureWindow is how long a rise in the cgroup's at-limit counter
// keeps the gate armed.
//
// THIS WINDOW IS WHAT STOPS THE GATE LATCHING SHUT, and that is a real failure
// mode rather than a hypothetical one. memory.current includes reclaimable
// page cache, and the kernel only reclaims under allocation pressure. A cgroup
// holding a large idle build cache therefore sits above any high-water mark
// indefinitely. Gating on that number alone is self-sustaining: the gate
// refuses every run, nothing allocates, nothing is reclaimed, the number never
// falls, and a pod that used to OOM occasionally instead idles forever — which
// is a worse outage than the one being fixed, and a much harder one to read.
//
// So a high reading is necessary but not sufficient. The cgroup must ALSO have
// been driven against its limit recently, which is what memory.events `max`
// counts. That only rises when something is actually allocating faster than
// the kernel can reclaim — the precise condition an OOM kill comes out of.
// A quiescent cgroup full of cache never trips it, and admitting a run there
// is correct: the cache is reclaimable and the run will reclaim it.
//
// 60s spans the incident pod's observed rate (roughly 2 per minute, sustained)
// with room for the gaps between bursts, so a burst in progress keeps the gate
// armed across consecutive ticks instead of flapping open between two
// increments.
const memoryPressureWindow = time.Minute

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
	// atLimit is the last observed memory.events `max` counter, and
	// atLimitRoseAt when it was last seen to increase. Together they answer
	// "is the cgroup being driven against its limit right now?", which
	// memory.current alone cannot — see memoryPressureWindow.
	atLimit       uint64
	haveAtLimit   bool
	atLimitRoseAt time.Time
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

// UnderPressure implements MemoryGate. It refuses only when BOTH terms hold:
// the cgroup is above the high-water mark, and it has been driven against its
// limit within memoryPressureWindow. See that constant for why the second term
// is not optional.
func (g *CgroupMemoryGate) UnderPressure() (bool, string) {
	if g == nil {
		return false, ""
	}
	footprint, rising := g.sample()
	used, ok := footprint.Cgroup.UsedFraction()
	// No cgroup, or one with no limit set: there is no ceiling to be near, so
	// there is nothing to refuse. Fail open, like every other condition does
	// on missing wiring.
	if !ok || used < g.highWater || !rising {
		return false, ""
	}
	return true, fmt.Sprintf("%s at %.0f%% of limit (threshold %.0f%%), %d at-limit episode(s); %s",
		memstat.FormatBytes(footprint.Cgroup.Current), used*100, g.highWater*100,
		footprint.Cgroup.AtLimit, footprint.Cgroup.Breakdown())
}

// sample returns the current footprint and whether the cgroup's at-limit
// counter has risen within memoryPressureWindow.
func (g *CgroupMemoryGate) sample() (memstat.Footprint, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	now := g.now()
	if !g.haveCached || now.Sub(g.cachedAt) >= memoryGateSampleTTL {
		g.cached = g.read()
		g.cachedAt = now
		g.haveCached = true
		g.observeAtLimit(now)
	}
	rising := g.haveAtLimit && !g.atLimitRoseAt.IsZero() &&
		now.Sub(g.atLimitRoseAt) <= memoryPressureWindow
	return g.cached, rising
}

// observeAtLimit records a rise in the at-limit counter. The first observation
// only establishes a baseline: a counter is monotonic for the life of the
// cgroup, so its absolute value says how much pressure there has been since
// the container started, not how much there is now. Only a change between two
// readings carries that.
func (g *CgroupMemoryGate) observeAtLimit(now time.Time) {
	// No cgroup, or a kernel that does not export the counter (cgroup v1, or
	// v2 without memory.events): there is no rise to observe, so the gate
	// cannot arm and every run is admitted. That is deliberate — the
	// threshold alone is not a safe refusal signal (see memoryPressureWindow)
	// and refusing on it would risk the latch this whole term exists to
	// prevent. The cost is that the gate is inert on those kernels, which
	// memstat surfaces on every heartbeat as "at-limit counter unavailable"
	// rather than leaving it to be inferred from a permanently-zero count.
	if g.cached.Cgroup == nil || !g.cached.Cgroup.AtLimitKnown {
		g.haveAtLimit = false
		return
	}
	current := g.cached.Cgroup.AtLimit
	if g.haveAtLimit && current > g.atLimit {
		g.atLimitRoseAt = now
	}
	g.atLimit = current
	g.haveAtLimit = true
}
