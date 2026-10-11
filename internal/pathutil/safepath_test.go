package pathutil_test

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/goobers/goobers/internal/pathutil"
	"github.com/goobers/goobers/internal/safepath"
)

type resolver struct {
	name string
	call func(string, string, bool) (string, error)
}

var resolvers = []resolver{
	{name: "pathutil", call: pathutil.ResolveRootedPath},
	{name: "safepath", call: safepath.Resolve},
}

func TestResolveRootedPathCompatibility(t *testing.T) {
	for _, resolver := range resolvers {
		t.Run(resolver.name, func(t *testing.T) {
			t.Run("lexical escape", func(t *testing.T) {
				root := t.TempDir()
				rel := filepath.Join("..", "outside")
				_, err := resolver.call(root, rel, true)
				want := fmt.Sprintf("path escapes root: %q", rel)
				if resolver.name == "safepath" {
					want = fmt.Sprintf("path %q escapes the root", rel)
				}
				assertError(t, err, want)
			})

			t.Run("absolute and volume paths", func(t *testing.T) {
				for _, rel := range []string{"/outside", `C:\outside`} {
					t.Run(rel, func(t *testing.T) {
						root := t.TempDir()
						resolvedRoot, err := filepath.EvalSymlinks(root)
						if err != nil {
							t.Fatalf("resolve root: %v", err)
						}
						full, err := resolver.call(root, rel, true)
						if runtime.GOOS == "windows" && filepath.VolumeName(rel) != "" {
							want := fmt.Sprintf("path escapes root: %q", rel)
							if resolver.name == "safepath" {
								want = fmt.Sprintf("path %q escapes the root", rel)
							}
							assertError(t, err, want)
							return
						}
						if err != nil {
							t.Fatalf("resolve rooted or volume-bound path: %v", err)
						}
						if want := filepath.Join(resolvedRoot, rel); full != want {
							t.Fatalf("resolved path = %q, want %q", full, want)
						}
					})
				}
			})

			t.Run("symlinked parent", func(t *testing.T) {
				root := t.TempDir()
				outside := t.TempDir()
				if err := os.Symlink(outside, filepath.Join(root, "link")); err != nil {
					t.Skipf("symlinks unavailable: %v", err)
				}
				rel := filepath.Join("link", "missing", "file")
				_, err := resolver.call(root, rel, true)
				assertError(t, err, fmt.Sprintf("path %q's directory escapes the root", rel))
				if _, err := os.Lstat(filepath.Join(outside, "missing")); !os.IsNotExist(err) {
					t.Fatalf("resolver created a directory outside the root: %v", err)
				}
			})

			t.Run("missing parent", func(t *testing.T) {
				rel := filepath.Join("missing", "nested", "file")
				root := t.TempDir()
				resolvedRoot, err := filepath.EvalSymlinks(root)
				if err != nil {
					t.Fatalf("resolve root: %v", err)
				}
				_, err = resolver.call(root, rel, false)
				assertError(t, err, fmt.Sprintf("path %q: no such directory", rel))

				full, err := resolver.call(root, rel, true)
				if err != nil {
					t.Fatalf("resolve while creating parents: %v", err)
				}
				if want := filepath.Join(resolvedRoot, rel); full != want {
					t.Fatalf("resolved path = %q, want %q", full, want)
				}
				info, err := os.Stat(filepath.Join(root, "missing"))
				if err != nil {
					t.Fatalf("stat created parent: %v", err)
				}
				if runtime.GOOS != "windows" && info.Mode().Perm() != 0o755 {
					t.Fatalf("created parent mode = %o, want 755", info.Mode().Perm())
				}
			})

			t.Run("symlink leaf", func(t *testing.T) {
				root := t.TempDir()
				target := filepath.Join(t.TempDir(), "target")
				if err := os.WriteFile(target, nil, 0o600); err != nil {
					t.Fatal(err)
				}
				rel := "file"
				if err := os.Symlink(target, filepath.Join(root, rel)); err != nil {
					t.Skipf("symlinks unavailable: %v", err)
				}
				_, err := resolver.call(root, rel, true)
				assertError(t, err, fmt.Sprintf("path %q is a symlink; refusing to read or write through it", rel))
			})
		})
	}
}

func TestResolveRootedPathEmptyPathCompatibility(t *testing.T) {
	root := t.TempDir()
	_, err := pathutil.ResolveRootedPath(root, "", true)
	assertError(t, err, "path escapes root: empty path")
	_, err = safepath.Resolve(root, "", true)
	assertError(t, err, "empty path")
}

func assertError(t *testing.T, err error, want string) {
	t.Helper()
	if err == nil || err.Error() != want {
		t.Fatalf("error = %v, want %q", err, want)
	}
}

func TestIsLexicallyContainedCases(t *testing.T) {
	for _, rel := range []string{"..", filepath.Join("..", "x"), "a/../..", "/abs", `\rooted`, "C:evil", ""} {
		if _, err := pathutil.IsLexicallyContained("", rel); err == nil {
			t.Errorf("IsLexicallyContained(%q) accepted escape", rel)
		}
	}
	for _, rel := range []string{"a", "a/b", "a/../b", "..foo"} {
		if _, err := pathutil.IsLexicallyContained("", rel); err != nil {
			t.Errorf("IsLexicallyContained(%q) = %v", rel, err)
		}
	}
}
