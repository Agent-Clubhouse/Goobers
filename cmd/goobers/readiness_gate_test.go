package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/apicontract"
	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/worktree"
)

const orphanWorktreeHelperEnv = "GOOBERS_TEST_HELPER_ORPHAN_WORKTREE"

// TestHelperCreateOrphanWorktree is not a real test: it is re-exec'd as a
// subprocess of the test binary (the same seam
// up_drain_process_unix_test.go's TestUpDrainEscapedSessionProcess uses) so
// the worktree it creates carries the SUBPROCESS's own PID, not the parent
// test process's — the parent is still alive for the rest of the test run
// and would never look crash-orphaned, but a subprocess that has already
// exited by the time the parent inspects it genuinely has a dead PID. No
// internal/worktree test-seam override (processAlive) needed: this is a
// real dead PID, exactly what a prior `goobers up` process leaves behind
// after actually crashing.
func TestHelperCreateOrphanWorktree(t *testing.T) {
	if os.Getenv(orphanWorktreeHelperEnv) != "1" {
		t.Skip("helper subprocess only")
	}
	l := instance.NewLayout(os.Getenv("GOOBERS_TEST_HELPER_ROOT"))
	manager, err := worktree.NewManager(l.WorkcopiesDir())
	if err != nil {
		t.Fatal(err)
	}
	_, err = manager.Create(context.Background(), worktree.CreateOptions{
		RepoURL: os.Getenv("GOOBERS_TEST_HELPER_REPO"),
		RunID:   os.Getenv("GOOBERS_TEST_HELPER_RUN_ID"),
		Gaggle:  "example",
		BaseRef: "main",
	})
	if err != nil {
		t.Fatal(err)
	}
}

// seedCrashOrphanWorktree creates a real worktree under root's "example"
// gaggle stamped with a PID that is dead by the time this function returns
// (see TestHelperCreateOrphanWorktree above). The next `goobers up` against
// root finds it during its crash-orphan Reap startup phase and removes it,
// exactly as it would a prior daemon process's leftover worktree.
func seedCrashOrphanWorktree(t *testing.T, root, repoURL, runID string) {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(executable, "-test.run=^TestHelperCreateOrphanWorktree$")
	cmd.Env = append(os.Environ(),
		orphanWorktreeHelperEnv+"=1",
		"GOOBERS_TEST_HELPER_ROOT="+root,
		"GOOBERS_TEST_HELPER_REPO="+repoURL,
		"GOOBERS_TEST_HELPER_RUN_ID="+runID,
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("seed crash-orphan worktree: %v\n%s", err, out)
	}
}

// TestReadinessGateBlocksVersionedRoutesDuringCrashOrphanRecovery is the
// regression test #5019's maintainer-ruling acceptance criteria call for: a
// real daemon started against an instance root with a POPULATED crash-orphan
// worktree set, held inside the "worktree-reap-crash-orphan" startup phase
// (crash recovery, not yet complete), asserting:
//
//  1. The new readiness-gate route (apicontract.RouteInstanceReadiness)
//     answers 200 with Ready=false and the live recovery phase — it is the
//     one route #5019 requires stay reachable during recovery.
//  2. RouteInstance (whose response contract #5019 requires stay unchanged —
//     no partial inventory) is unavailable (503), not silently degraded.
//  3. A mutating route (RouteCancelRun) is unavailable (503) too — recovery
//     blocks every other versioned route, not just the read side.
//
// Then, once recovery completes, both RouteInstance and the mutating route
// return to normal (RouteInstance answers 200; the mutating route no longer
// answers the recovery 503 — its own not-found/validation response proves
// the gate opened, not that the run happens to exist), and the readiness
// route reports Ready=true.
func TestReadinessGateBlocksVersionedRoutesDuringCrashOrphanRecovery(t *testing.T) {
	root := initDeterministicDemo(t)
	repoURL, err := repoCloneURL(apiv1.RepoRef{})
	if err != nil {
		t.Fatalf("repoCloneURL: %v", err)
	}
	seedCrashOrphanWorktree(t, root, repoURL, "crashed-orphan-1")

	address := freeLoopbackAddress(t)
	setAPIListenAddress(t, root, address)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	stdout := newStartupHoldWriter(`phase=worktree-reap-crash-orphan status=start target="example"`)
	t.Cleanup(stdout.Release)
	var stderr bytes.Buffer
	done := make(chan int, 1)
	go func() {
		done <- runUpContext(ctx, []string{"--quiet", root}, stdout, &stderr)
	}()

	select {
	case <-stdout.held:
	case code := <-done:
		t.Fatalf("daemon exited before crash-orphan reap: code = %d, stdout = %q, stderr = %q", code, stdout.String(), stderr.String())
	case <-time.After(30 * time.Second):
		t.Fatalf("daemon never reached crash-orphan reap: stdout = %q, stderr = %q", stdout.String(), stderr.String())
	}

	client := &http.Client{Timeout: 10 * time.Second}

	// (1) The readiness-gate route answers during recovery, reporting the
	// live phase rather than a constant.
	response, err := client.Get("http://" + address + apicontract.InstanceReadinessPath)
	if err != nil {
		t.Fatal(err)
	}
	var midRecovery httpapi.InstanceReadiness
	decodeErr := json.NewDecoder(response.Body).Decode(&midRecovery)
	midRecoveryStatus := response.StatusCode
	_ = response.Body.Close()
	if decodeErr != nil {
		t.Fatal(decodeErr)
	}
	if midRecoveryStatus != http.StatusOK || midRecovery.Ready {
		t.Fatalf("mid-recovery readiness status = %d, ready = %t, want %d / false", midRecoveryStatus, midRecovery.Ready, http.StatusOK)
	}
	if midRecovery.Recovery.Phase != "worktree-reap-crash-orphan" {
		t.Fatalf("mid-recovery readiness phase = %q, want %q", midRecovery.Recovery.Phase, "worktree-reap-crash-orphan")
	}

	// (2) RouteInstance stays unavailable — never a partial inventory.
	response, err = client.Get("http://" + address + apicontract.InstancePath)
	if err != nil {
		t.Fatal(err)
	}
	instanceStatus := response.StatusCode
	_ = response.Body.Close()
	if instanceStatus != http.StatusServiceUnavailable {
		t.Fatalf("mid-recovery /api/v1/instance status = %d, want %d", instanceStatus, http.StatusServiceUnavailable)
	}

	// (3) A mutating route is unavailable too, not just the read side.
	response, err = client.Post("http://"+address+"/api/v1/runs/does-not-exist/cancel", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	cancelStatus := response.StatusCode
	_ = response.Body.Close()
	if cancelStatus != http.StatusServiceUnavailable {
		t.Fatalf("mid-recovery run-cancel status = %d, want %d", cancelStatus, http.StatusServiceUnavailable)
	}

	stdout.Release()

	select {
	case <-stdout.started:
	case code := <-done:
		t.Fatalf("daemon exited: code = %d, stdout = %q, stderr = %q", code, stdout.String(), stderr.String())
	case <-time.After(30 * time.Second):
		t.Fatalf("timed out waiting for \"daemon started\": stdout = %q, stderr = %q", stdout.String(), stderr.String())
	}

	// Post-recovery: RouteInstance answers normally again.
	response, err = client.Get("http://" + address + apicontract.InstancePath)
	if err != nil {
		t.Fatal(err)
	}
	instanceStatus = response.StatusCode
	_ = response.Body.Close()
	if instanceStatus != http.StatusOK {
		t.Fatalf("post-recovery /api/v1/instance status = %d, want %d", instanceStatus, http.StatusOK)
	}

	// Post-recovery: the mutating route no longer answers the recovery 503 —
	// its own not-found response proves the gate opened, independent of
	// whether the named run exists.
	response, err = client.Post("http://"+address+"/api/v1/runs/does-not-exist/cancel", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	cancelStatus = response.StatusCode
	_ = response.Body.Close()
	if cancelStatus == http.StatusServiceUnavailable {
		t.Fatalf("post-recovery run-cancel status = %d, still the recovery gate's status", cancelStatus)
	}

	response, err = client.Get("http://" + address + apicontract.InstanceReadinessPath)
	if err != nil {
		t.Fatal(err)
	}
	var postRecovery httpapi.InstanceReadiness
	decodeErr = json.NewDecoder(response.Body).Decode(&postRecovery)
	postRecoveryStatus := response.StatusCode
	_ = response.Body.Close()
	if decodeErr != nil {
		t.Fatal(decodeErr)
	}
	if postRecoveryStatus != http.StatusOK || !postRecovery.Ready {
		t.Fatalf("post-recovery readiness status = %d, ready = %t, want %d / true", postRecoveryStatus, postRecovery.Ready, http.StatusOK)
	}

	cancel()
	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("code = %d, stdout = %q, stderr = %q", code, stdout.String(), stderr.String())
		}
	case <-time.After(30 * time.Second):
		t.Fatalf("daemon did not shut down: stdout = %q, stderr = %q", stdout.String(), stderr.String())
	}
}
