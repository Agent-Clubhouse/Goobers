//go:build integration

package recovery

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/goobers/goobers/test/testsupport/testdep"
)

func TestIntegrationFetchCurrentBaseIgnoresStaleTrackingBranch(t *testing.T) {
	testdep.Require(t, "git")
	for _, branch := range []string{"main", "master", "release/2026.09"} {
		t.Run(strings.ReplaceAll(branch, "/", "_"), func(t *testing.T) {
			testFetchCurrentBase(t, branch)
		})
	}
}

func testFetchCurrentBase(t *testing.T, branch string) {
	source, destination := t.TempDir(), t.TempDir()
	recoveryTestGit(t, source, "init", "--initial-branch="+branch)
	recoveryTestGit(t, source, "commit", "--allow-empty", "-m", "old base")
	old := recoveryTestGit(t, source, "rev-parse", "HEAD")
	recoveryTestGit(t, destination, "clone", source, ".")
	recoveryTestGit(t, source, "commit", "--allow-empty", "-m", "new base")
	want := recoveryTestGit(t, source, "rev-parse", "HEAD")
	fetchHead := filepath.Join(destination, ".git", "FETCH_HEAD")
	if err := os.WriteFile(fetchHead, []byte("existing fetch evidence"), 0o600); err != nil {
		t.Fatal(err)
	}
	baseRef := "refs/heads/" + branch
	got, err := FetchCurrentBase(context.Background(), destination, source, baseRef, nil)
	if err != nil || got != want || got == old {
		t.Fatalf("fetch reused stale base: %s %v", got, err)
	}
	for _, ref := range []string{"HEAD", "refs/remotes/origin/" + branch} {
		if got := recoveryTestGit(t, destination, "rev-parse", ref); got != old {
			t.Fatalf("fetch changed %s: %s", ref, got)
		}
	}
	if data, err := os.ReadFile(fetchHead); err != nil || string(data) != "existing fetch evidence" {
		t.Fatalf("fetch changed FETCH_HEAD: %q %v", data, err)
	}
	if refs := recoveryTestGit(t, destination, "for-each-ref", "--format=%(refname)", "refs/goobers/recovery-fetch/"); refs != "" {
		t.Fatalf("temporary fetch ref leaked: %s", refs)
	}
}

func TestIntegrationFetchCurrentBaseReportsAttemptedRef(t *testing.T) {
	testdep.Require(t, "git")
	repository, source := t.TempDir(), t.TempDir()
	recoveryTestGit(t, repository, "init", "--initial-branch=main")
	recoveryTestGit(t, repository, "commit", "--allow-empty", "-m", "destination")
	recoveryTestGit(t, source, "init", "--initial-branch=main")
	recoveryTestGit(t, source, "commit", "--allow-empty", "-m", "source")
	const missing = "refs/heads/missing-base"
	_, err := FetchCurrentBase(context.Background(), repository, source, missing, nil)
	if err == nil || !strings.Contains(err.Error(), missing) {
		t.Fatalf("missing base diagnostic = %v; want attempted ref %q", err, missing)
	}
}
