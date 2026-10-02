//go:build linux

package proc

import (
	"context"
	"os"
	"os/exec"
	"slices"
	"syscall"
	"testing"
	"time"
)

// selfReaper builds a reaper that classifies against the TEST process instead
// of pid 1. orphanReaper carries its identity as data precisely so the
// classification can be exercised without the test being container init.
func selfReaper(t *testing.T) *orphanReaper {
	t.Helper()
	self, ok := readProcStat(os.Getpid())
	if !ok {
		t.Fatal("readProcStat(self) failed; cannot determine this process's session")
	}
	return &orphanReaper{pid: os.Getpid(), session: self.session}
}

// startZombieChild starts a direct child that exits immediately and is never
// waited for, so it stays a zombie for the test to classify. ownSession
// reproduces a stage descendant (Configure/Setsid detaches every stage into its
// own session); otherwise the child inherits this process's session, the shape
// every plain exec.Command in the daemon has.
func startZombieChild(t *testing.T, ownSession bool) int {
	t.Helper()
	cmd := exec.Command("sh", "-c", "exit 0")
	if ownSession {
		cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start zombie fixture: %v", err)
	}
	pid := cmd.Process.Pid
	// Reap whatever the test did not, so a failing assertion cannot leak a
	// zombie into the rest of the package's tests.
	t.Cleanup(func() { _ = cmd.Wait() })
	if !waitUntil(t, 5*time.Second, func() bool { return zombie(pid) }) {
		t.Fatalf("fixture child %d never entered the zombie state", pid)
	}
	return pid
}

func enableChildRegistryForTest(t *testing.T) {
	t.Helper()
	trackedChildren.mu.Lock()
	previousEnabled, previousStarted := trackedChildren.enabled, trackedChildren.started
	trackedChildren.mu.Unlock()
	trackedChildren.enable()
	t.Cleanup(func() {
		trackedChildren.mu.Lock()
		defer trackedChildren.mu.Unlock()
		trackedChildren.enabled, trackedChildren.started = previousEnabled, previousStarted
	})
}

func gone(pid int) bool {
	_, ok := readProcStat(pid)
	return !ok
}

// TestSweepReapsForeignSessionOrphanAndSparesOwnSessionChild is the #3398
// regression. The leak: a descendant of a killed stage reparents onto the
// daemon (pid 1 in the shipped image), which waits for nothing but its own
// exec.Cmd children, so the descendant is a zombie forever.
//
// The second half is the constraint that makes the fix safe: a child that
// inherited the daemon's own session was started by some plain exec.Command
// whose Wait owns the exit status, and the sweep must leave it strictly alone.
func TestSweepReapsForeignSessionOrphanAndSparesOwnSessionChild(t *testing.T) {
	orphan := startZombieChild(t, true)
	owned := startZombieChild(t, false)

	reaper := selfReaper(t)
	candidates := reaper.orphanZombies()
	if !slices.Contains(candidates, orphan) {
		t.Errorf("orphanZombies() = %v, missing reparented orphan %d", candidates, orphan)
	}
	if slices.Contains(candidates, owned) {
		t.Errorf("orphanZombies() = %v, wrongly claims same-session child %d", candidates, owned)
	}

	reaper.sweep()

	if !waitUntil(t, 5*time.Second, func() bool { return gone(orphan) }) {
		t.Errorf("orphan %d still present after sweep; the zombie leak is not fixed", orphan)
	}
	if !zombie(owned) {
		t.Errorf("sweep consumed same-session child %d, stealing an exit status exec.Cmd owns", owned)
	}
}

// TestSweepLeavesStartChildrenForTheirOwnWait is the "don't steal exec.Cmd
// waits" contract. Start detaches every stage into its own session, so the
// session test alone would classify a live stage as an escaped orphan; the
// registry is what keeps the caller's Wait — the call whose result becomes the
// stage's reported exit status — working.
func TestSweepLeavesStartChildrenForTheirOwnWait(t *testing.T) {
	enableChildRegistryForTest(t)

	cmd := exec.Command("sh", "-c", "exit 0")
	if _, err := Start(cmd); err != nil {
		t.Fatalf("Start: %v", err)
	}
	pid := cmd.Process.Pid
	if !waitUntil(t, 5*time.Second, func() bool { return zombie(pid) }) {
		t.Fatalf("Start child %d never entered the zombie state", pid)
	}

	reaper := selfReaper(t)
	if candidates := reaper.orphanZombies(); slices.Contains(candidates, pid) {
		t.Fatalf("orphanZombies() = %v, claims tracked Start child %d", candidates, pid)
	}
	reaper.sweep()

	if err := cmd.Wait(); err != nil {
		t.Fatalf("Wait after sweep: %v (the reaper stole the exit status of a tracked child)", err)
	}
}

// TestReaperLoopReapsAndStopsOnContextCancel drives the goroutine itself: it
// must reap what is already pending at install time (a SIGCHLD that fired
// before the handler existed is never redelivered) and it must stop when the
// daemon's context is cancelled rather than outliving shutdown.
func TestReaperLoopReapsAndStopsOnContextCancel(t *testing.T) {
	orphan := startZombieChild(t, true)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		selfReaper(t).run(ctx, make(chan os.Signal, 1))
	}()

	reaped := waitUntil(t, 5*time.Second, func() bool { return gone(orphan) })
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("reap loop did not return after context cancel")
	}
	if !reaped {
		t.Errorf("reap loop left already-pending orphan %d unreaped", orphan)
	}
}

// TestTrackChildIsInertUntilTheReaperIsInstalled is the other half of the
// blast-radius guard: a daemon that is not container init must not accumulate a
// registry entry per spawned stage for the life of the process.
func TestTrackChildIsInertUntilTheReaperIsInstalled(t *testing.T) {
	trackedChildren.mu.Lock()
	enabled := trackedChildren.enabled
	trackedChildren.mu.Unlock()
	if enabled {
		t.Skip("registry already enabled by another test in this binary")
	}
	trackChild(os.Getpid())
	if trackedChildren.owns(os.Getpid()) {
		t.Errorf("trackChild recorded pid %d with no reaper installed", os.Getpid())
	}
}

// TestChildRegistryOwnsRejectsRecycledPID: an entry names a process, not a
// number. Without the start-time check a recycled pid would let a long-dead
// stage's registration shield a genuine orphan from ever being reaped —
// re-creating the leak this change removes.
func TestChildRegistryOwnsRejectsRecycledPID(t *testing.T) {
	enableChildRegistryForTest(t)

	cmd := exec.Command("sleep", "300")
	if err := cmd.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	pid := cmd.Process.Pid
	trackChild(pid)
	if !trackedChildren.owns(pid) {
		t.Fatalf("owns(%d) = false immediately after trackChild", pid)
	}

	trackedChildren.mu.Lock()
	trackedChildren.started[pid] = trackedChildren.started[pid].Add(-time.Second)
	trackedChildren.mu.Unlock()
	if trackedChildren.owns(pid) {
		t.Errorf("owns(%d) = true for a registration whose start time no longer matches", pid)
	}
}

// TestChildRegistryPruneDropsReapedChildren keeps the registry bounded by live
// children rather than by uptime — a daemon runs for the life of a pod and
// spawns a stage process per run.
func TestChildRegistryPruneDropsReapedChildren(t *testing.T) {
	enableChildRegistryForTest(t)

	cmd := exec.Command("sh", "-c", "exit 0")
	if err := cmd.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	pid := cmd.Process.Pid
	trackChild(pid)
	if err := cmd.Wait(); err != nil {
		t.Fatalf("Wait: %v", err)
	}

	trackedChildren.prune()
	trackedChildren.mu.Lock()
	_, retained := trackedChildren.started[pid]
	trackedChildren.mu.Unlock()
	if retained {
		t.Errorf("prune kept the entry for reaped pid %d; the registry would grow without bound", pid)
	}
}

// TestSweepReapsSameSessionZombieOnlyAfterGrace is the #5421 regression. A git
// grandchild whose git parent exits without waiting for it reparents onto the
// daemon (pid 1) still carrying the daemon's session, so the instant session
// test spares it forever and the daemon accumulates defunct git processes for
// its whole uptime. The backstop must leave such a zombie alone while a Wait
// could still be coming (the first sightings) and collect it once it has sat
// in state Z for the grace period.
func TestSweepReapsSameSessionZombieOnlyAfterGrace(t *testing.T) {
	abandoned := startZombieChild(t, false)

	clock := time.Unix(1_000_000, 0)
	reaper := selfReaper(t)
	reaper.now = func() time.Time { return clock }

	reaper.sweep()
	if !zombie(abandoned) {
		t.Fatalf("first sweep reaped same-session zombie %d with no grace; that could steal an exec.Cmd's exit status", abandoned)
	}

	clock = clock.Add(sameSessionZombieGrace - time.Second)
	reaper.sweep()
	if !zombie(abandoned) {
		t.Fatalf("sweep reaped same-session zombie %d before the grace elapsed", abandoned)
	}

	clock = clock.Add(time.Second)
	reaper.sweep()
	if !waitUntil(t, 5*time.Second, func() bool { return gone(abandoned) }) {
		t.Errorf("same-session zombie %d still present after %s in state Z; the #5421 leak is not fixed", abandoned, sameSessionZombieGrace)
	}
	if _, kept := reaper.lingering[abandoned]; kept {
		t.Errorf("lingering still holds reaped pid %d; the map must be bounded by present zombies", abandoned)
	}
}

// TestAbandonedSameSessionZombiesRestartsClockForRecycledPID: the grace is
// measured per process, not per pid number. A record whose start time no
// longer matches belongs to a process that is already gone, so the zombie now
// holding that pid must start its own grace period rather than inherit an age
// that would get it reaped while its Wait may still be coming.
func TestAbandonedSameSessionZombiesRestartsClockForRecycledPID(t *testing.T) {
	pid := startZombieChild(t, false)
	stat, ok := readProcStat(pid)
	if !ok {
		t.Fatalf("readProcStat(%d) failed", pid)
	}

	clock := time.Unix(1_000_000, 0)
	reaper := selfReaper(t)
	reaper.now = func() time.Time { return clock }
	reaper.lingering = map[int]lingeringZombie{
		pid: {startTicks: stat.startTicks + 1, firstSeen: clock.Add(-10 * sameSessionZombieGrace)},
	}

	if got := reaper.abandonedSameSessionZombies(); slices.Contains(got, pid) {
		t.Fatalf("abandonedSameSessionZombies() = %v, inherited a recycled pid's age for %d", got, pid)
	}
	if record := reaper.lingering[pid]; !record.firstSeen.Equal(clock) || record.startTicks != stat.startTicks {
		t.Errorf("lingering[%d] = %+v, want a fresh record first seen at %v", pid, record, clock)
	}
}
