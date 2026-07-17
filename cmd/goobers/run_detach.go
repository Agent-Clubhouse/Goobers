package main

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/signals"
)

const (
	detachedRunPollInterval  = 20 * time.Millisecond
	detachedRunWorkerCommand = "_run-no-wait-worker"
)

var newDetachedRunCommand = func(name, root string) (*exec.Cmd, error) {
	self, err := os.Executable()
	if err != nil {
		return nil, err
	}
	cmd := exec.Command(self, detachedRunWorkerCommand, name, root)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	return cmd, nil
}

// runDetachedTrigger starts a private worker and returns once that worker
// reports its accepted run ID. The worker owns the standalone scheduler until
// Starter.Start returns, including when the run pauses.
func runDetachedTrigger(ctx context.Context, l instance.Layout, name, root string, stdout, stderr io.Writer) int {
	output, err := os.CreateTemp(l.SchedulerDir(), ".run-no-wait-*.log")
	if err != nil {
		pf(stderr, "error: create detached run output: %v\n", err)
		return 2
	}
	outputPath := output.Name()
	defer func() {
		_ = output.Close()
		_ = os.Remove(outputPath)
	}()

	cmd, err := newDetachedRunCommand(name, root)
	if err != nil {
		pf(stderr, "error: prepare detached run: %v\n", err)
		return 2
	}
	cmd.Stdout = output
	cmd.Stderr = output
	if err := cmd.Start(); err != nil {
		pf(stderr, "error: start detached run: %v\n", err)
		return 2
	}

	done := make(chan error, 1)
	go func() {
		done <- cmd.Wait()
	}()

	ticker := time.NewTicker(detachedRunPollInterval)
	defer ticker.Stop()
	for {
		data, readErr := os.ReadFile(outputPath)
		if readErr != nil {
			_ = cmd.Process.Kill()
			pf(stderr, "error: read detached run output: %v\n", readErr)
			return 2
		}
		if line, runID, ok := detachedRunCreated(data); ok {
			pln(stdout, line)
			pf(stdout, "inspect with: goobers trace %s %s\n", runID, root)
			return 0
		}

		select {
		case waitErr := <-done:
			data, readErr := os.ReadFile(outputPath)
			if readErr != nil {
				pf(stderr, "error: read detached run output: %v\n", readErr)
				return 2
			}
			if line, runID, ok := detachedRunCreated(data); ok {
				pln(stdout, line)
				pf(stdout, "inspect with: goobers trace %s %s\n", runID, root)
				return 0
			}
			if len(data) > 0 {
				_, _ = stderr.Write(data)
				if data[len(data)-1] != '\n' {
					pln(stderr, "")
				}
			}
			var exitErr *exec.ExitError
			if errors.As(waitErr, &exitErr) && (exitErr.ExitCode() == 1 || exitErr.ExitCode() == 2) {
				return exitErr.ExitCode()
			}
			if waitErr == nil {
				pln(stderr, "error: detached run exited without reporting a run ID")
			} else {
				pf(stderr, "error: detached run: %v\n", waitErr)
			}
			return 2
		case <-ctx.Done():
			_ = cmd.Process.Kill()
			pf(stderr, "error: detached run interrupted before dispatch: %v\n", ctx.Err())
			return 2
		case <-ticker.C:
		}
	}
}

func runDetachedWorker(args []string, stdout, stderr io.Writer) int {
	ctx, stop := signals.SetupSignalContext()
	defer stop()
	return runDetachedWorkerContext(ctx, args, stdout, stderr)
}

func runDetachedWorkerContext(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) != 2 {
		pln(stderr, "error: invalid detached run worker invocation")
		return 2
	}
	name, root := args[0], args[1]
	l := instance.NewLayout(root)
	if _, err := os.Stat(l.ConfigFile()); err != nil {
		pf(stderr, "error: %s not found (not an instance root — run `goobers init` first)\n", l.ConfigFile())
		return 2
	}
	if err := os.MkdirAll(l.SchedulerDir(), 0o755); err != nil {
		pf(stderr, "error: %v\n", err)
		return 2
	}

	release, err := acquireInstanceLock(filepath.Join(l.SchedulerDir(), "up.lock"))
	if err != nil {
		return runDelegatedTrigger(ctx, l, name, root, true, stdout, stderr)
	}
	return runStandaloneTrigger(ctx, l, name, root, true, true, release, stdout, stderr)
}

func detachedRunCreated(data []byte) (line, runID string, ok bool) {
	lines := strings.Split(string(data), "\n")
	for _, candidate := range lines[:len(lines)-1] {
		if !strings.HasPrefix(candidate, "created run ") {
			continue
		}
		fields := strings.Fields(candidate)
		if len(fields) < 3 {
			continue
		}
		return candidate, fields[2], true
	}
	return "", "", false
}
