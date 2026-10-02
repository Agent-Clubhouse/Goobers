package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/launchreceipt"
)

func localLaunchTestContext(runID, stage string, number int, review bool) context.Context {
	return launchreceipt.WithBinding(context.Background(), launchreceipt.Binding{RunID: runID, Stage: stage, Number: number, Review: review, StartedSeq: 1, AttemptID: journal.StageAttemptID(runID, 0, stage, 1)})
}

func TestControllerKeyRefusesLocalAndWorkerAgentAndDeterministicFactories(t *testing.T) {
	for _, key := range []bool{false, true} {
		t.Run(map[bool]string{false: "keyless", true: "configured-key"}[key], func(t *testing.T) {
			root := t.TempDir()
			config := &instance.Config{}
			if key {
				config.API.PodTokenKeyFile = filepath.Join(root, "private-host-key")
			}
			shared, scrubber := journal.DefaultScrubber()
			cfg, _, err := buildRunnerConfig(runnerCompositionInput{Layout: instance.NewLayout(root), Config: config, SharedRegistry: shared, Goobers: map[string]apiv1.GooberSpec{"coder": {}}, InstructionsByGoober: map[string]string{"coder": "fixture"}})
			if err != nil {
				t.Fatal(err)
			}
			if key && !errors.Is(cfg.ExecutionRefusal, launchreceipt.ErrControllerKey) {
				t.Fatal("local runner lost host admission")
			}
			_, agentErr := cfg.NewAgentic("coder", runnerWiringHarnessRecorder{dir: t.TempDir()}, shared)
			_, deterministicErr := cfg.NewDeterministic(runnerWiringArtifactRecorder{}, shared)
			if key {
				if !errors.Is(agentErr, launchreceipt.ErrControllerKey) || !errors.Is(deterministicErr, launchreceipt.ErrControllerKey) {
					t.Fatalf("local factories: %v / %v", agentErr, deterministicErr)
				}
			} else {
				if agentErr != nil || deterministicErr != nil {
					t.Fatalf("keyless factories: %v / %v", agentErr, deterministicErr)
				}
				return
			}
			seams := &workerSeams{shared: shared, scrubber: scrubber}
			seams.snapshot.Store(&workerConfigSnapshot{cfg: config, gaggles: map[string]*builtGaggleSeams{"web": {seams: &gaggleSeams{cfg: cfg, runsDir: t.TempDir()}}}})
			env := apiv1.InvocationEnvelope{RunID: "worker-run", TaskID: "build", Gaggle: "web", Goober: "coder", Attempt: 1}
			_, invokeErr := seams.Agentic().Invoke(context.Background(), env)
			_, reviewErr := seams.Agentic().Review(context.Background(), env)
			_, shellErr := seams.Deterministic().Run(context.Background(), env, apiv1.DeterministicRun{Command: []string{"sh", "-c", "exit 0"}})
			for _, err := range []error{invokeErr, reviewErr, shellErr} {
				if !errors.Is(err, launchreceipt.ErrControllerKey) {
					t.Fatalf("worker admission: %v", err)
				}
				if strings.Contains(err.Error(), config.API.PodTokenKeyFile) {
					t.Fatal("admission disclosed host path")
				}
			}
		})
	}
}

func TestDoctorControllerKeyGuidanceDoesNotLaunchHarness(t *testing.T) {
	root := t.TempDir()
	key := filepath.Join(root, "private-signing-key")
	if err := instance.WriteConfig(instance.NewLayout(root).ConfigFile(), &instance.Config{API: instance.APIConfig{PodTokenKeyFile: key}, Runner: instance.RunnerConfig{HarnessCommand: map[string][]string{"copilot": {"missing-harness-must-not-execute"}}}}); err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr := runArgs(t, "doctor", "--harness-auth", "--report", "json", root)
	if code != 1 || !strings.Contains(stdout, "api.podTokenKeyFile") || !strings.Contains(stdout, "move the signing key") {
		t.Fatalf("guidance: %d %s %s", code, stdout, stderr)
	}
	if strings.Contains(stdout+stderr, key) {
		t.Fatal("doctor disclosed raw key path")
	}
}

func TestStatusControllerKeyWarningUsesExistingWarningSurface(t *testing.T) {
	root := initDemo(t)
	path := instance.NewLayout(root).ConfigFile()
	cfg, err := instance.LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	cfg.API.PodTokenKeyFile = filepath.Join(root, "private-signing-key")
	if err := instance.WriteConfig(path, cfg); err != nil {
		t.Fatal(err)
	}
	// Status uses configured fixture adapters; it must not read the key.
	if err := os.WriteFile(cfg.API.PodTokenKeyFile, []byte("fixture-secret-key"), 0600); err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr := runArgs(t, "status", "--json", root)
	if code != 0 || !strings.Contains(stdout, "api.podTokenKeyFile") {
		t.Fatalf("status: %d %s %s", code, stdout, stderr)
	}
	if strings.Contains(stdout+stderr, cfg.API.PodTokenKeyFile) || strings.Contains(stdout+stderr, "fixture-secret-key") {
		t.Fatal("status leaked private key metadata")
	}
}

func TestControllerKeyAdmissionPrecedesPinnedConfigAndHarnessPreflight(t *testing.T) {
	config := &instance.Config{API: instance.APIConfig{PodTokenKeyFile: "/private/never-read"}}
	w := &workerSeams{}
	w.snapshot.Store(&workerConfigSnapshot{cfg: config})
	for _, agentic := range []bool{false, true} {
		_, _, err := w.forInvocationGaggle(t.Context(), apiv1.InvocationEnvelope{ConfigGeneration: "forged-archive", GooberDigest: "forged-goober"}, agentic)
		if !errors.Is(err, launchreceipt.ErrControllerKey) {
			t.Fatalf("archive overrode host admission: %v", err)
		}
	}
	// Nil workflow settings prove neither path reaches credential/harness prep.
	if _, _, err := preflightSchedulerHarnesses(config, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	if _, _, err := preflightWorkerHarnesses(config, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
}
