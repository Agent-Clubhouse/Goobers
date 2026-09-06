package proc

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeCgroupTree builds a cgroup v2 tree the preparation path can be driven
// against on any host. The filesystem half of creating a bounded cgroup —
// resolving membership, checking delegation, writing memory.max, reading
// memory.events — is ordinary file I/O, so pointing the package at a
// temporary root exercises it on macOS and Windows too. Only the clone-time
// placement itself needs a real kernel.
func fakeCgroupTree(t *testing.T, relative, parentSubtreeControl string) string {
	t.Helper()
	root := t.TempDir()
	own := filepath.Join(root, filepath.FromSlash(strings.TrimPrefix(relative, "/")))
	if err := os.MkdirAll(own, 0o755); err != nil {
		t.Fatal(err)
	}
	// The delegation that matters is the PARENT's: a cgroup holding this
	// process can never delegate memory to its own children (cgroup v2's "no
	// internal processes" rule), so a bounded stage is created beside the
	// daemon, not beneath it.
	if err := os.WriteFile(filepath.Join(filepath.Dir(own), "cgroup.subtree_control"), []byte(parentSubtreeControl+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	selfCgroup := filepath.Join(t.TempDir(), "cgroup")
	if err := os.WriteFile(selfCgroup, []byte("0::"+relative+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	prevRoot, prevSelf := cgroupRoot, selfCgroupFile
	cgroupRoot, selfCgroupFile = root, selfCgroup
	t.Cleanup(func() { cgroupRoot, selfCgroupFile = prevRoot, prevSelf })
	return own
}

func TestPrepareStageCgroupCreatesABoundedChild(t *testing.T) {
	own := fakeCgroupTree(t, "/kubepods/pod123/api", "cpu memory pids")

	cg, err := prepareStageCgroup(4 << 30)
	if err != nil {
		t.Fatalf("prepareStageCgroup: %v", err)
	}
	t.Cleanup(func() { _ = cg.release() })

	if got, want := filepath.Dir(cg.path), filepath.Dir(own); got != want {
		t.Errorf("stage cgroup created under %s, want beside the daemon under %s", got, want)
	}
	if !strings.HasPrefix(filepath.Base(cg.path), stageCgroupPrefix) {
		t.Errorf("stage cgroup %q is not named so an operator can identify it", filepath.Base(cg.path))
	}
	// The bound is the whole point: a cgroup created without memory.max
	// written would place the child and enforce nothing.
	max, err := os.ReadFile(filepath.Join(cg.path, "memory.max"))
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(max)); got != "4294967296" {
		t.Errorf("memory.max = %q, want the requested 4GiB in bytes", got)
	}
}

// The daemon must not silently relocate itself to make delegation happen, so
// an undelegated memory controller is an explicit unavailability with a reason
// an operator can act on — not a half-configured cgroup.
func TestPrepareStageCgroupRefusesWithoutMemoryDelegation(t *testing.T) {
	fakeCgroupTree(t, "/kubepods/pod123/api", "cpu pids")

	_, err := prepareStageCgroup(1 << 30)
	if err == nil {
		t.Fatal("prepareStageCgroup succeeded with the memory controller undelegated")
	}
	for _, want := range []string{"memory controller is not delegated to children of", "cgroup.subtree_control"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

func TestOwnCgroupDirReadsV2Membership(t *testing.T) {
	own := fakeCgroupTree(t, "/kubepods/burstable/podabc/api", "memory")
	got, err := ownCgroupDir()
	if err != nil {
		t.Fatal(err)
	}
	if got != own {
		t.Errorf("ownCgroupDir() = %q, want %q", got, own)
	}
}

// A cgroup v1-only host has no "0::" line. Reporting unavailable is correct:
// v1's per-controller limits are not what this builds on, and pretending
// otherwise would produce a bound that silently enforces nothing.
func TestOwnCgroupDirRefusesAV1OnlyHost(t *testing.T) {
	root := t.TempDir()
	selfCgroup := filepath.Join(root, "cgroup")
	v1 := "11:memory:/kubepods/podabc\n10:cpu,cpuacct:/kubepods/podabc\n"
	if err := os.WriteFile(selfCgroup, []byte(v1), 0o644); err != nil {
		t.Fatal(err)
	}
	prevRoot, prevSelf := cgroupRoot, selfCgroupFile
	cgroupRoot, selfCgroupFile = root, selfCgroup
	t.Cleanup(func() { cgroupRoot, selfCgroupFile = prevRoot, prevSelf })

	if _, err := ownCgroupDir(); err == nil || !strings.Contains(err.Error(), "no cgroup v2 membership") {
		t.Fatalf("error = %v, want the v2-membership refusal", err)
	}
}

// Exceeded must be an OBSERVATION under the cgroup mechanism: the kernel's own
// per-cgroup kill counter, not an inference from an exit status.
func TestStageCgroupExceededReadsTheKernelKillCounter(t *testing.T) {
	fakeCgroupTree(t, "/daemon", "memory")
	cg, err := prepareStageCgroup(2 << 30)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cg.release() })

	bound := &MemoryBound{mechanism: MechanismCgroup, maxBytes: 2 << 30, impl: cg}

	// No events file yet: a stage that simply finished must not be reported
	// as memory-killed.
	if exceeded, _ := bound.Exceeded(); exceeded {
		t.Error("Exceeded() true with no memory.events present")
	}
	if err := os.WriteFile(filepath.Join(cg.path, "memory.events"), []byte("low 0\nhigh 0\nmax 12\noom 3\noom_kill 0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A cgroup that was merely pushed against its limit is NOT a kill. This is
	// the distinction that keeps a heavy-but-successful stage from being
	// reported as terminated.
	if exceeded, _ := bound.Exceeded(); exceeded {
		t.Error("Exceeded() true for a cgroup that hit its limit but had nothing killed")
	}

	if err := os.WriteFile(filepath.Join(cg.path, "memory.events"), []byte("max 40\noom 4\noom_kill 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	exceeded, reason := bound.Exceeded()
	if !exceeded {
		t.Fatal("Exceeded() false after the kernel recorded an oom_kill")
	}
	for _, want := range []string{"exceeded its", "per-stage memory bound", "#4070", "runner.stageMemoryLimit"} {
		if !strings.Contains(reason, want) {
			t.Errorf("reason %q does not mention %q", reason, want)
		}
	}
}

// RLIMIT_AS can only ever CORRELATE with the memory that gets a process
// killed, and the kernel keeps no record a parent can read. Reporting a
// verdict there would put a guess into the run journal as a finding.
func TestRlimitBoundNeverClaimsTheKill(t *testing.T) {
	bound := &MemoryBound{mechanism: MechanismRlimitAS, maxBytes: 1 << 30, impl: &rlimitBound{}}
	if exceeded, reason := bound.Exceeded(); exceeded || reason != "" {
		t.Errorf("Exceeded() = (%v, %q), want no claim under an address-space bound", exceeded, reason)
	}
	if bound.Mechanism() != MechanismRlimitAS {
		t.Errorf("Mechanism() = %q", bound.Mechanism())
	}
}

// A bound that is silently absent is worse than no bound: an operator reading
// a green config concludes the control plane is protected. Describe must say
// which mechanism is in force, or state plainly that none is and why.
func TestDescribeReportsWhetherAnythingIsEnforced(t *testing.T) {
	if got := (*MemoryBound)(nil).Describe(); !strings.Contains(got, "NOT ENFORCED") {
		t.Errorf("nil bound Describe() = %q, want an explicit not-enforced report", got)
	}
	if got := (*MemoryBound)(nil).MaxBytes(); got != 0 {
		t.Errorf("nil bound MaxBytes() = %d, want 0", got)
	}
	if err := (*MemoryBound)(nil).Release(); err != nil {
		t.Errorf("nil bound Release() = %v, want nil so callers can defer unconditionally", err)
	}

	unavailable := unenforcedBound("the memory controller is not delegated")
	got := unavailable.Describe()
	if !strings.Contains(got, "NOT ENFORCED") || !strings.Contains(got, "not delegated") {
		t.Errorf("Describe() = %q, want the reason a bound is absent", got)
	}

	enforced := &MemoryBound{mechanism: MechanismCgroup, maxBytes: 8 << 30}
	if got := enforced.Describe(); !strings.Contains(got, string(MechanismCgroup)) || !strings.Contains(got, "8589934592") {
		t.Errorf("Describe() = %q, want the mechanism and the bound", got)
	}
}

func TestReleaseIsIdempotent(t *testing.T) {
	fakeCgroupTree(t, "/daemon", "memory")
	cg, err := prepareStageCgroup(1 << 30)
	if err != nil {
		t.Fatal(err)
	}
	// On a real cgroupfs, rmdir(2) on a cgroup directory removes the cgroup
	// even though control files appear to live inside it — the filesystem is
	// virtual and those entries are not real children. An ordinary tmpdir
	// cannot model that, so the fake's control files are cleared first; what
	// is under test here is release's own behaviour (close the fd, rmdir
	// once, stay safe on a second call), not the kernel's unlink semantics.
	entries, err := os.ReadDir(cg.path)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if err := os.Remove(filepath.Join(cg.path, e.Name())); err != nil {
			t.Fatal(err)
		}
	}
	if err := cg.release(); err != nil {
		t.Fatalf("first release: %v", err)
	}
	if _, err := os.Stat(cg.path); !os.IsNotExist(err) {
		t.Errorf("stage cgroup %s survived release", cg.path)
	}
	// Deferred unconditionally at call sites, so a second call must be safe.
	if err := cg.release(); err != nil {
		t.Errorf("second release: %v", err)
	}
}

// A daemon at the cgroup-namespace ROOT — the Kubernetes and Docker default,
// where /proc/self/cgroup reads "0::/" — has no reachable parent to create a
// bounded sibling under, because its real parent lies outside the namespace.
//
// This is the single most likely deployment shape, so it must fail with an
// explanation an operator can act on, not a bare error. Getting this wrong is
// how the feature would ship as a code path that never executes while reading
// like protection.
func TestPrepareStageCgroupRefusesAtTheNamespaceRoot(t *testing.T) {
	root := t.TempDir()
	selfCgroup := filepath.Join(t.TempDir(), "cgroup")
	if err := os.WriteFile(selfCgroup, []byte("0::/\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	prevRoot, prevSelf := cgroupRoot, selfCgroupFile
	cgroupRoot, selfCgroupFile = root, selfCgroup
	t.Cleanup(func() { cgroupRoot, selfCgroupFile = prevRoot, prevSelf })

	_, err := prepareStageCgroup(1 << 30)
	if err == nil {
		t.Fatal("prepareStageCgroup succeeded at the cgroup-namespace root")
	}
	for _, want := range []string{"cgroup-namespace root", "sub-cgroup", "cgroup.subtree_control"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not tell an operator what the deployment needs (%q)", err, want)
		}
	}
}

// ProbeMemoryBound must report honestly rather than optimistically: a
// configured bound with no usable mechanism has to come back as MechanismNone
// with a reason, since that is the case the startup report exists to catch.
func TestProbeMemoryBoundReportsUnavailability(t *testing.T) {
	root := t.TempDir()
	selfCgroup := filepath.Join(t.TempDir(), "cgroup")
	if err := os.WriteFile(selfCgroup, []byte("0::/\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	prevRoot, prevSelf := cgroupRoot, selfCgroupFile
	cgroupRoot, selfCgroupFile = root, selfCgroup
	t.Cleanup(func() { cgroupRoot, selfCgroupFile = prevRoot, prevSelf })

	if mechanism, detail := ProbeMemoryBound(0, false); mechanism != MechanismNone || detail == "" {
		t.Errorf("ProbeMemoryBound(0) = (%q, %q), want none with a reason", mechanism, detail)
	}
	mechanism, detail := ProbeMemoryBound(8<<30, false)
	if mechanism != MechanismNone {
		t.Errorf("Mechanism = %q, want %q where no cgroup can be created and no fallback is allowed", mechanism, MechanismNone)
	}
	if !strings.Contains(detail, "no delegated cgroup") {
		t.Errorf("detail %q does not explain the unavailability", detail)
	}
	// With the fallback permitted the same host does offer a mechanism.
	if mechanism, _ := ProbeMemoryBound(8<<30, true); mechanism != MechanismRlimitAS {
		t.Errorf("Mechanism = %q with the fallback allowed, want %q", mechanism, MechanismRlimitAS)
	}
}
