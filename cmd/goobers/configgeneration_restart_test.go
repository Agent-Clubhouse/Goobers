package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/harness"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	harnesstest "github.com/goobers/goobers/test/testsupport/harness"
)

// Exercise admission, real reload, the agentic gate, CLI subprocess guard, and
// fresh daemon reconstruction. Only the external harness and repository are fake.
func TestUpPinsExecutionGenerationAcrossReloadAndRestart(t *testing.T) {
	for _, restart := range []bool{false, true} {
		t.Run(fmt.Sprintf("restart=%v", restart), func(t *testing.T) {
			root := initAcceptanceDemo(t)
			previousLookPath := runnerLookPath
			runnerLookPath = func(name string) (string, error) {
				if name == "goobers" {
					return os.Executable()
				}
				return previousLookPath(name)
			}
			t.Cleanup(func() { runnerLookPath = previousLookPath })
			l := instance.NewLayout(root)
			cfg, err := instance.LoadConfig(l.ConfigFile())
			if err != nil {
				t.Fatal(err)
			}
			cfg.RunConditions.Storage = &instance.StorageHealthConfig{WarningFloorBytes: 2, CriticalFloorBytes: 1, WarningFloorPercent: 0.000002, CriticalFloorPercent: 0.000001}
			if err := instance.WriteConfig(l.ConfigFile(), cfg); err != nil {
				t.Fatal(err)
			}
			workflows := filepath.Join(l.ConfigDir(), "gaggles", "example", "workflows")
			// This fixture is manually triggered and has a fake provider token.
			// Disable backlog polling so external auth cannot race local admission.
			manual := strings.Replace(acceptanceWorkflowYAML, "    - type: backlog-item\n      selector:\n        goobers: \"true\"", "    - type: manual", 1)
			wf := strings.Replace(manual, `command: ["true"]`, fmt.Sprintf(`command: ["goobers", "validate", %q]`, root), 1)
			writeFixture(t, filepath.Join(workflows, "acceptance.yaml"), wf)
			unrelated := strings.ReplaceAll(deterministicWorkflowYAML, "default-implement", "unrelated")
			writeFixture(t, filepath.Join(workflows, "unrelated.yaml"), unrelated)
			oldReload, oldSweep := configReloadInterval, delegationSweepInterval
			configReloadInterval, delegationSweepInterval = 20*time.Millisecond, 20*time.Millisecond
			t.Cleanup(func() { configReloadInterval, delegationSweepInterval = oldReload, oldSweep })
			entered := make(chan harness.RunRequest, 4)
			proceed := make(chan struct{})
			previous := newAgenticAdapter
			newAgenticAdapter = func(name string, _ map[string]string) harness.Adapter {
				return &harnesstest.FakeAdapter{Act: func(ctx context.Context, req harness.RunRequest) error {
					asset, err := os.ReadFile(filepath.Join(req.Workspace, ".goober-assets", "identity.txt"))
					if err != nil {
						return err
					}
					if string(asset) != name || !strings.Contains(req.Instructions, name+" fixture goober") {
						return fmt.Errorf("attempt used changed goober: asset=%q instructions=%q", asset, req.Instructions)
					}
					if name == "reviewer" {
						entered <- req
						select {
						case <-ctx.Done():
							return ctx.Err()
						case <-proceed:
						}
						return harnesstest.WriteCompletion(req.Workspace, req.CompletionPath, apiv1.Verdict{Decision: apiv1.VerdictPass, Rationale: "verified"})
					}
					if err := commitFixtureChange(req.Workspace, 1); err != nil {
						return err
					}
					return harnesstest.WriteCompletion(req.Workspace, req.CompletionPath, acceptanceAct(name, 1))
				}}
			}
			t.Cleanup(func() { newAgenticAdapter = previous })
			stop := startGenerationTestDaemon(t, root)
			requestID, err := writeTriggerRequestContext(t.Context(), l.SchedulerDir(), "example", "acceptance")
			if err != nil {
				t.Fatal(err)
			}
			runID, err := pollTriggerResponse(t.Context(), l.SchedulerDir(), requestID, 10*time.Second)
			if err != nil {
				t.Fatal(err)
			}
			first := waitGenerationGate(t, entered, l, runID)
			if first.Envelope.ConfigGeneration == "" {
				t.Fatal("admitted run has no generation")
			}
			writeFixture(t, filepath.Join(workflows, "unrelated.yaml"), strings.ReplaceAll(unrelated, "24h", "12h"))
			reviewer := filepath.Join(l.ConfigDir(), "gaggles", "example", "goobers", "reviewer")
			writeFixture(t, filepath.Join(reviewer, "instructions.md"), "Changed after admission.\n")
			writeFixture(t, filepath.Join(reviewer, "assets", "identity.txt"), "changed")
			waitForConfigEvent(t, l.SchedulerDir(), journal.EventConfigReloaded, 1)
			if restart {
				stop()
				dir, err := l.FindRunDir(runID)
				if err != nil {
					t.Fatal(err)
				}
				reader, err := journal.OpenRead(dir)
				if err != nil {
					t.Fatal(err)
				}
				if phase, err := reader.Phase(); err != nil || phase != journal.PhaseRunning {
					t.Fatalf("crash checkpoint=%s err=%v", phase, err)
				}
				stop = startGenerationTestDaemon(t, root)
				resumed := waitGenerationGate(t, entered, l, runID)
				if resumed.Envelope.ConfigGeneration != first.Envelope.ConfigGeneration {
					t.Fatal("restart switched admitted generation")
				}
			}
			close(proceed)
			waitForConfigValue(t, "pinned run completion", func() (journal.RunPhase, bool) {
				dir, err := l.FindRunDir(runID)
				if err != nil {
					return "", false
				}
				reader, err := journal.OpenRead(dir)
				if err != nil {
					return "", false
				}
				phase, err := reader.Phase()
				if err == nil && isTerminalPhase(phase) && phase != journal.PhaseCompleted {
					events, _ := reader.Events()
					t.Fatalf("run ended %s: %+v", phase, events)
				}
				return phase, err == nil && phase == journal.PhaseCompleted
			})
			stop()
		})
	}
}

func waitGenerationGate(t *testing.T, entered <-chan harness.RunRequest, l instance.Layout, runID string) harness.RunRequest {
	t.Helper()
	select {
	case req := <-entered:
		return req
	case <-time.After(20 * time.Second):
		dir, _ := l.FindRunDir(runID)
		if reader, err := journal.OpenRead(dir); err == nil {
			events, _ := reader.Events()
			for _, event := range events {
				if event.Error != nil {
					t.Logf("run error: %+v", event.Error)
				}
			}
			phase, _ := reader.Phase()
			t.Logf("phase=%s events=%+v", phase, events)
		}
		t.Fatal("review gate did not start")
	}
	return harness.RunRequest{}
}

func startGenerationTestDaemon(t *testing.T, root string) func() {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	force := make(chan struct{})
	output, diagnostics := newDaemonOutput(), newDaemonOutput()
	done := make(chan int, 1)
	go func() { done <- runUpContextWithForce(ctx, force, []string{root}, output, diagnostics) }()
	stopped := false
	stop := func() {
		if stopped {
			return
		}
		stopped = true
		cancel()
		close(force)
		select {
		case code := <-done:
			if code != 0 {
				t.Errorf("daemon code=%d stdout=%s stderr=%s", code, output.String(), diagnostics.String())
			}
		case <-time.After(30 * time.Second):
			t.Error("daemon did not stop")
		}
	}
	t.Cleanup(stop)
	select {
	case <-output.started:
	case code := <-done:
		stopped = true
		cancel()
		t.Fatalf("startup code=%d stdout=%s stderr=%s", code, output.String(), diagnostics.String())
	case <-time.After(30 * time.Second):
		t.Fatalf("startup timed out: %s %s", output.String(), diagnostics.String())
	}
	return stop
}
