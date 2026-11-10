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
	"github.com/goobers/goobers/internal/runtimewiring"
)

func TestUpAtProtectedGenerationCapacityKeepsDiagnostics(t *testing.T) {
	testUpAtProtectedGenerationCapacity(t, false, false)
}

func TestUpAtProtectedGenerationCapacityLegacyRollback(t *testing.T) {
	testUpAtProtectedGenerationCapacity(t, true, false)
}

func TestUpAtProtectedGenerationCapacityAfterCompaction(t *testing.T) {
	testUpAtProtectedGenerationCapacity(t, false, true)
}

func testUpAtProtectedGenerationCapacity(t *testing.T, legacy, compact bool) {
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
	if legacy {
		firstDir, err := store.Load(t.Context(), first)
		if err != nil {
			t.Fatal(err)
		}
		lastDir, err := store.Load(t.Context(), last)
		if err != nil {
			t.Fatal(err)
		}
		firstDigest, err := runtimewiring.ConfigDirectoryDigest(firstDir)
		if err != nil {
			t.Fatal(err)
		}
		lastDigest, err := runtimewiring.ConfigDirectoryDigest(lastDir)
		if err != nil {
			t.Fatal(err)
		}
		// Old daemons recorded only the effective digest, not configGeneration.
		if err := scheduler.Reload(nil, nil, time.Now(), firstDigest, lastDigest); err != nil {
			t.Fatal(err)
		}
		if err := scheduler.Reload(nil, nil, time.Now(), lastDigest, firstDigest); err != nil {
			t.Fatal(err)
		}
	} else {
		if err := scheduler.Reload(nil, nil, time.Now(), "A", "B", last); err != nil {
			t.Fatal(err)
		}
		if err := scheduler.Reload(nil, nil, time.Now(), "B", "A", first); err != nil {
			t.Fatal(err)
		}
	}
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}
	if compact {
		compactGenerationHistory(t, layout, first)
	}
	recoveredDir, err := store.Load(t.Context(), first)
	if err != nil {
		t.Fatal(err)
	}
	recoveredDigest, err := runtimewiring.ConfigDirectoryDigest(recoveredDir)
	if err != nil {
		t.Fatal(err)
	}
	writeFixture(t, filepath.Join(layout.ConfigDir(), "generation-note.txt"), "unretainable candidate")
	writeFixture(t, manifestPath, string(manifest)+"\n# unretainable candidate\n")
	var recoveryOptions []string
	if legacy {
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		var output, diagnostics bytes.Buffer
		code := runUpContextWithForce(ctx, make(chan struct{}), []string{root}, &output, &diagnostics)
		cancel()
		if code != 1 {
			t.Fatalf("ambiguous legacy startup code=%d stdout=%s stderr=%s", code, output.String(), diagnostics.String())
		}
		for _, want := range []string{"last applied legacy catalog is ambiguous", first, last, "archiveModifiedAt=", "source=config-generations/", "goobers up --recover-config-generation"} {
			if !strings.Contains(diagnostics.String(), want) {
				t.Fatalf("startup diagnostic missing %q: %s", want, diagnostics.String())
			}
		}
		events, err := journal.ReadInstanceLog(layout.SchedulerDir())
		if err != nil {
			t.Fatal(err)
		}
		for _, event := range events {
			if event.Type == journal.EventRunnerAnnotation && event.Runner["kind"] == appliedGenerationAnnotation {
				t.Fatalf("legacy refusal recorded inferred catalog: %+v", event)
			}
		}
		var bundle, bundleErrors bytes.Buffer
		if code := runDiagnosticsBundle([]string{"--json", root}, &bundle, &bundleErrors); code != 0 || !json.Valid(bundle.Bytes()) {
			t.Fatalf("legacy refusal lost diagnostics: code=%d stderr=%s", code, bundleErrors.String())
		}
		// The source remains C. Explicit operator selection restores A's catalog
		// without fabricating application history or overwriting configuration.
		recoveryOptions = []string{"--recover-config-generation", first}
	}
	stop := startGenerationTestDaemon(t, root, recoveryOptions...)
	defer stop()
	if compact {
		// Restart a second time after the startup annotation itself ages out.
		// Both reload and startup authority must survive ordinary retention.
		stop()
		compactGenerationHistory(t, layout, first)
		stop = startGenerationTestDaemon(t, root)
	}
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
	if health.DefinitionReload.AppliedDigest != recoveredDigest || (!legacy && !strings.Contains(health.DefinitionReload.RejectionReason, "64 generations")) {
		t.Fatalf("wrong recovery or missing limits: %+v", health.DefinitionReload)
	}
	var diagnostics, diagnosticErrors bytes.Buffer
	if code := runDiagnosticsBundle([]string{"--json", "--run", "retained-63", root}, &diagnostics, &diagnosticErrors); code != 0 || !json.Valid(diagnostics.Bytes()) {
		t.Fatalf("diagnostics unavailable at capacity: code=%d stderr=%s", code, diagnosticErrors.String())
	}
	requestID, err := writeTriggerRequestContextOptions(t.Context(), layout.SchedulerDir(), "example", "default-implement", false)
	if err != nil {
		t.Fatal(err)
	}
	runID, err := pollTriggerResponseForTest(t.Context(), layout.SchedulerDir(), requestID, 15*time.Second)
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
		setup, err := buildSchedulerSetup(context.Background(), layout, &wg, func(options *schedulerSetupOptions) {
			options.recoverGenerationCapacity = true
			if legacy {
				options.recoverConfigGeneration = first
			}
		})
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
	if generation, err := retainedStartupGeneration(t.Context(), layout); err == nil || generation != "" || !strings.Contains(err.Error(), "legacy catalog is ambiguous") {
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

func TestExplicitStartupGenerationRequiresRetainedCustody(t *testing.T) {
	layout := instance.NewLayout(initDeterministicDemo(t))
	store, err := executionGenerationStore(layout)
	if err != nil {
		t.Fatal(err)
	}
	data, foreign, err := configgeneration.CaptureForInstance(t.Context(), layout.ConfigDir(), "another-instance")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Keep(t.Context(), data, foreign, nil); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ name, generation, want string }{
		{"unknown", "sha256:" + strings.Repeat("0", 64), "strict verification"},
		{"foreign", foreign, "different instance"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			retainer := &configgeneration.Retainer{Store: store}
			defer func() { _ = retainer.Close() }()
			input := schedulerDefinitionsInput{Layout: layout, RecoveryGeneration: tc.generation}
			got, digest, err := recoverStartupGeneration(t.Context(), input, retainer)
			if err == nil || !strings.Contains(err.Error(), tc.want) || got.PinnedGeneration != "" || digest != "" {
				t.Fatalf("unverified selection accepted: input=%+v digest=%q err=%v", got, digest, err)
			}
		})
	}
}

func compactGenerationHistory(t *testing.T, layout instance.Layout, generation string) {
	t.Helper()
	cutoff := time.Now().Add(time.Hour)
	if _, err := journal.CompactInstanceEvents(layout.SchedulerDir(), cutoff, cutoff, false); err != nil {
		t.Fatal(err)
	}
	if got, err := lastAppliedExecutionGeneration(layout); err != nil || got != generation {
		t.Fatalf("compaction lost applied authority: got=%q want=%q err=%v", got, generation, err)
	}
}
