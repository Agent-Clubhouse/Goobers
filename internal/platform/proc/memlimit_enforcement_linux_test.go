//go:build linux

package proc

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestStageMemoryBoundKillsAnOverAllocatingChild is #4070's acceptance
// criterion in executable form: "a deliberately over-allocating child is
// terminated with a named, journaled reason rather than taking the daemon down
// with it."
//
// It is the only test here that proves ENFORCEMENT. Everything else in this
// package's memory-bound tests drives the filesystem and decision logic
// against a fake tree, which is what makes them runnable on any host — but a
// fake cgroup cannot kill anything, so passing them says nothing about
// whether the kernel actually honours the bound. Only a real cgroup can
// answer that, and the honest name for the rest is "the bound was prepared",
// not "the bound works".
//
// It is GATED, and deliberately not skipped silently on capability alone:
// the environment opt-in means a run that does not exercise enforcement says
// so, rather than a green package implying a bound nobody proved. Run with:
//
//	GOOBERS_CGROUP_ENFORCEMENT_TEST=1 go test ./internal/platform/proc -run OverAllocating
//
// It needs cgroup v2 with the memory controller delegated to child cgroups of
// the caller's own cgroup — a container started with a writable cgroupfs and
// `memory` in cgroup.subtree_control, or a user slice under systemd.
func TestStageMemoryBoundKillsAnOverAllocatingChild(t *testing.T) {
	if os.Getenv("GOOBERS_CGROUP_ENFORCEMENT_TEST") == "" {
		t.Skip("enforcement test skipped: set GOOBERS_CGROUP_ENFORCEMENT_TEST=1 on a host with a delegated cgroup v2 memory controller")
	}
	// A precondition failure must be LOUD, not a skip: the whole point of the
	// opt-in is that somebody asked for enforcement to be proven here.
	if _, err := prepareStageCgroup(64 << 20); err != nil {
		t.Fatalf("precondition: no delegated cgroup v2 memory controller, so enforcement cannot be proven: %v", err)
	}

	const bound = 64 << 20 // 64Mi: far above the helper's own footprint

	// The child is THIS test binary in helper mode, not a shell pipeline.
	// An earlier version used `head -c … /dev/zero | tr … > /dev/null`, which
	// streams through small pipe buffers and never holds anything resident —
	// it ran to completion inside a 64Mi bound and made the test report that
	// enforcement had failed. A cgroup bounds RESIDENT memory, so the child
	// has to actually hold and touch its pages.
	cmd := exec.Command(os.Args[0], "-test.run=TestMemoryBoundAllocatorHelper")
	cmd.Env = append(os.Environ(), memoryBoundHelperEnv+"=1")
	// Captured so a child that failed for some OTHER reason (skipped, panicked,
	// never allocated) reports why, instead of surfacing as an indistinguishable
	// "the bound did not hold".
	var childOutput bytes.Buffer
	cmd.Stdout = &childOutput
	cmd.Stderr = &childOutput

	tree, boundHandle, err := StartBounded(cmd, bound, false)
	if err != nil {
		t.Fatalf("StartBounded: %v", err)
	}
	t.Cleanup(func() { _ = boundHandle.Release() })
	if got := boundHandle.mechanism; got != MechanismCgroup {
		t.Fatalf("Mechanism() = %q, want %q — the enforcement path was not taken", got, MechanismCgroup)
	}

	// Assert PLACEMENT directly rather than inferring it from the kill. If the
	// child is not in the cgroup, "no kill" and "bound not enforced" look
	// identical, and the test would report the wrong cause.
	cg, ok := boundHandle.impl.(*stageCgroup)
	if !ok {
		t.Fatalf("bound impl is %T, want *stageCgroup", boundHandle.impl)
	}
	if procs, readErr := os.ReadFile(filepath.Join(cg.path, "cgroup.procs")); readErr != nil {
		t.Fatalf("read stage cgroup.procs: %v", readErr)
	} else if strings.TrimSpace(string(procs)) == "" {
		max, _ := os.ReadFile(filepath.Join(cg.path, "memory.max"))
		t.Fatalf("the child was never placed in the stage cgroup %s (cgroup.procs empty, memory.max=%q); "+
			"CLONE_INTO_CGROUP did not take effect", cg.path, strings.TrimSpace(string(max)))
	}

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case <-done:
	case <-time.After(60 * time.Second):
		_ = tree.Kill()
		t.Fatal("the over-allocating child was never terminated; the bound did not hold")
	}

	exceeded, reason := boundHandle.Exceeded()
	if !exceeded {
		t.Fatalf("Exceeded() false after a child allocated many times its bound.\nchild output:\n%s", childOutput.String())
	}
	// The reason is the deliverable: an unexplained exit 137 is what this
	// change exists to replace.
	for _, want := range []string{"exceeded its", "per-stage memory bound", "#4070", "runner.stageMemoryLimit"} {
		if !strings.Contains(reason, want) {
			t.Errorf("reason %q does not mention %q", reason, want)
		}
	}

	// And the daemon — this test process — is still here, which is the whole
	// property the feature exists for: the over-allocating child died, not the
	// process that spawned it.
	if !Alive(os.Getpid()) {
		t.Fatal("unreachable: the test process must have survived its child")
	}
}

// memoryBoundHelperEnv marks the re-executed test binary as the allocator.
const memoryBoundHelperEnv = "GOOBERS_MEMORY_BOUND_ALLOCATOR"

// TestMemoryBoundAllocatorHelper is not a test: it is the over-allocating
// child TestStageMemoryBoundKillsAnOverAllocatingChild spawns. Re-executing
// the test binary is the standard way to get a child with known, controlled
// allocation behaviour instead of depending on whatever shell utilities the
// host image happens to ship.
func TestMemoryBoundAllocatorHelper(t *testing.T) {
	if os.Getenv(memoryBoundHelperEnv) == "" {
		t.Skip("helper process; run only as the child of the enforcement test")
	}
	// Allocate far past the bound and TOUCH every page: a cgroup accounts
	// resident memory, so an untouched mapping would prove nothing.
	const chunk = 8 << 20
	var held [][]byte
	for i := 0; i < 64; i++ { // up to 512Mi against a 64Mi bound
		block := make([]byte, chunk)
		for p := 0; p < len(block); p += 4096 {
			block[p] = byte(i + 1)
		}
		held = append(held, block)
		time.Sleep(10 * time.Millisecond)
	}
	// Unreachable under a working bound; referenced so the allocation cannot
	// be optimised away.
	t.Fatalf("allocator survived with %d blocks held; the bound did not hold", len(held))
}
