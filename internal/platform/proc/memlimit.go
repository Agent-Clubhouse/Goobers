package proc

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"

	"github.com/goobers/goobers/internal/platform/cgroupfs"
)

// Per-stage memory bounds (#4070).
//
// THE INCIDENT. Stage subprocesses run inside the API pod's OWN memory cgroup,
// so a heavy stage evicts the control-plane daemon. In production on
// 2026-08-31 the cgroup went from 559Mi to 7.4Gi of anonymous memory in ~60s
// while the daemon's Go heap sat at 13Mi; the kernel OOM-killer took the
// daemon. Three of five daemon dirty restarts in one review window were this
// same mechanism, with single children measured at 4.8, 9.8 and 9.9 GiB.
//
// WHY THE EXISTING ADMISSION GATE CANNOT FIX IT. localscheduler's
// CgroupMemoryGate (#3949) refuses to ADMIT new runs while the cgroup is hot.
// In every one of those incidents the killing allocation was a single stage
// ALREADY RUNNING, which no admission decision can reach. Admission bounds how
// many stages start; only a per-child bound can stop one of them growing until
// the daemon dies.
//
// TWO MECHANISMS, AND WHY THEY ARE NOT INTERCHANGEABLE.
//
//   - A CHILD CGROUP with memory.max is the real thing. It bounds RSS, which
//     is exactly the quantity the kernel OOM-killer accounts, so the bound and
//     the failure it prevents are measured in the same unit. The child is
//     placed into it at clone time (SysProcAttr.CgroupFD), so there is no
//     window in which it runs unbounded, and memory.events records the kill —
//     the outcome is OBSERVED, not inferred.
//
//   - RLIMIT_AS bounds ADDRESS SPACE, which is a correlate of RSS, not RSS.
//     Go, the JVM and Node all reserve far more virtual address space than
//     they ever touch, so a number chosen to stop a 9.8 GiB RSS child can
//     break a well-behaved one that merely maps a large arena. It is applied
//     after Start (Linux offers no pre-exec rlimit through exec.Cmd), so a
//     child can allocate in the microseconds before it lands.
//
// That asymmetry is why they are not defaulted alike: see MemoryBoundPolicy in
// the caller. A cgroup bound is safe to derive automatically; an RLIMIT_AS
// bound is only ever applied when an operator asked for a specific number.

// Mechanism names how a per-child memory bound is (or is not) enforced.
type Mechanism string

const (
	// MechanismCgroup is a child cgroup with memory.max — an RSS bound the
	// kernel enforces, whose breach is recorded in memory.events.
	MechanismCgroup Mechanism = "cgroup"
	// MechanismRlimitAS is a post-start RLIMIT_AS — an address-space bound
	// that only CORRELATES with the memory that gets a process killed.
	MechanismRlimitAS Mechanism = "rlimit-as"
	// MechanismNone means nothing is enforced: the platform offers neither
	// mechanism, or no bound was requested. A stage runs exactly as before.
	MechanismNone Mechanism = "none"
)

// MemoryBound is an applied per-child memory bound. A nil *MemoryBound is
// valid and enforces nothing, so callers never have to branch before use.
type MemoryBound struct {
	mechanism Mechanism
	maxBytes  uint64
	// detail explains an unavailable mechanism, so an operator who expected a
	// bound can find out why there is none instead of assuming there is one.
	detail string
	impl   memoryBoundImpl
}

// Exceeded reports whether the child died because it breached this bound, and
// a named reason to journal when it did.
//
// The two mechanisms answer with different confidence, and the wording says
// so rather than flattening them:
//
//   - Under a cgroup bound the answer is OBSERVED. memory.events' oom_kill
//     counter is incremented by the kernel for this cgroup alone, so a
//     non-zero reading IS the kill, not evidence of one.
//   - Under RLIMIT_AS the answer is INFERRED. The child sees allocation
//     failures and dies however its runtime dies; nothing records "the rlimit
//     did this". Reporting it as certain would be a fabricated verdict.
func (b *MemoryBound) Exceeded() (bool, string) {
	if b == nil || b.mechanism == MechanismNone || b.impl == nil {
		return false, ""
	}
	exceeded, reason := b.impl.exceeded(b.maxBytes)
	if !exceeded {
		return false, ""
	}
	// The mechanism is part of the finding, not decoration: it is what tells a
	// reader whether the bound that fired measures resident memory (a cgroup)
	// or merely correlates with it (RLIMIT_AS), and therefore how much to
	// trust the number in deciding what to change.
	return true, fmt.Sprintf("%s [bound enforced via %s]", reason, b.mechanism)
}

// Release frees the bound's resources. Safe on a nil bound and idempotent, so
// it can be deferred unconditionally at the call site.
func (b *MemoryBound) Release() error {
	if b == nil || b.impl == nil {
		return nil
	}
	return b.impl.release()
}

// memoryBoundImpl is the platform half. Nil on platforms with no mechanism.
type memoryBoundImpl interface {
	exceeded(maxBytes uint64) (bool, string)
	release() error
}

// unenforcedBound builds the "nothing is enforced, and here is why" answer.
func unenforcedBound(detail string) *MemoryBound {
	return &MemoryBound{mechanism: MechanismNone, detail: detail}
}

// StartBounded is Start with a per-child memory bound applied.
//
// maxBytes of 0 means "no bound" and makes this exactly Start. A bound that
// cannot be established is NOT an error: the returned MemoryBound reports
// MechanismNone with the reason, and the stage runs unbounded as it always
// has. Failing the stage instead would turn a hardening feature into an
// outage on every host without a delegated cgroup.
func StartBounded(cmd *exec.Cmd, maxBytes uint64, allowRlimitFallback bool) (*Tree, *MemoryBound, error) {
	if maxBytes == 0 {
		return startUnbounded(cmd, "no bound requested")
	}
	return startBounded(cmd, maxBytes, allowRlimitFallback)
}

func startUnbounded(cmd *exec.Cmd, detail string) (*Tree, *MemoryBound, error) {
	tree, err := Start(cmd)
	if err != nil {
		return nil, nil, err
	}
	return tree, unenforcedBound(detail), nil
}

// cgroupRoot is where the cgroup v2 hierarchy is mounted. Var, not const, so
// tests drive the whole preparation path against a fake tree on any host —
// the same seam internal/platform/memstat uses, so the code that CREATES a
// cgroup and the code that READS one are exercised the same way.
var cgroupRoot = cgroupfs.DefaultRoot

// selfCgroupFile is the process's own cgroup membership. Var for the same
// reason.
var selfCgroupFile = "/proc/self/cgroup"

// stageCgroupPrefix names the child cgroups this creates, so an operator
// listing the pod's cgroup tree can tell them from the runtime's own.
const stageCgroupPrefix = "goobers-stage-"

var stageCgroupSeq struct {
	sync.Mutex
	n uint64
}

func nextStageCgroupName() string {
	stageCgroupSeq.Lock()
	defer stageCgroupSeq.Unlock()
	stageCgroupSeq.n++
	return fmt.Sprintf("%s%d-%d", stageCgroupPrefix, os.Getpid(), stageCgroupSeq.n)
}

// stageCgroup is one child cgroup created for one stage subprocess.
type stageCgroup struct {
	path string
	dir  *os.File
	once sync.Once
}

// prepareStageCgroup creates a SIBLING cgroup of the daemon's own, with
// memory.max set.
//
// WHY A SIBLING AND NOT A CHILD. cgroup v2's "no internal processes" rule
// means a cgroup that holds processes cannot enable controllers in its
// cgroup.subtree_control. The daemon always lives in its own cgroup, so that
// cgroup can NEVER delegate memory to children of itself — a child-cgroup
// design is not merely unsupported here, it is unreachable, and would ship as
// a code path that reads as protection while never once executing. Verified
// directly: enabling +memory on a cgroup holding the caller fails, and
// succeeds the moment the caller moves out of it.
//
// So a bounded stage goes BESIDE the daemon, under the daemon's parent, which
// can delegate memory precisely because the daemon is not in it.
//
// THIS REQUIRES THE DEPLOYMENT TO COOPERATE, in two ways that are not the
// container defaults:
//
//   - /sys/fs/cgroup must be writable. Docker and Kubernetes mount it
//     read-only unless asked otherwise.
//   - The daemon must run in a SUB-cgroup with `memory` in its parent's
//     cgroup.subtree_control, not at the cgroup-namespace root. At the root
//     there is no reachable parent — the real one is outside the namespace —
//     and no bound can be created.
//
// Nothing here relocates the daemon to arrange that. Moving a live control
// plane between cgroups mid-flight is a deployment decision, not something a
// stage spawn should do behind an operator's back. When the prerequisites are
// absent this reports why, the caller falls back or reports unenforced, and
// the daemon says so at startup.
func prepareStageCgroup(maxBytes uint64) (*stageCgroup, error) {
	own, err := ownCgroupDir()
	if err != nil {
		return nil, err
	}
	parent := filepath.Dir(own)
	if own == cgroupRoot || !strings.HasPrefix(parent, cgroupRoot) {
		return nil, fmt.Errorf("this process is at the cgroup-namespace root (%s), which has no reachable parent to "+
			"create a bounded sibling under; run the daemon in a sub-cgroup with \"memory\" delegated in the parent's "+
			"cgroup.subtree_control", own)
	}
	subtree, err := os.ReadFile(filepath.Join(parent, "cgroup.subtree_control"))
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", filepath.Join(parent, "cgroup.subtree_control"), err)
	}
	if !hasController(string(subtree), "memory") {
		return nil, fmt.Errorf("the memory controller is not delegated to children of %s "+
			"(cgroup.subtree_control = %q)", parent, strings.TrimSpace(string(subtree)))
	}

	path := filepath.Join(parent, nextStageCgroupName())
	if err := os.Mkdir(path, 0o755); err != nil {
		return nil, fmt.Errorf("create stage cgroup: %w", err)
	}
	cg := &stageCgroup{path: path}
	if err := os.WriteFile(filepath.Join(path, "memory.max"), []byte(fmt.Sprintf("%d\n", maxBytes)), 0o644); err != nil {
		_ = cg.release()
		return nil, fmt.Errorf("set memory.max: %w", err)
	}
	// memory.max ALONE IS NOT A BOUND ON A HOST WITH SWAP. Anonymous memory
	// over the limit is swapped out rather than killed, so an over-budget
	// stage keeps running — slowly, thrashing — and the bound silently becomes
	// a performance cliff instead of a limit. Measured directly: a child
	// allocating 512Mi under a 64Mi memory.max ran to completion, with
	// memory.events oom_kill still 0.
	//
	// Zeroing the swap allowance makes the bound mean the same thing wherever
	// the daemon runs: exceed it and the kernel kills you. Kubernetes usually
	// disables swap, so this changes nothing there and fixes the developer and
	// non-Kubernetes cases.
	//
	// Best-effort: the file is absent when the kernel was built without swap
	// accounting, and a bound that works for page cache is still better than
	// none. It is not an error, but it does mean the bound is softer, which is
	// why enforcement is proven by a test that actually kills something rather
	// than by this write succeeding.
	_ = os.WriteFile(filepath.Join(path, "memory.swap.max"), []byte("0\n"), 0o644)

	dir, err := os.Open(path)
	if err != nil {
		_ = cg.release()
		return nil, fmt.Errorf("open stage cgroup: %w", err)
	}
	cg.dir = dir
	return cg, nil
}

// ownCgroupDir resolves the directory of the cgroup this process is in.
func ownCgroupDir() (string, error) {
	data, err := os.ReadFile(selfCgroupFile)
	if err != nil {
		return "", fmt.Errorf("read %s: %w", selfCgroupFile, err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		// cgroup v2 is the single "0::<path>" entry. A v1-only host has no
		// such line, and v1's per-controller memory limits are not what this
		// builds on, so it reports unavailable rather than half-working.
		rest, ok := strings.CutPrefix(strings.TrimSpace(line), "0::")
		if !ok {
			continue
		}
		return filepath.Join(cgroupRoot, filepath.Clean("/"+rest)), nil
	}
	return "", fmt.Errorf("no cgroup v2 membership in %s", selfCgroupFile)
}

func hasController(subtreeControl, want string) bool {
	for _, field := range strings.Fields(subtreeControl) {
		if field == want {
			return true
		}
	}
	return false
}

// exceeded reads the kill counter the kernel maintains for this cgroup alone.
// This is an OBSERVATION of the kill, not an inference from the exit status:
// a stage killed for any other reason leaves oom_kill at zero.
func (c *stageCgroup) exceeded(maxBytes uint64) (bool, string) {
	events, ok := cgroupfs.ReadKeyedFile(filepath.Join(c.path, "memory.events"))
	if !ok {
		return false, ""
	}
	if events["oom_kill"] == 0 {
		return false, ""
	}
	return true, fmt.Sprintf("stage exceeded its %d-byte per-stage memory bound and was terminated by the kernel "+
		"(cgroup memory.events oom_kill=%d). The bound exists so one stage cannot evict the daemon it shares a pod "+
		"with (#4070); raise runner.stageMemoryLimit if this stage legitimately needs more.", maxBytes, events["oom_kill"])
}

func (c *stageCgroup) release() error {
	var err error
	c.once.Do(func() {
		if c.dir != nil {
			err = c.dir.Close()
		}
		// The cgroup can only be removed once empty. The caller releases
		// after Wait, so the child is gone; a residual EBUSY is reported
		// rather than retried, since a leaked empty cgroup directory is
		// harmless next to blocking a stage's completion path.
		if rmErr := os.Remove(c.path); rmErr != nil && err == nil && !os.IsNotExist(rmErr) {
			err = rmErr
		}
	})
	return err
}

// rlimitBound carries no state: nothing records that an address-space limit
// caused a death, which is exactly why this mechanism can only ever report
// an inference.
type rlimitBound struct{}

func (r *rlimitBound) exceeded(maxBytes uint64) (bool, string) {
	// Deliberately never claims the bound did it. An RLIMIT_AS breach surfaces
	// as whatever allocation failure the child's runtime produces, and the
	// kernel keeps no per-process record a parent can read afterwards. Saying
	// "probably" here would put a guess into the run journal as a finding.
	_ = maxBytes
	return false, ""
}

func (r *rlimitBound) release() error { return nil }

// ProbeMemoryBound reports which mechanism WOULD bound a stage at maxBytes,
// without starting one.
//
// It exists so a startup report can state the mechanism actually available
// rather than the one intended. A configured bound that is silently inert —
// no delegated cgroup, and no address-space fallback permitted — reads to an
// operator exactly like a bound that is working, and the difference only
// surfaces when a stage that should have been killed takes the daemon with it
// instead.
//
// The cgroup answer is a real attempt, not an inspection: it creates a child
// cgroup, writes memory.max, and releases it. A preparation that succeeds is
// evidence the mechanism works, in the way that reading a config file is not.
func ProbeMemoryBound(maxBytes uint64, allowRlimitFallback bool) (Mechanism, string) {
	if maxBytes == 0 {
		return MechanismNone, "no bound requested"
	}
	cg, err := prepareStageCgroup(maxBytes)
	if err == nil {
		_ = cg.release()
		return MechanismCgroup, ""
	}
	if allowRlimitFallback {
		return MechanismRlimitAS, "no delegated cgroup (" + err.Error() + ")"
	}
	return MechanismNone, "no delegated cgroup (" + err.Error() +
		") and a derived bound is never applied through the address-space fallback"
}
