package journal

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	platformlock "github.com/goobers/goobers/internal/platform/lock"
)

func TestWithIdleRunReaderProtectsAgainstConcurrentWriters(t *testing.T) {
	run, err := Create(t.TempDir(), testIdentity(), nil, WithClock(fixedClock()))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = run.Close() }()
	dir := run.Dir()
	called := false
	visit := func(*Reader) error { called = true; return nil }
	if entered, err := WithIdleRunReader(context.Background(), dir, visit); entered || err != nil || called {
		t.Fatalf("live writer bypassed: entered=%t called=%t err=%v", entered, called, err)
	}
	if err := run.Close(); err != nil {
		t.Fatal(err)
	}
	wantErr := errors.New("callback failed")
	entered, err := WithIdleRunReader(context.Background(), dir, func(reader *Reader) error {
		if reader.Dir() != dir {
			t.Fatal("wrong journal reader")
		}
		lock, err := platformlock.TryAcquireExisting(filepath.Join(dir, fileLock))
		if lock != nil {
			_ = lock.Release()
		}
		if !errors.Is(err, platformlock.ErrHeld) {
			t.Fatalf("writer lock not held: %v", err)
		}
		if entered, err := WithIdleRunReader(context.Background(), dir, visit); entered || err != nil || called {
			t.Fatalf("nested maintenance bypassed publication lock: %t %v", entered, err)
		}
		return wantErr
	})
	if !entered || !errors.Is(err, wantErr) {
		t.Fatalf("callback failure lost: entered=%t err=%v", entered, err)
	}
	if entered, err := WithIdleRunReader(context.Background(), dir, visit); !entered || err != nil || !called {
		t.Fatalf("locks leaked after callback: %t %v", entered, err)
	}
}

func TestWithIdleRunReaderRefusesPruningAndCancellation(t *testing.T) {
	run, err := Create(t.TempDir(), testIdentity(), nil, WithClock(fixedClock()))
	if err != nil {
		t.Fatal(err)
	}
	if err := run.Append(Event{Type: EventRunFinished, Status: string(PhaseCompleted)}); err != nil {
		t.Fatal(err)
	}
	if err := run.Close(); err != nil {
		t.Fatal(err)
	}
	visit := func(*Reader) error { t.Fatal("refused reader callback invoked"); return nil }
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if entered, err := WithIdleRunReader(ctx, run.Dir(), visit); entered || !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation ignored: %t %v", entered, err)
	}
	if reserved, err := ReserveTerminalForPrune(run.Dir()); !reserved || err != nil {
		t.Fatalf("reserve: %t %v", reserved, err)
	}
	if entered, err := WithIdleRunReader(context.Background(), run.Dir(), visit); entered || !errors.Is(err, ErrPruneReserved) {
		t.Fatalf("prune reservation ignored: %t %v", entered, err)
	}
}
