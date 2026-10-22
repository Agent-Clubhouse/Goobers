//go:build integration

package main

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/testgit"
	"github.com/goobers/goobers/test/testsupport/testdep"
)

func TestIntegrationCIRetargetReleaseBase(t *testing.T) {
	testdep.Require(t, "bash", "git")
	script := loadCIWorkflow(t).Jobs["scope"].step(t, "Verify retarget dispatch inputs").Run
	repository := t.TempDir()
	// Some Windows Git builds reject NUL as a config path.
	config := filepath.Join(repository, "absent-gitconfig")
	env := append(testgit.Environment(), "GIT_CONFIG_GLOBAL="+config, "GIT_CONFIG_SYSTEM="+config)
	git := func(args ...string) string {
		t.Helper()
		cmd := testgit.Command(args...)
		cmd.Dir = repository
		cmd.Env = env
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	git("init", "--initial-branch=main")
	git("config", "user.name", "CI retarget test")
	git("config", "user.email", "ci-test@example.invalid")
	git("-c", "commit.gpgsign=false", "commit", "--allow-empty", "-m", "main")
	main := git("rev-parse", "HEAD")
	git("update-ref", "refs/remotes/origin/main", main)
	// Release-only commits need not be reachable from main.
	git("checkout", "--orphan", "release-fixture")
	git("-c", "commit.gpgsign=false", "commit", "--allow-empty", "-m", "release base")
	base := git("rev-parse", "HEAD")
	git("-c", "commit.gpgsign=false", "commit", "--allow-empty", "-m", "PR head")
	head := git("rev-parse", "HEAD")
	merge := git("-c", "commit.gpgsign=false", "commit-tree", "HEAD^{tree}", "-p", base, "-p", head, "-m", "PR merge")
	git("checkout", "--detach", merge)
	for _, tc := range []struct {
		branch  string
		allowed bool
	}{
		{"main", true},
		{"release/v0.5", true},
		{"release/maintenance/v0.5", true},
		{"feature/backport", false},
		{"release-impostor/v0.5", false},
		{"release", false},
	} {
		t.Run(tc.branch, func(t *testing.T) {
			ref := "refs/remotes/origin/" + tc.branch
			git("update-ref", ref, base)
			cmd := exec.Command("bash", "--noprofile", "--norc", "-c", script)
			cmd.Dir = repository
			cmd.Env = append(env, "PR_NUMBER=1", "BASE_SHA="+base,
				"HEAD_SHA="+head, "MERGE_SHA="+merge, "GITHUB_SHA="+head)
			out, err := cmd.CombinedOutput()
			if (err == nil) != tc.allowed {
				t.Errorf("retarget allowed=%v, want %v: %v\n%s", err == nil, tc.allowed, err, out)
			}
			git("update-ref", "-d", ref)
			git("update-ref", "refs/remotes/origin/main", main)
		})
	}
}
