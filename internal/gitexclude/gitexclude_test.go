package gitexclude

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestAnchored(t *testing.T) {
	for _, tc := range []struct {
		name string
		rel  string
		want string
		ok   bool
	}{
		{name: "root file", rel: "claimed-item.json", want: "/claimed-item.json", ok: true},
		{name: "nested file", rel: "sub/dir/selection.json", want: "/sub/dir/selection.json", ok: true},
		{name: "dot prefix", rel: "./dedupe-candidates.json", want: "/dedupe-candidates.json", ok: true},
		{name: "glob metacharacters escaped", rel: "odd[1]*.json", want: `/odd\[1\]\*.json`, ok: true},
		{name: "empty", rel: ""},
		{name: "blank", rel: "   "},
		{name: "already anchored", rel: "/claimed-item.json"},
		{name: "escapes workspace", rel: "../outside.json"},
		{name: "escapes via cleanup", rel: "sub/../../outside.json"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := Anchored(tc.rel)
			if ok != tc.ok {
				t.Fatalf("Anchored(%q) ok = %t, want %t", tc.rel, ok, tc.ok)
			}
			if ok && got.Line != tc.want {
				t.Fatalf("Anchored(%q) = %q, want %q", tc.rel, got.Line, tc.want)
			}
		})
	}
}

// TestEnsureAppendsOnceAndPreservesContent is the append-once contract: an
// operator's own patterns survive, and a second pass adds nothing.
func TestEnsureAppendsOnceAndPreservesContent(t *testing.T) {
	ctx := context.Background()
	repo := initRepo(t)
	excludePath := filepath.Join(repo, ".git", "info", "exclude")
	// No trailing newline: the append must not corrupt the operator's line.
	writeFile(t, excludePath, "# operator's own\nbuild-output")

	patterns := []Pattern{{Line: "/mutations.jsonl"}, {Line: "/claimed-item.json"}}
	for pass := range 2 {
		if err := Ensure(ctx, repo, patterns...); err != nil {
			t.Fatalf("Ensure (pass %d): %v", pass, err)
		}
	}
	got := readLines(t, excludePath)
	want := []string{"# operator's own", "build-output", "/mutations.jsonl", "/claimed-item.json"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("exclude lines = %q, want %q", got, want)
	}
}

// TestEnsureHonoursAliases proves a legacy spelling already in the file
// satisfies its pattern, so a repository written by an older version is not
// given a second, equivalent line on every pass.
func TestEnsureHonoursAliases(t *testing.T) {
	repo := initRepo(t)
	excludePath := filepath.Join(repo, ".git", "info", "exclude")
	writeFile(t, excludePath, ".goobers\n")

	if err := Ensure(context.Background(), repo, Pattern{Line: ".goobers/", Aliases: []string{".goobers"}}); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if got := readLines(t, excludePath); len(got) != 1 || got[0] != ".goobers" {
		t.Fatalf("exclude lines = %q, want the legacy spelling alone", got)
	}
}

// TestEnsureNonRepository documents the signal a best-effort caller keys on: a
// directory git does not consider a repository fails rather than silently
// writing an exclude file nothing will read.
func TestEnsureNonRepository(t *testing.T) {
	dir := t.TempDir()
	if err := Ensure(context.Background(), dir, Pattern{Line: "/mutations.jsonl"}); err == nil {
		t.Fatal("Ensure in a non-repository returned nil")
	}
	if _, err := os.Stat(filepath.Join(dir, ".git")); !os.IsNotExist(err) {
		t.Fatalf("Ensure created git metadata in a non-repository: %v", err)
	}
}

// TestEnsureLinkedWorktreeSharesCommonExclude is the MEASURED fact the package
// doc rests on: `rev-parse --git-path info/exclude` in a linked worktree
// resolves to the mirror's COMMON exclude file, not a per-worktree one — which
// is why patterns are root-anchored, and which this asserts by showing a
// nested same-named path in the linked worktree stays visible.
func TestEnsureLinkedWorktreeSharesCommonExclude(t *testing.T) {
	ctx := context.Background()
	repo := initRepo(t)
	linked := filepath.Join(t.TempDir(), "linked")
	runGit(t, repo, "worktree", "add", "-q", linked, "-b", "linked")

	if err := Ensure(ctx, linked, Pattern{Line: "/claimed-item.json"}); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if _, err := os.Stat(filepath.Join(repo, ".git", "info", "exclude")); err != nil {
		t.Fatalf("linked-worktree Ensure did not write the common exclude file: %v", err)
	}
	writeFile(t, filepath.Join(linked, "claimed-item.json"), "{}\n")
	writeFile(t, filepath.Join(linked, "vendored", "claimed-item.json"), "{}\n")
	others := runGit(t, linked, "ls-files", "--others", "--exclude-standard")
	if strings.Contains(others, "\nclaimed-item.json") || strings.HasPrefix(others, "claimed-item.json") {
		t.Fatalf("root result file stayed visible: %q", others)
	}
	if !strings.Contains(others, "vendored/claimed-item.json") {
		t.Fatalf("root-anchored pattern swallowed a nested same-named path: %q", others)
	}
}

func initRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	runGit(t, dir, "init", "-q", "--initial-branch=main")
	runGit(t, dir, "config", "user.email", "test@example.invalid")
	runGit(t, dir, "config", "user.name", "test")
	writeFile(t, filepath.Join(dir, "seed.txt"), "seed\n")
	runGit(t, dir, "add", "seed.txt")
	runGit(t, dir, "commit", "-qm", "seed")
	return dir
}

func runGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v in %s: %v: %s", args, dir, err, out)
	}
	return string(out)
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func readLines(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var lines []string
	for _, line := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(line) != "" {
			lines = append(lines, line)
		}
	}
	return lines
}
