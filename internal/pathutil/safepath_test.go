package pathutil_test

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
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

func TestValidateSymlinkEscape(t *testing.T) {
	root := filepath.Join(string(filepath.Separator), "r", "root")
	tests := []struct {
		name     string
		resolved string
		wantErr  bool
	}{
		{"root itself", root, false},
		{"inside", filepath.Join(root, "a", "b"), false},
		{"sibling", filepath.Join(root, "..", "other"), true},
		{"parent", filepath.Dir(root), true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := pathutil.ValidateSymlinkEscape(root, tt.resolved, "orig")
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if err != nil && !strings.Contains(err.Error(), "symlink escape") {
				t.Fatalf("unexpected message: %v", err)
			}
		})
	}
	if err := pathutil.ValidateSymlinkEscape("rel", filepath.Join(string(filepath.Separator), "abs"), "o"); err == nil {
		t.Fatal("expected error when Rel fails")
	}
}

func TestResolveWithSymlinks(t *testing.T) {
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(base, "root")
	outside := filepath.Join(base, "outside")
	for _, d := range []string{filepath.Join(root, "sub"), outside} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, f := range []string{filepath.Join(root, "sub", "f.txt"), filepath.Join(outside, "secret.txt")} {
		if err := os.WriteFile(f, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	link := func(target, name string) {
		if err := os.Symlink(target, filepath.Join(root, name)); err != nil {
			t.Skipf("symlinks unsupported: %v", err)
		}
	}
	link(filepath.Join(root, "sub"), "inlink")
	link(outside, "outlink")
	link("loop2", "loop1")
	link("loop1", "loop2")

	tests := []struct {
		name    string
		rel     string
		want    string
		wantErr string
	}{
		{"plain", "sub/f.txt", filepath.Join(root, "sub", "f.txt"), ""},
		{"in-root symlink", "inlink/f.txt", filepath.Join(root, "sub", "f.txt"), ""},
		{"escaping symlink", "outlink/secret.txt", "", "symlink escape"},
		{"looping symlink", "loop1", "", "too many links"},
		{"missing", "nope.txt", "", "no such file"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := pathutil.ResolveWithSymlinks(root, filepath.Join(root, tt.rel), tt.rel)
			if tt.wantErr == "" {
				if err != nil || got != tt.want {
					t.Fatalf("got (%q, %v), want %q", got, err, tt.want)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) || got != "" {
				t.Fatalf("got (%q, %v), want error containing %q", got, err, tt.wantErr)
			}
		})
	}

	if _, err := pathutil.ResolveWithSymlinks(filepath.Join(base, "missing-root"), filepath.Join(root, "sub"), "sub"); err == nil || !strings.Contains(err.Error(), "resolve root") {
		t.Fatalf("expected resolve root error, got %v", err)
	}
}
