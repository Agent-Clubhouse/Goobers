package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeTargetBuilds replaces the compile and provenance steps so these tests
// exercise only the concurrent orchestration. Real cross-compiles of several
// targets starved a shared CI shard and timed out unrelated tests (#6493); the
// real build path stays covered by TestRunEndToEnd and the image tests.
// Earlier targets finish last, so completion order differs from target order.
// The returned func reports the peak number of builds in flight.
func fakeTargetBuilds(t *testing.T, unbuildable ...string) func() int32 {
	t.Helper()
	origBuild, origVerify := buildTargetBinary, verifyTargetBinary
	t.Cleanup(func() { buildTargetBinary, verifyTargetBinary = origBuild, origVerify })
	var inFlight, peak atomic.Int32
	var mu sync.Mutex
	order := map[string]int{}
	buildTargetBinary = func(target Target, _, binPath, _ string) (string, error) {
		n := inFlight.Add(1)
		defer inFlight.Add(-1)
		for {
			p := peak.Load()
			if n <= p || peak.CompareAndSwap(p, n) {
				break
			}
		}
		mu.Lock()
		idx := len(order)
		order[target.String()] = idx
		mu.Unlock()
		if idx == 0 {
			// Hold the first build until a second is in flight (bounded), so
			// the concurrency assertion does not depend on scheduler timing.
			for deadline := time.Now().Add(10 * time.Second); inFlight.Load() < 2 && time.Now().Before(deadline); {
				time.Sleep(time.Millisecond)
			}
		}
		time.Sleep(time.Duration(40-10*min(idx, 3)) * time.Millisecond)
		if slices.Contains(unbuildable, target.String()) {
			return "unsupported GOOS/GOARCH pair " + target.String(), fmt.Errorf("exit status 2")
		}
		return "", os.WriteFile(binPath, []byte("binary "+target.String()), 0o755)
	}
	verifyTargetBinary = func(string, string, string, Target) error { return nil }
	return peak.Load
}

// TestBuildReleaseTargetsConcurrentResultsKeepTargetOrder pins #5413's
// parallel build: targets build concurrently (bounded by the parallelism
// setting), but archives, skip notices and build lines come back in the
// requested target order, so SHA256SUMS inputs and the log stay deterministic
// however the builds finish.
func TestBuildReleaseTargetsConcurrentResultsKeepTargetOrder(t *testing.T) {
	t.Setenv(releaseBuildParallelismEnv, "3")
	peak := fakeTargetBuilds(t, "windows/ppc64")

	targets, err := parseTargets("linux/amd64,windows/ppc64,darwin/arm64,windows/amd64")
	if err != nil {
		t.Fatal(err)
	}
	opts := options{version: "v1.2.3", outDir: t.TempDir(), targets: targets, skipUnbuildable: true}
	docs := t.TempDir()
	if err := os.WriteFile(filepath.Join(docs, "README.md"), []byte("docs"), 0o644); err != nil {
		t.Fatal(err)
	}
	var stdout bytes.Buffer
	archives, skipped, err := buildReleaseTargets(opts, "-s -w", docs, nil, &stdout)
	if err != nil {
		t.Fatalf("buildReleaseTargets: %v", err)
	}
	var names []string
	for _, archive := range archives {
		names = append(names, filepath.Base(archive))
	}
	want := []string{"goobers_v1.2.3_linux_amd64.tar.gz", "goobers_v1.2.3_darwin_arm64.tar.gz", "goobers_v1.2.3_windows_amd64.zip"}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Fatalf("archives = %v, want target order %v", names, want)
	}
	if strings.Join(skipped, ",") != "windows/ppc64" {
		t.Fatalf("skipped = %v, want [windows/ppc64]", skipped)
	}
	var reported []string
	for _, line := range strings.Split(strings.TrimSpace(stdout.String()), "\n") {
		if fields := strings.Fields(line); len(fields) > 1 {
			reported = append(reported, fields[1])
		}
	}
	if got := strings.Join(reported, ","); got != "linux/amd64,windows/ppc64,darwin/arm64,windows/amd64" {
		t.Fatalf("report order = %s, want target order:\n%s", got, stdout.String())
	}
	entries, err := os.ReadDir(opts.outDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != len(want) {
		t.Fatalf("output holds %d entries, want only the %d archives (binaries removed)", len(entries), len(want))
	}
	if got := peak(); got < 2 || got > 3 {
		t.Fatalf("peak concurrent builds = %d, want between 2 and the configured bound 3", got)
	}
}

// Without -skip-unbuildable the first unbuildable target in requested order is
// the one reported, even when a later target also fails.
func TestBuildReleaseTargetsReportsFirstFailureInTargetOrder(t *testing.T) {
	t.Setenv(releaseBuildParallelismEnv, "3")
	fakeTargetBuilds(t, "windows/ppc64", "plan9/riscv64")

	targets, err := parseTargets("linux/amd64,windows/ppc64,plan9/riscv64")
	if err != nil {
		t.Fatal(err)
	}
	opts := options{version: "v1.2.3", outDir: t.TempDir(), targets: targets}
	_, _, err = buildReleaseTargets(opts, "-s -w", t.TempDir(), nil, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "build windows/ppc64 failed") {
		t.Fatalf("err = %v, want the first failing target in order (windows/ppc64)", err)
	}
}

func TestReleaseBuildParallelismDefaultsToHalfTheCPUsAndHonoursOverride(t *testing.T) {
	t.Setenv(releaseBuildParallelismEnv, "")
	want := max(1, min(4, runtime.NumCPU()/2))
	if got := releaseBuildParallelism(16); got != want {
		t.Fatalf("default parallelism = %d, want %d", got, want)
	}
	if got := releaseBuildParallelism(1); got != 1 {
		t.Fatalf("parallelism for one target = %d, want 1", got)
	}
	t.Setenv(releaseBuildParallelismEnv, "3")
	if got := releaseBuildParallelism(16); got != 3 {
		t.Fatalf("override parallelism = %d, want 3", got)
	}
	if got := releaseBuildParallelism(2); got != 2 {
		t.Fatalf("override capped by targets = %d, want 2", got)
	}
	for _, bad := range []string{"0", "-2", "many"} {
		t.Setenv(releaseBuildParallelismEnv, bad)
		if got := releaseBuildParallelism(16); got != want {
			t.Fatalf("invalid override %q: parallelism = %d, want default %d", bad, got, want)
		}
	}
}
