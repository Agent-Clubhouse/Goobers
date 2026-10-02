package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/selfupdate"
	daemonservice "github.com/goobers/goobers/internal/service"
)

type fakeDaemonServiceManager struct {
	status       daemonservice.Status
	startStatus  daemonservice.Status
	installErr   error
	uninstallErr error
	statusErr    error
	stopErr      error
	startErr     error
	installed    bool
	uninstalled  bool
	stopped      bool
	started      bool
}

func (m *fakeDaemonServiceManager) Install(context.Context) (daemonservice.Status, error) {
	m.installed = true
	return m.status, m.installErr
}

func (m *fakeDaemonServiceManager) Uninstall(context.Context) error {
	m.uninstalled = true
	return m.uninstallErr
}

func (m *fakeDaemonServiceManager) Status(context.Context) (daemonservice.Status, error) {
	return m.status, m.statusErr
}

func (m *fakeDaemonServiceManager) Stop(context.Context) error {
	m.stopped = true
	return m.stopErr
}

func (m *fakeDaemonServiceManager) Start(context.Context) (daemonservice.Status, error) {
	m.started = true
	return m.startStatus, m.startErr
}

func TestServiceInstall(t *testing.T) {
	root := serviceTestInstance(t)
	manager := &fakeDaemonServiceManager{status: daemonservice.Status{
		Platform: "linux", Supervisor: "systemd", Installed: true, Running: true, State: "active",
	}}
	useFakeDaemonServiceManager(t, manager)

	code, stdout, stderr := runArgs(t, "service", "install", root)
	if code != 0 || stderr != manualServiceRootHeader(t, root) {
		t.Fatalf("code = %d, stderr = %q", code, stderr)
	}
	if !manager.installed || !strings.Contains(stdout, "installed and running under systemd") {
		t.Fatalf("installed = %v, stdout = %q", manager.installed, stdout)
	}
}

func TestServiceUninstallIsIdempotent(t *testing.T) {
	root := serviceTestInstance(t)
	manager := &fakeDaemonServiceManager{status: daemonservice.Status{
		Platform: "darwin", Supervisor: "launchd", State: "not-installed",
	}}
	useFakeDaemonServiceManager(t, manager)

	code, stdout, stderr := runArgs(t, "service", "uninstall", root)
	if code != 0 || stderr != "" {
		t.Fatalf("code = %d, stderr = %q", code, stderr)
	}
	if manager.uninstalled || !strings.Contains(stdout, "not installed") {
		t.Fatalf("uninstalled = %v, stdout = %q", manager.uninstalled, stdout)
	}
}

func TestServiceStop(t *testing.T) {
	root := serviceTestInstance(t)
	manager := &fakeDaemonServiceManager{status: daemonservice.Status{
		Platform: "linux", Supervisor: "systemd", Installed: true, Running: true, State: "active",
	}}
	useFakeDaemonServiceManager(t, manager)

	code, stdout, stderr := runArgs(t, "service", "stop", root)
	if code != 0 || stderr != stoppedStatusRootHeader(t, root, 0) {
		t.Fatalf("code = %d, stderr = %q", code, stderr)
	}
	if !manager.stopped || !strings.Contains(stdout, "service stopped") {
		t.Fatalf("stopped = %v, stdout = %q", manager.stopped, stdout)
	}
}

func TestServiceStopNotInstalled(t *testing.T) {
	root := serviceTestInstance(t)
	manager := &fakeDaemonServiceManager{stopErr: daemonservice.ErrNotInstalled}
	useFakeDaemonServiceManager(t, manager)

	code, stdout, stderr := runArgs(t, "service", "stop", root)
	if code != 1 || stderr != stoppedStatusRootHeader(t, root, 0) || !strings.Contains(stdout, "not installed") {
		t.Fatalf("code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
}

func TestServiceStopReportsError(t *testing.T) {
	root := serviceTestInstance(t)
	manager := &fakeDaemonServiceManager{stopErr: errors.New("systemctl unavailable")}
	useFakeDaemonServiceManager(t, manager)

	code, _, stderr := runArgs(t, "service", "stop", root)
	if code != 1 || !strings.Contains(stderr, "systemctl unavailable") {
		t.Fatalf("code = %d, stderr = %q", code, stderr)
	}
}

func TestServiceStart(t *testing.T) {
	root := serviceTestInstance(t)
	manager := &fakeDaemonServiceManager{startStatus: daemonservice.Status{
		Platform: "darwin", Supervisor: "launchd", Installed: true, Running: true, State: "running",
	}}
	useFakeDaemonServiceManager(t, manager)

	code, stdout, stderr := runArgs(t, "service", "start", root)
	if code != 0 || stderr != manualServiceRootHeader(t, root) {
		t.Fatalf("code = %d, stderr = %q", code, stderr)
	}
	if !manager.started || !strings.Contains(stdout, "service running under launchd") {
		t.Fatalf("started = %v, stdout = %q", manager.started, stdout)
	}
}

func TestServiceStartNotInstalled(t *testing.T) {
	root := serviceTestInstance(t)
	manager := &fakeDaemonServiceManager{startErr: daemonservice.ErrNotInstalled}
	useFakeDaemonServiceManager(t, manager)

	code, stdout, stderr := runArgs(t, "service", "start", root)
	if code != 1 || stderr != manualServiceRootHeader(t, root) || !strings.Contains(stdout, "not installed") {
		t.Fatalf("code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
}

func TestServiceStartReportsError(t *testing.T) {
	root := serviceTestInstance(t)
	manager := &fakeDaemonServiceManager{startErr: errors.New("sc.exe unavailable")}
	useFakeDaemonServiceManager(t, manager)

	code, _, stderr := runArgs(t, "service", "start", root)
	if code != 1 || !strings.Contains(stderr, "sc.exe unavailable") {
		t.Fatalf("code = %d, stderr = %q", code, stderr)
	}
}

func TestServiceStatusJSONReturnsStopped(t *testing.T) {
	root := serviceTestInstance(t)
	manager := &fakeDaemonServiceManager{status: daemonservice.Status{
		Platform: "windows", Supervisor: "windows-service", Installed: true, Loaded: true, State: "stopped",
	}}
	useFakeDaemonServiceManager(t, manager)

	code, stdout, stderr := runArgs(t, "service", "status", "--json", root)
	if code != 1 || stderr != "" {
		t.Fatalf("code = %d, stderr = %q", code, stderr)
	}
	var status daemonservice.Status
	if err := json.Unmarshal([]byte(stdout), &status); err != nil {
		t.Fatalf("decode status: %v; output = %q", err, stdout)
	}
	if !status.Installed || status.Running || status.State != "stopped" {
		t.Fatalf("status = %+v", status)
	}
}

func TestServiceCommandRejectsNonInstance(t *testing.T) {
	manager := &fakeDaemonServiceManager{}
	useFakeDaemonServiceManager(t, manager)

	code, _, stderr := runArgs(t, "service", "install", t.TempDir())
	if code != 2 || !strings.Contains(stderr, "not an instance root") {
		t.Fatalf("code = %d, stderr = %q", code, stderr)
	}
	if manager.installed {
		t.Fatal("manager called for non-instance")
	}
}

func TestServiceStatusReportsQueryError(t *testing.T) {
	root := serviceTestInstance(t)
	manager := &fakeDaemonServiceManager{statusErr: errors.New("supervisor unavailable")}
	useFakeDaemonServiceManager(t, manager)

	code, _, stderr := runArgs(t, "service", "status", root)
	if code != 1 || !strings.Contains(stderr, "supervisor unavailable") {
		t.Fatalf("code = %d, stderr = %q", code, stderr)
	}
}

func TestServiceStatusHumanAndJSONOutput(t *testing.T) {
	status := daemonservice.Status{
		Platform: "linux", Supervisor: "systemd", Installed: true, Loaded: true, Running: true, State: "active", Account: "alice",
	}
	tests := []struct {
		name string
		args []string
		want string
	}{
		{
			name: "human",
			want: "service is running under systemd as alice\n",
		},
		{
			name: "json",
			args: []string{"--json"},
			want: "{\n" +
				"  \"platform\": \"linux\",\n" +
				"  \"supervisor\": \"systemd\",\n" +
				"  \"installed\": true,\n" +
				"  \"loaded\": true,\n" +
				"  \"running\": true,\n" +
				"  \"state\": \"active\",\n" +
				"  \"account\": \"alice\"\n" +
				"}\n",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := serviceTestInstance(t)
			useFakeDaemonServiceManager(t, &fakeDaemonServiceManager{status: status})
			args := append([]string{"service", "status"}, test.args...)
			args = append(args, root)
			code, stdout, stderr := runArgs(t, args...)
			if code != 0 || stdout != test.want || stderr != "" {
				t.Fatalf("code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
			}
		})
	}
}

func TestServiceNotInstalledExitCodesAndMessages(t *testing.T) {
	tests := []struct {
		name       string
		command    string
		wantCode   int
		wantStdout string
		wantStderr func(*testing.T, string) string
	}{
		{name: "uninstall", command: "uninstall", wantCode: 0, wantStdout: "service is not installed\n"},
		{name: "stop", command: "stop", wantCode: 1, wantStdout: "service is not installed\n", wantStderr: func(t *testing.T, root string) string {
			return stoppedStatusRootHeader(t, root, 0)
		}},
		{name: "start", command: "start", wantCode: 1, wantStdout: "service is not installed\n", wantStderr: manualServiceRootHeader},
		{name: "status", command: "status", wantCode: 1, wantStdout: "service is not installed (systemd)\n"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := serviceTestInstance(t)
			manager := &fakeDaemonServiceManager{
				status:   daemonservice.Status{Platform: "linux", Supervisor: "systemd", State: "not-installed"},
				stopErr:  daemonservice.ErrNotInstalled,
				startErr: daemonservice.ErrNotInstalled,
			}
			useFakeDaemonServiceManager(t, manager)
			code, stdout, stderr := runArgs(t, "service", test.command, root)
			wantStderr := ""
			if test.wantStderr != nil {
				wantStderr = test.wantStderr(t, root)
			}
			if code != test.wantCode || stdout != test.wantStdout || stderr != wantStderr {
				t.Fatalf("code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
			}
		})
	}
}

func TestServiceTaskStatusHumanAndJSONOutput(t *testing.T) {
	status := daemonservice.Status{
		Platform: "windows", Supervisor: "scheduled-task", Installed: true, Loaded: true, Running: true,
		State: "running", Account: `CONTOSO\alice`, Trigger: "logon", TaskName: `\Goobers\daemon`,
	}
	tests := []struct {
		name string
		args []string
		want string
	}{
		{
			name: "human",
			want: "scheduled task is running as CONTOSO\\alice\n",
		},
		{
			name: "json",
			args: []string{"--json"},
			want: "{\n" +
				"  \"platform\": \"windows\",\n" +
				"  \"supervisor\": \"scheduled-task\",\n" +
				"  \"installed\": true,\n" +
				"  \"loaded\": true,\n" +
				"  \"running\": true,\n" +
				"  \"state\": \"running\",\n" +
				"  \"account\": \"CONTOSO\\\\alice\",\n" +
				"  \"trigger\": \"logon\",\n" +
				"  \"taskName\": \"\\\\Goobers\\\\daemon\"\n" +
				"}\n",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := serviceTestInstance(t)
			manager := identityTaskManager{&fakeDaemonServiceManager{status: status}}
			useFakeScheduledTaskManager(t, manager)
			args := append([]string{"service", "task-status"}, test.args...)
			args = append(args, root)
			code, stdout, stderr := runArgs(t, args...)
			if code != 0 || stdout != test.want || stderr != "" {
				t.Fatalf("code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
			}
		})
	}
}

func TestServiceTaskNotInstalledExitCodesAndMessages(t *testing.T) {
	tests := []struct {
		name       string
		command    string
		wantStderr func(*testing.T, string) string
	}{
		{name: "uninstall", command: "task-uninstall", wantStderr: func(t *testing.T, root string) string {
			return stoppedStatusRootHeader(t, root, 0)
		}},
		{name: "stop", command: "task-stop", wantStderr: func(t *testing.T, root string) string {
			return stoppedStatusRootHeader(t, root, 0)
		}},
		{name: "start", command: "task-start", wantStderr: manualServiceRootHeader},
		{name: "status", command: "task-status"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := serviceTestInstance(t)
			manager := identityTaskManager{&fakeDaemonServiceManager{
				status:       daemonservice.Status{Platform: "windows", Supervisor: "scheduled-task", State: "not-installed"},
				uninstallErr: daemonservice.ErrNotInstalled,
				stopErr:      daemonservice.ErrNotInstalled,
				startErr:     daemonservice.ErrNotInstalled,
			}}
			useFakeScheduledTaskManager(t, manager)
			code, stdout, stderr := runArgs(t, "service", test.command, root)
			wantStderr := ""
			if test.wantStderr != nil {
				wantStderr = test.wantStderr(t, root)
			}
			if code != 1 || stdout != "scheduled task is not installed\n" || stderr != wantStderr {
				t.Fatalf("code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
			}
		})
	}
}

func TestServiceTaskStatusReportsLastFailureAndDaemonLog(t *testing.T) {
	root := serviceTestInstance(t)
	manager := identityTaskManager{&fakeDaemonServiceManager{status: daemonservice.Status{
		Installed:   true,
		State:       "ready",
		Account:     `CONTOSO\alice`,
		LastFailure: "0x00000001",
	}}}
	useFakeScheduledTaskManager(t, manager)

	code, stdout, stderr := runArgs(t, "service", "task-status", root)
	if code != 1 || stderr != "" {
		t.Fatalf("code = %d, stderr = %q", code, stderr)
	}
	for _, want := range []string{"last failure: 0x00000001", instance.NewLayout(root).DaemonLogFile()} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("stdout = %q, missing %q", stdout, want)
		}
	}
}

func TestExistingScheduledTaskStartupFailureIsCapturedInDaemonLogAndStatus(t *testing.T) {
	root := serviceTestInstance(t)
	diagnostic := `harness copilot preflight probe failed with configured runner.harnessPreflightArgs.copilot ["--obsolete-preflight"]: unknown flag: --obsolete-preflight; the installed CLI may no longer accept these flags`
	deps := serviceSuperviseDeps{
		runSupervisor: func(context.Context, selfupdate.SupervisorOptions) error {
			return errors.New(diagnostic)
		},
		isWindowsService: func() (bool, error) { return false, nil },
		runWindowsService: func(string, func(context.Context) int) (int, error) {
			t.Fatal("Windows service runner called by scheduled-task supervisor path")
			return 0, nil
		},
		setupSignalContext: func() (context.Context, func()) {
			ctx, cancel := context.WithCancel(context.Background())
			return ctx, cancel
		},
	}
	code := runServiceSuperviseWith([]string{serviceSuperviseDaemonLogFlag, root}, io.Discard, io.Discard, deps)
	if code != 1 {
		t.Fatalf("service supervisor code = %d, want startup failure", code)
	}
	logPath := instance.NewLayout(root).DaemonLogFile()
	log, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read daemon log: %v", err)
	}
	for _, want := range []string{serviceSupervisorFailureStartup + ":", diagnostic} {
		if !strings.Contains(string(log), want) {
			t.Fatalf("daemon log = %q, missing %q", log, want)
		}
	}

	manager := identityTaskManager{&fakeDaemonServiceManager{status: daemonservice.Status{
		Installed:   true,
		State:       "ready",
		Account:     `CONTOSO\alice`,
		LastFailure: "0x00000001",
	}}}
	useFakeScheduledTaskManager(t, manager)
	code, stdout, stderr := runArgs(t, "service", "task-status", root)
	if code != 1 || stderr != "" {
		t.Fatalf("task-status code = %d, stderr = %q", code, stderr)
	}
	for _, want := range []string{"last startup failure", diagnostic, logPath} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("stdout = %q, missing %q", stdout, want)
		}
	}

	code, stdout, stderr = runArgs(t, "service", "task-status", "--json", root)
	if code != 1 || stderr != "" {
		t.Fatalf("task-status --json code = %d, stderr = %q", code, stderr)
	}
	var status daemonservice.Status
	if err := json.Unmarshal([]byte(stdout), &status); err != nil {
		t.Fatalf("decode JSON status: %v; output = %q", err, stdout)
	}
	if status.DaemonLogPath != logPath {
		t.Fatalf("daemonLogPath = %q, want %q", status.DaemonLogPath, logPath)
	}
	if status.SupervisorFailure == nil ||
		status.SupervisorFailure.Kind != serviceSupervisorFailureStartup ||
		!strings.Contains(status.SupervisorFailure.Message, diagnostic) ||
		status.SupervisorFailure.RecordedAt == "" {
		t.Fatalf("supervisorFailure = %+v, want startup diagnostic", status.SupervisorFailure)
	}
}

func TestServiceSupervisorRecordsRuntimeFailureAfterReadiness(t *testing.T) {
	root := serviceTestInstance(t)
	diagnostic := "daemon child crashed after startup"
	deps := serviceSuperviseDeps{
		runSupervisor: func(_ context.Context, opts selfupdate.SupervisorOptions) error {
			pf(opts.Stdout, "daemon started at %s (1 workflow(s)); API listening at http://127.0.0.1:0/api\n", root)
			return errors.New(diagnostic)
		},
		isWindowsService: func() (bool, error) { return false, nil },
		runWindowsService: func(string, func(context.Context) int) (int, error) {
			t.Fatal("Windows service runner called by scheduled-task supervisor path")
			return 0, nil
		},
		setupSignalContext: func() (context.Context, func()) {
			ctx, cancel := context.WithCancel(context.Background())
			return ctx, cancel
		},
	}
	code := runServiceSuperviseWith([]string{serviceSuperviseDaemonLogFlag, root}, io.Discard, io.Discard, deps)
	if code != 1 {
		t.Fatalf("service supervisor code = %d, want runtime failure", code)
	}
	logPath := instance.NewLayout(root).DaemonLogFile()
	log, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read daemon log: %v", err)
	}
	if !strings.Contains(string(log), serviceSupervisorFailureRuntime+":") || strings.Contains(string(log), serviceSupervisorFailureStartup+":") {
		t.Fatalf("daemon log = %q, want runtime failure only", log)
	}

	manager := identityTaskManager{&fakeDaemonServiceManager{status: daemonservice.Status{
		Installed:   true,
		State:       "ready",
		Account:     `CONTOSO\alice`,
		LastFailure: "0x00000001",
	}}}
	useFakeScheduledTaskManager(t, manager)
	code, stdout, stderr := runArgs(t, "service", "task-status", root)
	if code != 1 || stderr != "" {
		t.Fatalf("task-status code = %d, stderr = %q", code, stderr)
	}
	for _, want := range []string{"last supervisor failure", diagnostic, logPath} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("stdout = %q, missing %q", stdout, want)
		}
	}
	if strings.Contains(stdout, "last startup failure") {
		t.Fatalf("runtime failure was reported as startup: %q", stdout)
	}
}

func TestServiceSupervisorWithoutDaemonLogFlagLeavesDaemonLogAlone(t *testing.T) {
	root := serviceTestInstance(t)
	deps := serviceSuperviseDeps{
		runSupervisor: func(context.Context, selfupdate.SupervisorOptions) error {
			return errors.New("boom")
		},
		isWindowsService:  func() (bool, error) { return false, nil },
		runWindowsService: func(string, func(context.Context) int) (int, error) { return 0, nil },
		setupSignalContext: func() (context.Context, func()) {
			ctx, cancel := context.WithCancel(context.Background())
			return ctx, cancel
		},
	}
	var stderr strings.Builder
	if code := runServiceSuperviseWith([]string{root}, io.Discard, &stderr, deps); code != 1 {
		t.Fatalf("code = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "error: supervise daemon: boom") {
		t.Fatalf("stderr = %q, want supervisor error", stderr.String())
	}
	if _, err := os.Stat(instance.NewLayout(root).DaemonLogFile()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("daemon log stat err = %v, want not exist (foreground/systemd/launchd keep their own streams)", err)
	}
}

func TestServiceSupervisorDaemonLogRedactsAndCapturesChildOutput(t *testing.T) {
	root := serviceTestInstance(t)
	deps := serviceSuperviseDeps{
		runSupervisor: func(_ context.Context, opts selfupdate.SupervisorOptions) error {
			pf(opts.Stderr, "child stderr line\n")
			return errors.New("probe failed: Authorization: Bearer sekret-token")
		},
		isWindowsService:  func() (bool, error) { return false, nil },
		runWindowsService: func(string, func(context.Context) int) (int, error) { return 0, nil },
		setupSignalContext: func() (context.Context, func()) {
			ctx, cancel := context.WithCancel(context.Background())
			return ctx, cancel
		},
	}
	if code := runServiceSuperviseWith([]string{serviceSuperviseDaemonLogFlag, root}, io.Discard, io.Discard, deps); code != 1 {
		t.Fatalf("code = %d, want 1", code)
	}
	log, err := os.ReadFile(instance.NewLayout(root).DaemonLogFile())
	if err != nil {
		t.Fatalf("read daemon log: %v", err)
	}
	if !strings.Contains(string(log), "child stderr line") || strings.Contains(string(log), "sekret-token") {
		t.Fatalf("daemon log = %q, want child output captured and credentials redacted", log)
	}
	if failure := latestServiceSupervisorFailure(instance.NewLayout(root).DaemonLogFile()); failure.Kind != serviceSupervisorFailureStartup {
		t.Fatalf("latest failure = %+v, want startup failure", failure)
	}
}

func TestServiceTaskStartIgnoresFailureLoggedBeforeThisStart(t *testing.T) {
	root := serviceTestInstance(t)
	stale := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339Nano) + " " + serviceSupervisorFailureStartup + ": error: supervise daemon: stale diagnostic\n"
	if err := os.WriteFile(instance.NewLayout(root).DaemonLogFile(), []byte(stale), 0o644); err != nil {
		t.Fatal(err)
	}
	manager := identityTaskManager{&fakeDaemonServiceManager{startErr: errors.New("service failed while starting: 0x00000001")}}
	useFakeScheduledTaskManager(t, manager)

	code, _, stderr := runArgs(t, "service", "task-start", root)
	if code != 1 || strings.Contains(stderr, "stale diagnostic") {
		t.Fatalf("code = %d, stderr = %q; want failure without the stale diagnostic", code, stderr)
	}
}

func TestServiceTaskStartReportsDaemonLogOnStartupFailure(t *testing.T) {
	root := serviceTestInstance(t)
	manager := identityTaskManager{&fakeDaemonServiceManager{startErr: errors.New("service failed while starting: 0x00000001")}}
	useFakeScheduledTaskManager(t, manager)

	code, _, stderr := runArgs(t, "service", "task-start", root)
	if code != 1 {
		t.Fatalf("code = %d, stderr = %q", code, stderr)
	}
	for _, want := range []string{"service failed while starting: 0x00000001", instance.NewLayout(root).DaemonLogFile()} {
		if !strings.Contains(stderr, want) {
			t.Fatalf("stderr = %q, missing %q", stderr, want)
		}
	}
}

func serviceTestInstance(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "instance.yaml"), []byte("apiVersion: goobers.dev/v1alpha1\nkind: Instance\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := instance.EnsureRootIdentity(context.Background(), root); err != nil {
		t.Fatal(err)
	}
	return root
}

func manualServiceRootHeader(t *testing.T, root string) string {
	t.Helper()
	id, err := instance.ReadRootIdentity(root)
	if err != nil {
		t.Fatal(err)
	}
	return fmt.Sprintf("Instance root: %q; instance ID: %q\n", canonicalStatusRoot(root), id)
}

func useFakeDaemonServiceManager(t *testing.T, manager daemonServiceManager) {
	t.Helper()
	previous := newDaemonServiceManager
	newDaemonServiceManager = func(string) (daemonServiceManager, error) {
		return manager, nil
	}
	t.Cleanup(func() {
		newDaemonServiceManager = previous
	})
}

func useFakeScheduledTaskManager(t *testing.T, manager scheduledTaskManager) {
	t.Helper()
	previous := newScheduledTaskManager
	newScheduledTaskManager = func(string) (scheduledTaskManager, error) {
		return manager, nil
	}
	t.Cleanup(func() {
		newScheduledTaskManager = previous
	})
}
