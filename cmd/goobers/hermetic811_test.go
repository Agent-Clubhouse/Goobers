package main

import (
	"os/exec"
	"strings"
	"testing"
)

// TestGitFsyncDisabledForSuite is #811's guard: TestMain must leave every git
// subprocess this suite spawns with fsync disabled, so a fixture/worktree git
// can never wedge on an uninterruptible fsync under the disk saturation of
// several concurrent `make ci` runs (the hang that opened 0 PRs overnight). It
// shells out to git exactly as the fixtures and runner do, so it verifies the
// GIT_CONFIG_* env actually reaches a child process — not just that the vars are
// set. If git is unavailable the rest of the suite can't run either, so a
// missing binary is a hard failure, not a skip.
func TestGitFsyncDisabledForSuite(t *testing.T) {
	out, err := exec.Command("git", "config", "--get", "core.fsync").CombinedOutput()
	if err != nil {
		t.Fatalf("git config --get core.fsync: %v\n%s", err, out)
	}
	if got := strings.TrimSpace(string(out)); got != "none" {
		t.Fatalf("core.fsync = %q, want \"none\" — the #811 fsync-disable seam (disableGitFsyncForTests) is not in effect", got)
	}
}
