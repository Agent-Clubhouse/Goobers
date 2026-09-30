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
	code := runServiceSuperviseWith([]string{root}, io.Discard, io.Discard, deps)
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
	code := runServiceSuperviseWith([]string{root}, io.Discard, io.Discard, deps)
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
