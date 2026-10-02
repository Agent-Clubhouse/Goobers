package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"runtime"

	"github.com/goobers/goobers/internal/instance"
	daemonservice "github.com/goobers/goobers/internal/service"
)

const serviceHelp = "Usage: goobers service <subcommand> [path]\n\n" +
	"Install and manage the goobers daemon under the current platform's user\n" +
	"supervisor: systemd on Linux, launchd on macOS, or the Windows Service\n" +
	"Control Manager. The stable service host launches the instance's mutable\n" +
	"binary, receives the same graceful-shutdown trigger as a foreground daemon,\n" +
	"and owns validated self-update handoff, health checks, and rollback.\n\n" +
	"Subcommands:\n" +
	"  install     install, enable, and start the service\n" +
	"  uninstall   gracefully stop, disable, and remove the service\n" +
	"  start       resume an installed-but-stopped service\n" +
	"  stop        halt the running service without disabling or removing it\n" +
	"  status      report whether the service is installed and running\n\n" +
	"On Windows, use `service task-install`, `service task-start`,\n" +
	"`service task-stop`, `service task-status`, and `service task-uninstall`\n" +
	"for the per-user Scheduled Task supervisor. `service install` creates a\n" +
	"machine SCM service as LocalSystem and requires explicit acknowledgement.\n\n" +
	"Run `goobers service install -h`, `goobers service uninstall -h`,\n" +
	"`goobers service start -h`, `goobers service stop -h`, or\n" +
	"`goobers service status -h` for details. Default path is \".\".\n"

const serviceInstallHelp = "Usage: goobers service install [--confirm-local-system] [--acknowledge-local-system] [path]\n\n" +
	"Install, enable, and start the goobers daemon for the instance at <path>.\n" +
	"Linux and macOS install a per-user service so provider credentials retain\n" +
	"the current user's ownership. Windows installation must run from an\n" +
	"elevated terminal. An existing installation is never overwritten; uninstall\n" +
	"it first when changing the stable host binary or instance path. Product\n" +
	"binary updates use the self-update workflow instead.\n\n" +
	"On Windows this creates a LocalSystem SCM service. It cannot access\n" +
	"user-scoped CLI accounts, Credential Manager, %%LOCALAPPDATA%%, mapped\n" +
	"drives, or user PATH. Use --confirm-local-system interactively or\n" +
	"--acknowledge-local-system in scripts after reviewing that warning.\n\n" +
	"Exit codes: 0 = installed and running, 1 = installation/start error,\n" +
	"2 = usage error or not an instance root.\n"

const serviceUninstallHelp = "Usage: goobers service uninstall [path]\n\n" +
	"Gracefully stop the managed daemon, disable it, and remove its supervisor\n" +
	"registration. Uninstalling an absent service is a successful no-op.\n\n" +
	"Exit codes: 0 = absent after the operation, 1 = stop/removal error,\n" +
	"2 = usage error or not an instance root.\n"

const serviceStatusHelp = "Usage: goobers service status [--json] [path]\n\n" +
	"Report the current platform supervisor, registration path (when applicable),\n" +
	"and whether the goobers daemon is installed, loaded, and running.\n\n" +
	"Exit codes: 0 = running, 1 = stopped/not installed/query error,\n" +
	"2 = usage error or not an instance root.\n"

const serviceStopHelp = "Usage: goobers service stop [path]\n\n" +
	"Halt the running goobers daemon without disabling or removing its\n" +
	"supervisor registration (#2073) — distinct from uninstall, which folds\n" +
	"stop, disable, and removal into one step. `goobers service status` then\n" +
	"reports \"installed, not running\"; `goobers service start` resumes it.\n" +
	"Stopping an already-stopped service is a successful no-op.\n\n" +
	"Exit codes: 0 = stopped (or already stopped), 1 = not installed/stop\n" +
	"error, 2 = usage error or not an instance root.\n"

const serviceStartHelp = "Usage: goobers service start [path]\n\n" +
	"Resume an installed-but-stopped goobers daemon (#2073) without\n" +
	"re-registering it. Starting an already-running service is a successful\n" +
	"no-op.\n\n" +
	"Exit codes: 0 = running, 1 = not installed/start error,\n" +
	"2 = usage error or not an instance root.\n"

type daemonServiceManager interface {
	Install(context.Context) (daemonservice.Status, error)
	Uninstall(context.Context) error
	Status(context.Context) (daemonservice.Status, error)
	Stop(context.Context) error
	Start(context.Context) (daemonservice.Status, error)
}

var newDaemonServiceManager = func(root string) (daemonServiceManager, error) {
	return daemonservice.New(root)
}

func runService(args []string, stdout, stderr io.Writer) int {
	if len(args) == 1 && (args[0] == "-h" || args[0] == "--help" || args[0] == "help") {
		pf(stdout, "%s", serviceHelp)
		return 0
	}
	if len(args) > 0 {
		pf(stderr, "error: unknown service command %q\n", args[0])
	}
	pf(stderr, "%s", serviceHelp)
	return 2
}

func runServiceInstall(args []string, stdout, stderr io.Writer) int {
	var confirm, ack bool
	return runServiceLifecycleCommand(args, stdout, stderr, serviceLifecycleSpec[daemonServiceManager, daemonservice.Status]{
		Name: "service install",
		ParseRoot: func(args []string, stderr io.Writer) (string, bool) {
			fs := newCLIFlagSet("service install", flag.ContinueOnError)
			fs.SetOutput(stderr)
			fs.BoolVar(&confirm, "confirm-local-system", false, "confirm the interactive LocalSystem service warning")
			fs.BoolVar(&ack, "acknowledge-local-system", false, "acknowledge LocalSystem and its user-resource limitations")
			fs.Usage = helpUsage(stderr, "service install")
			if err := fs.Parse(args); err != nil {
				return "", false
			}
			return serviceRootFromFlagSet(fs, stderr)
		},
		NewManager: newDaemonServiceManager,
		Check: func(manager daemonServiceManager, stderr io.Writer) int {
			if runtime.GOOS == "windows" {
				if _, realManager := manager.(*daemonservice.Manager); realManager && !confirm && !ack {
					pf(stderr, "warning: Windows service install creates a LocalSystem service. LocalSystem cannot inherit the interactive user's GitHub CLI accounts, Copilot/Claude sessions, %%LOCALAPPDATA%%, Credential Manager entries, mapped drives, or user PATH.\n")
					pf(stderr, "error: rerun interactively with --confirm-local-system or non-interactively with --acknowledge-local-system\n")
					return 2
				}
			}
			return 0
		},
		Before: prepareServiceRoot,
		Action: func(ctx context.Context, _ string, manager daemonServiceManager) (daemonservice.Status, error) {
			return manager.Install(ctx)
		},
		ErrorPrefix: "install service",
		Success: func(stdout io.Writer, status daemonservice.Status) {
			pf(stdout, "service installed and running under %s", status.Supervisor)
			if status.Account != "" {
				pf(stdout, " as %s", status.Account)
			}
			pln(stdout, "")
		},
	})
}

func runServiceUninstall(args []string, stdout, stderr io.Writer) int {
	return runServiceLifecycleCommand(args, stdout, stderr, serviceLifecycleSpec[daemonServiceManager, daemonservice.Status]{
		Name:       "service uninstall",
		NewManager: newDaemonServiceManager,
		Action: func(ctx context.Context, root string, manager daemonServiceManager) (daemonservice.Status, error) {
			status, err := manager.Status(ctx)
			if err != nil {
				return status, fmt.Errorf("query service: %w", err)
			}
			if !status.Installed {
				return status, nil
			}
			if err := displayRootInspection(instance.NewLayout(root), stderr); err != nil {
				return status, serviceLifecycleDisplayError{err}
			}
			if err := manager.Uninstall(ctx); err != nil {
				return status, fmt.Errorf("uninstall service: %w", err)
			}
			return status, nil
		},
		Error: func(_ string, err error, stderr io.Writer) int {
			var displayErr serviceLifecycleDisplayError
			if errors.As(err, &displayErr) {
				return 2
			}
			pf(stderr, "error: %v\n", err)
			return 1
		},
		Success: func(stdout io.Writer, status daemonservice.Status) {
			if !status.Installed {
				pln(stdout, "service is not installed")
				return
			}
			pf(stdout, "service uninstalled from %s\n", status.Supervisor)
		},
	})
}

func runServiceStop(args []string, stdout, stderr io.Writer) int {
	return runServiceLifecycleCommand(args, stdout, stderr, serviceLifecycleSpec[daemonServiceManager, struct{}]{
		Name:       "service stop",
		NewManager: newDaemonServiceManager,
		Before:     inspectServiceRoot,
		Action: func(ctx context.Context, _ string, manager daemonServiceManager) (struct{}, error) {
			return struct{}{}, manager.Stop(ctx)
		},
		ErrorPrefix:         "stop service",
		NotInstalled:        func(err error) bool { return errors.Is(err, daemonservice.ErrNotInstalled) },
		NotInstalledMessage: "service is not installed",
		NotInstalledExit:    1,
		Success:             func(stdout io.Writer, _ struct{}) { pln(stdout, "service stopped") },
	})
}

func runServiceStart(args []string, stdout, stderr io.Writer) int {
	return runServiceLifecycleCommand(args, stdout, stderr, serviceLifecycleSpec[daemonServiceManager, daemonservice.Status]{
		Name:       "service start",
		NewManager: newDaemonServiceManager,
		Before:     prepareServiceRoot,
		Action: func(ctx context.Context, _ string, manager daemonServiceManager) (daemonservice.Status, error) {
			return manager.Start(ctx)
		},
		ErrorPrefix:         "start service",
		NotInstalled:        func(err error) bool { return errors.Is(err, daemonservice.ErrNotInstalled) },
		NotInstalledMessage: "service is not installed",
		NotInstalledExit:    1,
		Success: func(stdout io.Writer, status daemonservice.Status) {
			pf(stdout, "service running under %s", status.Supervisor)
			if status.Account != "" {
				pf(stdout, " as %s", status.Account)
			}
			pln(stdout, "")
		},
	})
}

func runServiceStatus(args []string, stdout, stderr io.Writer) int {
	return runServiceStatusCommand(args, stdout, stderr, serviceStatusSpec[daemonServiceManager, daemonservice.Status]{
		Name:       "service status",
		NewManager: newDaemonServiceManager,
		Status: func(ctx context.Context, manager daemonServiceManager) (daemonservice.Status, error) {
			return manager.Status(ctx)
		},
		StatusErrorPrefix: "query service",
		EncodeErrorPrefix: "encode service status",
		Render: func(_ string, stdout io.Writer, status daemonservice.Status) {
			switch {
			case !status.Installed:
				pf(stdout, "service is not installed (%s)\n", status.Supervisor)
			case status.Running:
				pf(stdout, "service is running under %s", status.Supervisor)
				if status.Account != "" {
					pf(stdout, " as %s", status.Account)
				}
				pln(stdout, "")
			default:
				pf(stdout, "service is installed but %s under %s\n", status.State, status.Supervisor)
			}
		},
		Exit: func(status daemonservice.Status) int {
			if status.Running {
				return 0
			}
			return 1
		},
	})
}

type serviceLifecycleDisplayError struct{ error }

func prepareServiceRoot(root string, stderr io.Writer) int {
	if err := prepareManualRoot(instance.NewLayout(root), stderr); err != nil {
		pf(stderr, "error: %v\n", err)
		return 2
	}
	return 0
}

func inspectServiceRoot(root string, stderr io.Writer) int {
	if err := displayRootInspection(instance.NewLayout(root), stderr); err != nil {
		return 2
	}
	return 0
}

func parseServiceRoot(flagName, helpID string, args []string, stderr io.Writer) (string, bool) {
	fs := newCLIFlagSet(flagName, flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = helpUsage(stderr, helpID)
	if err := fs.Parse(args); err != nil {
		return "", false
	}
	return serviceRootFromFlagSet(fs, stderr)
}

func serviceRootFromFlagSet(fs *flag.FlagSet, stderr io.Writer) (string, bool) {
	if fs.NArg() > 1 {
		fs.Usage()
		return "", false
	}
	root := "."
	if fs.NArg() == 1 {
		root = fs.Arg(0)
	}
	layout := instance.NewLayout(root)
	if _, err := os.Stat(layout.ConfigFile()); err != nil {
		pf(stderr, "error: %s not found (not an instance root — run `goobers init` first)\n", layout.ConfigFile())
		return "", false
	}
	return root, true
}
