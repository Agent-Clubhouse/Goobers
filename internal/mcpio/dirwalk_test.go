package mcpio

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"testing"
)

func TestWalkFiles(t *testing.T) {
	// Create a temporary directory structure for testing
	tmpDir := t.TempDir()

	// Create test files and directories
	if err := os.WriteFile(filepath.Join(tmpDir, "file1.txt"), []byte("content1"), 0o644); err != nil {
		t.Fatalf("create file1: %v", err)
	}
	if err := os.WriteFile(filepath.Join(tmpDir, ".hidden.txt"), []byte("hidden"), 0o644); err != nil {
		t.Fatalf("create .hidden: %v", err)
	}
	subdir := filepath.Join(tmpDir, "subdir")
	if err := os.Mkdir(subdir, 0o755); err != nil {
		t.Fatalf("create subdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(subdir, "file2.txt"), []byte("content2"), 0o644); err != nil {
		t.Fatalf("create file2: %v", err)
	}

	hiddenDir := filepath.Join(tmpDir, ".hidden_dir")
	if err := os.Mkdir(hiddenDir, 0o755); err != nil {
		t.Fatalf("create .hidden_dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(hiddenDir, "file3.txt"), []byte("content3"), 0o644); err != nil {
		t.Fatalf("create file3: %v", err)
	}

	tests := []struct {
		name      string
		opts      WalkFilesOptions
		wantFiles []string // relative paths
		wantErr   bool
	}{
		{
			name:      "default options skip hidden dirs but keep hidden regular files",
			opts:      DefaultWalkFilesOptions(),
			wantFiles: []string{".hidden.txt", "file1.txt", "subdir/file2.txt"},
		},
		{
			name: "hidden files can be skipped explicitly",
			opts: WalkFilesOptions{
				SkipHiddenDirs:     true,
				SkipHiddenFiles:    true,
				SkipSymlinkEntries: true,
				SkipDirs:           true,
			},
			wantFiles: []string{"file1.txt", "subdir/file2.txt"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var found []string
			err := WalkFiles(tmpDir, func(path string, entry fs.DirEntry) error {
				rel, _ := filepath.Rel(tmpDir, path)
				found = append(found, filepath.ToSlash(rel))
				return nil
			}, tt.opts)

			if (err != nil) != tt.wantErr {
				t.Errorf("WalkFiles error = %v, wantErr %v", err, tt.wantErr)
			}

			sort.Strings(found)
			sort.Strings(tt.wantFiles)

			if len(found) != len(tt.wantFiles) {
				t.Errorf("WalkFiles found %d files, want %d", len(found), len(tt.wantFiles))
				t.Logf("found: %v", found)
				t.Logf("want: %v", tt.wantFiles)
				return
			}

			for i, f := range found {
				if f != tt.wantFiles[i] {
					t.Errorf("file[%d] = %q, want %q", i, f, tt.wantFiles[i])
				}
			}
		})
	}
}

func TestWalkFilesNotExistHandling(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing")

	if err := WalkFiles(missing, func(string, fs.DirEntry) error { return nil }, DefaultWalkFilesOptions()); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("WalkFiles missing root error = %v, want fs.ErrNotExist", err)
	}

	opts := DefaultWalkFilesOptions()
	opts.IgnoreNotExist = true
	if err := WalkFiles(missing, func(string, fs.DirEntry) error { return nil }, opts); err != nil {
		t.Fatalf("WalkFiles with IgnoreNotExist error = %v, want nil", err)
	}
}

func TestWalkFilesReturnsCallbackNotExist(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "file"), nil, 0o644); err != nil {
		t.Fatal(err)
	}

	opts := DefaultWalkFilesOptions()
	opts.IgnoreNotExist = true
	if err := WalkFiles(root, func(string, fs.DirEntry) error { return fs.ErrNotExist }, opts); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("WalkFiles callback error = %v, want fs.ErrNotExist", err)
	}
}

func TestWalkFilesAppliesSkipDirPredicateToRoot(t *testing.T) {
	root := t.TempDir()
	var called, visited bool
	opts := DefaultWalkFilesOptions()
	opts.SkipDirPredicate = func(path string, _ fs.DirEntry) bool {
		called = true
		return path == root
	}

	if err := WalkFiles(root, func(string, fs.DirEntry) error {
		visited = true
		return nil
	}, opts); err != nil {
		t.Fatal(err)
	}
	if visited {
		t.Fatal("WalkFiles invoked callback after root skip predicate matched")
	}
	if !called {
		t.Fatal("WalkFiles did not apply the skip predicate to the root")
	}
}

func TestWalkFilesSymlinkHandling(t *testing.T) {
	tmpDir := t.TempDir()

	// Create a regular file
	if err := os.WriteFile(filepath.Join(tmpDir, "regular.txt"), []byte("content"), 0o644); err != nil {
		t.Fatalf("create regular: %v", err)
	}

	// Create a symlink (skip test if symlinks not supported)
	symlinkPath := filepath.Join(tmpDir, "link.txt")
	targetPath := filepath.Join(tmpDir, "regular.txt")
	if err := os.Symlink(targetPath, symlinkPath); err != nil {
		t.Skip("symlinks not supported on this system")
	}

	tests := []struct {
		name         string
		opts         WalkFilesOptions
		wantFiles    []string
		skipSymlinks bool
	}{
		{
			name: "skip symlinks (default)",
			opts: WalkFilesOptions{
				SkipHiddenDirs:     true,
				SkipSymlinkEntries: true,
				SkipDirs:           true,
			},
			wantFiles:    []string{"regular.txt"},
			skipSymlinks: true,
		},
		{
			name: "include symlinks",
			opts: WalkFilesOptions{
				SkipHiddenDirs:     true,
				SkipSymlinkEntries: false,
				SkipDirs:           true,
			},
			wantFiles:    []string{"link.txt", "regular.txt"},
			skipSymlinks: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var found []string
			err := WalkFiles(tmpDir, func(path string, entry fs.DirEntry) error {
				rel, _ := filepath.Rel(tmpDir, path)
				found = append(found, filepath.ToSlash(rel))
				return nil
			}, tt.opts)

			if err != nil {
				t.Errorf("WalkFiles error = %v", err)
			}

			sort.Strings(found)
			sort.Strings(tt.wantFiles)

			if len(found) != len(tt.wantFiles) {
				t.Errorf("WalkFiles found %d files, want %d", len(found), len(tt.wantFiles))
				t.Logf("found: %v", found)
				t.Logf("want: %v", tt.wantFiles)
				return
			}

			for i, f := range found {
				if f != tt.wantFiles[i] {
					t.Errorf("file[%d] = %q, want %q", i, f, tt.wantFiles[i])
				}
			}
		})
	}
}

func BenchmarkWalkFiles(b *testing.B) {
	tmpDir := b.TempDir()

	// Create a directory structure
	for i := 0; i < 10; i++ {
		subdir := filepath.Join(tmpDir, "subdir"+string(rune(i)))
		if err := os.Mkdir(subdir, 0o755); err != nil {
			b.Fatalf("create subdir: %v", err)
		}
		for j := 0; j < 10; j++ {
			filePath := filepath.Join(subdir, "file"+string(rune(j))+".txt")
			if err := os.WriteFile(filePath, []byte("content"), 0o644); err != nil {
				b.Fatalf("create file: %v", err)
			}
		}
	}

	opts := DefaultWalkFilesOptions()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		_ = WalkFiles(tmpDir, func(path string, entry fs.DirEntry) error {
			return nil
		}, opts)
	}
}
