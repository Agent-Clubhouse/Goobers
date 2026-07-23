package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/goobers/goobers/providers"
)

func TestUnifiedHunkScopeParsesStandardAndCombinedHeaders(t *testing.T) {
	tests := map[string]string{
		"@@ -10,4 +10,7 @@ func runStatus() {":         "func runStatus() {",
		"@@@ -10,4 -12,5 +10,8 @@@ func runStatus() {": "func runStatus() {",
	}
	for header, want := range tests {
		if got, ok := unifiedHunkScope(header); !ok || got != want {
			t.Errorf("unifiedHunkScope(%q) = %q, %v; want %q, true", header, got, ok, want)
		}
	}
}

func TestMatchStructuralCollisionsRejectsMinorSameFunctionEdit(t *testing.T) {
	conflicts := []rebaseConflictLocation{{Path: "status.go", Scope: "func runStatus() {"}}
	current := []providers.ChangedFile{{
		Path:  "status.go",
		Patch: "@@ -2,3 +2,3 @@ func runStatus() {\n-\told()\n+\tcurrent()\n }",
	}}
	minorSibling := []providers.ChangedFile{{
		Path:  "status.go",
		Patch: "@@ -2,3 +2,3 @@ func runStatus() {\n-\told()\n+\tsibling()\n }",
	}}

	if got := matchStructuralCollisions(conflicts, current, 609, minorSibling); len(got) != 0 {
		t.Fatalf("minor same-function edit matched as structural: %+v", got)
	}
}

func TestRenderStructuralCollisionContextPairsPRHunks(t *testing.T) {
	context := renderStructuralCollisionContext(696, []structuralCollision{{
		SiblingNumber: 609,
		Path:          "cmd/goobers/status.go",
		Function:      "func runStatus() {",
		CurrentHunk:   "@@ current @@\n+warnings()",
		SiblingHunk:   "@@ sibling @@\n+renderFrame()",
	}})
	for _, want := range []string{
		"PR #696 relevant hunk", "@@ current @@",
		"Merged sibling PR #609 relevant hunk", "@@ sibling @@",
	} {
		if !strings.Contains(context, want) {
			t.Fatalf("context = %q, want %q", context, want)
		}
	}
}

func TestMatchStructuralCollisionPrefersFunctionHunkForRename(t *testing.T) {
	conflicts := []rebaseConflictLocation{{Path: "old.go", Scope: "func runStatus() {"}}
	current := []providers.ChangedFile{{
		Path:  "old.go",
		Patch: "@@ -2,3 +2,3 @@ func runStatus() {\n-\told()\n+\tcurrent()\n }",
	}}
	sibling := []providers.ChangedFile{{
		Path: "new.go", PreviousPath: "old.go", Status: "renamed",
		Patch: "@@ -2,10 +2,4 @@ func runStatus() {\n-\ta()\n-\tb()\n-\tc()\n-\td()\n-\te()\n-\tf()\n-\tg()\n-\th()\n+\tframe := buildFrame()\n+\trender(frame)\n }",
	}}

	got := matchStructuralCollisions(conflicts, current, 609, sibling)
	if len(got) != 1 {
		t.Fatalf("collisions = %+v, want one", got)
	}
	if !strings.Contains(got[0].SiblingHunk, "buildFrame") || strings.Contains(got[0].SiblingHunk, "rename from") {
		t.Fatalf("sibling hunk = %q, want the function hunk rather than rename-only metadata", got[0].SiblingHunk)
	}
}

func TestMatchStructuralCollisionAggregatesSplitFunctionRewrite(t *testing.T) {
	conflicts := []rebaseConflictLocation{{Path: "status.go", Scope: "func runStatus() {"}}
	current := []providers.ChangedFile{{
		Path:  "status.go",
		Patch: "@@ -2,3 +2,3 @@ func runStatus() {\n-\told()\n+\tcurrent()\n }",
	}}
	sibling := []providers.ChangedFile{{
		Path: "status.go",
		Patch: "@@ -2,5 +2,2 @@ func runStatus() {\n-\ta()\n-\tb()\n-\tc()\n-\td()\n+\tpartOne()\n" +
			"@@ -12,5 +9,2 @@ func runStatus() {\n-\te()\n-\tf()\n-\tg()\n-\th()\n+\tpartTwo()\n",
	}}

	got := matchStructuralCollisions(conflicts, current, 609, sibling)
	if len(got) != 1 {
		t.Fatalf("collisions = %+v, want aggregate structural match", got)
	}
	for _, want := range []string{"partOne", "partTwo"} {
		if !strings.Contains(got[0].SiblingHunk, want) {
			t.Fatalf("sibling hunk = %q, want all matching function hunks including %q", got[0].SiblingHunk, want)
		}
	}
}

func TestFunctionStructurallyChangedPathsFollowsRename(t *testing.T) {
	dir := t.TempDir()
	runGitT(t, dir, "init", "-b", "main")
	runGitT(t, dir, "config", "user.name", "test")
	runGitT(t, dir, "config", "user.email", "test@example.com")
	oldSource := "package status\n\nfunc runStatus() {\n\ta()\n\tb()\n\tc()\n\td()\n\te()\n\tf()\n\tg()\n\th()\n}\n"
	if err := os.WriteFile(filepath.Join(dir, "old.go"), []byte(oldSource), 0o644); err != nil {
		t.Fatalf("write old.go: %v", err)
	}
	runGitT(t, dir, "add", "old.go")
	runGitT(t, dir, "commit", "-m", "old function")
	oldRevision := strings.TrimSpace(runGitOutputT(t, dir, "rev-parse", "HEAD"))

	if err := os.Rename(filepath.Join(dir, "old.go"), filepath.Join(dir, "new.go")); err != nil {
		t.Fatalf("rename function file: %v", err)
	}
	newSource := "package status\n\nfunc runStatus() {\n\tframe := buildFrame()\n\trender(frame)\n}\n"
	if err := os.WriteFile(filepath.Join(dir, "new.go"), []byte(newSource), 0o644); err != nil {
		t.Fatalf("write new.go: %v", err)
	}
	runGitT(t, dir, "add", "-A")
	runGitT(t, dir, "commit", "-m", "rename and restructure function")
	newRevision := strings.TrimSpace(runGitOutputT(t, dir, "rev-parse", "HEAD"))

	changed, err := functionStructurallyChangedPaths(dir, oldRevision, newRevision, "old.go", "new.go", "runStatus")
	if err != nil {
		t.Fatalf("functionStructurallyChangedPaths: %v", err)
	}
	if !changed {
		t.Fatal("renamed and substantially shortened function was not structural")
	}
}

func TestFunctionStructurallyChangedPathsDetectsSixLineCompleteRewrite(t *testing.T) {
	dir := t.TempDir()
	runGitT(t, dir, "init", "-b", "main")
	runGitT(t, dir, "config", "user.name", "test")
	runGitT(t, dir, "config", "user.email", "test@example.com")
	oldSource := "package status\n\nfunc runStatus() {\n\ta()\n\tb()\n\tc()\n\td()\n\te()\n\tf()\n}\n"
	if err := os.WriteFile(filepath.Join(dir, "status.go"), []byte(oldSource), 0o644); err != nil {
		t.Fatalf("write old status.go: %v", err)
	}
	runGitT(t, dir, "add", "status.go")
	runGitT(t, dir, "commit", "-m", "old function")
	oldRevision := strings.TrimSpace(runGitOutputT(t, dir, "rev-parse", "HEAD"))

	newSource := "package status\n\nfunc runStatus() {\n\tone()\n\ttwo()\n\tthree()\n\tfour()\n\tfive()\n\tsix()\n}\n"
	if err := os.WriteFile(filepath.Join(dir, "status.go"), []byte(newSource), 0o644); err != nil {
		t.Fatalf("write rewritten status.go: %v", err)
	}
	runGitT(t, dir, "commit", "-am", "rewrite function")
	newRevision := strings.TrimSpace(runGitOutputT(t, dir, "rev-parse", "HEAD"))

	changed, err := functionStructurallyChangedPaths(dir, oldRevision, newRevision, "status.go", "status.go", "runStatus")
	if err != nil {
		t.Fatalf("functionStructurallyChangedPaths: %v", err)
	}
	if !changed {
		t.Fatal("complete rewrite of a six-line function was not structural")
	}
}

func TestCommitIntroducedBetweenUsesFailedRebaseBase(t *testing.T) {
	baseSHA, _, laterBaseSHA := initStructuralCollisionCheckpointRepo(t, "goobers/impl/remediation-364")

	introduced, err := commitIntroducedBetween(".", laterBaseSHA, baseSHA, baseSHA)
	if err != nil {
		t.Fatalf("commitIntroducedBetween at failed-rebase base: %v", err)
	}
	if introduced {
		t.Fatal("a sibling merged after the failed rebase was attributed to that earlier conflict")
	}
	introduced, err = commitIntroducedBetween(".", laterBaseSHA, baseSHA, laterBaseSHA)
	if err != nil {
		t.Fatalf("commitIntroducedBetween at later base: %v", err)
	}
	if !introduced {
		t.Fatal("sibling merge present in the failed-rebase base delta was not detected")
	}
}
