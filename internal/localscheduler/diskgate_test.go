package localscheduler

import (
	"errors"
	"strings"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/platform/diskstat"
)

// gateWith builds a StorageGate whose sampled reading is scripted, so
// threshold arithmetic is tested without depending on the machine the test
// runs on. Each call to Sample consumes the next reading in readings; a nil
// entry simulates a measurement failure.
func gateWith(warningBytes int64, warningPercent float64, criticalBytes int64, criticalPercent float64, readings ...*diskstat.Footprint) *StorageGate {
	gate := NewStorageGate("/instance-root", warningBytes, warningPercent, criticalBytes, criticalPercent)
	i := 0
	gate.read = func(string) (diskstat.Footprint, error) {
		reading := readings[i]
		if i < len(readings)-1 {
			i++
		}
		if reading == nil {
			return diskstat.Footprint{}, errors.New("diskstat: simulated failure")
		}
		return *reading, nil
	}
	return gate
}

func footprintAt(available, total uint64) *diskstat.Footprint {
	return &diskstat.Footprint{AvailableBytes: available, TotalBytes: total, FreeBytes: available}
}

func TestStorageGateStartsMeasurementUnavailableBeforeFirstSample(t *testing.T) {
	gate := NewStorageGate("/instance-root", 0, 10, 0, 5)
	if stats := gate.Stats(); stats.Tier != StorageMeasurementUnavailable {
		t.Fatalf("Tier = %v before first Sample, want StorageMeasurementUnavailable", stats.Tier)
	}
	if pressured, _ := gate.UnderPressure(); pressured {
		t.Fatal("UnderPressure() = true before first Sample, want fail-open")
	}
}

func TestStorageGateThresholdCrossing(t *testing.T) {
	const total = 100 * 1024 * 1024 * 1024 // 100Gi
	for name, tc := range map[string]struct {
		available uint64
		wantTier  StorageTier
	}{
		"well above both floors": {available: 50 << 30, wantTier: StorageHealthy},
		"just above warning":     {available: 11 << 30, wantTier: StorageHealthy},
		"below warning":          {available: 8 << 30, wantTier: StorageWarning},
		"just below critical":    {available: 5<<30 - 1, wantTier: StorageCritical},
		"deep in critical":       {available: 1 << 30, wantTier: StorageCritical},
	} {
		t.Run(name, func(t *testing.T) {
			// Percent-only floors: 10% warning, 5% critical of a 100Gi disk.
			gate := gateWith(0, 10, 0, 5, footprintAt(tc.available, total))
			gate.Sample()
			if stats := gate.Stats(); stats.Tier != tc.wantTier {
				t.Fatalf("Tier = %v, want %v (available=%d)", stats.Tier, tc.wantTier, tc.available)
			}
		})
	}
}

func TestStorageGateByteFloorTakesPrecedenceWhenStricter(t *testing.T) {
	const total = 100 * 1024 * 1024 * 1024
	// 5% of 100Gi is 5Gi, but an explicit 10Gi byte floor is stricter and must
	// win — StorageHealthConfig's doc comment: the higher of the two applies.
	gate := gateWith(0, 0, 10<<30, 5, footprintAt(8<<30, total))
	gate.Sample()
	if stats := gate.Stats(); stats.Tier != StorageCritical {
		t.Fatalf("Tier = %v, want StorageCritical when the byte floor is stricter than the percent floor", stats.Tier)
	}
}

func TestStorageGateUnderPressureReportsTheEffectiveFloorNotTheRawBytesConfig(t *testing.T) {
	const total = 100 * 1024 * 1024 * 1024 // 100Gi
	// Percent-only critical floor (5% of 100Gi = 5Gi): criticalFloorBytes is
	// zero, so the detail message must report the resolved 5Gi floor, not the
	// raw zero config value.
	gate := gateWith(0, 0, 0, 5, footprintAt(1<<30, total))
	gate.Sample()
	_, detail := gate.UnderPressure()
	if !strings.Contains(detail, "5.0Gi") {
		t.Fatalf("detail = %q, want it to report the effective 5Gi floor, not the unset criticalFloorBytes", detail)
	}
}

func TestStorageGateHysteresisHoldsCriticalUntilPastTheResumeMargin(t *testing.T) {
	const total = 100 * 1024 * 1024 * 1024
	criticalFloor := uint64(5 << 30)
	resumeAt := criticalFloor + uint64(float64(criticalFloor)*hysteresisFraction)

	gate := gateWith(0, 0, int64(criticalFloor), 0,
		footprintAt(1<<30, total), // deep critical
	)
	gate.Sample()
	if stats := gate.Stats(); stats.Tier != StorageCritical {
		t.Fatalf("Tier = %v, want StorageCritical", stats.Tier)
	}

	// Free space climbs back above the floor but not past the hysteresis
	// margin: must still refuse admission, or the gate would flap right at
	// the boundary (#4873).
	gate.read = func(string) (diskstat.Footprint, error) {
		return *footprintAt(criticalFloor+1, total), nil
	}
	gate.Sample()
	if stats := gate.Stats(); stats.Tier != StorageCritical {
		t.Fatalf("Tier = %v just above the floor, want StorageCritical to hold under hysteresis", stats.Tier)
	}

	// Free space climbs past the resume margin: now it may recover.
	gate.read = func(string) (diskstat.Footprint, error) {
		return *footprintAt(resumeAt+1, total), nil
	}
	gate.Sample()
	if stats := gate.Stats(); stats.Tier == StorageCritical {
		t.Fatalf("Tier = %v past the resume margin, want recovery out of StorageCritical", stats.Tier)
	}
}

func TestStorageGateWarningDoesNotUseHysteresis(t *testing.T) {
	const total = 100 * 1024 * 1024 * 1024
	gate := gateWith(10<<30, 0, 0, 0, footprintAt(8<<30, total))
	gate.Sample()
	if stats := gate.Stats(); stats.Tier != StorageWarning {
		t.Fatalf("Tier = %v, want StorageWarning", stats.Tier)
	}
	// Recovers immediately on crossing back — no hysteresis on the warning
	// tier, since nothing behavioral flaps on it (see hysteresisFraction doc).
	gate.read = func(string) (diskstat.Footprint, error) {
		return *footprintAt(10<<30+1, total), nil
	}
	gate.Sample()
	if stats := gate.Stats(); stats.Tier != StorageHealthy {
		t.Fatalf("Tier = %v after recovering above the warning floor, want StorageHealthy immediately", stats.Tier)
	}
}

func TestStorageGateFailsOpenOnMeasurementFailure(t *testing.T) {
	const total = 100 * 1024 * 1024 * 1024
	gate := gateWith(0, 0, 5<<30, 0, footprintAt(1<<30, total))
	gate.Sample()
	if stats := gate.Stats(); stats.Tier != StorageCritical {
		t.Fatalf("Tier = %v, want StorageCritical to start", stats.Tier)
	}

	// Simulate the filesystem becoming unmeasurable: must fail open rather
	// than latch the prior critical tier (#4873: "failure to measure capacity
	// is observable but fail-open").
	gate.read = func(string) (diskstat.Footprint, error) {
		return diskstat.Footprint{}, errors.New("diskstat: simulated failure")
	}
	gate.Sample()
	stats := gate.Stats()
	if stats.Tier != StorageMeasurementUnavailable {
		t.Fatalf("Tier = %v after a failed sample, want StorageMeasurementUnavailable", stats.Tier)
	}
	if stats.Error == "" {
		t.Fatal("Stats().Error is empty after a failed sample, want the underlying error recorded")
	}
	if pressured, _ := gate.UnderPressure(); pressured {
		t.Fatal("UnderPressure() = true while measurement is unavailable, want fail-open")
	}
}

func TestStorageGateSampleReportsChangedOnlyOnTransition(t *testing.T) {
	const total = 100 * 1024 * 1024 * 1024
	gate := gateWith(0, 10, 0, 5, footprintAt(50<<30, total))

	tier, changed := gate.Sample()
	if tier != StorageHealthy || !changed {
		t.Fatalf("first Sample() = (%v, %v), want (StorageHealthy, true) — the initial measurement is always a transition out of StorageMeasurementUnavailable", tier, changed)
	}

	tier, changed = gate.Sample()
	if tier != StorageHealthy || changed {
		t.Fatalf("repeat Sample() with the same reading = (%v, %v), want (StorageHealthy, false)", tier, changed)
	}

	gate.read = func(string) (diskstat.Footprint, error) {
		return *footprintAt(1<<30, total), nil
	}
	tier, changed = gate.Sample()
	if tier != StorageCritical || !changed {
		t.Fatalf("Sample() after crossing into critical = (%v, %v), want (StorageCritical, true)", tier, changed)
	}
}

func TestNilStorageGateAdmits(t *testing.T) {
	var gate *StorageGate
	if pressured, detail := gate.UnderPressure(); pressured || detail != "" {
		t.Fatalf("nil gate UnderPressure() = (%v, %q), want (false, \"\")", pressured, detail)
	}
	if stats := gate.Stats(); stats.Tier != StorageMeasurementUnavailable {
		t.Fatalf("nil gate Stats().Tier = %v, want StorageMeasurementUnavailable", stats.Tier)
	}
}

func TestAdmitRefusesNewRunsWhenStorageIsCritical(t *testing.T) {
	c := NewConditions()
	gate := gateWith(0, 0, 5<<30, 0, footprintAt(1<<30, 100<<30))
	gate.Sample()
	c.SetDiskGate(gate)

	ok, reason := c.Admit("wf", apiv1.ReadinessConditions{}, time.Now())
	if ok {
		t.Fatal("Admit succeeded under critical storage pressure, want refusal")
	}
	if !strings.HasPrefix(reason, ReasonStorageCritical) {
		t.Fatalf("reason = %q, want prefix %q", reason, ReasonStorageCritical)
	}
}

func TestAdmitProceedsWhenStorageGateIsClear(t *testing.T) {
	c := NewConditions()
	gate := gateWith(0, 0, 5<<30, 0, footprintAt(50<<30, 100<<30))
	gate.Sample()
	c.SetDiskGate(gate)

	ok, _ := c.Admit("wf", apiv1.ReadinessConditions{}, time.Now())
	if !ok {
		t.Fatal("Admit refused with healthy storage")
	}
}

func TestAdmitIsUnaffectedWithoutADiskGate(t *testing.T) {
	c := NewConditions()
	ok, _ := c.Admit("wf", apiv1.ReadinessConditions{}, time.Now())
	if !ok {
		t.Fatal("Admit refused with no disk gate wired, want fail-open")
	}
}

func TestReserveContinuationIgnoresStoragePressure(t *testing.T) {
	c := NewConditions()
	gate := gateWith(0, 0, 5<<30, 0, footprintAt(1<<30, 100<<30))
	gate.Sample()
	c.SetDiskGate(gate)

	identity := WorkflowIdentity{Workflow: "wf"}
	ok, reason := c.ReserveContinuation(identity, apiv1.ReadinessConditions{})
	if !ok {
		t.Fatalf("ReserveContinuation refused under critical storage: %q, want existing work to keep draining", reason)
	}
}
