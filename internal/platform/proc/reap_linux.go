//go:build linux

package proc

import (
	"context"
	"errors"
	"os"
	"os/signal"
	"strconv"
	"sync"
	"syscall"
	"time"
)

// containerInitPID is the pid a process holds when it is its pid namespace's
// init. Guarding on it is the whole safety story for this feature: only an init
// inherits orphans it never started, and only an init has no other init above
// it to do the reaping.
const containerInitPID = 1

// orphanReapDebounce collapses a SIGCHLD burst — a tree kill exits a whole
// stage tree at once — into a single sweep. Delaying a reap by this much costs
// nothing (the processes are already dead) and saves a /proc walk per exit.
var orphanReapDebounce = 100 * time.Millisecond

// orphanReapSweepInterval is the backstop for a SIGCHLD that arrives while the
// buffer-1 signal channel is already full and is therefore dropped by
// signal.Notify. A dropped signal must never mean a permanently unreaped
// zombie, which is the very bug this exists to fix.
var orphanReapSweepInterval = 30 * time.Second

func startOrphanReaper(ctx context.Context) bool {
	pid := os.Getpid()
	if pid != containerInitPID {
		return false
	}
	// The reaper classifies candidates by session (see orphanZombies), so it
	// cannot start without knowing its own. /proc/self/stat is unreadable only
	// on a system where the classification would be unsafe anyway, so declining
	// to install is the correct answer rather than reaping blind.
	self, ok := readProcStat(pid)
	if !ok {
		return false
	}
	trackedChildren.enable()
	notifications := make(chan os.Signal, 1)
	signal.Notify(notifications, syscall.SIGCHLD)
	reaper := &orphanReaper{pid: pid, session: self.session}
	go reaper.run(ctx, notifications)
	return true
}

// orphanReaper holds the identity a sweep classifies against. Both fields are
// data rather than calls to os.Getpid so a test can drive a sweep from an
// ordinary (non-init) test process.
type orphanReaper struct {
	pid     int
	session int
}

func (r *orphanReaper) run(ctx context.Context, notifications chan os.Signal) {
	defer signal.Stop(notifications)
	ticker := time.NewTicker(orphanReapSweepInterval)
	defer ticker.Stop()
	for {
		// Sweep first: orphans can already be waiting at install time, and on
		// every later iteration this is the work the wakeup asked for.
		r.sweep()
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-notifications:
			timer := time.NewTimer(orphanReapDebounce)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
		}
	}
}

// sweep collects the exit status of every zombie child no exec.Cmd in this
// process owns.
func (r *orphanReaper) sweep() {
	for _, pid := range r.orphanZombies() {
		reapPID(pid)
	}
	trackedChildren.prune()
}

// reapPID waits for one specific pid.
//
// Deliberately NOT wait4(-1, WNOHANG): a wildcard wait races every cmd.Wait in
// the process and will happily consume a stage's exit status, after which the
// runner's Wait fails with "waitid: no child processes" and a completed stage
// is reported as a runner error. Waiting the exact pid this sweep already
// classified as an orphan cannot take a status anyone else is waiting for.
func reapPID(pid int) {
	var status syscall.WaitStatus
	for {
		_, err := syscall.Wait4(pid, &status, syscall.WNOHANG, nil)
		if !errors.Is(err, syscall.EINTR) {
			return
		}
	}
}

// orphanZombies lists the pids this process should reap: dead children that
// belong to nobody else.
//
// "Belongs to nobody else" is decided by two complementary tests, because Go
// offers no hook to enumerate the exec.Cmd children a process is waiting for:
//
//   - Session. A plain exec.Command child (git, gh, the hundreds of them the
//     daemon runs) INHERITS the daemon's session, and its exit status belongs
//     to the cmd.Wait that started it. Those are skipped. The cost is that a
//     genuine orphan left behind by such a child is skipped too — the safe
//     direction, and not the leak this fixes, since stages run detached.
//   - Registry. Start detaches each stage into its OWN session (Setsid), which
//     makes a live stage look exactly like an escaped orphan to the session
//     test. trackedChildren remembers those pids so they are skipped as well.
//
// What survives both tests is precisely a process from a stage's session that
// this daemon never started — a reparented descendant of a dead stage.
func (r *orphanReaper) orphanZombies() []int {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}
	var orphans []int
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}
		stat, ok := readProcStat(pid)
		if !ok || stat.ppid != r.pid || stat.state != 'Z' {
			continue
		}
		if stat.session == r.session || trackedChildren.owns(pid) {
			continue
		}
		orphans = append(orphans, pid)
	}
	return orphans
}

// trackedChildren records what Start spawned. It is inert until the reaper
// enables it, so a daemon that is not container init never pays for it and
// never accumulates entries.
var trackedChildren = &childRegistry{}

type childRegistry struct {
	mu      sync.Mutex
	enabled bool
	started map[int]time.Time
}

// trackChild registers a pid Start just spawned. It runs on the spawn path, so
// the disabled case must stay free of syscalls.
func trackChild(pid int) {
	trackedChildren.track(pid)
}

func (c *childRegistry) enable() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.enabled = true
	c.started = make(map[int]time.Time)
}

func (c *childRegistry) track(pid int) {
	c.mu.Lock()
	enabled := c.enabled
	c.mu.Unlock()
	if !enabled {
		return
	}
	// Start time is recorded alongside the pid so a recycled pid cannot let a
	// stale entry shield a real orphan from being reaped.
	started, ok := StartTime(pid)
	if !ok {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.started[pid] = started
}

// owns reports whether pid is still the process Start registered under it.
func (c *childRegistry) owns(pid int) bool {
	c.mu.Lock()
	started, ok := c.started[pid]
	c.mu.Unlock()
	if !ok {
		return false
	}
	current, ok := StartTime(pid)
	return ok && current.Equal(started)
}

// prune drops entries whose process has been waited for (its /proc entry is
// gone) or whose pid has been recycled onto something else, which bounds the
// registry by the number of live Start children instead of by uptime.
func (c *childRegistry) prune() {
	c.mu.Lock()
	defer c.mu.Unlock()
	for pid, started := range c.started {
		if current, ok := StartTime(pid); !ok || !current.Equal(started) {
			delete(c.started, pid)
		}
	}
}
