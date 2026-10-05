package providers

import (
	"context"
	"testing"
)

// TestADOClaimReceiptsCarryCanonicalWorkItemURL pins #6796: the claim and
// claim-release attempt receipts must carry the same navigable work-item URL as
// every other ADO receipt for the item. Without it the read model records the
// claim rows under an empty repository, and Cost then splits one work item into
// a qualified and an unqualified aggregate.
func TestADOClaimReceiptsCarryCanonicalWorkItemURL(t *testing.T) {
	fake := &adoClaimFake{tags: "goobers:approved"}
	server := fake.server(t, true)
	recorder := &adoMutationRecorder{}
	provider := NewADOProvider("org", "project", "token", func(p *ADOProvider) { p.BaseURL = server.URL })
	provider.SetMutationRecorder(recorder)
	req := ClaimWorkItemRequest{
		Repository: RepositoryRef{Provider: ProviderADO, Name: "repo", Project: "project"},
		ID:         "42",
		RunID:      "run-42",
	}
	if _, err := provider.ClaimWorkItem(context.Background(), req); err != nil {
		t.Fatalf("ClaimWorkItem: %v", err)
	}
	if _, err := provider.ReleaseWorkItemClaim(context.Background(), req); err != nil {
		t.Fatalf("ReleaseWorkItemClaim: %v", err)
	}

	want := server.URL + "/org/project/_workitems/edit/42"
	seen := map[string]bool{}
	for _, ref := range recorder.refs {
		if ref.Operation != "claim" && ref.Operation != "claim-release" {
			continue
		}
		seen[ref.Operation] = true
		if ref.URL != want {
			t.Errorf("%s receipt URL = %q, want %q (receipt %+v)", ref.Operation, ref.URL, want, ref)
		}
	}
	if !seen["claim"] || !seen["claim-release"] {
		t.Fatalf("recorded receipts %+v, want both claim and claim-release", recorder.refs)
	}
}
