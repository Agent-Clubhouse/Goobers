package journal

import (
	"path/filepath"
	"testing"
	"time"
)

func TestRunRootMaintenanceLocksSerializeOperations(t *testing.T) {
	root := filepath.Join(t.TempDir(), "runs")
	first, err := AcquireRunRootMaintenanceLocks([]string{root, root})
	if err != nil {
		t.Fatal(err)
	}
	firstHeld := true
	defer func() {
		if firstHeld {
			_ = first.Release()
		}
	}()

	acquired := make(chan *RunRootMaintenanceLocks, 1)
	errs := make(chan error, 1)
	go func() {
		second, err := AcquireRunRootMaintenanceLocks([]string{root})
		if err != nil {
			errs <- err
			return
		}
		acquired <- second
	}()

	select {
	case second := <-acquired:
		_ = second.Release()
		t.Fatal("second maintenance operation acquired a held run-root lock")
	case err := <-errs:
		t.Fatal(err)
	case <-time.After(200 * time.Millisecond):
	}

	if err := first.Release(); err != nil {
		t.Fatal(err)
	}
	firstHeld = false
	select {
	case second := <-acquired:
		if err := second.Release(); err != nil {
			t.Fatal(err)
		}
	case err := <-errs:
		t.Fatal(err)
	case <-time.After(5 * time.Second):
		t.Fatal("second maintenance operation did not acquire released lock")
	}
}
