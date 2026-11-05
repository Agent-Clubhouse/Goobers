//go:build integration && !windows

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/testgit"
	"github.com/goobers/goobers/test/testsupport/testdep"
)

func TestIntegrationReleaseBaselineDoesNotDirtyCheckout(t *testing.T) {
	testdep.Require(t, "bash", "git", "mktemp", "go")
	workflow := loadReleaseAuthorizationWorkflow(t)
	var script string
	for _, step := range workflow.Jobs["build"].Steps {
		if step.Name == "Resolve feature baseline" {
			script = step.Run
		}
	}
	if script == "" {
		t.Fatal("missing baseline resolver")
	}
	repo := t.TempDir()
	git := func(args ...string) string {
		t.Helper()
		cmd := testgit.Command(args...)
		cmd.Dir = repo
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	git("init", "--initial-branch=main")
	git("config", "user.name", "Publisher staging fixture")
	git("config", "user.email", "fixture@example.invalid")
	git("-c", "commit.gpgsign=false", "commit", "--allow-empty", "-m", "stable")
	git("tag", "v0.4.0")
	git("switch", "-c", "release/v0.5")
	git("commit", "--allow-empty", "-m", "release branch")
	git("tag", "v0.5.0")
	git("switch", "main")
	config, err := os.ReadFile(filepath.Join(moduleRoot(t), ".github", "release-baselines.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(repo, ".github"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, ".github", "release-baselines.json"), config, 0o644); err != nil {
		t.Fatal(err)
	}
	git("add", ".github/release-baselines.json")
	git("-c", "commit.gpgsign=false", "commit", "--allow-empty", "-m", "candidate")
	git("tag", "v0.6.0-alpha.4")
	runnerTemp := t.TempDir()
	outputs := filepath.Join(runnerTemp, "outputs")
	// Run the actual shared resolver from the fixture checkout. Build outside
	// that checkout so neither the tool nor Go's module files dirty it.
	notesTool := filepath.Join(t.TempDir(), "notes")
	build := exec.Command("go", "build", "-o", notesTool, "./release/notes")
	build.Dir = filepath.Join("..", "..")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build notes resolver: %v\n%s", err, out)
	}
	// Only the download is a declared double; execute the workflow's actual
	// resolver, Git queries and outputs. No GitHub credentials/API calls.
	stub := `go() {
  [[ "$1 $2" == 'run ./release/notes' ]] || return 40
  shift 2
  "$NOTES_TOOL" "$@"
}
gh() {
  [[ "$1 $2 $3" == 'release download v0.5.0' ]] || return 41
  while (( $# )); do
    if [[ "$1" == --dir ]]; then
      printf '{}\n' > "$2/feature-registry.json"
      printf '{}\n' > "$2/dsl-support-matrix.json"
      return
    fi
    shift
  done
  return 42
}
`
	cmd := exec.Command("bash", "--noprofile", "--norc", "-c", stub+script)
	cmd.Dir = repo
	cmd.Env = append(testgit.Environment(), "TAG=v0.6.0-alpha.4", "RUNNER_TEMP="+runnerTemp, "GITHUB_OUTPUT="+outputs, "NOTES_TOOL="+notesTool)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("actual baseline resolver: %v\n%s", err, out)
	}
	data, err := os.ReadFile(outputs)
	if err != nil {
		t.Fatal(err)
	}
	values := make(map[string]string)
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		key, value, _ := strings.Cut(line, "=")
		values[key] = value
	}
	if values["first"] != "false" {
		t.Fatalf("did not actually download the prior baseline: %s", data)
	}
	for _, key := range []string{"path", "support-path"} {
		rel, err := filepath.Rel(runnerTemp, values[key])
		if err != nil || !filepath.IsLocal(rel) {
			t.Fatalf("baseline path escaped runner temp: %q", values[key])
		}
		if data, err := os.ReadFile(values[key]); err != nil || string(data) != "{}\n" {
			t.Fatalf("baseline not present: %v", err)
		}
	}
	if status := git("status", "--porcelain=v1", "--untracked-files=all"); status != "" {
		t.Fatalf("baseline resolution dirtied checkout: %s", status)
	}
}
