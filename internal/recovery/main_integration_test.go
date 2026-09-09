//go:build integration

package recovery

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/goobers/goobers/test/testsupport/testdep"
)

func TestIntegrationFetchMainIgnoresStaleTrackingBranch(t *testing.T) {
	testdep.Require(t, "git")
	source, destination := t.TempDir(), t.TempDir()
	recoveryTestGit(t, source, "init", "--initial-branch=main")
	recoveryTestGit(t, source, "commit", "--allow-empty", "-m", "old main")
	old := recoveryTestGit(t, source, "rev-parse", "HEAD")
	recoveryTestGit(t, destination, "clone", source, ".")
	recoveryTestGit(t, source, "commit", "--allow-empty", "-m", "new main")
	want := recoveryTestGit(t, source, "rev-parse", "HEAD")
	fetchHead := filepath.Join(destination, ".git", "FETCH_HEAD")
	if err := os.WriteFile(fetchHead, []byte("existing fetch evidence"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := FetchCurrentMain(context.Background(), destination, source, nil)
	if err != nil || got != want || got == old {
		t.Fatalf("fetch reused stale main: %s %v", got, err)
	}
	for _, ref := range []string{"HEAD", "refs/remotes/origin/main"} {
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
