package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/worktree"
)

func TestBuildBaselineHealthIsOptIn(t *testing.T) {
	root := t.TempDir()
	layout := instance.Layout{Root: root}
	mgr, err := worktree.NewManager(filepath.Join(root, "workcopies"))
	if err != nil {
		t.Fatalf("new worktree manager: %v", err)
	}

	t.Run("unconfigured", func(t *testing.T) {
		health, err := buildBaselineHealth(layout, &instance.Config{}, mgr)
		if err != nil {
			t.Fatalf("buildBaselineHealth: %v", err)
		}
		if health != nil {
			t.Fatal("health = non-nil, want nil: base-health awareness is opt-in")
		}
	})

	t.Run("enabled without a worktree manager", func(t *testing.T) {
		cfg := &instance.Config{BaselineHealth: &instance.BaselineHealthConfig{Enabled: true}}
		health, err := buildBaselineHealth(layout, cfg, nil)
		if err != nil {
			t.Fatalf("buildBaselineHealth: %v", err)
		}
		if health != nil {
			t.Fatal("health = non-nil, want nil: a baseline cannot be measured without a worktree manager")
		}
	})

	t.Run("enabled", func(t *testing.T) {
		cfg := &instance.Config{BaselineHealth: &instance.BaselineHealthConfig{Enabled: true, SharedRepairLane: true, ProbeTimeoutSeconds: 60}}
		health, err := buildBaselineHealth(layout, cfg, mgr)
		if err != nil {
			t.Fatalf("buildBaselineHealth: %v", err)
		}
		adapter, ok := health.(*baselineHealthAdapter)
		if !ok {
			t.Fatalf("health = %T, want the daemon adapter", health)
		}
		if !adapter.evaluator.RepairLane {
			t.Fatal("repair lane = false, want the explicitly configured shared repair lane honored")
		}
		if adapter.evaluator.ProbeTimeout != cfg.BaselineProbeTimeout() {
			t.Fatalf("probe timeout = %s, want %s", adapter.evaluator.ProbeTimeout, cfg.BaselineProbeTimeout())
		}
	})

	t.Run("state is instance-scoped and durable", func(t *testing.T) {
		cfg := &instance.Config{BaselineHealth: &instance.BaselineHealthConfig{Enabled: true}}
		stateDir := t.TempDir()
		statePath := filepath.Join(stateDir, baselineStateFileName)
		if err := os.WriteFile(statePath, []byte(`{"version":1,"observations":{},"blockers":{}}`), 0o600); err != nil {
			t.Fatalf("seed store: %v", err)
		}
		if _, err := buildBaselineHealth(instance.Layout{Root: stateDir}, cfg, mgr); err != nil {
			t.Fatalf("buildBaselineHealth over existing state: %v", err)
		}

		if err := os.WriteFile(statePath, []byte("{not json"), 0o600); err != nil {
			t.Fatalf("corrupt store: %v", err)
		}
		if _, err := buildBaselineHealth(instance.Layout{Root: stateDir}, cfg, mgr); err == nil {
			t.Fatal("buildBaselineHealth error = nil, want an unreadable store surfaced at wiring time")
		}
	})
}
