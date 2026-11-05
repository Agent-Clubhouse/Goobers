package harness

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// No .gitignore or local exclusions are installed: the postcondition must
// never ask the agent to commit runtime files, or preserve them in a patch.
func TestCommitPostconditionExcludesEveryReservedArtifact(t *testing.T) {
	for _, tracked := range []bool{false, true} {
		name := "untracked"
		if tracked {
			name = "already tracked"
		}
		t.Run(name, func(t *testing.T) {
			workspace := commitPostconditionRepo(t)
			artifacts := []string{
				".goobers/mcp/home/logs/debug.log", ".goober-assets/skill.md",
				"mutations.jsonl", ".goobers-launcher-session-123/native.jsonl",
			}
			write := func(path, data string) {
				t.Helper()
				full := filepath.Join(workspace, path)
				if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(full, []byte(data), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if tracked {
				for _, path := range artifacts {
					write(path, "old runtime data\n")
				}
				postconditionTestGit(t, workspace, "add", "-A")
				postconditionTestGit(t, workspace, "commit", "-qm", "legacy artifacts")
			}
			check := observeCommitPostcondition(context.Background(), ModeInvoke, modifyRepositoryEnvelope(workspace))
			for _, path := range artifacts {
				write(path, "private runtime marker\n")
			}
			if tracked {
				// A real index populated by the agent must also be excluded.
				postconditionTestGit(t, workspace, "add", "-A")
			}
			before := postconditionTestGit(t, workspace, "diff", "--cached", "--binary")
			payload := []byte(`{"status":"success"}`)
			if status, dirty := check.violatedBy(context.Background(), payload); dirty {
				t.Fatalf("runtime-only changes requested a commit: %s", status)
			}
			patch, err := check.captureDiff(context.Background())
			if err != nil || len(patch) != 0 {
				t.Fatalf("runtime-only patch = %q, err = %v", patch, err)
			}

			// Root scoping must preserve legitimate same-named project files.
			write("project/.goobers/debug.log", "project source marker\n")
			write("main.go", "package main\n// project change\n")
			if _, dirty := check.violatedBy(context.Background(), payload); !dirty {
				t.Fatal("project changes did not request a commit")
			}
			patch, err = check.captureDiff(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(patch), "private runtime marker") ||
				!strings.Contains(string(patch), "project source marker") ||
				!strings.Contains(string(patch), "project change") {
				t.Fatalf("patch did not isolate project changes: %s", patch)
			}
			if after := postconditionTestGit(t, workspace, "diff", "--cached", "--binary"); after != before {
				t.Fatal("capture changed the agent's real index")
			}
		})
	}
}
