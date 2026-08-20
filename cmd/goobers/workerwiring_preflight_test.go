package main

import (
	"errors"
	"os"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

// TestForGaggleSurvivesHarnessPreflightFailure is #2812's worker-path
// counterpart to daemon.go's appendHarnessPreflightWarnings: forGaggle must
// build a gaggle's executors successfully even when preflightHarnesses
// reports a live-probe failure for one of its harnesses (expired/over-quota
// credential), printing the failure to stderr rather than either swallowing
// it or failing the gaggle closed. Failing closed here would reproduce the
// exact bug #2812 fixed, just at the worker layer instead of the daemon
// layer — forGaggle is invoked lazily per activity dispatch, so one broken
// harness would otherwise block every dispatch for this gaggle, including
// ones that never touch that harness.
func TestForGaggleSurvivesHarnessPreflightFailure(t *testing.T) {
	root := initDemo(t)

	fixtureRepo := newDaemonFixtureRepo(t)
	previousRepoCloneURL := repoCloneURL
	repoCloneURL = func(apiv1.RepoRef) (string, error) { return fixtureRepo, nil }
	t.Cleanup(func() { repoCloneURL = previousRepoCloneURL })

	previousPreflight := preflightHarnesses
	preflightHarnesses = func(map[string]apiv1.GooberSpec, []apiv1.Workflow, []string, map[string][]string) (harnessPreflightInfo, harnessPreflightFailures, error) {
		return harnessPreflightInfo{}, harnessPreflightFailures{
			apiv1.HarnessCopilot: errors.New("harness preflight: you have exceeded your monthly quota"),
		}, nil
	}
	t.Cleanup(func() { preflightHarnesses = previousPreflight })

	stderrRead, stderrWrite, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	previousStderr := os.Stderr
	os.Stderr = stderrWrite
	t.Cleanup(func() { os.Stderr = previousStderr })

	seams, err := newWorkerSeams(root, nil)
	if err != nil {
		t.Fatalf("newWorkerSeams: %v", err)
	}

	g, forGaggleErr := seams.forGaggle("example")

	if err := stderrWrite.Close(); err != nil {
		t.Fatal(err)
	}
	os.Stderr = previousStderr
	var buf strings.Builder
	readBuf := make([]byte, 4096)
	for {
		n, readErr := stderrRead.Read(readBuf)
		buf.Write(readBuf[:n])
		if readErr != nil {
			break
		}
	}
	stderr := buf.String()

	if forGaggleErr != nil {
		t.Fatalf("forGaggle returned an error on a live-probe failure (should be non-fatal): %v", forGaggleErr)
	}
	if g == nil || g.cfg.NewAgentic == nil {
		t.Fatal("forGaggle did not construct the gaggle's executors despite the harness preflight failure")
	}
	if !strings.Contains(stderr, `harness "copilot"`) || !strings.Contains(stderr, "unavailable") {
		t.Errorf("stderr = %q, want it to report the copilot harness as unavailable", stderr)
	}

	// forGaggle is memoized per gaggle (workerSeams.mu-guarded byGaggle map):
	// a second call for the same gaggle must hit the cache, not re-run
	// preflight and print the warning again.
	if _, err := seams.forGaggle("example"); err != nil {
		t.Fatalf("second forGaggle call: %v", err)
	}
}
