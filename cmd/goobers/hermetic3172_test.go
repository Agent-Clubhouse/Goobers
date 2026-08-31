package main

import (
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/testgit"
)

// TestGitAutoMaintenanceDisabledForSuite is #3172's guard: TestMain must leave
// every git subprocess this suite spawns — the fixtures AND the runner code
// under test, which shells out to git through production paths that inherit
// this process's environment — with automatic background housekeeping off. A
// detached `git gc --auto` triggered by a fixture push outlives the test that
// triggered it and mutates `origin.git/objects/pack` while t.TempDir's
// RemoveAll walks it, failing cleanup with "directory not empty". It shells out
// to git exactly as those callers do, so it verifies the GIT_CONFIG_* env
// actually reaches a child process rather than only that the vars are set. If
// git is unavailable the rest of the suite can't run either, so a missing
// binary is a hard failure, not a skip.
func TestGitAutoMaintenanceDisabledForSuite(t *testing.T) {
	for _, tc := range []struct{ key, want string }{
		{"gc.auto", "0"},
		{"gc.autoDetach", "false"},
		{"maintenance.auto", "false"},
	} {
		out, err := testgit.AmbientCommand("config", "--get", tc.key).CombinedOutput()
		if err != nil {
			t.Fatalf("git config --get %s: %v\n%s", tc.key, err, out)
		}
		if got := strings.TrimSpace(string(out)); got != tc.want {
			t.Errorf("%s = %q, want %q — the #3172 auto-maintenance seam "+
				"(disableGitAutoMaintenanceForTests) is not in effect; a detached auto gc "+
				"can race t.TempDir cleanup on a fixture's objects/pack again", tc.key, got, tc.want)
		}
	}
}
