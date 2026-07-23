package main

import (
	"context"
	"encoding/json"
	"flag"
	"io"
	"os"

	"github.com/goobers/goobers/internal/instance"
	daemonservice "github.com/goobers/goobers/internal/service"
)

const serviceHelp = "Usage: goobers service <subcommand> [path]\n\n" +
	"Install and manage the goobers daemon under the current platform's user\n" +
	"supervisor: systemd on Linux, launchd on macOS, or the Windows Service\n" +
	"Control Manager. The managed daemon runs `goobers up <path>`, receives the\n" +
	"same graceful-shutdown trigger as a foreground daemon, and restarts after\n" +
	"an unexpected exit with supervisor backoff.\n\n" +
	"Subcommands:\n" +
	"  install     install, enable, and start the service\n" +
	"  uninstall   gracefully stop, disable, and remove the service\n" +
	"  status      report whether the service is installed and running\n\n" +
	"Run `goobers service install -h`, `goobers service uninstall -h`, or\n" +
	"`goobers service status -h` for details. Default path is \".\".\n"

const serviceInstallHelp = "Usage: goobers service install [path]\n\n" +
	"Install, enable, and start the goobers daemon for the instance at <path>.\n" +
	"Linux and macOS install a per-user service so provider credentials retain\n" +
	"the current user's ownership. Windows installation must run from an\n" +
	"elevated terminal. An existing installation is never overwritten; uninstall\n" +
	"it first when changing the binary or instance path.\n\n" +
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

type daemonServiceManager interface {
	Install(context.Context) (daemonservice.Status, error)
	Uninstall(context.Context) error
	Status(context.Context) (daemonservice.Status, error)
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
	root, ok := parseServiceRoot("service install", "service install", args, stderr)
	if !ok {
		return 2
	}
	manager, err := newDaemonServiceManager(root)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	status, err := manager.Install(context.Background())
	if err != nil {
		pf(stderr, "error: install service: %v\n", err)
		return 1
	}
	pf(stdout, "service installed and running under %s\n", status.Supervisor)
	return 0
}

func runServiceUninstall(args []string, stdout, stderr io.Writer) int {
	root, ok := parseServiceRoot("service uninstall", "service uninstall", args, stderr)
	if !ok {
		return 2
	}
	manager, err := newDaemonServiceManager(root)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	status, err := manager.Status(context.Background())
	if err != nil {
		pf(stderr, "error: query service: %v\n", err)
		return 1
	}
	if !status.Installed {
		pln(stdout, "service is not installed")
		return 0
	}
	if err := manager.Uninstall(context.Background()); err != nil {
		pf(stderr, "error: uninstall service: %v\n", err)
		return 1
	}
	pf(stdout, "service uninstalled from %s\n", status.Supervisor)
	return 0
}

func runServiceStatus(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("service status", flag.ContinueOnError)
	fs.SetOutput(stderr)
	asJSON := fs.Bool("json", false, "render status as JSON")
	fs.Usage = helpUsage(stderr, "service status")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	root, ok := serviceRootFromFlagSet(fs, stderr)
	if !ok {
		return 2
	}
	manager, err := newDaemonServiceManager(root)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	status, err := manager.Status(context.Background())
	if err != nil {
		pf(stderr, "error: query service: %v\n", err)
		return 1
	}
	if *asJSON {
		encoder := json.NewEncoder(stdout)
		encoder.SetIndent("", "  ")
		if err := encoder.Encode(status); err != nil {
			pf(stderr, "error: encode service status: %v\n", err)
			return 1
		}
	} else {
		switch {
		case !status.Installed:
			pf(stdout, "service is not installed (%s)\n", status.Supervisor)
		case status.Running:
			pf(stdout, "service is running under %s\n", status.Supervisor)
		default:
			pf(stdout, "service is installed but %s under %s\n", status.State, status.Supervisor)
		}
	}
	if status.Running {
		return 0
	}
	return 1
}

func parseServiceRoot(flagName, helpID string, args []string, stderr io.Writer) (string, bool) {
	fs := flag.NewFlagSet(flagName, flag.ContinueOnError)
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
