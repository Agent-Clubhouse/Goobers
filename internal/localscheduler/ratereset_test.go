package localscheduler

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

// TestRateResetRoundTrip proves a written marker reads back at the same
// instant (UTC RFC3339Nano round-trip) and that WriteRateReset creates the
// scheduler dir if it doesn't exist yet.
func TestRateResetRoundTrip(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "scheduler") // deliberately not pre-created
	at := time.Now().Add(-3 * time.Minute)

	if err := WriteRateReset(dir, at); err != nil {
		t.Fatalf("WriteRateReset: %v", err)
	}
	got, ok, err := ReadRateReset(dir)
	if err != nil {
		t.Fatalf("ReadRateReset: %v", err)
	}
	if !ok {
		t.Fatal("ReadRateReset: ok=false, want a marker")
	}
	if !got.Equal(at) {
		t.Fatalf("read %s, want %s", got, at)
	}
}

// TestRateResetMissingIsCleanZero proves the common case — no marker — is a
// clean (ok=false, nil error) result, not an error every instance would hit.
func TestRateResetMissingIsCleanZero(t *testing.T) {
	dir := t.TempDir()
	got, ok, err := ReadRateReset(dir)
	if err != nil {
		t.Fatalf("ReadRateReset on a dir with no marker: %v", err)
	}
	if ok {
		t.Fatalf("ok=true with no marker written, got %s", got)
	}
}

// TestRateResetMalformedIsError proves a corrupt marker surfaces as an error
// rather than being silently ignored (which would quietly disable the reset an
// operator is relying on).
func TestRateResetMalformedIsError(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, rateResetFileName), []byte("not-a-timestamp"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := ReadRateReset(dir); err == nil {
		t.Fatalf("expected an error on a malformed marker, got ok=%v nil error", ok)
	}
}

// TestRateResetOverwrites proves a later reset supersedes an earlier one
// (idempotent, single-file — never accumulates).
func TestRateResetOverwrites(t *testing.T) {
	dir := t.TempDir()
	earlier := time.Now().Add(-time.Hour)
	later := time.Now()
	if err := WriteRateReset(dir, earlier); err != nil {
		t.Fatal(err)
	}
	if err := WriteRateReset(dir, later); err != nil {
		t.Fatal(err)
	}
	got, ok, err := ReadRateReset(dir)
	if err != nil || !ok {
		t.Fatalf("ReadRateReset: ok=%v err=%v", ok, err)
	}
	if !got.Equal(later) {
		t.Fatalf("read %s, want the later reset %s", got, later)
	}
}

func TestWriteRateResetPublishesAtomically(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("directory permissions do not gate file creation on Windows")
	}
	if os.Getuid() == 0 {
		t.Skip("root bypasses directory permissions")
	}
	oldTime := time.Date(2026, time.August, 15, 10, 11, 12, 13, time.UTC)
	newTime := oldTime.Add(time.Hour)
	oldContent := []byte(oldTime.Format(time.RFC3339Nano) + "\n")
	newContent := []byte(newTime.Format(time.RFC3339Nano) + "\n")

	for _, tc := range []struct {
		name    string
		initial []byte
	}{
		{name: "absent"},
		{name: "old", initial: oldContent},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, rateResetFileName)
			if tc.initial != nil {
				if err := os.WriteFile(path, tc.initial, 0o644); err != nil {
					t.Fatal(err)
				}
			}
			// Force the staged write to fail: the atomic helper stages through a
			// uniquely named sibling, so an unwritable directory is what blocks it.
			if err := os.Chmod(dir, 0o500); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

			if err := WriteRateReset(dir, newTime); err == nil {
				t.Fatal("WriteRateReset succeeded with an unusable atomic temp path")
			}
			got, err := os.ReadFile(path)
			if tc.initial == nil {
				if !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("marker after failed write: data=%q err=%v, want absent", got, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != string(tc.initial) {
				t.Fatalf("marker after failed write = %q, want complete old content %q", got, tc.initial)
			}
		})
	}

	dir := t.TempDir()
	if err := WriteRateReset(dir, newTime); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(dir, rateResetFileName))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(newContent) {
		t.Fatalf("marker after successful write = %q, want complete new content %q", got, newContent)
	}
}
