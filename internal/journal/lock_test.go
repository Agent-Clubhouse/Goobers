package journal

import (
	"path/filepath"
	"testing"
	"time"
)

// TestRecoverSerializesConcurrentWriters is #243's core acceptance test: two
// writers opening the same run directory (e.g. `goobers run abort` racing a
// live daemon's own Resume of a crashed run) must never both hold an
// independent *Run over one events.jsonl at the same time. The second
// Recover call blocks until the first releases its lock via Close, rather
// than proceeding immediately and risking interleaved appends.
func TestRecoverSerializesConcurrentWriters(t *testing.T) {
	root := t.TempDir()
	run, err := Create(root, testIdentity(), nil, WithClock(fixedClock()))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := run.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	dir := filepath.Join(root, testIdentity().RunID)

	first, _, err := Recover(dir, WithClock(fixedClock()))
	if err != nil {
		t.Fatalf("first Recover: %v", err)
	}

	second := make(chan struct{})
	go func() {
		r2, _, err := Recover(dir, WithClock(fixedClock()))
		if err != nil {
			t.Errorf("second Recover: %v", err)
			close(second)
			return
		}
		_ = r2.Close()
		close(second)
	}()

	// The second Recover must still be blocked shortly after being started
	// — it must NOT have proceeded while the first writer still holds the
	// lock.
	select {
	case <-second:
		t.Fatal("second Recover returned before the first Close — the lock did not serialize the two writers")
	case <-time.After(200 * time.Millisecond):
	}

	if err := first.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}

	select {
	case <-second:
	case <-time.After(5 * time.Second):
		t.Fatal("second Recover did not proceed after the first released its lock")
	}
}

// TestCreateSerializesAgainstConcurrentRecover confirms the lock is
// symmetric: a Recover holding the run dir must block a concurrent Create
// attempt on a run id that Recover is still resuming... actually Create
// only ever targets a brand-new id (Mkdir's own EEXIST already refuses a
// second Create of the same id), so this test instead confirms Create
// itself acquires and later releases the lock — a subsequent Recover must
// be able to proceed once Create's own Run is closed, not find the lock
// file left stuck held.
func TestCreateReleasesLockOnClose(t *testing.T) {
	root := t.TempDir()
	run, err := Create(root, testIdentity(), nil, WithClock(fixedClock()))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := run.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	dir := filepath.Join(root, testIdentity().RunID)

	done := make(chan struct{})
	go func() {
		r2, _, err := Recover(dir, WithClock(fixedClock()))
		if err != nil {
			t.Errorf("Recover after Create+Close: %v", err)
			close(done)
			return
		}
		_ = r2.Close()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Recover did not proceed after Create released its lock on Close — lock leaked")
	}
}
