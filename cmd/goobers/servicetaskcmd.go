package main

import (
	"context"
	"errors"
	"io"
	"time"

	"github.com/goobers/goobers/internal/instance"
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
	return runServiceLifecycleCommand(args, stdout, stderr, serviceLifecycleSpec[scheduledTaskManager, service.Status]{
		Name:       "service task-install",
		NewManager: newScheduledTaskManager,
		Before:     prepareServiceRoot,
		Action: func(ctx context.Context, _ string, manager scheduledTaskManager) (service.Status, error) {
			return manager.InstallTask(ctx)
		},
		ErrorPrefix: "install scheduled task",
		Success: func(stdout io.Writer, status service.Status) {
			pf(stdout, "scheduled task installed and running as %s\n", status.Account)
		},
	})
}

func runServiceTaskUninstall(args []string, stdout, stderr io.Writer) int {
	return runServiceTaskErrorCommand(args, stdout, stderr, "service task-uninstall", func(ctx context.Context, manager scheduledTaskManager) error {
		return manager.UninstallTask(ctx)
	}, "scheduled task uninstalled")
}

func runServiceTaskStop(args []string, stdout, stderr io.Writer) int {
	return runServiceTaskErrorCommand(args, stdout, stderr, "service task-stop", func(ctx context.Context, manager scheduledTaskManager) error {
		return manager.StopTask(ctx)
	}, "scheduled task stopped")
}

func runServiceTaskStart(args []string, stdout, stderr io.Writer) int {
	var startedAt time.Time
	return runServiceLifecycleCommand(args, stdout, stderr, serviceLifecycleSpec[scheduledTaskManager, service.Status]{
		Name:       "service task-start",
		NewManager: newScheduledTaskManager,
		Before: func(root string, stderr io.Writer) int {
			if exit := prepareServiceRoot(root, stderr); exit != 0 {
				return exit
			}
			// Ignore failure lines from earlier runs while allowing clock slack.
			startedAt = time.Now().Add(-time.Second)
			return 0
		},
		Action: func(ctx context.Context, _ string, manager scheduledTaskManager) (service.Status, error) {
			return manager.StartTask(ctx)
		},
		Error: func(root string, err error, stderr io.Writer) int {
			layout := instance.NewLayout(root)
			pf(stderr, "error: start scheduled task: %v", err)
			if failure := latestServiceSupervisorFailure(layout.DaemonLogFile()); failure.Message != "" && !failure.RecordedAt.Before(startedAt) {
				pf(stderr, "; %s: %s", serviceSupervisorFailureLabel(failure.Kind), failure.Message)
			}
			pf(stderr, "; inspect daemon log %s\n", layout.DaemonLogFile())
			return 1
		},
		NotInstalled:        func(err error) bool { return errors.Is(err, service.ErrNotInstalled) },
		NotInstalledMessage: "scheduled task is not installed",
		NotInstalledExit:    1,
		Success: func(stdout io.Writer, status service.Status) {
			pf(stdout, "scheduled task running as %s\n", status.Account)
		},
	})
}

func runServiceTaskStatus(args []string, stdout, stderr io.Writer) int {
	return runServiceStatusCommand(args, stdout, stderr, serviceStatusSpec[scheduledTaskManager, service.Status]{
		Name:       "service task-status",
		NewManager: newScheduledTaskManager,
		Status: func(ctx context.Context, manager scheduledTaskManager) (service.Status, error) {
			return manager.TaskStatus(ctx)
		},
		Transform:         enrichTaskStatusWithSupervisorFailure,
		StatusErrorPrefix: "query scheduled task",
		EncodeErrorPrefix: "encode scheduled task status",
		Render: func(root string, stdout io.Writer, status service.Status) {
			if !status.Installed {
				pln(stdout, "scheduled task is not installed")
			} else {
				pf(stdout, "scheduled task is %s as %s", status.State, status.Account)
				if status.LastFailure != "" {
					layout := instance.NewLayout(root)
					pf(stdout, " (last failure: %s", status.LastFailure)
					if status.SupervisorFailure != nil {
						pf(stdout, "; %s: %s", serviceSupervisorFailureLabel(status.SupervisorFailure.Kind), status.SupervisorFailure.Message)
					}
					pf(stdout, "; inspect daemon log %s)", layout.DaemonLogFile())
				}
				pln(stdout, "")
			}
		},
		Exit: func(status service.Status) int {
			if status.Running {
				return 0
			}
			return 1
		},
	})
}

func enrichTaskStatusWithSupervisorFailure(root string, status service.Status) service.Status {
	if !status.Installed || status.LastFailure == "" {
		return status
	}
	layout := instance.NewLayout(root)
	status.DaemonLogPath = layout.DaemonLogFile()
	status.SupervisorFailure = latestServiceSupervisorFailure(status.DaemonLogPath).statusPayload()
	return status
}

func serviceSupervisorFailureLabel(kind string) string {
	if kind == serviceSupervisorFailureStartup {
		return "last startup failure"
	}
	return "last supervisor failure"
}

func runServiceTaskErrorCommand(args []string, stdout, stderr io.Writer, name string, action func(context.Context, scheduledTaskManager) error, success string) int {
	return runServiceLifecycleCommand(args, stdout, stderr, serviceLifecycleSpec[scheduledTaskManager, struct{}]{
		Name:       name,
		NewManager: newScheduledTaskManager,
		Before:     inspectServiceRoot,
		Action: func(ctx context.Context, _ string, manager scheduledTaskManager) (struct{}, error) {
			return struct{}{}, action(ctx, manager)
		},
		ErrorPrefix:         name,
		NotInstalled:        func(err error) bool { return errors.Is(err, service.ErrNotInstalled) },
		NotInstalledMessage: "scheduled task is not installed",
		NotInstalledExit:    1,
		Success:             func(stdout io.Writer, _ struct{}) { pln(stdout, success) },
	})
}
