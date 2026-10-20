//go:build windows

package proc

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

func TestWindowsQuiescenceRefusesMissingOrBreakawayCustody(t *testing.T) {
	for _, tree := range []*Tree{{}, {closed: true}, {strictCustody: true, closed: true}} {
		if err := tree.StopAndWait(t.Context()); !errors.Is(err, ErrQuiescenceUnobservable) {
			t.Fatalf("missing custody accepted: %v", err)
		}
	}
}

// This test qualifies the Windows runtime only when actually executed there;
// cross-compiling it is not evidence of Job membership or accounting behavior.
func TestWindowsQuiescenceWaitsForOwnedJobDescendants(t *testing.T) {
	root := t.TempDir()
	child, grandchild := filepath.Join(root, "child.pid"), filepath.Join(root, "grandchild.pid")
	cmd := exec.Command(os.Args[0], "-test.run=^TestProcessTreeHelper$")
	cmd.Env = append(os.Environ(), "GOOBERS_PROC_HELPER_ROLE=root", "GOOBERS_PROC_HELPER_PID="+child, "GOOBERS_PROC_HELPER_GRANDCHILD="+grandchild)
	// Exercise the memory-bound entry point too; it must preserve strict custody
	// even when this platform has no supported memory enforcement mechanism.
	tree, bound, err := StartBounded(cmd, 1024, true, StartOptions{RequireQuiescence: true})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = bound.Release() }()
	waited := make(chan error, 1)
	go func() { waited <- cmd.Wait() }()
	defer func() {
		_ = tree.Kill()
		select {
		case <-waited:
		case <-time.After(10 * time.Second):
			t.Error("command Wait did not join")
		}
	}()
	var limits windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION
	if err := windows.QueryInformationJobObject(tree.job, windows.JobObjectExtendedLimitInformation, uintptr(unsafe.Pointer(&limits)), uint32(unsafe.Sizeof(limits)), nil); err != nil {
		t.Fatal(err)
	}
	if limits.BasicLimitInformation.LimitFlags&(windows.JOB_OBJECT_LIMIT_BREAKAWAY_OK|windows.JOB_OBJECT_LIMIT_SILENT_BREAKAWAY_OK) != 0 {
		t.Fatal("strict Job permits breakaway")
	}
	var pids []int
	deadline := time.Now().Add(25 * time.Second)
	for _, marker := range []string{child, grandchild} {
		for {
			data, readErr := os.ReadFile(marker)
			pid, parseErr := strconv.Atoi(strings.TrimSpace(string(data)))
			if readErr == nil && parseErr == nil {
				pids = append(pids, pid)
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("descendant did not start")
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	for _, pid := range pids {
		if !Alive(pid) {
			t.Fatal("descendant exited before stop")
		}
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	if err := tree.StopAndWait(ctx); err != nil {
		t.Fatal(err)
	}
	for _, pid := range pids {
		if Alive(pid) {
			t.Fatalf("writer %d survived successful custody proof", pid)
		}
	}
	if err := tree.StopAndWait(ctx); err != nil {
		t.Fatalf("retained proof not idempotent: %v", err)
	}
}
