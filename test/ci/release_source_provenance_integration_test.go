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
	testdep.Require(t, "bash", "git", "mktemp", "grep", "head")
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
	git("tag", "v0.4.5")
	git("-c", "commit.gpgsign=false", "commit", "--allow-empty", "-m", "candidate")
	git("tag", "v0.5.0-beta.6")
	runnerTemp := t.TempDir()
	outputs := filepath.Join(runnerTemp, "outputs")
	// Only the download is a declared double; execute the workflow's actual
	// path resolution, Git queries and outputs. No GitHub credentials/API calls.
	stub := `gh() {
  [[ "$1 $2 $3" == 'release download v0.4.5' ]] || return 41
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
	cmd.Env = append(testgit.Environment(), "TAG=v0.5.0-beta.6", "RUNNER_TEMP="+runnerTemp, "GITHUB_OUTPUT="+outputs)
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
