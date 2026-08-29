package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

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
	if code != 0 || stderr != "" {
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
	if code != 0 || stderr != "" {
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
	if code != 1 || stderr != "" || !strings.Contains(stdout, "not installed") {
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
	if code != 0 || stderr != "" {
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
	if code != 1 || stderr != "" || !strings.Contains(stdout, "not installed") {
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

func serviceTestInstance(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "instance.yaml"), []byte("apiVersion: goobers.dev/v1alpha1\nkind: Instance\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return root
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
