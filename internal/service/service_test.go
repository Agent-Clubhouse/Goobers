package service

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

type commandCall struct {
	name string
	args []string
}

type commandResponse struct {
	output string
	code   int
	err    error
	repeat int
}

type fakeRunner struct {
	calls     []commandCall
	responses []commandResponse
}

func (r *fakeRunner) Run(_ context.Context, name string, args ...string) ([]byte, int, error) {
	r.calls = append(r.calls, commandCall{name: name, args: append([]string(nil), args...)})
	if len(r.responses) == 0 {
		return nil, 0, nil
	}
	response := r.responses[0]
	if response.repeat > 1 {
		r.responses[0].repeat--
	} else {
		r.responses = r.responses[1:]
	}
	return []byte(response.output), response.code, response.err
}

func TestSystemdInstallStatusAndUninstall(t *testing.T) {
	runner := &fakeRunner{responses: []commandResponse{
		{},
		{},
		{},
		{
			output: "LoadState=loaded\nActiveState=active\nUnitFileState=enabled\n",
			repeat: serviceReadinessChecks,
		},
		{},
		{output: "LoadState=loaded\nActiveState=inactive\nUnitFileState=disabled\n"},
		{},
	}}
	manager := newTestManager(t, Config{
		GOOS:         "linux",
		Executable:   "/opt/Goobers Bin/goobers",
		InstanceRoot: "/srv/Goobers Instance",
		HomeDir:      t.TempDir(),
		UserName:     "test",
		Runner:       runner,
	})

	status, err := manager.Install(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !status.Installed || !status.Loaded || !status.Running || status.State != "active" {
		t.Fatalf("status = %+v", status)
	}
	unit, err := os.ReadFile(manager.systemdPath())
	if err != nil {
		t.Fatal(err)
	}
	text := string(unit)
	for _, want := range []string{
		`ExecStart="/opt/Goobers Bin/goobers" up "/srv/Goobers Instance"`,
		"WorkingDirectory=/srv/Goobers Instance",
		"Restart=on-failure",
		"RestartSec=5",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("unit missing %q:\n%s", want, text)
		}
	}

	if err := manager.Uninstall(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(manager.systemdPath()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unit still exists: %v", err)
	}
	wantCommands := []commandCall{
		{name: "loginctl", args: []string{"enable-linger", "test"}},
		{name: "systemctl", args: []string{"--user", "daemon-reload"}},
		{name: "systemctl", args: []string{"--user", "enable", "--now", "goobers.service"}},
	}
	statusCommand := commandCall{
		name: "systemctl",
		args: []string{"--user", "show", "--property=LoadState", "--property=ActiveState", "--property=UnitFileState", "goobers.service"},
	}
	for range serviceReadinessChecks {
		wantCommands = append(wantCommands, statusCommand)
	}
	wantCommands = append(wantCommands,
		commandCall{name: "systemctl", args: []string{"--user", "disable", "--now", "goobers.service"}},
		statusCommand,
		commandCall{name: "systemctl", args: []string{"--user", "daemon-reload"}},
	)
	if !reflect.DeepEqual(runner.calls, wantCommands) {
		t.Fatalf("commands = %#v, want %#v", runner.calls, wantCommands)
	}
}

func TestLaunchdInstallStatusAndUninstall(t *testing.T) {
	runner := &fakeRunner{responses: []commandResponse{
		{},
		{},
		{},
		{output: "state = running\n", repeat: serviceReadinessChecks},
		{output: "state = running\n"},
		{},
		{output: "Could not find service", code: 113},
	}}
	manager := newTestManager(t, Config{
		GOOS:         "darwin",
		Executable:   "/Applications/Goobers & Co/goobers",
		InstanceRoot: "/Users/test/Goobers & Co",
		HomeDir:      t.TempDir(),
		UID:          "501",
		Runner:       runner,
	})

	status, err := manager.Install(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !status.Installed || !status.Loaded || !status.Running || status.State != "running" {
		t.Fatalf("status = %+v", status)
	}
	plist, err := os.ReadFile(manager.launchdPath())
	if err != nil {
		t.Fatal(err)
	}
	text := string(plist)
	for _, want := range []string{
		"/Applications/Goobers &amp; Co/goobers",
		"/Users/test/Goobers &amp; Co",
		"<key>ThrottleInterval</key>",
		"<integer>5</integer>",
		"<key>SuccessfulExit</key>",
		"<false/>",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("plist missing %q:\n%s", want, text)
		}
	}

	if err := manager.Uninstall(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(manager.launchdPath()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("plist still exists: %v", err)
	}
}

func TestWindowsInstallStatusAndUninstall(t *testing.T) {
	const stopped = "TYPE               : 10  WIN32_OWN_PROCESS\nSTATE              : 1  STOPPED\n"
	const running = "TYPE               : 10  WIN32_OWN_PROCESS\nSTATE              : 4  RUNNING\n"
	runner := &fakeRunner{responses: []commandResponse{
		{output: "OpenService FAILED 1060: The specified service does not exist.\n", code: 1060},
		{},
		{},
		{},
		{},
		{},
		{output: running, repeat: serviceReadinessChecks},
		{output: stopped},
		{},
	}}
	manager := newTestManager(t, Config{
		GOOS:         "windows",
		Executable:   `C:\Program Files\goobers\goobers.exe`,
		InstanceRoot: `C:\ProgramData\goobers\instance`,
		Runner:       runner,
	})

	status, err := manager.Install(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !status.Installed || !status.Running || status.State != "running" {
		t.Fatalf("status = %+v", status)
	}
	failureCall := runner.calls[3]
	if failureCall.name != "sc.exe" || !reflect.DeepEqual(failureCall.args, []string{
		"failure", "goobers", "reset=", "86400",
		"actions=", "restart/5000/restart/30000/restart/60000",
	}) {
		t.Fatalf("failure policy command = %#v", failureCall)
	}
	failureFlagCall := runner.calls[4]
	if failureFlagCall.name != "sc.exe" || !reflect.DeepEqual(
		failureFlagCall.args,
		[]string{"failureflag", "goobers", "1"},
	) {
		t.Fatalf("failure flag command = %#v", failureFlagCall)
	}
	createArgs := strings.Join(runner.calls[1].args, " ")
	if !strings.Contains(createArgs, `"C:\Program Files\goobers\goobers.exe" up "C:\ProgramData\goobers\instance"`) {
		t.Fatalf("create args = %q", createArgs)
	}

	if err := manager.Uninstall(context.Background()); err != nil {
		t.Fatal(err)
	}
	last := runner.calls[len(runner.calls)-1]
	if last.name != "sc.exe" || !reflect.DeepEqual(last.args, []string{"delete", "goobers"}) {
		t.Fatalf("last command = %#v", last)
	}
}

func TestInstallRejectsExistingDefinition(t *testing.T) {
	manager := newTestManager(t, Config{
		GOOS:         "linux",
		Executable:   "/usr/local/bin/goobers",
		InstanceRoot: "/srv/goobers",
		HomeDir:      t.TempDir(),
		UserName:     "test",
		Runner:       &fakeRunner{},
	})
	if err := os.MkdirAll(filepath.Dir(manager.systemdPath()), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(manager.systemdPath(), []byte("existing"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Install(context.Background()); !errors.Is(err, ErrAlreadyInstalled) {
		t.Fatalf("Install error = %v, want ErrAlreadyInstalled", err)
	}
}

func TestWindowsUninstallWaitsForGracefulStop(t *testing.T) {
	const running = "TYPE               : 10  WIN32_OWN_PROCESS\nSTATE              : 4  RUNNING\n"
	const stopPending = "TYPE               : 10  WIN32_OWN_PROCESS\nSTATE              : 3  STOP_PENDING\n"
	const stopped = "TYPE               : 10  WIN32_OWN_PROCESS\nSTATE              : 1  STOPPED\n"
	runner := &fakeRunner{responses: []commandResponse{
		{output: running},
		{},
		{output: stopPending},
		{output: stopped},
		{},
	}}
	manager := newTestManager(t, Config{
		GOOS:         "windows",
		Executable:   `C:\Program Files\goobers\goobers.exe`,
		InstanceRoot: `C:\ProgramData\goobers\instance`,
		Runner:       runner,
	})
	if err := manager.Uninstall(context.Background()); err != nil {
		t.Fatal(err)
	}
	want := []commandCall{
		{name: "sc.exe", args: []string{"query", "goobers"}},
		{name: "sc.exe", args: []string{"stop", "goobers"}},
		{name: "sc.exe", args: []string{"query", "goobers"}},
		{name: "sc.exe", args: []string{"query", "goobers"}},
		{name: "sc.exe", args: []string{"delete", "goobers"}},
	}
	if !reflect.DeepEqual(runner.calls, want) {
		t.Fatalf("commands = %#v, want %#v", runner.calls, want)
	}
}

func TestWaitUntilRunningRejectsTransientRunning(t *testing.T) {
	transientErr := errors.New("end transient-running test")
	calls := 0
	_, err := waitUntilRunning(context.Background(), func(context.Context) (Status, error) {
		calls++
		switch calls {
		case 1:
			return Status{Installed: true, Running: true, State: "running"}, nil
		case 2:
			return Status{Installed: true, State: "stopped"}, nil
		default:
			return Status{}, transientErr
		}
	})
	if !errors.Is(err, transientErr) {
		t.Fatalf("waitUntilRunning() error = %v, want transient-running failure", err)
	}
	if calls != 3 {
		t.Fatalf("status checks = %d, want 3", calls)
	}
}

func TestWindowsStatusIgnoresLocalizedLabels(t *testing.T) {
	output := "NOMBRE_SERVICIO: goobers\n" +
		"        TIPO               : 10  WIN32_OWN_PROCESS\n" +
		"        ESTADO             : 4  EN_EJECUCION\n" +
		"        CODIGO_SALIDA_WIN32: 0  (0x0)\n"
	manager := newTestManager(t, Config{
		GOOS:         "windows",
		Executable:   `C:\Program Files\goobers\goobers.exe`,
		InstanceRoot: `C:\ProgramData\goobers\instance`,
		Runner: &fakeRunner{responses: []commandResponse{{
			output: output,
		}}},
	})

	status, err := manager.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !status.Installed || !status.Loaded || !status.Running || status.State != "running" {
		t.Fatalf("status = %+v", status)
	}
}

func TestSystemdRollbackRetainsUnitWhenStopFails(t *testing.T) {
	runner := &fakeRunner{responses: []commandResponse{
		{},
		{},
		{output: "enable failed after starting unit", code: 1},
		{output: "failed to stop unit", code: 1},
	}}
	manager := newTestManager(t, Config{
		GOOS:         "linux",
		Executable:   "/usr/local/bin/goobers",
		InstanceRoot: "/srv/goobers",
		HomeDir:      t.TempDir(),
		UserName:     "test",
		Runner:       runner,
	})

	if _, err := manager.Install(context.Background()); err == nil {
		t.Fatal("Install() error = nil, want rollback failure")
	}
	if _, err := os.Stat(manager.systemdPath()); err != nil {
		t.Fatalf("systemd unit was removed after failed stop: %v", err)
	}
	last := runner.calls[len(runner.calls)-1]
	if !reflect.DeepEqual(last, commandCall{
		name: "systemctl",
		args: []string{"--user", "disable", "--now", "goobers.service"},
	}) {
		t.Fatalf("last command = %#v", last)
	}
}

func TestLaunchdRollbackRetainsPlistWhenStopFails(t *testing.T) {
	runner := &fakeRunner{responses: []commandResponse{
		{},
		{output: "enable failed after bootstrap", code: 1},
		{output: "failed to boot out service", code: 1},
	}}
	manager := newTestManager(t, Config{
		GOOS:         "darwin",
		Executable:   "/usr/local/bin/goobers",
		InstanceRoot: "/srv/goobers",
		HomeDir:      t.TempDir(),
		UID:          "501",
		Runner:       runner,
	})

	if _, err := manager.Install(context.Background()); err == nil {
		t.Fatal("Install() error = nil, want rollback failure")
	}
	if _, err := os.Stat(manager.launchdPath()); err != nil {
		t.Fatalf("launchd plist was removed after failed stop: %v", err)
	}
	last := runner.calls[len(runner.calls)-1]
	if !reflect.DeepEqual(last, commandCall{
		name: "launchctl",
		args: []string{"bootout", "gui/501/com.agent-clubhouse.goobers"},
	}) {
		t.Fatalf("last command = %#v", last)
	}
}

func TestWindowsRollbackStopsServiceBeforeDelete(t *testing.T) {
	const running = "TYPE               : 10  WIN32_OWN_PROCESS\nSTATE              : 4  RUNNING\n"
	runner := &fakeRunner{responses: []commandResponse{
		{output: "OpenService FAILED 1060", code: 1060},
		{},
		{},
		{},
		{},
		{output: "start failed after service entered running state", code: 1},
		{output: running},
		{output: "failed to stop service", code: 1},
	}}
	manager := newTestManager(t, Config{
		GOOS:         "windows",
		Executable:   `C:\Program Files\goobers\goobers.exe`,
		InstanceRoot: `C:\ProgramData\goobers\instance`,
		Runner:       runner,
	})

	if _, err := manager.Install(context.Background()); err == nil {
		t.Fatal("Install() error = nil, want rollback failure")
	}
	last := runner.calls[len(runner.calls)-1]
	if !reflect.DeepEqual(last, commandCall{name: "sc.exe", args: []string{"stop", "goobers"}}) {
		t.Fatalf("last command = %#v, service registration was not retained", last)
	}
}

func TestWindowsFailureFlagErrorRollsBackRegistration(t *testing.T) {
	const stopped = "TYPE               : 10  WIN32_OWN_PROCESS\nSTATE              : 1  STOPPED\n"
	runner := &fakeRunner{responses: []commandResponse{
		{output: "OpenService FAILED 1060", code: 1060},
		{},
		{},
		{},
		{output: "failure flag denied", code: 5},
		{output: stopped},
		{},
	}}
	manager := newTestManager(t, Config{
		GOOS:         "windows",
		Executable:   `C:\Program Files\goobers\goobers.exe`,
		InstanceRoot: `C:\ProgramData\goobers\instance`,
		Runner:       runner,
	})

	if _, err := manager.Install(context.Background()); err == nil ||
		!strings.Contains(err.Error(), "failure flag denied") {
		t.Fatalf("Install() error = %v, want failureflag error", err)
	}
	last := runner.calls[len(runner.calls)-1]
	if !reflect.DeepEqual(last, commandCall{name: "sc.exe", args: []string{"delete", "goobers"}}) {
		t.Fatalf("last command = %#v, want rollback delete", last)
	}
}

func TestQuoteWindowsCommandArgPreservesTrailingBackslash(t *testing.T) {
	got := quoteWindowsCommandArg(`C:\ProgramData\goobers\`)
	if got != `"C:\ProgramData\goobers\\"` {
		t.Fatalf("quoted path = %q", got)
	}
}

func TestLaunchdStatusDistinguishesUnloadedFromQueryFailure(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(home, "Library", "LaunchAgents", launchdLabel+".plist")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("plist"), 0o644); err != nil {
		t.Fatal(err)
	}

	unloaded := newTestManager(t, Config{
		GOOS:         "darwin",
		Executable:   "/usr/local/bin/goobers",
		InstanceRoot: "/srv/goobers",
		HomeDir:      home,
		UID:          "501",
		Runner: &fakeRunner{responses: []commandResponse{{
			output: "Could not find service",
			code:   113,
		}}},
	})
	status, err := unloaded.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !status.Installed || status.Loaded || status.State != "stopped" {
		t.Fatalf("status = %+v", status)
	}

	failed := newTestManager(t, Config{
		GOOS:         "darwin",
		Executable:   "/usr/local/bin/goobers",
		InstanceRoot: "/srv/goobers",
		HomeDir:      home,
		UID:          "501",
		Runner: &fakeRunner{responses: []commandResponse{{
			output: "permission denied",
			code:   1,
		}}},
	})
	if _, err := failed.Status(context.Background()); err == nil || !strings.Contains(err.Error(), "permission denied") {
		t.Fatalf("Status error = %v", err)
	}
}

func newTestManager(t *testing.T, config Config) *Manager {
	t.Helper()
	manager, err := NewWithConfig(config)
	if err != nil {
		t.Fatal(err)
	}
	return manager
}
