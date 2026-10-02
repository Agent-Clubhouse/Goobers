package v20

import (
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

// TestConfigRepoFlagSwapsPolicyActionCapabilities pins TUT-A8: --config-repo
// makes push-branch/open-pr require configrepo:write and NOT the product-repo
// capabilities, while the unflagged commands keep their requirements.
func TestConfigRepoFlagSwapsPolicyActionCapabilities(t *testing.T) {
	missing := func(command []string, declared ...string) []string {
		task := apiv1.Task{Name: "t", Capabilities: declared, Run: &apiv1.DeterministicRun{Command: command}}
		var problems []string
		for _, action := range prescribedCommandPolicyActions(task) {
			problems = append(problems, missingPolicyActionCapabilities(task, action, policyActionContracts[action])...)
		}
		return problems
	}
	for command, product := range map[string]string{"push-branch": "repo:push", "open-pr": "provider:pr:write"} {
		if got := missing([]string{"goobers", command, "--config-repo"}, "configrepo:write"); len(got) != 0 {
			t.Errorf("%s --config-repo with configrepo:write: unexpected problems %v", command, got)
		}
		if got := missing([]string{"goobers", command, "--config-repo"}, product); len(got) == 0 {
			t.Errorf("%s --config-repo declaring only %s must be refused", command, product)
		}
		if got := missing([]string{"goobers", command}, product); len(got) != 0 {
			t.Errorf("%s with %s: unexpected problems %v", command, product, got)
		}
		if got := missing([]string{"goobers", command}, "configrepo:write"); len(got) == 0 {
			t.Errorf("%s without the flag must still require %s", command, product)
		}
	}
}
