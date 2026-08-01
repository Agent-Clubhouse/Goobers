package worktree

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

// TestIsTransientFileLockError asserts the classifier matches the Windows
// sharing-violation phrasings that surface from BOTH git's stderr and
// os.RemoveAll's *PathError during worktree teardown, and rejects unrelated
// failures so deterministic errors are never retried.
func TestIsTransientFileLockError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"os removeall used by another process", errors.New("remove C:\\runs\\x: The process cannot access the file because it is being used by another process."), true},
		{"git permission denied", errors.New("git [worktree remove --force ...]: exit status 255: error: failed to delete 'C:/runs/x': Permission denied"), true},
		{"access is denied", errors.New("Access is denied."), true},
		{"sharing violation", errors.New("open x: sharing violation"), true},
		{"wrapped", fmt.Errorf("remove stale worktree directory: %w", errors.New("used by another process")), true},
		{"unrelated", errors.New("NU1201: project is not compatible"), false},
		{"missing ref", errors.New("fatal: invalid reference: goobers/impl/x"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isTransientFileLockError(tc.err); got != tc.want {
				t.Fatalf("isTransientFileLockError(%v) = %t, want %t", tc.err, got, tc.want)
			}
		})
	}
}

// TestRetryOnFileLockSucceedsAfterTransientLocks proves a lock that clears
// after a few attempts results in overall success, and that op is retried.
func TestRetryOnFileLockSucceedsAfterTransientLocks(t *testing.T) {
	calls := 0
	err := retryOnFileLock(context.Background(), func() error {
		calls++
		if calls < 3 {
			return errors.New("The process cannot access the file because it is being used by another process.")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("retryOnFileLock returned %v, want nil", err)
	}
	if calls != 3 {
		t.Fatalf("op called %d times, want 3", calls)
	}
}

// TestRetryOnFileLockDoesNotRetryDeterministic proves a non-lock error is
// returned on the first attempt without wasting the retry budget.
func TestRetryOnFileLockDoesNotRetryDeterministic(t *testing.T) {
	calls := 0
	want := errors.New("NU1201: not compatible")
	err := retryOnFileLock(context.Background(), func() error {
		calls++
		return want
	})
	if !errors.Is(err, want) {
		t.Fatalf("retryOnFileLock returned %v, want %v", err, want)
	}
	if calls != 1 {
		t.Fatalf("op called %d times, want 1 (no retry on deterministic error)", calls)
	}
}

// TestRetryOnFileLockGivesUpAfterBudget proves a persistent lock exhausts the
// bounded attempt budget and surfaces the last error rather than looping
// forever.
func TestRetryOnFileLockGivesUpAfterBudget(t *testing.T) {
	calls := 0
	err := retryOnFileLock(context.Background(), func() error {
		calls++
		return errors.New("used by another process")
	})
	if err == nil {
		t.Fatal("retryOnFileLock returned nil, want the persistent lock error")
	}
	if calls != fileLockRetryAttempts {
		t.Fatalf("op called %d times, want %d", calls, fileLockRetryAttempts)
	}
}

// TestRetryOnFileLockAbortsOnContextCancel proves a cancelled context stops
// the retry loop promptly instead of sleeping out the full backoff schedule.
func TestRetryOnFileLockAbortsOnContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	err := retryOnFileLock(ctx, func() error {
		return errors.New("used by another process")
	})
	if err == nil {
		t.Fatal("retryOnFileLock returned nil, want joined cancel error")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error %v does not wrap context.Canceled", err)
	}
	if elapsed := time.Since(start); elapsed > fileLockRetryBackoff*time.Duration(fileLockRetryAttempts) {
		t.Fatalf("took %s, expected prompt abort on cancel", elapsed)
	}
}
