//go:build integration

package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/gaggletemplate"
	"github.com/goobers/goobers/internal/testgit"
	"github.com/goobers/goobers/test/testsupport/testdep"
)

func TestIntegrationTrackedTemplateCommittedSourceAndAncestry(t *testing.T) {
	testdep.Require(t, "git")
	root, _, tree := templateTestSetup(t)
	repo := t.TempDir()
	git := func(args ...string) string {
		t.Helper()
		cmd := testgit.Command(append([]string{"-C", repo}, args...)...)
		cmd.Env = append(cmd.Env, "GIT_AUTHOR_NAME=Template Test", "GIT_AUTHOR_EMAIL=template@example.invalid",
			"GIT_COMMITTER_NAME=Template Test", "GIT_COMMITTER_EMAIL=template@example.invalid")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git: %v: %s", err, out)
		}
		return strings.TrimSpace(string(out))
	}
	git("init", "-b", "main")
	packageRoot := filepath.Join(repo, "templates", "example")
	if err := tree.Write(packageRoot); err != nil {
		t.Fatal(err)
	}
	git("add", ".")
	git("commit", "-m", "initial template")
	source := gaggletemplate.Source{SchemaVersion: 1, Repository: repo, Ref: "main", Directory: "templates/example", Gaggle: "orders"}
	first, revision, err := resolveGaggleTemplate(context.Background(), root, source, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(packageRoot, "gaggle.yaml"), []byte("uncommitted invalid definition"), 0644); err != nil {
		t.Fatal(err)
	}
	committed, unchanged, err := resolveGaggleTemplate(context.Background(), root, source, revision)
	if err != nil || unchanged != revision || committed.Digest() != first.Digest() {
		t.Fatalf("read dirty template checkout instead of accepted commit: %s %v", unchanged, err)
	}
	if err := tree.Write(packageRoot); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "unrelated.txt"), []byte("unrelated change"), 0644); err != nil {
		t.Fatal(err)
	}
	git("add", ".")
	git("commit", "-m", "unrelated")
	second, newer, err := resolveGaggleTemplate(context.Background(), root, source, revision)
	if err != nil || newer == revision || first.Digest() != second.Digest() {
		t.Fatalf("unrelated source commit changed template: %s %v", newer, err)
	}
	// A new orphan branch makes the old accepted commit non-ancestral.
	git("checkout", "--orphan", "replacement")
	git("commit", "-m", "rewritten template history")
	git("branch", "-f", "main", "HEAD")
	if _, _, err := resolveGaggleTemplate(context.Background(), root, source, revision); err == nil ||
		!strings.Contains(err.Error(), "diverged") {
		t.Fatalf("rewritten source accepted: %v", err)
	}
}
