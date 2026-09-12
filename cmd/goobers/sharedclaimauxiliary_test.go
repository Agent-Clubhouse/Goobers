package main

import (
	"context"
	"testing"

	"github.com/goobers/goobers/internal/claimsclient"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/sharedclaim"
	"github.com/goobers/goobers/providers"
)

func TestPinnedClaimPolicyAllowsNamespacedTargetLease(t *testing.T) {
	for _, mode := range []string{"local", "shared"} {
		t.Run(mode, func(t *testing.T) {
			layout, run := newPinnedClaimResolverRun(t, mode)
			resolver := pinnedSharedClaimResolver{layout: layout, store: func(context.Context, providers.RepositoryRef) (sharedclaim.Store, error) {
				return &pinnedClaimTestStore{}, nil
			}}
			key := claimsclient.Key{Gaggle: "example", Provider: "github", ExternalID: decompositionTargetLeaseExternalID("42")}
			binding, err := resolver.Admission(t.Context(), key, "shared-run", decompositionTargetLeaseWorkflow)
			if err != nil {
				t.Fatal(err)
			}
			if mode == "local" {
				if binding != nil {
					t.Fatal("local auxiliary reservation constructed a remote binding")
				}
				return
			}
			ordinaryKey := key
			ordinaryKey.ExternalID = "42"
			ordinary, err := resolver.Admission(t.Context(), ordinaryKey, "shared-run", "claim")
			if err != nil || binding.RemoteKey == ordinary.RemoteKey || binding.Owner == ordinary.Owner {
				t.Fatalf("target reservation must have distinct key and ownership: %v", err)
			}
			entry := claimsclient.Entry{Gaggle: key.Gaggle, Provider: key.Provider, ExternalID: key.ExternalID, RunID: "shared-run", Workflow: decompositionTargetLeaseWorkflow, SharedOwner: binding.Owner}
			if err := run.Append(journal.Event{Type: journal.EventRunFinished, Status: string(journal.PhaseCompleted)}); err != nil {
				t.Fatal(err)
			}
			if _, err := resolver.Admission(t.Context(), key, "shared-run", decompositionTargetLeaseWorkflow); err == nil {
				t.Fatal("terminal run acquired an auxiliary shared lease")
			}
			if _, err := resolver.Release(t.Context(), entry); err != nil {
				t.Fatalf("terminal owning auxiliary release: %v", err)
			}
			entry.ExternalID = "42"
			if _, err := resolver.Release(t.Context(), entry); err == nil {
				t.Fatal("auxiliary label authorized release of an ordinary item")
			}
		})
	}
}

func TestPinnedClaimPolicyRejectsForgedAuxiliaryIdentity(t *testing.T) {
	layout, _ := newPinnedClaimResolverRun(t, "local")
	resolver := pinnedSharedClaimResolver{layout: layout}
	for _, tc := range []struct{ id, run, workflow, gaggle string }{
		{"42", "shared-run", decompositionTargetLeaseWorkflow, "example"},
		{decompositionTargetLeaseExternalID(""), "shared-run", decompositionTargetLeaseWorkflow, "example"},
		{decompositionTargetLeaseExternalID("42"), "missing-run", decompositionTargetLeaseWorkflow, "example"},
		{decompositionTargetLeaseExternalID("42"), "shared-run", "unrelated-workflow", "example"},
		{decompositionTargetLeaseExternalID("42"), "shared-run", decompositionTargetLeaseWorkflow, "another-gaggle"},
	} {
		key := claimsclient.Key{Gaggle: tc.gaggle, Provider: "github", ExternalID: tc.id}
		if _, err := resolver.Admission(t.Context(), key, tc.run, tc.workflow); err == nil {
			t.Fatalf("unverified auxiliary claim accepted: %+v", tc)
		}
	}
}
