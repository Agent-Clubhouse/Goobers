package localscheduler

import (
	"os"
	"path/filepath"
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
