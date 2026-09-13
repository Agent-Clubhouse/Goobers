package localscheduler

import (
	"fmt"
	"sync"
	"time"

	"github.com/goobers/goobers/internal/platform/diskstat"
)

// ReasonStorageCritical prefixes the refusal of a new run because the
// filesystem containing the instance root has crossed the critical low-disk
// floor (#4873). Stable and grep-able like the other Reason* constants
// above, so a journal reader can match on it without parsing prose.
const ReasonStorageCritical = "conditions: storage-critical"

// DiskGate reports whether new-run admission should be refused because the
// instance root's filesystem is critically low on free space. Mirrors
// MemoryGate's contract exactly: cheap, non-blocking, safe to call while
// Conditions holds its own lock — sampling the filesystem happens elsewhere,
// on its own schedule (see StorageGate.Sample), not on the admission path.
type DiskGate interface {
	UnderPressure() (bool, string)
}

// StorageTier identifies tiered low-disk protection's current state (#4873).
type StorageTier int

const (
	// StorageHealthy means free space is above both floors.
	StorageHealthy StorageTier = iota
	// StorageWarning means free space is below the warning floor but at or
	// above the critical floor: storage health is degraded, but new runs are
	// still admitted.
	StorageWarning
	// StorageCritical means free space is below the critical floor: new-run
	// admission and dispatch are stopped, though everything else (journal
	// appends, stage surrender, terminalization, recovery, cleanup,
	// retention, diagnostics, operator commands) keeps running so existing
	// work can drain and reclaim space.
	StorageCritical
	// StorageMeasurementUnavailable means the most recent sample failed —
	// unsupported platform, or an unreadable instance root. This is
	// observable but always fails open: an unmeasurable filesystem never
	// refuses admission and never takes the daemon down.
	StorageMeasurementUnavailable
)

// String renders the tier the way goobers status and the Instance API name
// it — "admission-stopped" for StorageCritical rather than "critical", since
// that is the externally observable behavior the tier actually causes.
func (t StorageTier) String() string {
	switch t {
	case StorageWarning:
		return "warning"
	case StorageCritical:
		return "admission-stopped"
	case StorageMeasurementUnavailable:
		return "measurement-unavailable"
	default:
		return "healthy"
	}
}

// hysteresisFraction is the extra headroom, above the critical floor, that
// free space must climb back past before admission resumes (#4873: "use
// hysteresis between stop and resume thresholds to prevent scheduling flaps
// near the boundary"). Reclaiming space is rarely a clean single step — a
// retention sweep or a completing run's own cleanup frees it in bursts — so
// resuming exactly at the floor would flap open and shut on that noise. 10%
// is a fixed implementation margin, not a policy knob: unlike the floors
// themselves, an operator has no reason to tune how much daylight separates
// stop from resume.
const hysteresisFraction = 0.10

// StorageGate is the DiskGate backed by a periodic free-space sample of the
// filesystem containing path. It is also the source of the StorageHealthStats
// goobers status and the Instance API report — the gate and the report read
// exactly the same sampled state, so they cannot disagree about what tier the
// daemon is in.
type StorageGate struct {
	path string

	warningFloorBytes    int64
	warningFloorPercent  float64
	criticalFloorBytes   int64
	criticalFloorPercent float64

	read func(string) (diskstat.Footprint, error)
	now  func() time.Time

	mu         sync.Mutex
	tier       StorageTier
	footprint  diskstat.Footprint
	measuredAt time.Time
	lastErr    string
}

// NewStorageGate returns a gate for the filesystem containing path. A
// non-positive floor disables that floor's check (both percent forms and
// both byte forms may be zero — see StorageHealthConfig). The gate starts in
// StorageMeasurementUnavailable until Sample is called for the first time,
// which the daemon does once at startup before wiring the gate in (#4873's
// "measure ... at startup").
func NewStorageGate(path string, warningFloorBytes int64, warningFloorPercent float64, criticalFloorBytes int64, criticalFloorPercent float64) *StorageGate {
	return &StorageGate{
		path:                 path,
		warningFloorBytes:    warningFloorBytes,
		warningFloorPercent:  warningFloorPercent,
		criticalFloorBytes:   criticalFloorBytes,
		criticalFloorPercent: criticalFloorPercent,
		read:                 diskstat.Read,
		now:                  time.Now,
		tier:                 StorageMeasurementUnavailable,
	}
}

// Sample takes one free-space reading and updates the gate's tier. Called
// once at startup and then on a ticker for the life of the daemon (#4873's
// "startup and periodically") — never on the admission path itself, so
// UnderPressure stays a cheap in-memory read regardless of how expensive a
// stat call is on a given filesystem. Returns the resulting tier and whether
// it differs from the tier before this sample, so a caller can emit
// deduplicated status/log/telemetry signals only on an actual transition
// (#4873: "emit deduplicated status, log and telemetry signals").
func (g *StorageGate) Sample() (tier StorageTier, changed bool) {
	if g == nil {
		return StorageMeasurementUnavailable, false
	}
	footprint, err := g.read(g.path)

	g.mu.Lock()
	defer g.mu.Unlock()
	previous := g.tier
	g.measuredAt = g.now()
	if err != nil {
		// Fail open (#4873: "failure to measure capacity is observable but
		// fail-open; it must not take down the daemon"): an unmeasurable
		// filesystem must never itself refuse admission, so the tier goes to
		// StorageMeasurementUnavailable rather than latching whatever tier
		// was last observed.
		g.tier = StorageMeasurementUnavailable
		g.lastErr = err.Error()
		return g.tier, g.tier != previous
	}
	g.lastErr = ""
	g.footprint = footprint
	g.tier = nextTier(g.tier, footprint, g.warningFloorBytes, g.warningFloorPercent, g.criticalFloorBytes, g.criticalFloorPercent)
	return g.tier, g.tier != previous
}

// nextTier computes the tier for one reading given the previous tier, so the
// critical-to-non-critical transition alone can apply hysteresisFraction.
// Every other transition (healthy<->warning, anything->critical) reacts
// immediately: hysteresis exists only to stop the specific flap #4873 calls
// out — repeatedly stopping and resuming admission right at the resume edge —
// not to delay warning signals, which cost nothing to flap on.
func nextTier(previous StorageTier, footprint diskstat.Footprint, warningFloorBytes int64, warningFloorPercent float64, criticalFloorBytes int64, criticalFloorPercent float64) StorageTier {
	criticalFloor := effectiveFloor(criticalFloorBytes, criticalFloorPercent, footprint.TotalBytes)
	warningFloor := effectiveFloor(warningFloorBytes, warningFloorPercent, footprint.TotalBytes)
	available := footprint.AvailableBytes

	if previous == StorageCritical && criticalFloor > 0 {
		resumeAt := criticalFloor + uint64(float64(criticalFloor)*hysteresisFraction)
		if available < resumeAt {
			return StorageCritical
		}
	} else if criticalFloor > 0 && available < criticalFloor {
		return StorageCritical
	}

	if warningFloor > 0 && available < warningFloor {
		return StorageWarning
	}
	return StorageHealthy
}

// effectiveFloor combines an absolute and a percentage floor, both optional:
// a non-positive value disables that form of the check. When both are set,
// the higher of the two applies (StorageHealthConfig's doc comment: an
// operator opting into both wants the stricter enforced). Zero means "no
// floor" for this tier.
func effectiveFloor(floorBytes int64, floorPercent float64, totalBytes uint64) uint64 {
	var floor uint64
	if floorBytes > 0 {
		floor = uint64(floorBytes)
	}
	if floorPercent > 0 {
		percentFloor := uint64(floorPercent / 100 * float64(totalBytes))
		if percentFloor > floor {
			floor = percentFloor
		}
	}
	return floor
}

// UnderPressure implements DiskGate. Only StorageCritical refuses admission —
// StorageWarning degrades health without refusing anything, and
// StorageMeasurementUnavailable fails open like every other condition on
// missing or broken wiring.
func (g *StorageGate) UnderPressure() (bool, string) {
	if g == nil {
		return false, ""
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.tier != StorageCritical {
		return false, ""
	}
	return true, fmt.Sprintf("%s free of %s available on %s (floor %s)",
		diskstat.FormatBytes(g.footprint.AvailableBytes), diskstat.FormatBytes(g.footprint.TotalBytes), g.path,
		diskstat.FormatBytes(uint64(g.criticalFloorBytes)))
}

// StorageHealthStats is a race-safe snapshot of the gate's current state, for
// goobers status and the Instance API (#4873): "status/API output must
// distinguish warning, admission-stopped, measurement-unavailable and
// healthy states and include free bytes plus effective thresholds."
type StorageHealthStats struct {
	Tier                 StorageTier
	Path                 string
	FreeBytes            uint64
	TotalBytes           uint64
	WarningFloorBytes    int64
	WarningFloorPercent  float64
	CriticalFloorBytes   int64
	CriticalFloorPercent float64
	MeasuredAt           time.Time
	Error                string
}

// Stats returns a race-safe snapshot of the gate's current state.
func (g *StorageGate) Stats() StorageHealthStats {
	if g == nil {
		return StorageHealthStats{Tier: StorageMeasurementUnavailable}
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return StorageHealthStats{
		Tier:                 g.tier,
		Path:                 g.path,
		FreeBytes:            g.footprint.AvailableBytes,
		TotalBytes:           g.footprint.TotalBytes,
		WarningFloorBytes:    g.warningFloorBytes,
		WarningFloorPercent:  g.warningFloorPercent,
		CriticalFloorBytes:   g.criticalFloorBytes,
		CriticalFloorPercent: g.criticalFloorPercent,
		MeasuredAt:           g.measuredAt,
		Error:                g.lastErr,
	}
}
