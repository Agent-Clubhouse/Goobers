package safeio

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/goobers/goobers/internal/platform/safeopen"
)

func TestReadRegularInRoot(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "small"), []byte("content"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "at-limit"), []byte("12345678"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "over-limit"), []byte("123456789"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "directory"), 0o700); err != nil {
		t.Fatal(err)
	}

	t.Run("successful read", func(t *testing.T) {
		data, err := ReadRegularInRoot(dir, "small", 8)
		if err != nil {
			t.Fatal(err)
		}
		if string(data) != "content" {
			t.Fatalf("data = %q, want content", data)
		}
	})
	t.Run("file at limit", func(t *testing.T) {
		data, err := ReadRegularInRoot(dir, "at-limit", 8)
		if err != nil {
			t.Fatal(err)
		}
		if string(data) != "12345678" {
			t.Fatalf("data = %q, want 12345678", data)
		}
	})
	t.Run("file over limit", func(t *testing.T) {
		if _, err := ReadRegularInRoot(dir, "over-limit", 8); !errors.Is(err, ErrLimitExceeded) {
			t.Fatalf("error = %v, want ErrLimitExceeded", err)
		}
	})
	t.Run("non-existent file", func(t *testing.T) {
		if _, err := ReadRegularInRoot(dir, "missing", 8); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("error = %v, want os.ErrNotExist", err)
		}
	})
	t.Run("directory", func(t *testing.T) {
		if _, err := ReadRegularInRoot(dir, "directory", 8); !errors.Is(err, safeopen.ErrNotRegular) {
			t.Fatalf("error = %v, want safeopen.ErrNotRegular", err)
		}
	})
}

func TestReadRegularFileGrowthAfterStat(t *testing.T) {
	path := filepath.Join(t.TempDir(), "growing")
	if err := os.WriteFile(path, []byte("12345678"), 0o600); err != nil {
		t.Fatal(err)
	}
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("123456789"), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := readRegular(file, info, 8); !errors.Is(err, ErrLimitExceededDuringRead) {
		t.Fatalf("error = %v, want ErrLimitExceededDuringRead", err)
	}
}
