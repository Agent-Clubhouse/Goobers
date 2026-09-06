package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"io"

	"github.com/goobers/goobers/internal/service"
)

type scheduledTaskManager interface {
	InstallTask(context.Context) (service.Status, error)
	UninstallTask(context.Context) error
	StopTask(context.Context) error
	StartTask(context.Context) (service.Status, error)
	TaskStatus(context.Context) (service.Status, error)
}

var newScheduledTaskManager = func(root string) (scheduledTaskManager, error) {
	return service.New(root)
}

func runServiceTaskInstall(args []string, stdout, stderr io.Writer) int {
	root, ok := parseServiceRoot("service task-install", "service task-install", args, stderr)
	if !ok {
		return 2
	}
	manager, err := newScheduledTaskManager(root)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	status, err := manager.InstallTask(context.Background())
	if err != nil {
		pf(stderr, "error: install scheduled task: %v\n", err)
		return 1
	}
	pf(stdout, "scheduled task installed and running as %s\n", status.Account)
	return 0
}

func runServiceTaskUninstall(args []string, stdout, stderr io.Writer) int {
	return runTaskErrorCommand(args, stdout, stderr, "service task-uninstall", func(m scheduledTaskManager) error {
		return m.UninstallTask(context.Background())
	}, "scheduled task uninstalled")
}

func runServiceTaskStop(args []string, stdout, stderr io.Writer) int {
	return runTaskErrorCommand(args, stdout, stderr, "service task-stop", func(m scheduledTaskManager) error {
		return m.StopTask(context.Background())
	}, "scheduled task stopped")
}

func runServiceTaskStart(args []string, stdout, stderr io.Writer) int {
	root, ok := parseServiceRoot("service task-start", "service task-start", args, stderr)
	if !ok {
		return 2
	}
	manager, err := newScheduledTaskManager(root)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	status, err := manager.StartTask(context.Background())
	if errors.Is(err, service.ErrNotInstalled) {
		pln(stdout, "scheduled task is not installed")
		return 1
	}
	if err != nil {
		pf(stderr, "error: start scheduled task: %v\n", err)
		return 1
	}
	pf(stdout, "scheduled task running as %s\n", status.Account)
	return 0
}

func runServiceTaskStatus(args []string, stdout, stderr io.Writer) int {
	fs := newCLIFlagSet("service task-status", flag.ContinueOnError)
	fs.SetOutput(stderr)
	asJSON := fs.Bool("json", false, "render status as JSON")
	fs.Usage = helpUsage(stderr, "service task-status")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	root, ok := serviceRootFromFlagSet(fs, stderr)
	if !ok {
		return 2
	}
	manager, err := newScheduledTaskManager(root)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	status, err := manager.TaskStatus(context.Background())
	if err != nil {
		pf(stderr, "error: query scheduled task: %v\n", err)
		return 1
	}
	if *asJSON {
		encoder := json.NewEncoder(stdout)
		encoder.SetIndent("", "  ")
		if err := encoder.Encode(status); err != nil {
			pf(stderr, "error: encode scheduled task status: %v\n", err)
			return 1
		}
	} else if !status.Installed {
		pln(stdout, "scheduled task is not installed")
	} else {
		pf(stdout, "scheduled task is %s as %s\n", status.State, status.Account)
	}
	if status.Running {
		return 0
	}
	return 1
}

func runTaskErrorCommand(args []string, stdout, stderr io.Writer, name string, action func(scheduledTaskManager) error, success string) int {
	root, ok := parseServiceRoot(name, name, args, stderr)
	if !ok {
		return 2
	}
	manager, err := newScheduledTaskManager(root)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	if err := action(manager); errors.Is(err, service.ErrNotInstalled) {
		pln(stdout, "scheduled task is not installed")
		return 1
	} else if err != nil {
		pf(stderr, "error: %s: %v\n", name, err)
		return 1
	}
	pln(stdout, success)
	return 0
}
