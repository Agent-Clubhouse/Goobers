//go:build !linux && !darwin

package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/configsync"
)

func TestRunRenderRefusesUnsupportedAtomicPublication(t *testing.T) {
	cfg := writeRepo(t)
	parent := t.TempDir()
	out := filepath.Join(parent, "rendered")
	stderr := outputFile(t)

	if code := run([]string{"--config", cfg, "--out", out}, devnull(t), stderr); code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	message := readOutput(t, stderr)
	if !strings.Contains(message, "atomic manifest publication is unsupported") ||
		!strings.Contains(message, "render on Linux or macOS, or use --apply") {
		t.Fatalf("stderr did not explain the refusal and alternatives:\n%s", message)
	}
	if _, err := os.Lstat(out); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("output created despite refusal: %v", err)
	}
	if _, err := os.Lstat(configsync.ManifestGenerationDir(out)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("generation directory created despite refusal: %v", err)
	}
}
