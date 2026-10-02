package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func commitAll(t *testing.T, root string) string {
	t.Helper()
	git(t, root, "add", ".")
	git(t, root, "-c", "user.name=Gate Test", "-c", "user.email=gate@example.invalid", "-c", "commit.gpgsign=false", "commit", "--allow-empty", "-m", "change")
	return git(t, root, "rev-parse", "HEAD")
}

func TestPullRequestGrowthDeclarations(t *testing.T) {
	for _, scenario := range []string{"unchanged", "decrease", "lines", "files", "justified", "empty", "old", "modified", "symlink", "snapshot-only"} {
		t.Run(scenario, func(t *testing.T) {
			root := fixture(t)
			declaration := justificationDir + "issue-123.md"
			if scenario == "old" || scenario == "modified" {
				put(t, root, declaration, "Previous growth rationale.\n")
				commitAll(t, root)
			}
			base := git(t, root, "rev-parse", "HEAD")
			switch scenario {
			case "unchanged":
			case "decrease":
				if err := os.Remove(filepath.Join(root, "cmd/goobers/b.go")); err != nil {
					t.Fatal(err)
				}
			case "files":
				put(t, root, "cmd/goobers/c.go", "")
			default:
				put(t, root, "cmd/goobers/a.go", "package main\n\n// a\n// added adapter\n")
			}
			switch scenario {
			case "justified", "modified":
				put(t, root, declaration, "This change adds required CLI adapter wiring.\n")
			case "empty":
				put(t, root, declaration, " \n")
			case "snapshot-only":
				put(t, root, baselinePath, "!non-test-line-count 7\n!non-test-file-count 2\n!non-test-line-count-justification\t7\told scheme\n")
			case "symlink":
				if err := os.MkdirAll(filepath.Dir(filepath.Join(root, declaration)), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink("../../baseline.txt", filepath.Join(root, declaration)); err != nil {
					t.Skip(err)
				}
			}
			head := commitAll(t, root)
			code, output := invoke(root, "-base-ref", base, "-head-ref", head)
			wantFailure := scenario != "unchanged" && scenario != "decrease" && scenario != "justified"
			if (code != 0) != wantFailure {
				t.Fatalf("status %d: %s", code, output)
			}
			if wantFailure && !strings.Contains(output, "unjustified cmd/goobers growth") {
				t.Fatal(output)
			}
		})
	}
}

func TestConcurrentMainGrowthDoesNotCountAgainstPullRequest(t *testing.T) {
	root := fixture(t)
	base := git(t, root, "rev-parse", "HEAD")
	git(t, root, "checkout", "-b", "proposal")
	put(t, root, "notes.txt", "No command growth.\n")
	head := commitAll(t, root)
	git(t, root, "checkout", "-b", "concurrent-main", base)
	put(t, root, "cmd/goobers/concurrent.go", "package main\n// Unrelated merged adapter.\n")
	main := commitAll(t, root)
	// Even the checked-out synthetic merge includes unrelated growth. The gate
	// must count immutable head objects, rather than this working tree.
	git(t, root, "-c", "user.name=Gate Test", "-c", "user.email=gate@example.invalid", "-c", "commit.gpgsign=false", "merge", "--no-edit", head)
	if code, output := invoke(root, "-base-ref", main, "-head-ref", head); code != 0 || !strings.Contains(output, "PR non-test-file-count +0") {
		t.Fatalf("unrelated growth: %d %s", code, output)
	}
	if code, output := invoke(root); code != 0 {
		t.Fatalf("main report failed: %s", output)
	}
	git(t, root, "checkout", "proposal")
	put(t, root, "cmd/goobers/proposal.go", "package main\n")
	head = commitAll(t, root)
	if code, output := invoke(root, "-base-ref", main, "-head-ref", head); code == 0 {
		t.Fatalf("own growth passed: %s", output)
	}
	put(t, root, justificationDir+"proposal.md", "Required adapter for proposal.\n")
	head = commitAll(t, root)
	if code, output := invoke(root, "-base-ref", main, "-head-ref", head); code != 0 {
		t.Fatalf("own declaration failed: %s", output)
	}
	// No total target needs updating when main grows again.
	git(t, root, "checkout", "concurrent-main")
	put(t, root, "cmd/goobers/another.go", "package main\n")
	main = commitAll(t, root)
	if code, output := invoke(root, "-base-ref", main, "-head-ref", head); code != 0 {
		t.Fatalf("concurrent merge invalidated declaration: %s", output)
	}
}

func TestPullRequestRevisionValidation(t *testing.T) {
	root := fixture(t)
	for _, args := range [][]string{{"-base-ref", "missing", "-head-ref", "HEAD"}, {"-base-ref", "", "-head-ref", "HEAD"}, {"-head-ref", "missing"}, {"-head-ref", "--all"}} {
		if code, output := invoke(root, args...); code == 0 {
			t.Fatalf("accepted %v: %s", args, output)
		}
	}
}
