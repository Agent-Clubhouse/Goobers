package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// The shipped image probes are shell scripts that otherwise execute only
// inside a release build. Every other test around them asserts their TEXT —
// TestImageStageSmokeRequiresSuccessfulCompletion stubs Docker, and
// TestImageDSL3SmokeAnnotatesWorkflowPreviewOnEveryPlatform matches
// substrings. Both pass just as happily on a script that cannot run.
//
// That is how v0.4.0-rc.5 was signed and notarized before anyone learned its
// DSL 3.0 probe no longer validated (#5047 scoped preview authorization to the
// owning Workflow; the probe still annotated the Manifest). The candidate was
// scrapped.
//
// So the probes run for real here, in ordinary CI, against a freshly built
// binary — using the same constants the release ships, so the two cannot
// drift.
func TestShippedImageProbesRunWithRealBinary(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell probes; the Windows scripts are covered by TestImageDSL3SmokeAnnotatesWorkflowPreviewOnEveryPlatform")
	}
	if testing.Short() {
		t.Skip("builds the goobers binary")
	}

	binDir := t.TempDir()
	build := exec.Command("go", "build", "-o", filepath.Join(binDir, "goobers"), "./cmd/goobers")
	build.Dir = ".." // release/ sits directly under the repository root
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build goobers: %v\n%s", err, output)
	}
	// The image always has coreutils and goobers on a normal system path. CI
	// runs this suite with a PATH that does not always carry /usr/bin and
	// /bin, and the shipped probe uses mv — without these the script dies
	// "mv: not found" for reasons that say nothing about the probe.
	pathEnv := "PATH=" + strings.Join([]string{
		binDir, os.Getenv("PATH"), "/usr/bin", "/bin",
	}, string(os.PathListSeparator))

	t.Run("stage", func(t *testing.T) {
		stage := exec.Command("/bin/sh", "-ec", linuxImageStagePrepare)
		stage.Env = append(os.Environ(), pathEnv, "GOOBERS_STAGE_SMOKE_DIR="+t.TempDir())
		output, err := stage.CombinedOutput()
		if err != nil {
			t.Fatalf("shipped stage probe failed outside a container — it would fail the release too: %v\n%s", err, output)
		}
		if !strings.Contains(string(output), "valid") {
			t.Fatalf("stage probe never validated the quickstart instance:\n%s", output)
		}
	})

	t.Run("dsl3", func(t *testing.T) {
		dsl3 := exec.Command("/bin/sh", "-ec", linuxImageDSL3Prepare)
		dsl3.Env = append(os.Environ(), pathEnv, "GOOBERS_DSL3_SMOKE_DIR="+filepath.Join(t.TempDir(), "dsl3-demo"))
		output, err := dsl3.CombinedOutput()
		if err != nil {
			t.Fatalf("shipped DSL 3.0 probe failed outside a container — it would fail the release too: %v\n%s", err, output)
		}
		text := string(output)
		// Assert it exercised the preview path rather than silently degrading
		// into a 2.0 run, which would prove nothing.
		for _, want := range []string{"3.0 (preview)", "config/ valid"} {
			if !strings.Contains(text, want) {
				t.Fatalf("probe did not exercise the DSL 3.0 path, missing %q:\n%s", want, text)
			}
		}
		if strings.Contains(text, "DVL012") {
			t.Fatalf("probe carries the deprecated non-authorizing Manifest annotation:\n%s", text)
		}
	})
}

// The prepare/run split exists so CI can cover the container-free half — not
// to quietly drop the demo run from the release. Pin that each shipped
// constant is still its prepare half plus the run.
func TestShippedImageProbesStillRunTheDemo(t *testing.T) {
	for name, probe := range map[string]struct{ full, prepare string }{
		"stage": {linuxImageStageSmoke, linuxImageStagePrepare},
		"dsl3":  {linuxImageDSL3Smoke, linuxImageDSL3Prepare},
	} {
		t.Run(name, func(t *testing.T) {
			if !strings.HasPrefix(probe.full, probe.prepare) {
				t.Fatal("shipped probe no longer begins with the half CI executes; they have drifted")
			}
			if run := strings.TrimPrefix(probe.full, probe.prepare); !strings.Contains(run, "goobers run demo") {
				t.Error("shipped probe no longer runs the demo in the image")
			}
			if strings.Contains(probe.prepare, "goobers run demo") {
				t.Error("CI half runs the sandboxed demo; a hermetic stage cannot see a test's PATH and will exit 127")
			}
		})
	}
}
