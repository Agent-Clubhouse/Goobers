//go:build !windows

package credentials

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestResolverResolvesFromOwnerOnlyFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "token")
	writeFile(t, path, "file-secret\n")
	resolver, err := NewResolver([]TokenRef{{Name: "gh", File: path}})
	if err != nil {
		t.Fatal(err)
	}
	got, err := resolver.Resolve(context.Background(), "gh")
	if err != nil {
		t.Fatal(err)
	}
	if got != "file-secret" {
		t.Fatalf("Resolve = %q, want file-secret", got)
	}
}

func TestResolverRejectsInsecureTokenFile(t *testing.T) {
	modes := []fs.FileMode{0o640, 0o604, 0o644, 0o660, 0o777}
	for _, mode := range modes {
		t.Run(fmt.Sprintf("%#o", mode), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "token")
			writeFile(t, path, "file-secret\n")
			if err := os.Chmod(path, mode); err != nil {
				t.Fatalf("chmod %q: %v", path, err)
			}
			resolver, err := NewResolver([]TokenRef{{Name: "gh", File: path}})
			if err != nil {
				t.Fatalf("NewResolver: %v", err)
			}
			_, err = resolver.Resolve(context.Background(), "gh")
			if err == nil {
				t.Fatal("Resolve: want error for insecure token file, got nil")
			}
			for _, want := range []string{path, fmt.Sprintf("mode %#o", mode)} {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("Resolve error = %q, want it to contain %q", err, want)
				}
			}
		})
	}
}
