package main

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/instance"
)

func TestRunDetachedTriggerReturnsAtDispatchWhileChildContinues(t *testing.T) {
	root := t.TempDir()
	l := instance.NewLayout(root)
	if err := os.MkdirAll(l.SchedulerDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(root, "child-finished")

	previous := newDetachedRunCommand
	newDetachedRunCommand = func(_, _ string) (*exec.Cmd, error) {
		return exec.Command("sh", "-c",
			`printf 'created run async-1 (workflow=demo gaggle=test)\n'; sleep 1; touch "$1"`,
			"sh", marker), nil
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
			return exec.Command("sh", "-c", `printf 'error: rejected before dispatch\n'; exit "$1"`,
				"sh", strconv.Itoa(wantCode)), nil
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

func TestDetachedRunCreatedRequiresCompleteLine(t *testing.T) {
	if _, _, ok := detachedRunCreated([]byte("created run partial-id")); ok {
		t.Fatal("accepted a partially written created-run line")
	}
	line, runID, ok := detachedRunCreated([]byte("created run complete-id (workflow=demo)\n"))
	if !ok || line != "created run complete-id (workflow=demo)" || runID != "complete-id" {
		t.Fatalf("line = %q, runID = %q, ok = %v", line, runID, ok)
	}
}
