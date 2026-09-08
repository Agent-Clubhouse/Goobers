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

const successfulImageDSL3SmokeOutput = `FIX Workflow/demo (gaggles/demo/workflows/demo.yaml): migrated to dslVersion 3.0 (written)
DSLVERSION Workflow/demo: 3.0 (preview)
stage curate finished (run=123, attempt=1, status=success, elapsed=0s)
stage implement finished (run=123, attempt=1, status=success, elapsed=0s)
stage review finished (run=123, attempt=1, status=success, elapsed=0s)
stage merge-preview finished (run=123, attempt=1, status=success, elapsed=0s)
finished: phase=completed
`

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
				if !strings.Contains(strings.Join(args, " "), "--read-only") {
					t.Fatal("wrong smoke command or privileges")
				}
				if calls == 2 && args[len(args)-1] == linuxImageDSL3Smoke {
					return []byte(successfulImageDSL3SmokeOutput), nil
				}
				if args[len(args)-1] != linuxImageStageSmoke {
					t.Fatal("wrong DSL 2.0 command")
				}
				return []byte(tc.output), tc.err
			}
			evidence, err := verifyImageStageSmoke(dockerImageDescription{ID: "sha256:" + strings.Repeat("1", 64), OS: "linux", Architecture: "arm64"})
			if (err == nil) != tc.want {
				t.Fatalf("accepted=%v want=%v error=%v", err == nil, tc.want, err)
			}
			wantCalls := 1
			if tc.want {
				wantCalls = 2
			}
			if calls != wantCalls {
				t.Fatalf("unexpected retry: %d", calls)
			}
			if tc.want && (evidence == nil || evidence.Output != strings.TrimSpace(tc.output) || evidence.DSL3 == nil || evidence.DSL3.Output != strings.TrimSpace(successfulImageDSL3SmokeOutput)) {
				t.Fatal("completion evidence lost")
			}
		})
	}
}

func TestImageDSL3SmokeRequiresMigrationPreviewAndEveryStage(t *testing.T) {
	for _, tc := range []struct {
		name, remove, replace string
	}{
		{"missing migration", "FIX Workflow/demo (gaggles/demo/workflows/demo.yaml): migrated to dslVersion 3.0 (written)", ""},
		{"dry run migration", "(written)", "(dry run)"},
		{"still DSL 2", "DSLVERSION Workflow/demo: 3.0 (preview)", "DSLVERSION Workflow/demo: 2.0 (supported)"},
		{"false version marker", "DSLVERSION Workflow/demo: 3.0 (preview)", "log mentions DSLVERSION Workflow/demo: 3.0 (preview)"},
		{"missing fourth stage", "stage merge-preview finished", "stage merge-preview started"},
		{"failed stage", "stage review finished (run=123, attempt=1, status=success,", "stage review finished (run=123, attempt=1, status=failed,"},
		{"missing completion", "finished: phase=completed", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			output := strings.ReplaceAll(successfulImageDSL3SmokeOutput, tc.remove, tc.replace)
			if err := validateImageDSL3SmokeOutput(output); err == nil {
				t.Fatal("accepted incomplete DSL 3.0 runtime evidence")
			}
		})
	}
	if err := validateImageDSL3SmokeOutput(strings.ReplaceAll(successfulImageDSL3SmokeOutput, "\n", "\r\n")); err != nil {
		t.Fatalf("Windows line endings: %v", err)
	}
}

func TestImageDSL3SmokeUsesFreshOfflineContainer(t *testing.T) {
	for _, platform := range []string{"linux", "windows"} {
		t.Run(platform, func(t *testing.T) {
			original := dockerImageCommand
			t.Cleanup(func() { dockerImageCommand = original })
			dockerImageCommand = func(_ time.Duration, args ...string) ([]byte, error) {
				if imageArgument(args, "--network") != "none" || !strings.Contains(strings.Join(args, " "), "--rm") {
					t.Fatal("DSL 3.0 smoke must use a fresh offline container")
				}
				entrypoint, script := "/bin/sh", linuxImageDSL3Smoke
				if platform == "windows" {
					entrypoint, script = "powershell", windowsImageDSL3Smoke
				}
				if imageArgument(args, "--entrypoint") != entrypoint || args[len(args)-1] != script {
					t.Fatal("DSL 3.0 smoke uses the wrong platform command")
				}
				return []byte(successfulImageDSL3SmokeOutput), nil
			}
			evidence, err := verifyImageDSL3Smoke(dockerImageDescription{ID: "sha256:" + strings.Repeat("1", 64), OS: platform, Architecture: "amd64"})
			if err != nil || evidence == nil {
				t.Fatalf("DSL 3.0 smoke: %v", err)
			}
		})
	}
}

func TestReleaseRefusesImageBatchWhenDSL3FailsAfterDSL2(t *testing.T) {
	useImageTestBinaries(t)
	engine := useFakeImageEngine(t, Target{OS: "linux", Arch: "arm64"})
	engine.badDSL3Family = "goobers-harness-claude"
	root := t.TempDir()
	err := run(append(imageTestArgs(root), "-targets", "linux/arm64", "-build-images", "-image-prefix", "local-dsl3-failure"), io.Discard, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "DSL 3.0 smoke failed") {
		t.Fatalf("DSL 3.0 failure must block image finalization: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "images")); !os.IsNotExist(err) {
		t.Fatal("failed DSL 3.0 image batch finalized")
	}
	assertNoImageStaging(t, root)
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
