package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/platform/lock"
	"github.com/goobers/goobers/internal/secretstore"
)

func TestSchedulerObservationRollbackAfterProjectorStarts(t *testing.T) {
	l := instance.NewLayout(initDeterministicDemo(t))
	cfg, err := instance.LoadConfig(l.ConfigFile())
	if err != nil {
		t.Fatal(err)
	}
	stores, err := secretstore.NewRegistry(cfg.SecretStores)
	if err != nil {
		t.Fatal(err)
	}
	// The last acquisition fails after telemetry, both stores and the projector.
	if err := os.Remove(filepath.Join(l.SchedulerDir(), "events.jsonl")); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(l.SchedulerDir(), "events.jsonl"), 0o755); err != nil {
		t.Fatal(err)
	}
	reg := journal.NewRegistryScrubber()
	owned := &schedulerObservation{telemetryExporterHealth: newTelemetryExporterHealth(cfg)}
	t.Cleanup(func() { _ = owned.Close() })
	err = owned.open(context.Background(), l, cfg, reg, reg, stores, schedulerSetupOptions{})
	if err == nil || !strings.Contains(err.Error(), "open instance log") {
		t.Fatalf("open error = %v", err)
	}
	if owned.tel == nil || owned.rollupDB == nil || owned.readModel == nil || owned.watermarks == nil || owned.stopProjector == nil {
		t.Fatal("failure did not reach the intended partial-construction boundary")
	}
	if _, err := owned.readModel.State(context.Background()); err == nil {
		t.Fatal("read model remains open after rollback")
	}
	if _, err := owned.watermarks.Count(context.Background()); err == nil {
		t.Fatal("watermarks remain open after rollback")
	}
	if _, err := owned.rollupDB.InstanceSummaryStats(context.Background(), time.Time{}); err == nil {
		t.Fatal("rollup remains open after rollback")
	}
	if err := owned.Close(); err != nil {
		t.Fatalf("repeated rollback: %v", err)
	}
}

func TestSchedulerOwnedShutdownOrderAndIdempotence(t *testing.T) {
	l := instance.NewLayout(initDeterministicDemo(t))
	setup, err := buildSchedulerSetup(context.Background(), l, &sync.WaitGroup{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = setup.Shutdown(context.Background()) })
	var names []string
	for _, step := range setup.shutdownSteps(context.Background()) {
		names = append(names, step.name)
	}
	want := []string{"telemetry local flush", "scheduler telemetry ingest", "telemetry client", "telemetry rollup database", "read model projector", "read model store", "source watermark store", "instance log", "config generation leases"}
	if !reflect.DeepEqual(names, want) {
		t.Fatalf("shutdown order = %v, want %v", names, want)
	}
	// Wrap the actual stop to verify that both dependent stores are still open
	// when the projector drains, and repeated cleanup does not stop it twice.
	original := setup.observation.stopProjector
	calls := 0
	setup.observation.stopProjector = func() {
		calls++
		if _, err := setup.ReadModel.State(context.Background()); err != nil {
			t.Errorf("store closed before projector stopped: %v", err)
		}
		if _, err := setup.Watermarks.Count(context.Background()); err != nil {
			t.Errorf("intake closed before projector stopped: %v", err)
		}
		original()
	}
	if err := setup.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := setup.observation.Close(); err != nil {
		t.Fatal(err)
	}
	if err := setup.runtime.Close(); err != nil {
		t.Fatal(err)
	}
	if err := setup.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("projector stopped %d times", calls)
	}
	assertGenerationLeasesReleased(t, l)
}

func TestSchedulerRuntimeRollbackReleasesAcquiredGeneration(t *testing.T) {
	l := instance.NewLayout(initDeterministicDemo(t))
	cfg, err := instance.LoadConfig(l.ConfigFile())
	if err != nil {
		t.Fatal(err)
	}
	set, report, err := loadConfigDirectory(l.ConfigDir())
	if err != nil {
		t.Fatal(err)
	}
	stores, err := secretstore.NewRegistry(cfg.SecretStores)
	if err != nil {
		t.Fatal(err)
	}
	owned := &schedulerRuntime{}
	t.Cleanup(func() { _ = owned.Close() })
	reached := false
	err = owned.open(schedulerDefinitionsInput{Layout: l, Config: cfg, Definitions: set, Validation: report, WaitGroup: &sync.WaitGroup{}, CredentialStores: stores, SharedRegistry: journal.NewRegistryScrubber(), StartupProgress: func(message string) {
		if message == "initializing retained legacy runtime" {
			reached = true
			// Definitions have retained a generation. Force the retained legacy
			// runner to open a workcopy root occupied by a regular file.
			if err := os.MkdirAll(l.RunsDir(), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.RemoveAll(l.WorkcopiesDir()); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(l.WorkcopiesDir(), []byte("blocked"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}})
	if !reached || err == nil {
		t.Fatalf("late runtime failure: reached=%v err=%v", reached, err)
	}
	assertGenerationLeasesReleased(t, l)
	if err := owned.Close(); err != nil {
		t.Fatal(err)
	}
}

func assertGenerationLeasesReleased(t *testing.T, l instance.Layout) {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(l.Root, "config-generations", "*", "lease.lock"))
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) == 0 {
		t.Fatal("no retained generation to test")
	}
	for _, path := range paths {
		held, err := lock.TryAcquire(path)
		if err != nil {
			t.Fatalf("generation lease survived cleanup: %v", err)
		}
		if err := held.Release(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestSchedulerClaimStateReleasesLockOnFailure(t *testing.T) {
	for _, failure := range []string{"open", "migration"} {
		t.Run(failure, func(t *testing.T) {
			l := newClaimTestLayout(t)
			log, _, err := journal.OpenInstanceLog(l.SchedulerDir())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = log.Close() })
			path := filepath.Join(l.SchedulerDir(), claimLedgerFileName)
			if failure == "open" {
				if err := os.WriteFile(path, []byte("invalid json"), 0o600); err != nil {
					t.Fatal(err)
				}
			} else {
				run, err := journal.Create(l.RunsDir(), journal.RunIdentity{RunID: "run-one", Workflow: "workflow", Gaggle: "example"}, nil)
				if err != nil {
					t.Fatal(err)
				}
				if err := run.Close(); err != nil {
					t.Fatal(err)
				}
				legacy := localscheduler.ClaimEntry{ItemID: "issue-1", RunID: "run-one", Workflow: "workflow", ExpiresAt: time.Now().Add(time.Hour)}
				scoped := legacy
				scoped.Gaggle, scoped.Provider, scoped.ExternalID = "example", "github", "issue-1"
				data, err := json.Marshal(map[string]any{"schema": "goobers.dev/scheduler/claims/v1", "entries": map[string]localscheduler.ClaimEntry{"issue-1": legacy, "v2|example|github|issue-1": scoped}})
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, data, 0o600); err != nil {
					t.Fatal(err)
				}

			}
			state, err := openSchedulerClaimState(schedulerClaimInput{Layout: l, InstanceLog: log, Providers: map[string]apiv1.Provider{"example": apiv1.ProviderGitHub}})
			if err == nil || state != nil {
				t.Fatalf("failure returned state=%v err=%v", state, err)
			}
			if failure == "migration" && !strings.Contains(err.Error(), "scoped claim already exists") {
				t.Fatalf("migration error: %v", err)
			}
			held, err := lock.TryAcquire(filepath.Join(l.SchedulerDir(), claimLockFileName))
			if err != nil {
				t.Fatalf("claim lock survived failure: %v", err)
			}
			if err := held.Release(); err != nil {
				t.Fatal(err)
			}
		})
	}
}
