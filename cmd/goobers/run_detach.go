package main

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"

	"github.com/goobers/goobers/internal/instance"
)

const detachedRunPollInterval = 20 * time.Millisecond

var newDetachedRunCommand = func(name, root string) (*exec.Cmd, error) {
	self, err := os.Executable()
	if err != nil {
		return nil, err
	}
	cmd := exec.Command(self, "run", name, root)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	return cmd, nil
}

// runDetachedTrigger starts a blocking `goobers run` child and returns once
// that child reports its accepted run ID. The child owns the standalone
// scheduler and keeps the run alive after this CLI process exits.
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
