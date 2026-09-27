package main

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/capability"
	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/internal/instance"
)

func TestPinnedGenerationKeepsMergeRevocationLive(t *testing.T) {
	for _, grant := range []capability.Capability{capability.GitHubPRMerge, capability.ADOPRComplete} {
		t.Run(string(grant), func(t *testing.T) {
			root := initDeterministicDemo(t)
			layout := instance.NewLayout(root)
			path := filepath.Join(layout.ConfigDir(), "gaggles", "example", "workflows", "default-implement.yaml")
			admitted := strings.Replace(deterministicWorkflowYAML, "      run:", "      capabilities: [\""+string(grant)+"\"]\n      run:", 1)
			writeFixture(t, path, admitted)
			env := apiv1.InvocationEnvelope{RunID: "pinned-run", TaskID: "pinned-run:local-ci", Gaggle: "example", WorkflowID: "default-implement", ConfigGeneration: "sha256:" + strings.Repeat("a", 64), Capabilities: []string{string(grant)}}
			calls := 0
			fence := withCurrentMergeAuthority(layout, func(ctx context.Context, _ apiv1.InvocationEnvelope) (context.Context, context.CancelFunc, error) {
				calls++
				return ctx, func() {}, nil
			})
			_, stop, err := fence(t.Context(), env)
			stop()
			if err != nil || calls != 1 {
				t.Fatalf("admitted merge refused: %v calls=%d", err, calls)
			}
			writeFixture(t, path, strings.ReplaceAll(admitted, "24h", "12h"))
			if err := requireCurrentMergeAuthority(layout, env); err != nil {
				t.Fatalf("unrelated edit revoked merge: %v", err)
			}
			writeFixture(t, path, deterministicWorkflowYAML)
			_, stop, err = fence(t.Context(), env)
			stop()
			if err == nil || calls != 1 {
				t.Fatalf("revoked merge reached executor: err=%v calls=%d", err, calls)
			}
			t.Setenv(executor.ConfigGenerationEnvVar, env.ConfigGeneration)
			t.Setenv(executor.RunIDEnvVar, env.RunID)
			t.Setenv(executor.InstanceRootEnvVar, root)
			t.Setenv(executor.GaggleEnvVar, env.Gaggle)
			t.Setenv(executor.WorkflowEnvVar, env.WorkflowID)
			t.Setenv(executor.TaskEnvVar, "local-ci")
			t.Setenv(executor.CredentialEnvVar(string(grant)), "already-injected-credential")
			if _, err := providerToken(grant); err == nil {
				t.Fatal("already-injected merge credential survived revocation")
			}
			env.Capabilities = []string{"repo:push"}
			if err := requireCurrentMergeAuthority(layout, env); err != nil {
				t.Fatalf("revocation invalidated ordinary pinned stage: %v", err)
			}
		})
	}
}
