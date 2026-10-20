package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/configgeneration"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/localscheduler"
)

func TestUpAtProtectedGenerationCapacityKeepsDiagnostics(t *testing.T) {
	root := initDeterministicDemo(t)
	layout := instance.NewLayout(root)
	address := freeLoopbackAddress(t)
	setAPIListenAddress(t, root, address)
	owner, err := layout.EnsureIdentity(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	store, err := executionGenerationStore(layout)
	if err != nil {
		t.Fatal(err)
	}
	manifestPath := filepath.Join(layout.ConfigDir(), "manifest.yaml")
	manifest, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	pins := map[string]bool{}
	var first, last string
	for i := 0; i < configgeneration.MaxGenerations; i++ {
		writeFixture(t, filepath.Join(layout.ConfigDir(), "generation-note.txt"), fmt.Sprintf("generation %d", i))
		writeFixture(t, manifestPath, string(manifest)+fmt.Sprintf("\n# generation %d\n", i))
		data, digest, err := configgeneration.CaptureForInstance(t.Context(), layout.ConfigDir(), owner)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := store.Keep(t.Context(), data, digest, pins); err != nil {
			t.Fatal(err)
		}
		run, err := journal.Create(layout.ForGaggle("example").RunsDir(), journal.RunIdentity{RunID: fmt.Sprintf("retained-%02d", i), Gaggle: "example", Workflow: "default-implement", WorkflowVersion: 1, ConfigGeneration: digest, Trigger: journal.Trigger{Kind: journal.TriggerManual}}, nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := run.Append(journal.Event{Type: journal.EventRunFinished, Status: string(journal.PhaseEscalated)}); err != nil {
			t.Fatal(err)
		}
		if err := run.Close(); err != nil {
			t.Fatal(err)
		}
		pins[digest] = true
		last = digest
		if first == "" {
			first = digest
		}
	}
	// A committed A -> B -> A reload must win over the newest run's B pin.
	log, _, err := journal.OpenInstanceLog(layout.SchedulerDir())
	if err != nil {
		t.Fatal(err)
	}
	scheduler := localscheduler.New(nil, log)
	if err := scheduler.Reload(nil, nil, time.Now(), "A", "B", last); err != nil {
		t.Fatal(err)
	}
	if err := scheduler.Reload(nil, nil, time.Now(), "B", "A", first); err != nil {
		t.Fatal(err)
	}
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}
	recoveredDir, err := store.Load(t.Context(), first)
	if err != nil {
		t.Fatal(err)
	}
	recoveredDigest, err := configDirectoryDigest(recoveredDir)
	if err != nil {
		t.Fatal(err)
	}
	writeFixture(t, filepath.Join(layout.ConfigDir(), "generation-note.txt"), "unretainable candidate")
	writeFixture(t, manifestPath, string(manifest)+"\n# unretainable candidate\n")
	stop := startGenerationTestDaemon(t, root)
	defer stop()
	health := readDaemonHealth(t, address)
	if health.DefinitionReload == nil || health.DefinitionReload.State != "rejected" {
		t.Fatalf("capacity status: %+v", health.DefinitionReload)
	}
	if len(retainedGenerationNames(t, layout)) != configgeneration.MaxGenerations {
		t.Fatal("generation count changed")
	}
	for digest := range pins {
		if _, err := store.Load(t.Context(), digest); err != nil {
			t.Fatalf("protected generation lost: %v", err)
		}
	}
	if _, err := os.Stat(filepath.Join(layout.ForGaggle("example").RunsDir(), "retained-63", "run.yaml")); err != nil {
		t.Fatal(err)
	}
	if health.DefinitionReload.AppliedDigest != recoveredDigest || !strings.Contains(health.DefinitionReload.RejectionReason, "64 generations") {
		t.Fatalf("wrong recovery or missing limits: %+v", health.DefinitionReload)
	}
	var diagnostics, diagnosticErrors bytes.Buffer
	if code := runDiagnosticsBundle([]string{"--json", "--run", "retained-63", root}, &diagnostics, &diagnosticErrors); code != 0 || !json.Valid(diagnostics.Bytes()) {
		t.Fatalf("diagnostics unavailable at capacity: code=%d stderr=%s", code, diagnosticErrors.String())
	}
	requestID, err := writeTriggerRequestContext(t.Context(), layout.SchedulerDir(), "example", "default-implement")
	if err != nil {
		t.Fatal(err)
	}
	runID, err := pollTriggerResponse(t.Context(), layout.SchedulerDir(), requestID, 15*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	directory, err := layout.FindRunDir(runID)
	if err != nil {
		t.Fatal(err)
	}
	reader, err := journal.OpenRead(directory)
	if err != nil {
		t.Fatal(err)
	}
	identity, err := reader.Identity()
	if err != nil || identity.ConfigGeneration != first {
		t.Fatalf("admitted candidate instead of recovered generation: %+v %v", identity, err)
	}
	waitForConfigValue(t, "retained-generation run completion", func() (journal.RunPhase, bool) {
		phase, err := reader.Phase()
		return phase, err == nil && phase == journal.PhaseCompleted
	})

	// Supported non-destructive remediation: restore the already retained
	// configuration. No owner, lease, archive or journal needs to be removed.
	writeFixture(t, filepath.Join(layout.ConfigDir(), "generation-note.txt"), "generation 0")
	writeFixture(t, manifestPath, string(manifest)+"\n# generation 0\n")
	waitForConfigValue(t, "retained configuration rollback", func() (string, bool) {
		status := readDaemonHealth(t, address).DefinitionReload
		return status.State, status.State == "current" && status.AppliedDigest == status.ObservedDigest
	})
	stop()

	if runtime.GOOS != "windows" {
		// Simulate volume ownership processing changing archived 0644 to 0664.
		path := filepath.Join(recoveredDir, "generation-note.txt")
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, info.Mode().Perm()|0020); err != nil {
			t.Fatal(err)
		}
		defer func() { _ = os.Chmod(path, info.Mode().Perm()) }()
		writeFixture(t, filepath.Join(layout.ConfigDir(), "generation-note.txt"), "unretainable candidate")
		writeFixture(t, manifestPath, string(manifest)+"\n# unretainable candidate\n")
		var wg sync.WaitGroup
		setup, err := buildSchedulerSetup(context.Background(), layout, &wg, func(options *schedulerSetupOptions) { options.recoverGenerationCapacity = true })
		if setup != nil {
			_ = setup.Shutdown(context.Background())
		}
		if err == nil || !strings.Contains(err.Error(), "strict verification") || !strings.Contains(err.Error(), "permission modes") {
			t.Fatalf("changed permissions silently recovered: %v", err)
		}
		if len(retainedGenerationNames(t, layout)) != configgeneration.MaxGenerations {
			t.Fatal("verification failure evicted a generation")
		}
	}
}

func TestCapacityRejectionRemainsVisibleForEqualEffectiveDigests(t *testing.T) {
	reloader := configReloader{appliedDigest: "same-yaml", observedDigest: "same-yaml", rejectedDigest: "same-yaml", retentionRejected: true}
	// Captured non-YAML assets can differ even when the effective YAML digest
	// does not; startup still rejected that distinct execution generation.
	if status := reloader.reloadStatus(time.Now()); status.State != "rejected" {
		t.Fatalf("hidden retention rejection: %+v", status)
	}
}

func TestStartupGenerationLegacyAndAppliedOwnership(t *testing.T) {
	layout := instance.NewLayout(t.TempDir())
	for i, generation := range []string{"older", "newer"} {
		run, err := journal.Create(layout.ForGaggle("example").RunsDir(), journal.RunIdentity{
			RunID: fmt.Sprintf("run-%d", i), ConfigGeneration: generation,
			StartedAt: time.Unix(int64(i+1), 0),
		}, nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := run.Append(journal.Event{Type: journal.EventRunFinished, Status: string(journal.PhaseEscalated)}); err != nil {
			t.Fatal(err)
		}
		if err := run.Close(); err != nil {
			t.Fatal(err)
		}
	}
	if generation, err := retainedStartupGeneration(t.Context(), layout); err != nil || generation != "newer" {
		t.Fatalf("legacy recovery = %q, %v", generation, err)
	}
	log, _, err := journal.OpenInstanceLog(layout.SchedulerDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = log.Close() }()
	if err := log.Append(journal.Event{Type: journal.EventRunnerAnnotation, Runner: map[string]any{
		"kind": appliedGenerationAnnotation, "configGeneration": "catalog-without-run",
	}}); err != nil {
		t.Fatal(err)
	}
	if generation, err := retainedStartupGeneration(t.Context(), layout); err != nil || generation != "catalog-without-run" {
		t.Fatalf("applied recovery = %q, %v", generation, err)
	}
	pins, err := retainedExecutionGenerationPins(t.Context(), layout)
	if err != nil || !pins["catalog-without-run"] || !pins["older"] || !pins["newer"] {
		t.Fatalf("catalog or journal ownership lost: %v %v", pins, err)
	}
}
