package service

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/instance"
)

func TestWindowsStatusReportsStartupExitCodes(t *testing.T) {
	for _, tc := range []struct {
		name, output, want string
	}{
		{
			name:   "application failure",
			output: "TYPE: 10 WIN32_OWN_PROCESS\nSTATE: 1 STOPPED\nWIN32_EXIT_CODE: 1066 (0x42a)\nSERVICE_EXIT_CODE: 1 (0x1)\n",
			want:   "Windows service-specific exit code 1 (Win32 exit code 1066)",
		},
		{
			name:   "localized native failure",
			output: "TIPO: 10 WIN32_OWN_PROCESS\nESTADO: 1 DETENIDO\nCODIGO_SALIDA_WIN32: 5 (0x5)\nCODIGO_SALIDA_SERVICIO: 0 (0x0)\n",
			want:   "Windows service Win32 exit code 5",
		},
		{
			name:   "clean stop",
			output: "TYPE: 10 WIN32_OWN_PROCESS\nSTATE: 1 STOPPED\nWIN32_EXIT_CODE: 0 (0x0)\nSERVICE_EXIT_CODE: 0 (0x0)\n",
		},
		{
			name:   "running does not surface stale failure",
			output: "TYPE: 10 WIN32_OWN_PROCESS\nSTATE: 4 RUNNING\nWIN32_EXIT_CODE: 1066 (0x42a)\nSERVICE_EXIT_CODE: 1 (0x1)\n",
		},
		{
			name:   "pending does not surface stale failure",
			output: "TYPE: 10 WIN32_OWN_PROCESS\nSTATE: 2 START_PENDING\nWIN32_EXIT_CODE: 1066 (0x42a)\nSERVICE_EXIT_CODE: 1 (0x1)\n",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			manager := newTestManager(t, Config{GOOS: "windows", Executable: `C:\Goobers\goobers.exe`, InstanceRoot: t.TempDir(), Runner: &fakeRunner{
				responses: []commandResponse{{output: tc.output}},
			}})
			status, err := manager.Status(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if status.LastFailure != tc.want {
				t.Fatalf("LastFailure = %q, want %q", status.LastFailure, tc.want)
			}
		})
	}
}

func TestWindowsInstallStartupCrashReportsFailureAndRemovesRegistration(t *testing.T) {
	const running = "TYPE: 10 WIN32_OWN_PROCESS\nSTATE: 4 RUNNING\n"
	const failed = "TYPE: 10 WIN32_OWN_PROCESS\nSTATE: 1 STOPPED\nWIN32_EXIT_CODE: 1066 (0x42a)\nSERVICE_EXIT_CODE: 1 (0x1)\n"
	for _, observedRunning := range []bool{false, true} {
		name := "before running"
		if observedRunning {
			name = "after transient running"
		}
		t.Run(name, func(t *testing.T) {
			runner := &fakeRunner{responses: []commandResponse{
				{output: "OpenService FAILED 1060", code: 1060},
				{}, {}, {}, {}, {}, // Create, description, recovery policy, recovery flag, start.
			}}
			if observedRunning {
				runner.responses = append(runner.responses, commandResponse{output: running})
			}
			runner.responses = append(runner.responses,
				commandResponse{output: failed}, // Startup probe must reject this immediately.
				commandResponse{output: failed}, // Rollback confirms service already stopped.
				commandResponse{},               // Delete registration, cancelling recovery.
			)
			root := t.TempDir()
			manager := newTestManager(t, Config{GOOS: "windows", Executable: `C:\Goobers\goobers.exe`, InstanceRoot: root, Runner: runner})
			status, err := manager.Install(context.Background())
			if err == nil || status.Running {
				t.Fatalf("Install() = %+v, %v; want startup failure", status, err)
			}
			for _, want := range []string{"service failed while starting", "service-specific exit code 1", instance.NewLayout(root).DaemonLogFile()} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("startup error = %q, want %q", err, want)
				}
			}
			last := runner.calls[len(runner.calls)-1]
			if last.name != "sc.exe" || !reflect.DeepEqual(last.args, []string{"delete", Name}) || len(runner.responses) != 0 {
				t.Fatalf("startup failure did not remove registration: calls=%+v, remaining=%+v", runner.calls, runner.responses)
			}
		})
	}
}

// Reproduce #4210's delayed startup crash after the former one-second success
// window. Use the real polling interval so this exercises the actual elapsed
// install gate instead of only asserting a configured duration.
func TestWindowsInstallRejectsCrashAfterFirstRecoveryInterval(t *testing.T) {
	const running = "TYPE: 10 WIN32_OWN_PROCESS\nSTATE: 4 RUNNING\n"
	const failed = "TYPE: 10 WIN32_OWN_PROCESS\nSTATE: 1 STOPPED\nWIN32_EXIT_CODE: 1066 (0x42a)\nSERVICE_EXIT_CODE: 1 (0x1)\n"
	runner := &fakeRunner{responses: []commandResponse{
		{code: 1060}, {}, {}, {}, {}, {},
		{output: running, repeat: int(windowsFirstRecoveryDelay/serviceStatusInterval) + 2},
		{output: failed}, {output: failed}, {},
	}}
	manager := newTestManager(t, Config{GOOS: "windows", Executable: `C:\Goobers\goobers.exe`, InstanceRoot: t.TempDir(), Runner: runner})
	started := time.Now()
	_, err := manager.Install(context.Background())
	if err == nil || !strings.Contains(err.Error(), "service-specific exit code 1") {
		t.Fatalf("Install() = %v, want delayed startup failure", err)
	}
	if elapsed := time.Since(started); elapsed < windowsFirstRecoveryDelay {
		t.Fatalf("install returned after %s, before the delayed failure", elapsed)
	}
	if last := runner.calls[len(runner.calls)-1]; !reflect.DeepEqual(last.args, []string{"delete", Name}) {
		t.Fatalf("last command = %+v; want failed service removed", last)
	}
}

func TestWindowsStartupObservationHonorsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	started := time.Now()
	_, err := waitUntilRunning(ctx, func(probeCtx context.Context) (Status, error) {
		deadline, ok := probeCtx.Deadline()
		if !ok || time.Until(deadline) > serviceStartupTimeout {
			t.Fatal("startup status probe is not bounded")
		}
		cancel()
		return Status{Supervisor: "windows-service", Installed: true, Running: true, State: "running"}, nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("waitUntilRunning() = %v, want cancellation", err)
	}
	if elapsed := time.Since(started); elapsed >= windowsReadinessWindow {
		t.Fatalf("canceled startup waited for readiness window: %s", elapsed)
	}
}

func TestWindowsStartupAllowsSlowHealthySCMQueries(t *testing.T) {
	started := time.Now()
	calls := 0
	status, err := waitUntilRunning(context.Background(), func(ctx context.Context) (Status, error) {
		calls++
		// Launching sc.exe on a slow host can exceed the nominal poll interval.
		timer := time.NewTimer(200 * time.Millisecond)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return Status{}, ctx.Err()
		case <-timer.C:
			return Status{Supervisor: "windows-service", Installed: true, Running: true, State: "running"}, nil
		}
	})
	if err != nil || !status.Running {
		t.Fatalf("healthy slow SCM startup = %+v, %v, after %d probes", status, err, calls)
	}
	if elapsed := time.Since(started); elapsed < windowsReadinessWindow {
		t.Fatalf("healthy startup passed after only %s", elapsed)
	}
	if calls >= int(windowsReadinessWindow/serviceStatusInterval)+1 {
		t.Fatalf("startup still required a fixed probe count: %d", calls)
	}
}
