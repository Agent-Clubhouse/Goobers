package journal

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
)

// Concurrent writers must never interleave through a shared staging file: the
// published file has to be exactly one writer's payload, whole.
func TestWriteFileAtomicConcurrentWritersPublishWholePayload(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.yaml")

	const writers = 8
	payloads := make([][]byte, writers)
	for i := range payloads {
		payloads[i] = bytes.Repeat([]byte{byte('a' + i)}, 64*1024+i)
	}

	var wg sync.WaitGroup
	errs := make([]error, writers)
	start := make(chan struct{})
	for i := range payloads {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			errs[i] = WriteFileAtomic(path, payloads[i], 0o644)
		}(i)
	}
	close(start)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("writer %d: %v", i, err)
		}
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	matched := -1
	for i, payload := range payloads {
		if bytes.Equal(got, payload) {
			matched = i
			break
		}
	}
	if matched < 0 {
		t.Fatalf("published file is not any single writer's payload in full: len=%d prefix=%q", len(got), got[:min(len(got), 16)])
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.Name() != "state.yaml" {
			t.Fatalf("staging file %q left behind after successful writes", entry.Name())
		}
	}
}

// A <name>.tmp file staged by a prior binary version is inert: it is neither
// read nor reused, and the atomic write still publishes correctly.
func TestWriteFileAtomicToleratesLegacyTempLeftover(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.yaml")
	legacy := path + ".tmp"

	if err := os.WriteFile(path, []byte("original\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(legacy, []byte("half-written leftov"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := WriteFileAtomic(path, []byte("published\n"), 0o644); err != nil {
		t.Fatalf("WriteFileAtomic: %v", err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "published\n" {
		t.Fatalf("target = %q, want %q", got, "published\n")
	}
	stale, err := os.ReadFile(legacy)
	if err != nil {
		t.Fatalf("legacy temp must remain readable: %v", err)
	}
	if string(stale) != "half-written leftov" {
		t.Fatalf("legacy temp = %q, want it untouched", stale)
	}
}

// A failure on the commit path leaves the original bytes and drops the staged
// file, so a fault mid-write can never truncate what was already on disk.
func TestWriteFileAtomicCommitFailureLeavesOriginalIntact(t *testing.T) {
	dir := t.TempDir()
	// The target is a directory, so staging succeeds and the rename fails.
	path := filepath.Join(dir, "target")
	if err := os.Mkdir(path, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, "child"), []byte("kept\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := WriteFileAtomic(path, []byte("new bytes\n"), 0o644); err == nil {
		t.Fatal("WriteFileAtomic over a directory: want error, got nil")
	}

	kept, err := os.ReadFile(filepath.Join(path, "child"))
	if err != nil {
		t.Fatalf("original content must survive a failed write: %v", err)
	}
	if string(kept) != "kept\n" {
		t.Fatalf("original = %q, want %q", kept, "kept\n")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.Name() != "target" {
			t.Fatalf("staging file %q left behind after a failed write", entry.Name())
		}
	}
}

// A write that cannot even stage (read-only directory, the full-disk analogue)
// fails with a clear error and leaves the target byte-identical.
func TestWriteFileAtomicUnwritableDirLeavesOriginalIntact(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("directory permissions do not gate file creation on Windows")
	}
	if os.Getuid() == 0 {
		t.Skip("root bypasses directory permissions")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "state.yaml")
	original := []byte("original contents\n")
	if err := os.WriteFile(path, original, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	err := WriteFileAtomic(path, []byte("replacement\n"), 0o644)
	if err == nil {
		t.Fatal("WriteFileAtomic into a read-only dir: want error, got nil")
	}
	if !strings.Contains(err.Error(), path) && !strings.Contains(err.Error(), dir) {
		t.Fatalf("error %q should name the file or directory it could not write", err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, original) {
		t.Fatalf("target = %q, want it unmodified (%q)", got, original)
	}
}

func TestWriteFileAtomicAppliesPerm(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX file modes are not modeled on Windows")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "secret.yaml")
	if err := WriteFileAtomic(path, []byte("x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v, want 0600", info.Mode().Perm())
	}
}
