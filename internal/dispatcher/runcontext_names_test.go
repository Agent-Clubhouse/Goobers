package dispatcher_test

import (
	"testing"

	"github.com/goobers/goobers/internal/dispatcher"
	"github.com/goobers/goobers/internal/executor"
)

// internal/dispatcher sits BENEATH internal/executor, so it restates the
// run-context variable names rather than importing them. This test lives in the
// _test package, which may import both, and pins the restatement against the
// originals — the one place the two can be compared without inverting the
// dependency.
//
// If this fails, a rename in executor has silently stopped the dispatcher from
// stripping that variable, and it will leak to stages that must not see it.
func TestRunContextEnvMatchesExecutor(t *testing.T) {
	for _, want := range []string{
		executor.RepoProviderEnvVar,
		executor.RepoOwnerEnvVar,
		executor.RepoProjectEnvVar,
		executor.RepoNameEnvVar,
		executor.BranchNamespaceEnvVar,
		executor.BaseBranchEnvVar,
		executor.TriggerRefEnvVar,
		executor.NeedsHumanAssigneeEnvVar,
	} {
		found := false
		for _, got := range dispatcher.DispatcherRunIdentityEnv {
			if got == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("executor variable %q is not in DispatcherRunIdentityEnv; it will leak to non-CLI stages", want)
		}
	}
}
