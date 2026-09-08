package main

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestImageStageSmokeRequiresSuccessfulCompletion(t *testing.T) {
	for _, tc := range []struct {
		name, output string
		err          error
		want         bool
	}{
		{name: "completed", output: "stage review finished\nfinished: phase=completed\n", want: true},
		{name: "failed phase", output: "finished: phase=failed\n"},
		{name: "missing phase", output: "quickstart initialized\n"},
		{name: "embedded text", output: "diagnostic mentions finished: phase=completed inside text"},
		{name: "nonzero process", output: "finished: phase=completed\n", err: errors.New("container failed")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			original := dockerImageCommand
			t.Cleanup(func() { dockerImageCommand = original })
			calls := 0
			dockerImageCommand = func(_ time.Duration, args ...string) ([]byte, error) {
				calls++
				if imageArgument(args, "--network") != "none" || imageArgument(args, "--entrypoint") != "/bin/sh" {
					t.Fatal("stage smoke lost its offline shell boundary")
				}
				if args[len(args)-1] != linuxImageStageSmoke || !strings.Contains(strings.Join(args, " "), "--read-only") {
					t.Fatal("wrong smoke command or privileges")
				}
				return []byte(tc.output), tc.err
			}
			evidence, err := verifyImageStageSmoke(dockerImageDescription{ID: "sha256:" + strings.Repeat("1", 64), OS: "linux", Architecture: "arm64"})
			if (err == nil) != tc.want {
				t.Fatalf("accepted=%v want=%v error=%v", err == nil, tc.want, err)
			}
			if calls != 1 {
				t.Fatalf("unexpected retry: %d", calls)
			}
			if tc.want && (evidence == nil || evidence.Output != strings.TrimSpace(tc.output)) {
				t.Fatal("completion evidence lost")
			}
		})
	}
}

func TestReleaseRefusesImageBatchWhenHarnessCannotRunStage(t *testing.T) {
	useImageTestBinaries(t)
	engine := useFakeImageEngine(t, Target{OS: "linux", Arch: "arm64"})
	engine.badSmokeFamily = "goobers-harness-claude"
	root := t.TempDir()
	err := run(append(imageTestArgs(root), "-targets", "linux/arm64", "-build-images", "-image-prefix", "local-smoke-failure"), io.Discard, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "mock demo did not report completed phase") {
		t.Fatalf("stage failure did not block image finalization: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "images")); !os.IsNotExist(err) {
		t.Fatal("failed image batch finalized")
	}
	for tag := range engine.tags {
		if strings.HasPrefix(tag, "goobers-release-input:") {
			t.Fatal("temporary base alias leaked")
		}
	}
	assertNoImageStaging(t, root)
}
