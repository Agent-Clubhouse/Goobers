package mergeresolve

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/platform/durability"
)

func TestConflictWriteFailureRestoresCurrentAndPreviousPaths(t *testing.T) {
	for _, published := range []bool{false, true} {
		name := "before publication"
		if published {
			name = "after publication"
		}
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			modes := make(map[string]os.FileMode)
			for _, path := range []string{"first.json", "second.json"} {
				if err := os.WriteFile(filepath.Join(dir, path), []byte("original "+path), 0o755); err != nil {
					t.Fatal(err)
				}
				info, err := os.Stat(filepath.Join(dir, path))
				if err != nil {
					t.Fatal(err)
				}
				modes[path] = info.Mode().Perm()
			}
			failure := errors.New("injected durability failure")
			write := func(path string, data []byte, mode os.FileMode, opts ...durability.Option) error {
				if filepath.Base(path) == "second.json" && string(data) == "resolved" {
					if published {
						if err := durability.WriteFileAtomic(path, data, mode, opts...); err != nil {
							return err
						}
					}
					return failure
				}
				return durability.WriteFileAtomic(path, data, mode, opts...)
			}
			git := func(...string) ([]byte, error) {
				t.Error("must not stage after a write failure")
				return nil, nil
			}
			status, err := writeResolvedConflicts(dir, git, []conflictResolution{
				{"first.json", []byte("resolved")}, {"second.json", []byte("resolved")},
			}, write)
			if status != StatusUnsafe || !errors.Is(err, failure) {
				t.Fatalf("resolution = %v, %v", status, err)
			}
			for _, path := range []string{"first.json", "second.json"} {
				data, err := os.ReadFile(filepath.Join(dir, path))
				if err != nil || string(data) != "original "+path {
					t.Fatalf("restored %s = %q, %v", path, data, err)
				}
				info, err := os.Stat(filepath.Join(dir, path))
				if err != nil || info.Mode().Perm() != modes[path] {
					t.Fatalf("restored mode for %s: %v, %v", path, info, err)
				}
			}
		})
	}
}

func TestConflictRollbackReportsFailedRestoration(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "package.json")
	if err := os.WriteFile(path, []byte("original"), 0o644); err != nil {
		t.Fatal(err)
	}
	publicationFailure := errors.New("sync failed")
	restoreFailure := errors.New("restore failed")
	write := func(path string, data []byte, mode os.FileMode, opts ...durability.Option) error {
		if string(data) == "original" {
			return restoreFailure
		}
		if err := durability.WriteFileAtomic(path, data, mode, opts...); err != nil {
			return err
		}
		return publicationFailure
	}
	git := func(...string) ([]byte, error) {
		t.Error("must not stage after a write failure")
		return nil, nil
	}
	status, err := writeResolvedConflicts(dir, git, []conflictResolution{{"package.json", []byte("resolved")}}, write)
	if status != StatusUnsafe || !errors.Is(err, publicationFailure) || !errors.Is(err, restoreFailure) || !strings.Contains(err.Error(), path) {
		t.Fatalf("rollback error lost cause or affected path: status=%v err=%v", status, err)
	}
}
