package main

import (
	"testing"

	"github.com/goobers/goobers/internal/claimsclient"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/providers"
)

// TestLedgerOnlyClaimKeysAreNotWorkItems pins every synthetic claim key this
// binary mints to a namespace localscheduler.IsWorkItemClaimKey knows. The
// status/portal claim projection resolves work-item keys through the
// provider's work-item API; a new synthetic key outside those namespaces
// would be looked up as a work item again (#6798).
func TestLedgerOnlyClaimKeysAreNotWorkItems(t *testing.T) {
	repo := providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "acme", Name: "app"}
	for name, key := range map[string]string{
		"pr claim":            pullRequestClaimKey(536),
		"advisory pr claim":   advisoryClaimKey(repo, "security", 536).ExternalID,
		"decomposition lease": decompositionTargetLeaseExternalID("3344"),
		"merge lock":          claimsclient.MergeLockKey("goobers", "github", "acme", "app").ExternalID,
	} {
		if localscheduler.IsWorkItemClaimKey(key) {
			t.Errorf("%s key %q is classified as a work item", name, key)
		}
	}
	if !localscheduler.IsWorkItemClaimKey("3344") {
		t.Error("a bare work-item id must stay a work-item claim key")
	}
}
