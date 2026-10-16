package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/goobers/goobers/internal/configgeneration"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/localscheduler"
)

func TestGenerationPruningProtectsRetainedJournalsAndExternalHistories(t *testing.T) {
	root := initDeterministicDemo(t)
	layout := instance.NewLayout(root)
	owner, err := layout.EnsureIdentity(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	store, err := executionGenerationStore(layout)
	if err != nil {
		t.Fatal(err)
	}
	store.DurablePins = func(ctx context.Context) (map[string]bool, error) {
		return retainedExecutionGenerationPins(ctx, layout)
	}
	pins := make([]string, 0, configgeneration.MaxGenerations)
	var removableRun, removableDigest string
	for i := 0; i < configgeneration.MaxGenerations; i++ {
		writeFixture(t, filepath.Join(layout.ConfigDir(), "generation-note.txt"), fmt.Sprintf("generation %d", i))
		data, digest, err := configgeneration.CaptureForInstance(t.Context(), layout.ConfigDir(), owner)
		if err != nil {
			t.Fatal(err)
		}
		if i < 2 {
			_, lease, err := store.KeepExternallyOwned(t.Context(), data, digest)
			if err != nil {
				t.Fatal(err)
			}
			if err := lease.Release(); err != nil {
				t.Fatal(err)
			}
		} else {
			_, lease, err := store.KeepAndAcquire(t.Context(), data, digest, nil)
			if err != nil {
				t.Fatal(err)
			}
			runID := fmt.Sprintf("retained-generation-%d", i)
			run, err := journal.Create(layout.ForGaggle("example").RunsDir(), journal.RunIdentity{RunID: runID, Gaggle: "example", Workflow: "default-implement", WorkflowVersion: 1, ConfigGeneration: digest, Trigger: journal.Trigger{Kind: journal.TriggerManual}}, nil)
			if err != nil {
				t.Fatal(err)
			}
			if i%2 == 0 {
				if err := run.Append(journal.Event{Type: journal.EventRunFinished, Status: string(journal.PhaseFailed)}); err != nil {
					t.Fatal(err)
				}
			}
			if err := run.Close(); err != nil {
				t.Fatal(err)
			}
			if err := lease.Release(); err != nil {
				t.Fatal(err)
			}
			removableRun, removableDigest = runID, digest
		}
		pins = append(pins, digest)
	}
	// Reconstruct the catalogue owner from disk: no active-run registry and no
	// Retainer memory is available after this simulated process restart.
	restarted, err := executionGenerationStore(layout)
	if err != nil {
		t.Fatal(err)
	}
	restarted.DurablePins = store.DurablePins
	writeFixture(t, filepath.Join(layout.ConfigDir(), "generation-note.txt"), "overflow")
	data, digest, err := configgeneration.CaptureForInstance(t.Context(), layout.ConfigDir(), owner)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := restarted.Keep(t.Context(), data, digest, nil); err == nil {
		t.Fatal("pruning evicted a retained run or external history")
	}
	for _, pin := range pins {
		if _, err := restarted.Load(t.Context(), pin); err != nil {
			t.Fatalf("protected generation %s lost: %v", pin, err)
		}
	}
	directory, err := layout.FindRunDir(removableRun)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(directory); err != nil {
		t.Fatal(err)
	}
	if _, err := restarted.Keep(t.Context(), data, digest, nil); err != nil {
		t.Fatalf("retired journal did not release capacity: %v", err)
	}
	if _, err := restarted.Load(t.Context(), removableDigest); err == nil {
		t.Fatal("unreferenced generation was not pruned")
	}
}

func TestDirectEngineInputPublishesGenerationBeforeDispatch(t *testing.T) {
	root := initDeterministicDemo(t)
	layout := instance.NewLayout(root)
	cfg, err := instance.LoadConfig(layout.ConfigFile())
	if err != nil {
		t.Fatal(err)
	}
	input, release, err := pinnedDirectEngineInput(t.Context(), layout, cfg, "example", "default-implement", "", false)
	if err != nil {
		t.Fatal(err)
	}
	release()
	if input.ConfigGeneration == "" {
		t.Fatal("external engine input omitted generation")
	}
	store, err := executionGenerationStore(layout)
	if err != nil {
		t.Fatal(err)
	}
	directory, err := store.Load(t.Context(), input.ConfigGeneration)
	if err != nil {
		t.Fatalf("engine could dispatch before archive publication: %v", err)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(directory), "external-owner")); err != nil {
		t.Fatalf("external history has no durable retention owner: %v", err)
	}
}

// A pinned rebuild must reuse the run's admitted generation. Re-retaining its
// extracted tree is not byte-stable (Windows reads mode bits back differently),
// and the fresh identity failed every later CLI stage after restart (#5445).
func TestPinnedGenerationRebuildReusesAdmittedGeneration(t *testing.T) {
	root := initDeterministicDemo(t)
	layout := instance.NewLayout(root)
	cfg, err := instance.LoadConfig(layout.ConfigFile())
	if err != nil {
		t.Fatal(err)
	}
	retainer, err := newExecutionGenerationRetainer(layout)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := retainer.Close(); err != nil {
			t.Error(err)
		}
	})
	pinned, admitted, err := retainExecutionGeneration(t.Context(), layout, retainer)
	if err != nil {
		t.Fatal(err)
	}
	before := retainedGenerationNames(t, layout)
	// Simulate an extraction whose re-capture differs only in mode bits.
	drifted := filepath.Join(t.TempDir(), "config")
	if err := os.CopyFS(drifted, os.DirFS(pinned.ConfigDir())); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(drifted, "manifest.yaml"), 0o444); err != nil {
		t.Fatal(err)
	}
	set, report, err := loadConfigDirectory(drifted)
	if err != nil {
		t.Fatal(err)
	}
	build := schedulerGenerationBuilder(schedulerDefinitionsInput{
		Layout: layout, Config: cfg, RunnerRegistry: newDaemonRunnerRegistry(),
		ProviderQuota: localscheduler.NewProviderQuotaState(), Generations: []*configgeneration.Retainer{retainer},
	})
	definitions, err := build(layout.WithConfigDir(drifted), admitted, set, report)
	if err != nil {
		t.Fatal(err)
	}
	if definitions.Runners["example"] == nil {
		t.Fatal("pinned rebuild produced no runner for the run's gaggle")
	}
	if after := retainedGenerationNames(t, layout); !slices.Equal(after, before) {
		t.Fatalf("pinned rebuild minted a new generation instead of reusing %s: before=%v after=%v", admitted, before, after)
	}
}

func retainedGenerationNames(t *testing.T, layout instance.Layout) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(layout.Root, "config-generations"))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, entry := range entries {
		if entry.IsDir() {
			names = append(names, entry.Name())
		}
	}
	return names
}
