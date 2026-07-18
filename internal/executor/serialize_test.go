package executor

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/instance"
)

// serializeGroupEnv builds an envelope for a stage that declares the given
// InputSerializeGroup, with a distinct TaskID so two concurrent runs record
// artifacts under different keys (no shared-map race in the fake recorder).
func serializeGroupEnv(t *testing.T, taskID, group string) apiv1.InvocationEnvelope {
	t.Helper()
	env := baseEnvelope(t)
	env.TaskID = taskID
	env.Inputs = map[string]interface{}{InputSerializeGroup: group}
	return env
}

// TestShellExecutor_SerializeGroupSerializesConcurrentStages is the regression
// guard for the #811/#812 local-ci hang: two concurrent `make ci` runs on one
// machine saturate disk I/O until a test's t.TempDir teardown stalls past the
// stage timeout. Stages declaring the same InputSerializeGroup must run one at
// a time instance-wide, so the disk-bound gate can never pile up.
//
// The command uses mkdir (atomic create) as a mutual-exclusion detector: if a
// peer is already inside its critical section the dir exists and the loser
// records an overlap. With serialization no overlap can ever occur, and the two
// 400ms critical sections are forced end-to-end, so total wall time is bounded
// below by their sum — a lower bound serialization guarantees, proving the lock
// actually engaged rather than the two goroutines merely happening to serialize.
func TestShellExecutor_SerializeGroupSerializesConcurrentStages(t *testing.T) {
	instanceRoot := t.TempDir()
	shared := t.TempDir()
	busy := filepath.Join(shared, "busy")
	overlap := filepath.Join(shared, "overlap")
	const criticalSection = 400 * time.Millisecond
	script := fmt.Sprintf(
		`if ! mkdir %q 2>/dev/null; then : > %q; exit 0; fi; sleep %.1f; rmdir %q`,
		busy, overlap, criticalSection.Seconds(), busy,
	)
	run := apiv1.DeterministicRun{Command: []string{"sh", "-c", script}}

	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		exec, _ := newTestExecutor(t, nil)
		exec.InstanceRoot = instanceRoot // shared lock namespace across both runs
		env := serializeGroupEnv(t, fmt.Sprintf("task-%d", i), "local-ci")
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			result, err := exec.Run(context.Background(), env, run)
			if err != nil {
				t.Errorf("Run: %v", err)
				return
			}
			if result.Status != apiv1.ResultSuccess {
				t.Errorf("status = %v, want success (%+v)", result.Status, result)
			}
		}()
	}
	began := time.Now()
	close(start)
	wg.Wait()
	elapsed := time.Since(began)

	if _, err := os.Stat(overlap); err == nil {
		t.Fatal("serializeGroup did not serialize: the two stages overlapped in their critical section")
	}
	if elapsed < 2*criticalSection {
		t.Fatalf("elapsed = %s, want >= %s — the two stages ran in parallel, the lock was not held", elapsed, 2*criticalSection)
	}
}

// TestShellExecutor_SerializeGroupWaitsForHeldLock proves the flock actually
// blocks (not merely that two goroutines happened not to overlap): with an
// external holder holding the group's instance-scoped lock, a stage declaring
// that group must not run its command until the lock is released. Mirrors
// cmd/goobers' TestMergePRWaitsForHeldMergeLock for the merge lock.
func TestShellExecutor_SerializeGroupWaitsForHeldLock(t *testing.T) {
	instanceRoot := t.TempDir()
	dir := instance.NewLayout(instanceRoot).SchedulerDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	lockPath := filepath.Join(dir, "stage-local-ci.lock")
	held, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Flock(int(held.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}

	exec, _ := newTestExecutor(t, nil)
	exec.InstanceRoot = instanceRoot
	env := serializeGroupEnv(t, "task-blocked", "local-ci")
	// Marker the command writes only once it actually starts (i.e. once it wins
	// the lock). Its absence while the external holder holds means it is waiting.
	marker := filepath.Join(t.TempDir(), "ran")
	run := apiv1.DeterministicRun{Command: []string{"sh", "-c", fmt.Sprintf(": > %q", marker)}}

	done := make(chan struct{})
	go func() {
		defer close(done)
		if _, err := exec.Run(context.Background(), env, run); err != nil {
			t.Errorf("Run: %v", err)
		}
	}()

	// While the lock is held, the stage must not have run its command.
	const holdFor = 300 * time.Millisecond
	time.Sleep(holdFor)
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("stage ran its command while the group lock was externally held — it was not serialized")
	}

	// Release; the stage must now acquire the lock and run promptly.
	releasedAt := time.Now()
	_ = syscall.Flock(int(held.Fd()), syscall.LOCK_UN)
	_ = held.Close()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("stage did not complete after the group lock was released")
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("stage completed but never ran its command: %v", err)
	}
	if waited := time.Since(releasedAt); waited > 5*time.Second {
		t.Fatalf("stage took %s to run after lock release — not promptly unblocked", waited)
	}
}

// TestShellExecutor_SerializeGroupIgnoredWithoutInstanceRoot locks in the
// documented fallback: with no InstanceRoot configured (e.g. a unit test that
// never stands up an instance) a declared serializeGroup is silently skipped
// rather than erroring, so serialization is opt-in on configured instances only.
func TestShellExecutor_SerializeGroupIgnoredWithoutInstanceRoot(t *testing.T) {
	exec, _ := newTestExecutor(t, nil) // InstanceRoot left empty
	env := serializeGroupEnv(t, "task-noroot", "local-ci")
	result, err := exec.Run(context.Background(), env, apiv1.DeterministicRun{Command: []string{"sh", "-c", "exit 0"}})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.Status != apiv1.ResultSuccess {
		t.Fatalf("status = %v, want success", result.Status)
	}
}
