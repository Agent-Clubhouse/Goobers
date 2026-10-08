package lock

import (
	"os"
	"path/filepath"
	"testing"
)

func TestAnnouncedHolderIsReadableWhileHeldAndClearedOnRelease(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.lock")
	if _, ok := ReadHolder(path); ok {
		t.Fatal("ReadHolder reported a holder before any acquisition")
	}

	held, err := TryAcquire(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := held.Announce("test.section"); err != nil {
		t.Fatal(err)
	}
	holder, ok := ReadHolder(path)
	if !ok || holder.Operation != "test.section" || holder.PID != os.Getpid() || holder.AcquiredAt.IsZero() {
		t.Fatalf("ReadHolder = %+v, %v; want this process's announced section", holder, ok)
	}

	if err := held.Release(); err != nil {
		t.Fatal(err)
	}
	if holder, ok := ReadHolder(path); ok {
		t.Fatalf("ReadHolder after Release = %+v, want no holder", holder)
	}
	if err := held.Announce("late"); err == nil {
		t.Fatal("Announce on a released handle succeeded")
	}
}

func TestReadHolderIgnoresTornRecord(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.lock")
	if err := os.WriteFile(path+holderSuffix, []byte(`{"operation":"tor`), 0o644); err != nil {
		t.Fatal(err)
	}
	if holder, ok := ReadHolder(path); ok {
		t.Fatalf("ReadHolder on a torn record = %+v, want no holder", holder)
	}
}
