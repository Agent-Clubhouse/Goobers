//go:build unix

package readservice

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"

	"github.com/goobers/goobers/internal/instance"
)

func TestRunAttributionRejectsEscapingAncestorAndFIFO(t *testing.T) {
	root, _, runID := seedStoredAttributionRun(t)
	layout := instance.NewLayout(root)
	original := layout.RunsDir()
	outside := filepath.Join(t.TempDir(), "runs")
	if err := os.Rename(original, outside); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, original); err != nil {
		t.Fatal(err)
	}
	service := &Local{sources: LocalSources{Layout: layout}}
	if _, err := service.RunAttribution(context.Background(), runID, "core"); !errors.Is(err, ErrArtifactIntegrity) {
		t.Fatalf("ancestor escaped: %v", err)
	}
	if err := os.Remove(original); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(outside, original); err != nil {
		t.Fatal(err)
	}
	record := filepath.Join(original, runID, "attribution.json")
	if err := os.Remove(record); err != nil {
		t.Fatal(err)
	}
	if err := unix.Mkfifo(record, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := service.RunAttribution(context.Background(), runID, "core"); !errors.Is(err, ErrArtifactIntegrity) {
		t.Fatalf("FIFO accepted: %v", err)
	}
}
func TestAttributionReadRootPinsDirectoryAcrossReplacement(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "run")
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, "record"), []byte("original"), 0600); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	if err := os.Rename(path, path+"-old"); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "record"), []byte("foreign"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, path); err != nil {
		t.Fatal(err)
	}
	b := attributionReadBudget{ctx: context.Background(), root: root, remaining: 100}
	data, err := b.read("record", 100)
	if err != nil || string(data) != "original" {
		t.Fatalf("replacement followed: %q %v", data, err)
	}
}
