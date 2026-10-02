package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Isolated filesystem tests for the daemon API address file (#4575):
// publishDaemonAPIAddress must replace the file atomically and never leak its
// temporary file, and removeDaemonAPIAddress must treat a missing file as
// already removed. Every test works in its own t.TempDir().

func readAPIAddressFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read address file: %v", err)
	}
	return string(data)
}

// assertOnlyAPIAddressEntries fails when dir holds anything other than the
// named entries, which is how a leaked temporary address file shows up.
func assertOnlyAPIAddressEntries(t *testing.T, dir string, want ...string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	got := make([]string, 0, len(entries))
	for _, entry := range entries {
		got = append(got, entry.Name())
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("directory entries = %q, want exactly %q (leaked temporary file?)", got, want)
	}
}

func TestPublishDaemonAPIAddressWritesNewFileWithoutTempLeak(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, daemonAPIAddressFileName)

	if err := publishDaemonAPIAddress(path, "127.0.0.1:41234"); err != nil {
		t.Fatalf("publishDaemonAPIAddress: %v", err)
	}
	if got := readAPIAddressFile(t, path); got != "127.0.0.1:41234\n" {
		t.Fatalf("address file = %q, want the address plus a newline", got)
	}
	assertOnlyAPIAddressEntries(t, dir, daemonAPIAddressFileName)
}

func TestPublishDaemonAPIAddressReplacesExistingAddress(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, daemonAPIAddressFileName)
	if err := os.WriteFile(path, []byte("127.0.0.1:1111\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := publishDaemonAPIAddress(path, "127.0.0.1:2222"); err != nil {
		t.Fatalf("publishDaemonAPIAddress: %v", err)
	}
	if got := readAPIAddressFile(t, path); got != "127.0.0.1:2222\n" {
		t.Fatalf("address file = %q, want the replacement address", got)
	}
	assertOnlyAPIAddressEntries(t, dir, daemonAPIAddressFileName)
}

// TestPublishDaemonAPIAddressTempFileLivesBesideTarget pins the atomicity
// precondition: the temporary file is created in the target's own directory
// (a same-filesystem rename) under a hidden name readers never match.
func TestPublishDaemonAPIAddressTempFileLivesBesideTarget(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, daemonAPIAddressFileName)
	var gotDir, gotPattern string
	original := createDaemonAPIAddressTempFile
	t.Cleanup(func() { createDaemonAPIAddressTempFile = original })
	createDaemonAPIAddressTempFile = func(dir, pattern string) (daemonAPIAddressTempFile, error) {
		gotDir, gotPattern = dir, pattern
		return original(dir, pattern)
	}

	if err := publishDaemonAPIAddress(path, "127.0.0.1:3333"); err != nil {
		t.Fatalf("publishDaemonAPIAddress: %v", err)
	}
	if gotDir != dir {
		t.Fatalf("temp dir = %q, want the target's directory %q", gotDir, dir)
	}
	if !strings.HasPrefix(gotPattern, "."+daemonAPIAddressFileName+"-") {
		t.Fatalf("temp pattern = %q, want a hidden %q-prefixed name", gotPattern, daemonAPIAddressFileName)
	}
}

func TestPublishDaemonAPIAddressCreateFailureLeavesNothing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing-scheduler-dir", daemonAPIAddressFileName)

	err := publishDaemonAPIAddress(path, "127.0.0.1:4444")
	if err == nil || !strings.Contains(err.Error(), "create daemon API address file") {
		t.Fatalf("publishDaemonAPIAddress error = %v, want a create failure", err)
	}
	if _, statErr := os.Stat(path); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("address file stat = %v, want not-exist", statErr)
	}
}

// failingAPIAddressFile wraps a real temporary file and fails Write or Close.
type failingAPIAddressFile struct {
	*os.File
	writeErr error
	closeErr error
}

func (f *failingAPIAddressFile) Write(p []byte) (int, error) {
	if f.writeErr != nil {
		return 0, f.writeErr
	}
	return f.File.Write(p)
}

// WriteString shadows the embedded *os.File method, which io.WriteString
// would otherwise call directly and bypass Write.
func (f *failingAPIAddressFile) WriteString(s string) (int, error) {
	return f.Write([]byte(s))
}

func (f *failingAPIAddressFile) Close() error {
	err := f.File.Close()
	if f.closeErr != nil {
		return f.closeErr
	}
	return err
}

func TestPublishDaemonAPIAddressWriteOrCloseFailureKeepsPriorAddressAndCleansTemp(t *testing.T) {
	for _, tc := range []struct {
		name     string
		file     func(*os.File) *failingAPIAddressFile
		wantText string
	}{
		{
			name: "write",
			file: func(f *os.File) *failingAPIAddressFile {
				return &failingAPIAddressFile{File: f, writeErr: errors.New("disk full")}
			},
			wantText: "write daemon API address file: disk full",
		},
		{
			name: "close",
			file: func(f *os.File) *failingAPIAddressFile {
				return &failingAPIAddressFile{File: f, closeErr: errors.New("flush failed")}
			},
			wantText: "close daemon API address file: flush failed",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, daemonAPIAddressFileName)
			if err := os.WriteFile(path, []byte("127.0.0.1:1111\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			original := createDaemonAPIAddressTempFile
			t.Cleanup(func() { createDaemonAPIAddressTempFile = original })
			var tempPath string
			createDaemonAPIAddressTempFile = func(dir, pattern string) (daemonAPIAddressTempFile, error) {
				f, err := os.CreateTemp(dir, pattern)
				if err != nil {
					return nil, err
				}
				tempPath = f.Name()
				return tc.file(f), nil
			}

			err := publishDaemonAPIAddress(path, "127.0.0.1:5555")
			if err == nil || !strings.Contains(err.Error(), tc.wantText) {
				t.Fatalf("publishDaemonAPIAddress error = %v, want %q", err, tc.wantText)
			}
			if got := readAPIAddressFile(t, path); got != "127.0.0.1:1111\n" {
				t.Fatalf("address file = %q, want the prior address untouched", got)
			}
			if _, statErr := os.Stat(tempPath); !errors.Is(statErr, os.ErrNotExist) {
				t.Fatalf("temporary file %q stat = %v, want removed", tempPath, statErr)
			}
			assertOnlyAPIAddressEntries(t, dir, daemonAPIAddressFileName)
		})
	}
}

// TestPublishDaemonAPIAddressReplaceFailureCleansTemp forces the final
// replacement to fail by making the target a non-empty directory.
func TestPublishDaemonAPIAddressReplaceFailureCleansTemp(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, daemonAPIAddressFileName)
	if err := os.MkdirAll(filepath.Join(path, "occupied"), 0o755); err != nil {
		t.Fatal(err)
	}

	err := publishDaemonAPIAddress(path, "127.0.0.1:6666")
	if err == nil || !strings.Contains(err.Error(), "publish daemon API address") {
		t.Fatalf("publishDaemonAPIAddress error = %v, want a replace failure", err)
	}
	assertOnlyAPIAddressEntries(t, dir, daemonAPIAddressFileName)
	if info, statErr := os.Stat(path); statErr != nil || !info.IsDir() {
		t.Fatalf("target stat = %v, %v; want the pre-existing directory untouched", info, statErr)
	}
}

func TestRemoveDaemonAPIAddressRemovesFileAndToleratesMissing(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, daemonAPIAddressFileName)
	sibling := filepath.Join(dir, "up.lock")
	for _, file := range []string{path, sibling} {
		if err := os.WriteFile(file, []byte("x\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	if err := removeDaemonAPIAddress(path); err != nil {
		t.Fatalf("removeDaemonAPIAddress: %v", err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("address file stat = %v, want removed", err)
	}
	// Only the address file goes; the rest of the scheduler directory stays.
	assertOnlyAPIAddressEntries(t, dir, "up.lock")

	// Already gone (a clean prior shutdown, or a second removal): not an error.
	if err := removeDaemonAPIAddress(path); err != nil {
		t.Fatalf("removeDaemonAPIAddress on a missing file: %v", err)
	}
	if err := removeDaemonAPIAddress(filepath.Join(dir, "no-such-dir", daemonAPIAddressFileName)); err != nil {
		t.Fatalf("removeDaemonAPIAddress under a missing directory: %v", err)
	}
}

func TestRemoveDaemonAPIAddressReportsOtherErrors(t *testing.T) {
	path := filepath.Join(t.TempDir(), daemonAPIAddressFileName)
	if err := os.MkdirAll(filepath.Join(path, "occupied"), 0o755); err != nil {
		t.Fatal(err)
	}
	err := removeDaemonAPIAddress(path)
	if err == nil || !strings.Contains(err.Error(), "remove daemon API address") {
		t.Fatalf("removeDaemonAPIAddress error = %v, want a wrapped removal failure", err)
	}
}
