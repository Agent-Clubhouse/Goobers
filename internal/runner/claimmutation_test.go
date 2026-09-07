package runner

import (
	"context"
	"path/filepath"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/gate"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/journal"
)

func TestClaimFailureSidecarProjectsStructuredError(t *testing.T) {
	r, runsDir := newTestRunnerWithDeterministic(t, func(ArtifactRecorder, SecretRegistrar) (invoke.Deterministic, error) {
		return mutationSidecarDeterministic{fact: `{"provider":"github","kind":"issue","id":"7","operation":"claim-verification","runId":"lease-owner","providerRunId":"provider-owner","outcome":"conflict","errorCode":"provider_ledger_ownership_mismatch"}`}, nil
	}, gate.NewAutomatedEvaluator())
	_, err := r.Start(context.Background(), StartInput{
		RunID: "reconcile", Machine: fixtureMachine(t), Gaggle: "acme-web",
		Trigger: journal.Trigger{Kind: journal.TriggerManual},
		RepoRef: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web", Branch: "main"},
	})
	if err != nil {
		t.Fatal(err)
	}
	rd, err := journal.OpenRead(filepath.Join(runsDir, "reconcile"))
	if err != nil {
		t.Fatal(err)
	}
	events, err := rd.Events()
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, event := range events {
		if event.ExternalRef == nil || event.ExternalRef.Kind != "issue" || event.ExternalRef.ID != "7" {
			continue
		}
		found = true
		if event.Type != journal.EventError || event.Error == nil || event.Error.Code != "provider_ledger_ownership_mismatch" || event.Runner["claimRunId"] != "lease-owner" || event.Runner["providerRunId"] != "provider-owner" || event.Runner["outcome"] != "conflict" {
			t.Fatalf("claim failure misprojected: %+v", event)
		}
	}
	if !found {
		t.Fatal("claim failure absent from actual run journal")
	}
}
