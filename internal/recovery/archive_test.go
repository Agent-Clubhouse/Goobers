package recovery

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	platformlock "github.com/goobers/goobers/internal/platform/lock"
)

func archiveTestStream(data string) func(io.Writer) error {
	return func(w io.Writer) error {
		_, err := io.WriteString(w, data)
		return err
	}
}

func TestPublishArchivePreservesOriginalOnFailureAndConflict(t *testing.T) {
	path := filepath.Join(t.TempDir(), "snapshot.bundle")
	ctx := context.Background()
	first, err := publishArchive(ctx, path, 1024, archiveTestStream("original"))
	if err != nil {
		t.Fatal(err)
	}
	if again, err := publishArchive(ctx, path, 1024, archiveTestStream("original")); err != nil || again != first {
		t.Fatalf("identical retry failed: %s %v", again, err)
	}
	if digest, err := publishArchive(ctx, path, 1024, archiveTestStream("replacement")); !errors.Is(err, ErrRecordConflict) || digest != "" {
		t.Fatalf("conflicting archive accepted: %q %v", digest, err)
	}
	failed := func(w io.Writer) error {
		if _, err := io.WriteString(w, "partial"); err != nil {
			return err
		}
		return io.ErrUnexpectedEOF
	}
	if digest, err := publishArchive(ctx, path, 1024, failed); !errors.Is(err, io.ErrUnexpectedEOF) || digest != "" {
		t.Fatalf("partial capture acknowledged: %q %v", digest, err)
	}
	if got, err := os.ReadFile(path); err != nil || string(got) != "original" {
		t.Fatalf("original archive lost: %q %v", got, err)
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil || len(entries) != 2 {
		t.Fatalf("temporary archive leaked (only archive and lock expected): %v %v", entries, err)
	}
}

func TestPublishArchiveRefusesBudgetCancellationAndHeldLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "snapshot.bundle")
	if digest, err := publishArchive(context.Background(), path, 2, archiveTestStream("too large")); err == nil || digest != "" {
		t.Fatalf("oversized archive accepted: %q %v", digest, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	stream := func(w io.Writer) error {
		cancel()
		return archiveTestStream("complete but cancelled")(w)
	}
	if digest, err := publishArchive(ctx, path, 1024, stream); !errors.Is(err, context.Canceled) || digest != "" {
		t.Fatalf("cancelled capture published: %q %v", digest, err)
	}
	handle, err := platformlock.TryAcquire(path + ".lock")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = handle.Release() }()
	if digest, err := publishArchive(context.Background(), path, 1024, archiveTestStream("blocked")); !errors.Is(err, platformlock.ErrHeld) || digest != "" {
		t.Fatalf("publisher lock ignored: %q %v", digest, err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed publication left an archive: %v", err)
	}
}
