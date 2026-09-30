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
	"github.com/goobers/goobers/providers"
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

// TestADOLandingAuthorityKeepsRevocationLive pins that both landing names stay
// in the config-generation fence on Azure DevOps (docs/design/ado-parity-dsl-2-0.md
// §3.3): a pinned stage whose github:pr:merge or ado:pr:complete is revoked
// cannot land with its already-injected credential, and a revoked
// ado:pr:complete fails closed instead of falling back to github:pr:merge.
func TestADOLandingAuthorityKeepsRevocationLive(t *testing.T) {
	ado := providers.RepositoryRef{Provider: providers.ProviderADO, Owner: "example-org", Project: "example-project", Name: "example-repo"}
	merge, complete := capability.GitHubPRMerge, capability.ADOPRComplete
	for _, tc := range []struct {
		name      string
		declared  []capability.Capability
		revokedTo []capability.Capability
		want      capability.Capability
	}{
		{name: "github-pr-merge", declared: []capability.Capability{merge}, want: merge},
		{name: "ado-pr-complete", declared: []capability.Capability{complete}, want: complete},
		{name: "ado-pr-complete-revoked-no-fallback", declared: []capability.Capability{merge, complete}, revokedTo: []capability.Capability{merge}, want: complete},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := initDeterministicDemo(t)
			layout := instance.NewLayout(root)
			path := filepath.Join(layout.ConfigDir(), "gaggles", "example", "workflows", "default-implement.yaml")
			writeFixture(t, path, workflowYAMLDeclaring(tc.declared))
			t.Setenv(executor.ConfigGenerationEnvVar, "sha256:"+strings.Repeat("a", 64))
			t.Setenv(executor.RunIDEnvVar, "pinned-run")
			t.Setenv(executor.InstanceRootEnvVar, root)
			t.Setenv(executor.GaggleEnvVar, "example")
			t.Setenv(executor.WorkflowEnvVar, "default-implement")
			t.Setenv(executor.TaskEnvVar, "local-ci")
			for _, grant := range tc.declared {
				t.Setenv(executor.CredentialEnvVar(string(grant)), "already-injected-credential")
			}
			got, err := landingAuthority(ado)
			if err != nil || got != tc.want {
				t.Fatalf("admitted landing authority = %q, %v; want %q", got, err, tc.want)
			}
			writeFixture(t, path, workflowYAMLDeclaring(tc.revokedTo))
			got, err = landingAuthority(ado)
			if err == nil {
				t.Fatalf("revoked %s still authorized an ADO land as %q", tc.want, got)
			}
			// A declared ado:pr:complete never falls back, so its refusal
			// must not suggest github:pr:merge would do.
			wantMsg := "needs " + string(merge) + " (or " + string(complete) + ")"
			if tc.want == complete {
				wantMsg = "with the declared " + string(complete) + " (no fallback to " + string(merge) + ")"
			}
			if !strings.Contains(err.Error(), wantMsg) {
				t.Fatalf("revocation error = %q, want it to say %q", err, wantMsg)
			}
		})
	}
}

// workflowYAMLDeclaring is the deterministic demo workflow with its local-ci
// stage declaring caps (none leaves the fixture unchanged).
func workflowYAMLDeclaring(caps []capability.Capability) string {
	if len(caps) == 0 {
		return deterministicWorkflowYAML
	}
	quoted := make([]string, 0, len(caps))
	for _, c := range caps {
		quoted = append(quoted, `"`+string(c)+`"`)
	}
	return strings.Replace(deterministicWorkflowYAML, "      run:", "      capabilities: ["+strings.Join(quoted, ", ")+"]\n      run:", 1)
}
