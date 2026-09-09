package recovery

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	platformlock "github.com/goobers/goobers/internal/platform/lock"
)

func TestRetainedPublicationRejectsCleanupLocationsBeforeWriting(t *testing.T) {
	repository := t.TempDir()
	child := filepath.Join(repository, "archive")
	if err := os.Mkdir(child, 0o700); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	for _, tc := range []struct {
		name, directory string
		roots           []string
	}{
		{"repository itself", repository, []string{repository}},
		{"repository child", child, []string{repository}},
		{"other cleanup root", outside, []string{outside}},
		{"undeclared cleanup", outside, nil},
		{"empty cleanup root", outside, []string{""}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := PublishRetainedState(context.Background(), repository, tc.directory, tc.roots, storageTestRecord(), 1<<20)
			if err == nil || got != (Record{}) {
				t.Fatalf("unsafe publication acknowledged: %+v %v", got, err)
			}
			for _, name := range []string{".publish.lock", BundleFileName, RecordFileName} {
				if _, err := os.Lstat(filepath.Join(tc.directory, name)); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("unsafe location wrote %s: %v", name, err)
				}
			}
		})
	}
}

func TestRetainedPublicationHonorsCoordinatorLock(t *testing.T) {
	repository, directory := t.TempDir(), t.TempDir()
	handle, err := platformlock.TryAcquire(filepath.Join(directory, ".publish.lock"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = handle.Release() }()
	got, err := PublishRetainedState(context.Background(), repository, directory, []string{repository}, storageTestRecord(), 1<<20)
	if !errors.Is(err, platformlock.ErrHeld) || got != (Record{}) {
		t.Fatalf("publication bypassed coordinator lock: %+v %v", got, err)
	}
}

func TestIndependentArchiveResolvesSymlinkAliases(t *testing.T) {
	root := t.TempDir()
	cleanup := filepath.Join(root, "cleanup")
	archive := filepath.Join(cleanup, "archive")
	if err := os.MkdirAll(archive, 0o700); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(root, "alias")
	if err := os.Symlink(cleanup, alias); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if err := requireIndependentArchive(filepath.Join(alias, "archive"), []string{cleanup}, true); err == nil {
		t.Fatal("archive alias bypassed cleanup guard")
	}
	if err := requireIndependentArchive(archive, []string{alias}, true); err == nil {
		t.Fatal("cleanup alias bypassed cleanup guard")
	}
	if err := requireIndependentArchive(t.TempDir(), []string{alias}, true); err != nil {
		t.Fatalf("independent archive rejected: %v", err)
	}
}
