//go:build integration

package recovery

import (
	"context"
	"strings"
	"testing"

	"github.com/goobers/goobers/test/testsupport/testdep"
)

func TestIntegrationRecoveryGitSupportsExplicitForeignOwnedWorkspace(t *testing.T) {
	testdep.Require(t, "git")
	repository := t.TempDir()
	recoveryTestGit(t, repository, "init", "--initial-branch=main")
	recoveryTestGit(t, repository, "commit", "--allow-empty", "-m", "base")
	want := recoveryTestGit(t, repository, "rev-parse", "HEAD")
	var out boundedRefOutput
	// Git's ownership-test switch exercises the same trust check as a pod
	// mounting a host-owned checkout, without privileged chown in the test.
	err := recoveryGitWithEnv(context.Background(), repository, &out,
		[]string{"GIT_TEST_ASSUME_DIFFERENT_OWNER=1"}, "rev-parse", "--verify", "HEAD")
	if err != nil || strings.TrimSpace(out.String()) != want {
		t.Fatalf("explicit recovery workspace refused: %q %v", out.String(), err)
	}
	foreign := t.TempDir()
	recoveryTestGit(t, foreign, "init", "--initial-branch=main")
	recoveryTestGit(t, foreign, "commit", "--allow-empty", "-m", "foreign")
	out.Reset()
	// A second target must not inherit a wildcard ownership bypass. These
	// extra -C arguments are test-only; production operations use their bound
	// repository argument and never accept user-supplied Git option vectors.
	err = recoveryGitWithEnv(context.Background(), repository, &out,
		[]string{"GIT_TEST_ASSUME_DIFFERENT_OWNER=1"}, "-C", foreign, "rev-parse", "--verify", "HEAD")
	if err == nil || out.Len() != 0 {
		t.Fatalf("ownership exception leaked to another repository: %q %v", out.String(), err)
	}
}
