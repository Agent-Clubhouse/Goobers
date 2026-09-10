package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/goobers/goobers/internal/claimsclient"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
)

func TestLegacyClaimFallbackRefusesPartialPinsAndSharedConfiguration(t *testing.T) {
	for _, mode := range []string{"legacy", "digest-only", "input-only", "shared-config"} {
		t.Run(mode, func(t *testing.T) {
			layout := instance.NewLayout(initDemo(t))
			identity := journal.RunIdentity{RunID: "legacy", Workflow: "old-local-workflow", Gaggle: "example"}
			var inputs map[string][]byte
			if mode == "digest-only" {
				identity.WorkflowDigest = "sha256:partial-pin"
			}
			if mode == "input-only" {
				inputs = map[string][]byte{journal.PinnedWorkflowDefinitionInputName: []byte("invalid pin must not become local")}
			}
			run, err := journal.Create(layout.RunsDir(), identity, inputs)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = run.Close() })
			if mode == "shared-config" {
				path := filepath.Join(layout.ConfigDir(), "gaggles", "example", "workflows", "shared-policy.yaml")
				data := []byte("apiVersion: goobers.dev/v1alpha1\nkind: Workflow\ndslVersion: '2.0'\nmetadata:\n  name: shared-policy\nspec:\n  gaggle: example\n  triggers:\n    - type: manual\n  readiness:\n    claimVisibility: shared\n  start: check\n  tasks:\n    - name: check\n      type: deterministic\n      goal: Verify repository\n      run:\n        command: ['true']\n")
				if err := os.WriteFile(path, data, 0o600); err != nil {
					t.Fatal(err)
				}
				if _, report, err := instance.LoadConfigDir(layout.ConfigDir()); err != nil {
					t.Fatalf("shared fixture must be valid, not rejected because of a parse error: %v (%s)", err, validationIssueSummary(report))
				}
			}
			resolver := stageSharedClaimResolver(layout)
			binding, err := resolver.Admission(t.Context(), claimsclient.Key{Gaggle: "example", Provider: "github", ExternalID: "42"}, "legacy", "old-local-workflow")
			if mode == "legacy" {
				if err != nil || binding != nil {
					t.Fatalf("legacy local claim: %+v %v", binding, err)
				}
			} else if err == nil {
				t.Fatal("unverified shared or partially pinned run fell back to local admission")
			}
		})
	}
}
