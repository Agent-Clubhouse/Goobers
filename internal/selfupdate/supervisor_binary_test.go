package selfupdate

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/journal"
)

// This file is #6348's platform scenario: the supervisor driving REAL
// executables through a healthy promotion and a failed-candidate rollback.
// supervisor_test.go covers the state machine with an in-memory launcher; here
// every hop is the production one: execLauncher spawns `<binary> up <root>`
// through proc.Tree, ensureCurrentBinary reads `<binary> version --json`
// through execRunner, the activation and rollback copy real executables, and
// the drain handoff is the file-based stop request a real daemon consumes.
//
// The daemon is a stub compiled from stubDaemonSource with the local go
// toolchain, not `goobers` itself: what is under test is the supervisor's
// process and file handling, and a stub keeps the scenario to a few seconds.
//
// How CI runs it: it is an ordinary unit test with no build tag, so it runs in
// every `go test ./internal/selfupdate/...` invocation: CI's Linux race-enabled
// unit shards (`go run ./test/ci group unit`) on every PR, and the nightly macOS
// workflow against main. It skips itself on Windows (replacing an
// executable image that just exited races the OS releasing its lock, which this
// scenario does not model) and wherever no go toolchain is on PATH.

const stubDaemonSource = `package main

import (
	"fmt"
	"os"
	"path/filepath"
	"time"
)

var (
	version = "unset"
	mode    = "healthy"
)

func main() {
	if len(os.Args) > 2 && os.Args[1] == "version" && os.Args[2] == "--json" {
		fmt.Printf("{\"version\":%q,\"commit\":%q}\n", version, "commit-"+version)
		return
	}
	if len(os.Args) != 3 || os.Args[1] != "up" {
		os.Exit(2)
	}
	root := os.Args[2]
	launches, err := os.OpenFile(filepath.Join(root, "stub-launches.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		os.Exit(4)
	}
	fmt.Fprintln(launches, version)
	launches.Close()
	if mode == "broken" {
		os.Exit(3)
	}
	lock := filepath.Join(root, "scheduler", "up.lock")
	if err := os.MkdirAll(filepath.Dir(lock), 0o755); err != nil {
		os.Exit(5)
	}
	if f, err := os.OpenFile(lock, os.O_CREATE|os.O_WRONLY, 0o644); err == nil {
		f.Close()
	}
	stop := filepath.Join(root, "updates", "stop-request")
	for {
		now := time.Now()
		// The instance root is gone (test cleanup): never outlive it.
		if err := os.Chtimes(lock, now, now); err != nil {
			return
		}
		if err := os.Remove(stop); err == nil {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
}
`

type stubDaemons struct {
	healthyV1, healthyV2, brokenV2 string
}

func buildStubDaemons(t *testing.T) stubDaemons {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("real-binary supervisor scenario is not run on Windows; see the file comment")
	}
	goTool, err := exec.LookPath("go")
	if err != nil {
		t.Skip("real-binary supervisor scenario needs a go toolchain on PATH")
	}
	dir := t.TempDir()
	src := filepath.Join(dir, "stubdaemon.go")
	if err := os.WriteFile(src, []byte(stubDaemonSource), 0o644); err != nil {
		t.Fatal(err)
	}
	build := func(name, version, mode string) string {
		out := filepath.Join(dir, name)
		cmd := exec.Command(goTool, "build", "-o", out, "-ldflags", "-X main.version="+version+" -X main.mode="+mode, src)
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("build stub daemon %s: %v\n%s", name, err, output)
		}
		return out
	}
	return stubDaemons{
		healthyV1: build("healthy-v1", "v1.0.0", "healthy"),
		healthyV2: build("healthy-v2", "v2.0.0", "healthy"),
		brokenV2:  build("broken-v2", "v2.0.0", "broken"),
	}
}

// stageRealRequest installs v1 as the supervised daemon and stages candidate
// as the requested update.
func stageRealRequest(t *testing.T, v1, candidate string) string {
	t.Helper()
	root := t.TempDir()
	if err := copyExecutable(v1, currentBinary(root, runtime.GOOS)); err != nil {
		t.Fatal(err)
	}
	staged := filepath.Join(stagingDir(root), "v2.0.0", binaryName(runtime.GOOS))
	if err := copyExecutable(candidate, staged); err != nil {
		t.Fatal(err)
	}
	request := Request{
		RunID: "self-update-scenario", Policy: PolicyOnRelease, Owner: "acme", Repository: "goobers",
		Target: "v2.0.0", StagedPath: staged, RequestedAt: time.Now().UTC(),
		HealthTicks: 2, HealthTimeout: (30 * time.Second).String(), Status: "requested",
	}
	if err := writeRequest(root, request); err != nil {
		t.Fatal(err)
	}
	return root
}

func runRealSupervisor(t *testing.T, root, executable string, escalator escalator) (context.CancelFunc, <-chan error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done, finished := make(chan error, 1), make(chan struct{})
	go func() {
		defer close(finished)
		done <- RunSupervisor(ctx, SupervisorOptions{
			Root: root, Escalator: escalator,
			PollInterval: 20 * time.Millisecond, DrainTimeout: 10 * time.Second,
			executable: executable,
			supervisor: versionInfo{Version: "v1.0.0", Commit: "commit-v1.0.0"},
		})
	}()
	// A test that fails before stopRealSupervisor must still drain the real
	// daemon process before t.TempDir removes its root.
	t.Cleanup(func() {
		cancel()
		select {
		case <-finished:
		case <-time.After(waitForBudget):
			t.Error("supervisor did not stop during test cleanup")
		}
	})
	return cancel, done
}

func stopRealSupervisor(t *testing.T, cancel context.CancelFunc, done <-chan error) {
	t.Helper()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("RunSupervisor after cancel = %v, want a clean drain", err)
		}
	case <-time.After(waitForBudget):
		t.Fatal("supervisor did not stop after cancel")
	}
}

func activeVersion(t *testing.T, binary string) string {
	t.Helper()
	info, err := readVersion(context.Background(), execRunner{}, filepath.Dir(binary), binary)
	if err != nil {
		t.Fatalf("read version of %s: %v", binary, err)
	}
	return info.Version
}

func stubLaunches(t *testing.T, root string) []string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(root, "stub-launches.log"))
	if err != nil {
		t.Fatal(err)
	}
	return strings.Fields(string(raw))
}

func instanceEventTypes(t *testing.T, root string) map[journal.EventType]bool {
	t.Helper()
	events, err := journal.ReadInstanceLog(filepath.Join(root, "scheduler"))
	if err != nil {
		t.Fatal(err)
	}
	types := map[journal.EventType]bool{}
	for _, event := range events {
		types[event.Type] = true
	}
	return types
}

func requestRetired(root string) bool {
	_, err := os.Stat(requestPath(root))
	return errors.Is(err, os.ErrNotExist)
}

func TestSupervisorRealBinaryPromotesHealthyCandidate(t *testing.T) {
	stubs := buildStubDaemons(t)
	root := stageRealRequest(t, stubs.healthyV1, stubs.healthyV2)
	escalations := make(chan Request, 1)

	cancel, done := runRealSupervisor(t, root, stubs.healthyV1, fakeEscalator{escalations})
	waitForSupervisor(t, done, "the healthy candidate was promoted and its request retired", func() bool {
		return requestRetired(root)
	})

	if got := activeVersion(t, currentBinary(root, runtime.GOOS)); got != "v2.0.0" {
		t.Fatalf("active binary version = %s, want the promoted v2.0.0", got)
	}
	if got := activeVersion(t, previousBinary(root, runtime.GOOS)); got != "v1.0.0" {
		t.Fatalf("retained previous binary version = %s, want v1.0.0", got)
	}
	if got := strings.Join(stubLaunches(t, root), ","); got != "v1.0.0,v2.0.0" {
		t.Fatalf("daemon launches = %s, want v1.0.0 drained then v2.0.0", got)
	}
	types := instanceEventTypes(t, root)
	if !types[journal.EventDaemonUpdateHealthy] || types[journal.EventDaemonUpdateRolledBack] {
		t.Fatalf("instance events = %v, want healthy and no rollback", types)
	}
	if len(escalations) != 0 {
		t.Fatal("a healthy promotion filed a rollback escalation")
	}
	stopRealSupervisor(t, cancel, done)
}

func TestSupervisorRealBinaryRollsBackFailedCandidate(t *testing.T) {
	stubs := buildStubDaemons(t)
	root := stageRealRequest(t, stubs.healthyV1, stubs.brokenV2)
	escalations := make(chan Request, 2)

	cancel, done := runRealSupervisor(t, root, stubs.healthyV1, fakeEscalator{escalations})
	waitForSupervisor(t, done, "the failed candidate was rolled back, escalated and retired", func() bool {
		return len(escalations) == 1 && requestRetired(root)
	})

	escalated := <-escalations
	if escalated.Target != "v2.0.0" || !strings.Contains(escalated.Reason, "candidate daemon exited") {
		t.Fatalf("escalated request = %+v, want the v2.0.0 rollback with the candidate's exit as reason", escalated)
	}
	if got := activeVersion(t, currentBinary(root, runtime.GOOS)); got != "v1.0.0" {
		t.Fatalf("active binary version = %s, want the restored v1.0.0", got)
	}
	waitForSupervisor(t, done, "the restored daemon was relaunched", func() bool {
		return strings.Join(stubLaunches(t, root), ",") == "v1.0.0,v2.0.0,v1.0.0"
	})
	types := instanceEventTypes(t, root)
	if !types[journal.EventDaemonUpdateRolledBack] || !types[journal.EventDaemonUpdateEscalated] || types[journal.EventDaemonUpdateHealthy] {
		t.Fatalf("instance events = %v, want rolled_back and escalated without healthy", types)
	}
	stopRealSupervisor(t, cancel, done)
}
