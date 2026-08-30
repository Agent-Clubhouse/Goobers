package mcpio

import (
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
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
				FollowSymlinks:     false,
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

func TestWalkFilesIgnoresNotExistDuringMutation(t *testing.T) {
	tmpDir := t.TempDir()
	keepDir := filepath.Join(tmpDir, "a")
	if err := os.Mkdir(keepDir, 0o755); err != nil {
		t.Fatalf("create keep dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(keepDir, "keep.txt"), []byte("ok"), 0o644); err != nil {
		t.Fatalf("create keep file: %v", err)
	}
	if err := os.WriteFile(filepath.Join(keepDir, "zeta.txt"), []byte("later"), 0o644); err != nil {
		t.Fatalf("create later file: %v", err)
	}
	deadDir := filepath.Join(tmpDir, "b")
	if err := os.Mkdir(deadDir, 0o755); err != nil {
		t.Fatalf("create dead dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(deadDir, "gone.txt"), []byte("gone"), 0o644); err != nil {
		t.Fatalf("create dead file: %v", err)
	}

	var seen []string
	err := WalkFiles(tmpDir, func(path string, entry fs.DirEntry) error {
		if path == filepath.Join(keepDir, "keep.txt") {
			if err := os.RemoveAll(deadDir); err != nil {
				t.Fatalf("remove dead dir during walk: %v", err)
			}
			return fs.ErrNotExist
		}
		rel, _ := filepath.Rel(tmpDir, path)
		seen = append(seen, filepath.ToSlash(rel))
		return nil
	}, DefaultWalkFilesOptions())
	if err != nil {
		t.Fatalf("WalkFiles returned unexpected error while a sibling dir disappeared: %v", err)
	}
	if !contains(seen, filepath.ToSlash(filepath.Join("a", "zeta.txt"))) {
		t.Fatalf("WalkFiles stopped after a skipped ErrNotExist: %v", seen)
	}
	if contains(seen, filepath.ToSlash(filepath.Join("b", "gone.txt"))) {
		t.Fatalf("WalkFiles visited a file in the removed directory: %v", seen)
	}
}

func TestWalkYAMLFiles(t *testing.T) {
	tmpDir := t.TempDir()

	// Create test files
	files := []string{
		"config.yaml",
		"manifest.yml",
		"data.json",
		"readme.txt",
		"subdir/workflow.yaml",
	}

	for _, f := range files {
		dir := filepath.Dir(f)
		if dir != "." {
			fullDir := filepath.Join(tmpDir, dir)
			if err := os.MkdirAll(fullDir, 0o755); err != nil {
				t.Fatalf("create dir: %v", err)
			}
		}
		if err := os.WriteFile(filepath.Join(tmpDir, f), []byte("content"), 0o644); err != nil {
			t.Fatalf("create file: %v", err)
		}
	}

	var found []string
	err := WalkYAMLFiles(tmpDir, func(path string, entry fs.DirEntry) error {
		rel, _ := filepath.Rel(tmpDir, path)
		found = append(found, filepath.ToSlash(rel))
		return nil
	})

	if err != nil {
		t.Errorf("WalkYAMLFiles error = %v", err)
	}

	sort.Strings(found)
	want := []string{"config.yaml", "manifest.yml", "subdir/workflow.yaml"}
	sort.Strings(want)

	if len(found) != len(want) {
		t.Errorf("WalkYAMLFiles found %d files, want %d", len(found), len(want))
		t.Logf("found: %v", found)
		t.Logf("want: %v", want)
		return
	}

	for i, f := range found {
		if f != want[i] {
			t.Errorf("file[%d] = %q, want %q", i, f, want[i])
		}
	}
}

func TestSumFileSizes(t *testing.T) {
	tmpDir := t.TempDir()

	// Create test files with known sizes
	files := map[string]string{
		"file1.txt":        "12345",      // 5 bytes
		"file2.txt":        "abcdefgh",   // 8 bytes
		"subdir/file3.txt": "xyz",        // 3 bytes
		"subdir/file4.txt": "1234567890", // 10 bytes
	}

	var expectedSize int64
	for path, content := range files {
		fullPath := filepath.Join(tmpDir, path)
		dir := filepath.Dir(fullPath)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("create dir: %v", err)
		}
		if err := os.WriteFile(fullPath, []byte(content), 0o644); err != nil {
			t.Fatalf("create file: %v", err)
		}
		expectedSize += int64(len(content))
	}

	size, err := SumFileSizes(tmpDir, DefaultWalkFilesOptions())
	if err != nil {
		t.Errorf("SumFileSizes error = %v", err)
	}

	if size != expectedSize {
		t.Errorf("SumFileSizes = %d, want %d", size, expectedSize)
	}
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
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
				FollowSymlinks:     false,
				SkipSymlinkEntries: true,
				SkipDirs:           true,
			},
			wantFiles:    []string{"regular.txt"},
			skipSymlinks: true,
		},
		{
			// When SkipSymlinkEntries=false, symlinked files are included
			// (on most systems, they report as regular via IsRegular())
			name: "include symlinks",
			opts: WalkFilesOptions{
				SkipHiddenDirs:     true,
				FollowSymlinks:     false,
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

func TestCollectFiles(t *testing.T) {
	tmpDir := t.TempDir()

	// Create test files
	files := []string{
		"file1.yaml",
		"file2.json",
		"file3.yaml",
		"subdir/file4.yaml",
	}

	for _, f := range files {
		fullPath := filepath.Join(tmpDir, f)
		dir := filepath.Dir(fullPath)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("create dir: %v", err)
		}
		if err := os.WriteFile(fullPath, []byte("content"), 0o644); err != nil {
			t.Fatalf("create file: %v", err)
		}
	}

	// Collect only YAML files
	collected, err := CollectFiles(tmpDir, func(path string, entry fs.DirEntry) bool {
		return strings.HasSuffix(entry.Name(), ".yaml")
	}, DefaultWalkFilesOptions())

	if err != nil {
		t.Errorf("CollectFiles error = %v", err)
	}

	sort.Strings(collected)
	want := []string{
		filepath.Join(tmpDir, "file1.yaml"),
		filepath.Join(tmpDir, "file3.yaml"),
		filepath.Join(tmpDir, "subdir/file4.yaml"),
	}
	sort.Strings(want)

	if len(collected) != len(want) {
		t.Errorf("CollectFiles found %d files, want %d", len(collected), len(want))
		return
	}

	for i, f := range collected {
		if f != want[i] {
			t.Errorf("file[%d] = %q, want %q", i, f, want[i])
		}
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
