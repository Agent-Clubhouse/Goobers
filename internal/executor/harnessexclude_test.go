package executor

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/testgit"
)

// TestEffectiveResultFile covers both shapes ExcludeStageArtifacts has to
// register: the explicitly declared resultFile, and the implicit registry
// default a `goobers` provider subcommand carries without declaring anything.
func TestEffectiveResultFile(t *testing.T) {
	for _, tc := range []struct {
		name         string
		inputs       map[string]any
		command      []string
		want         string
		wantImplicit string
	}{
		{
			name:    "declared",
			inputs:  map[string]any{InputResultFile: "selection.json"},
			command: []string{"goobers", "backlog-query"},
			want:    "selection.json",
		},
		{
			name:         "implicit provider default",
			command:      []string{"goobers", "backlog-query"},
			want:         "claimed-item.json",
			wantImplicit: "claimed-item.json",
		},
		{name: "not the goobers CLI", command: []string{"make", "ci"}},
		{name: "goobers subcommand without a default", command: []string{"goobers", "status"}},
		{name: "bare goobers", command: []string{"goobers"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, implicit := effectiveResultFile(apiv1.InvocationEnvelope{Inputs: tc.inputs}, tc.command)
			if got != tc.want || implicit != tc.wantImplicit {
				t.Fatalf("effectiveResultFile = (%q, %q), want (%q, %q)", got, implicit, tc.want, tc.wantImplicit)
			}
		})
	}
}

// TestExcludeStageArtifactsRegistersAndRepeats is the regression for #5119:
// the stage's own result file and mutation sidecar become invisible to git,
// root-anchored, and a second stage in the same workspace adds no duplicate.
func TestExcludeStageArtifactsRegistersAndRepeats(t *testing.T) {
	ctx := context.Background()
	workspace := initExcludeTestRepo(t)
	excludePath := filepath.Join(workspace, ".git", "info", "exclude")
	if err := os.WriteFile(excludePath, []byte("# pre-existing\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	for pass := range 2 {
		ExcludeStageArtifacts(ctx, workspace, "claimed-item.json")
		data, err := os.ReadFile(excludePath)
		if err != nil {
			t.Fatalf("read exclude (pass %d): %v", pass, err)
		}
		content := string(data)
		if !strings.Contains(content, "# pre-existing") {
			t.Fatalf("pass %d dropped existing exclude content: %q", pass, content)
		}
		for _, want := range []string{"/claimed-item.json", "/mutations.jsonl"} {
			if strings.Count(content, want+"\n") != 1 {
				t.Fatalf("pass %d: %q appears %d times in %q, want once", pass, want, strings.Count(content, want+"\n"), content)
			}
		}
	}

	writeExcludeTestFile(t, filepath.Join(workspace, "claimed-item.json"), "{}\n")
	writeExcludeTestFile(t, filepath.Join(workspace, "mutations.jsonl"), "{}\n")
	writeExcludeTestFile(t, filepath.Join(workspace, "nested", "claimed-item.json"), "{}\n")
	writeExcludeTestFile(t, filepath.Join(workspace, "agent-work.txt"), "real work\n")

	others := runExcludeTestGit(t, workspace, "ls-files", "--others", "--exclude-standard")
	visible := strings.Fields(others)
	want := []string{"agent-work.txt", "nested/claimed-item.json"}
	if strings.Join(visible, " ") != strings.Join(want, " ") {
		t.Fatalf("untracked files = %q, want %q (root-anchored exclusion only)", visible, want)
	}
}

// TestExcludeStageArtifactsTrackedFileSurvives is the safety constraint: an
// exclude never applies to a tracked path, so a repository that legitimately
// commits a file of one of these names keeps its change visible and capturable.
func TestExcludeStageArtifactsTrackedFileSurvives(t *testing.T) {
	workspace := initExcludeTestRepo(t)
	writeExcludeTestFile(t, filepath.Join(workspace, "claimed-item.json"), "{\"committed\":true}\n")
	runExcludeTestGit(t, workspace, "add", "claimed-item.json")
	runExcludeTestGit(t, workspace, "commit", "-qm", "track the result-file name")

	ExcludeStageArtifacts(context.Background(), workspace, "claimed-item.json")
	writeExcludeTestFile(t, filepath.Join(workspace, "claimed-item.json"), "{\"committed\":false}\n")

	if status := runExcludeTestGit(t, workspace, "status", "--porcelain"); !strings.Contains(status, "claimed-item.json") {
		t.Fatalf("tracked result-file change disappeared from status: %q", status)
	}
	if diff := runExcludeTestGit(t, workspace, "diff", "--name-only"); !strings.Contains(diff, "claimed-item.json") {
		t.Fatalf("tracked result-file change disappeared from diff: %q", diff)
	}
}

// TestExcludeStageArtifactsSkipsNonRepository proves a scratch workspace — and
// an empty one — is left exactly as it was, with no error reaching the stage.
func TestExcludeStageArtifactsSkipsNonRepository(t *testing.T) {
	scratch := t.TempDir()
	ExcludeStageArtifacts(context.Background(), scratch, "claimed-item.json")
	ExcludeStageArtifacts(context.Background(), "", "claimed-item.json")
	entries, err := os.ReadDir(scratch)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("scratch workspace gained %d entries", len(entries))
	}
}

func initExcludeTestRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	runExcludeTestGit(t, dir, "init", "-q", "--initial-branch=main")
	runExcludeTestGit(t, dir, "config", "user.email", "test@example.invalid")
	runExcludeTestGit(t, dir, "config", "user.name", "test")
	writeExcludeTestFile(t, filepath.Join(dir, "seed.txt"), "seed\n")
	runExcludeTestGit(t, dir, "add", "seed.txt")
	runExcludeTestGit(t, dir, "commit", "-qm", "seed")
	return dir
}

func runExcludeTestGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := testgit.Command(args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v in %s: %v: %s", args, dir, err, out)
	}
	return string(out)
}

func writeExcludeTestFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}
