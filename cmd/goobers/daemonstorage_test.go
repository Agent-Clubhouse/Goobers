package main

import (
	"testing"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/localscheduler"
)

func disabledDaemonStorageGate(root string, cfg *instance.Config) (*localscheduler.StorageGate, instance.StorageThresholds) {
	// The gate can still measure/report the fixture filesystem, but no reading
	// can reject a test run. Keep the configured ticker cadence intact.
	thresholds := instance.StorageThresholds{
		CheckInterval: cfg.RunConditions.ResolveStorageThresholds(cfg.Retention.RecoveryEffective()).CheckInterval,
	}
	return localscheduler.NewStorageGate(root, 0, 0, 0, 0, false, false), thresholds
}

func TestDaemonTestsDisableStorageAdmission(t *testing.T) {
	cfg := &instance.Config{RunConditions: instance.RunConditions{
		Storage: &instance.StorageHealthConfig{
			WarningFloorBytes: 1 << 50, CriticalFloorBytes: 1 << 49,
			WarningFloorPercent: 100, CriticalFloorPercent: 100,
			CheckInterval: "2m",
		},
	}}
	gate, thresholds := newDaemonStorageGate(t.TempDir(), cfg)
	stats := gate.Stats()
	if stats.WarningFloorBytes != 0 || stats.CriticalFloorBytes != 0 || stats.WarningFloorPercent != 0 || stats.CriticalFloorPercent != 0 {
		t.Fatalf("daemon fixture retained host-dependent admission floors: %+v", stats)
	}
	want := instance.StorageThresholds{CheckInterval: cfg.RunConditions.ResolveStorageThresholds(cfg.Retention.RecoveryEffective()).CheckInterval}
	if thresholds != want {
		t.Fatalf("fixture thresholds = %+v, want %+v", thresholds, want)
	}
}

func TestProductionDaemonStorageDefaultsRemainEnabled(t *testing.T) {
	cfg := &instance.Config{}
	gate, thresholds := buildDaemonStorageGate(t.TempDir(), cfg)
	stats := gate.Stats()
	want := cfg.RunConditions.ResolveStorageThresholds(cfg.Retention.RecoveryEffective())
	if thresholds != want || stats.CriticalFloorBytes != want.CriticalFloorBytes || stats.CriticalFloorPercent != want.CriticalFloorPercent {
		t.Fatalf("production storage defaults changed: stats=%+v thresholds=%+v want=%+v", stats, thresholds, want)
	}
	if stats.CriticalFloorBytes <= 0 || stats.CriticalFloorPercent <= 0 {
		t.Fatalf("production disk admission was disabled: %+v", stats)
	}
}
