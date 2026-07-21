package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/instance"
)

const (
	detachedRunWorkerTestRoot = "GOOBERS_TEST_DETACHED_RUN_WORKER_ROOT"
	detachedRunHelperMode     = "GOOBERS_TEST_DETACHED_RUN_HELPER_MODE"
	detachedRunHelperMarker   = "GOOBERS_TEST_DETACHED_RUN_HELPER_MARKER"
	detachedRunHelperExitCode = "GOOBERS_TEST_DETACHED_RUN_HELPER_EXIT_CODE"
)

func TestDetachedRunWorkerProcess(t *testing.T) {
	root := os.Getenv(detachedRunWorkerTestRoot)
	if root == "" {
		return
	}
	os.Exit(run([]string{detachedRunWorkerCommand, "default-implement", root}, os.Stdout, os.Stderr))
}

func TestRunDetachedTriggerHelperProcess(t *testing.T) {
	switch os.Getenv(detachedRunHelperMode) {
	case "":
		return
	case "async":
		_, _ = fmt.Fprintln(os.Stdout, "created run async-1 (workflow=demo gaggle=test)")
		time.Sleep(time.Second)
		if err := os.WriteFile(os.Getenv(detachedRunHelperMarker), nil, 0o600); err != nil {
			_, _ = fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		os.Exit(0)
	case "exit":
		_, _ = fmt.Fprintln(os.Stderr, "error: rejected before dispatch")
		code, err := strconv.Atoi(os.Getenv(detachedRunHelperExitCode))
		if err != nil {
			_, _ = fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		os.Exit(code)
	default:
		os.Exit(2)
	}
}

func TestRunDetachedTriggerReturnsAtDispatchWhileChildContinues(t *testing.T) {
	root := t.TempDir()
	l := instance.NewLayout(root)
	if err := os.MkdirAll(l.SchedulerDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(root, "child-finished")

	previous := newDetachedRunCommand
	newDetachedRunCommand = func(_, _ string) (*exec.Cmd, error) {
		cmd := exec.Command(os.Args[0], "-test.run=^TestRunDetachedTriggerHelperProcess$")
		cmd.Env = append(os.Environ(),
			detachedRunHelperMode+"=async",
			detachedRunHelperMarker+"="+marker,
		)
		return cmd, nil
	}
	t.Cleanup(func() { newDetachedRunCommand = previous })

	var stdout, stderr bytes.Buffer
	code := runDetachedTrigger(context.Background(), l, "demo", root, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("code = %d, stderr = %q", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "created run async-1") ||
		!strings.Contains(stdout.String(), "inspect with: goobers trace async-1 "+root) {
		t.Fatalf("stdout = %q", stdout.String())
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("child completed before detached trigger returned, err = %v", err)
	}

	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, err := os.Stat(marker); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("detached child did not continue after trigger returned")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestRunDetachedTriggerPreservesPredispatchExitCodes(t *testing.T) {
	root := t.TempDir()
	l := instance.NewLayout(root)
	if err := os.MkdirAll(l.SchedulerDir(), 0o755); err != nil {
		t.Fatal(err)
	}

	previous := newDetachedRunCommand
	t.Cleanup(func() { newDetachedRunCommand = previous })
	for _, wantCode := range []int{1, 2} {
		newDetachedRunCommand = func(_, _ string) (*exec.Cmd, error) {
			cmd := exec.Command(os.Args[0], "-test.run=^TestRunDetachedTriggerHelperProcess$")
			cmd.Env = append(os.Environ(),
				detachedRunHelperMode+"=exit",
				detachedRunHelperExitCode+"="+strconv.Itoa(wantCode),
			)
			return cmd, nil
		}

		var stdout, stderr bytes.Buffer
		code := runDetachedTrigger(context.Background(), l, "demo", root, &stdout, &stderr)
		if code != wantCode {
			t.Errorf("child exit %d: code = %d, stderr = %q", wantCode, code, stderr.String())
		}
		if stdout.Len() != 0 {
			t.Errorf("child exit %d: stdout = %q, want empty", wantCode, stdout.String())
		}
		if !strings.Contains(stderr.String(), "rejected before dispatch") {
			t.Errorf("child exit %d: stderr = %q", wantCode, stderr.String())
		}
	}
}

// TestDetachedRunWorkerReleasesLockWhenRunPauses exercised the
// pause-and-release-lock path via a human gate: hitting it made the runner
// checkpoint and return early without holding up.lock, letting a detached
// worker exit while genuinely still pending on an external actor. #706 made
// human gates rejected at validation time, and no other gate type takes that
// early-return branch (an agentic gate's Review call runs synchronously in
// the same detached process and holds the lock for the duration), so the
// capability this test covered has no remaining production path. Rewrite
// once durable pause/resume (#168/#465) lands with whatever mechanism
// replaces the removed human-gate branch in internal/runner/run.go.
func TestDetachedRunWorkerReleasesLockWhenRunPauses(t *testing.T) {
	t.Skip("pause-and-release-lock had no non-human-gate implementation after #706; rewrite against #168/#465's durable pause/resume mechanism")
}

func TestDetachedRunCreatedRequiresCompleteLine(t *testing.T) {
	if _, _, ok := detachedRunCreated([]byte("created run partial-id")); ok {
		t.Fatal("accepted a partially written created-run line")
	}
	line, runID, ok := detachedRunCreated([]byte("created run complete-id (workflow=demo)\n"))
	if !ok || line != "created run complete-id (workflow=demo)" || runID != "complete-id" {
		t.Fatalf("line = %q, runID = %q, ok = %v", line, runID, ok)
	}
}
