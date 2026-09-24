package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

const upgradeHelp = "Usage: goobers upgrade [--version <tag>] [--channel stable|dogfood|beta] [path]\n\n" +
	"Reconcile a Microsoft-managed Windows installation, including its daemon\n" +
	"and supervisor, through the installed Microsoft Goobers Setup application.\n" +
	"Without --version, use the saved installation channel (stable by default).\n" +
	"An explicit version is temporary: the enabled updater later restores the\n" +
	"channel's exact supported version, including a downgrade when necessary.\n" +
	"Disable the Setup update Scheduled Task to hold a manually selected version.\n" +
	"Other installation types must use their owning package manager.\n"

func runUpgrade(args []string, stdout, stderr io.Writer) int {
	return runUpgradeWith(args, stdout, stderr, runtime.GOOS, os.Getenv("LOCALAPPDATA"),
		func(executable string, arguments []string) error {
			command := exec.Command(executable, arguments...)
			command.Stdin, command.Stdout, command.Stderr = os.Stdin, stdout, stderr
			return command.Run()
		})
}

func runUpgradeWith(args []string, stdout, stderr io.Writer, goos, localAppData string,
	run func(string, []string) error,
) int {
	fs := newCLIFlagSet("upgrade", flag.ContinueOnError)
	fs.SetOutput(stderr)
	version := fs.String("version", "", "one-time Goobers release tag")
	channel := fs.String("channel", "", "saved installation channel: stable, dogfood, or beta")
	fs.Usage = func() { fmt.Fprint(stderr, upgradeHelp) }
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() > 1 {
		fs.Usage()
		return 2
	}
	empty := false
	fs.Visit(func(f *flag.Flag) {
		if strings.TrimSpace(f.Value.String()) == "" {
			fmt.Fprintf(stderr, "error: --%s requires a nonempty value\n", f.Name)
			empty = true
		}
	})
	if empty {
		return 2
	}
	if *channel != "" && *channel != "stable" && *channel != "dogfood" && *channel != "beta" {
		fmt.Fprintln(stderr, "error: channel must be stable, dogfood, or beta")
		return 2
	}
	if goos != "windows" || !filepath.IsAbs(localAppData) {
		fmt.Fprintln(stderr, "error: upgrade requires a Microsoft-managed Windows installation; use your installation's package manager")
		return 1
	}
	executable := filepath.Join(localAppData, "Programs", "Goobers.MS", "CurrentVersion", "Goobers.MS.Setup.exe")
	if info, err := os.Stat(executable); err != nil || info.IsDir() {
		fmt.Fprintf(stderr, "error: Microsoft Goobers Setup is unavailable at %s; install or repair Setup before upgrading\n", executable)
		return 1
	}
	arguments := []string{"--update"}
	if *version != "" {
		arguments = append(arguments, "--goobers-version", *version)
	}
	if *channel != "" {
		arguments = append(arguments, "--channel", *channel)
	}
	if fs.NArg() == 1 {
		root, err := filepath.Abs(fs.Arg(0))
		if err != nil {
			fmt.Fprintf(stderr, "error: resolve instance: %v\n", err)
			return 1
		}
		arguments = append(arguments, "--instance", root)
	}
	fmt.Fprintln(stdout, "Reconciling Goobers through Microsoft Setup; waiting for daemon and supervisor health...")
	if err := run(executable, arguments); err != nil {
		fmt.Fprintf(stderr, "error: Microsoft Goobers upgrade failed: %v\n", err)
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			return exit.ExitCode()
		}
		return 1
	}
	return 0
}
