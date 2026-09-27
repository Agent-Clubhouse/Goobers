package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/capability"
	"github.com/goobers/goobers/internal/decomposition"
	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/providers"
)

// writeTopologyBInput writes value as JSON into a fresh file and returns it.
func writeTopologyBInput(t *testing.T, name string, value any) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal %s: %v", name, err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	return path
}

func setupTopologyBValidatePlan(t *testing.T) {
	t.Setenv(executor.InputEnvVar("planFile"), writeTopologyBInput(t, "plan.json", map[string]any{}))
	t.Setenv(executor.InputEnvVar("selectionFile"), writeTopologyBInput(t, "selection.json", map[string]any{}))
}

func setupTopologyBPublishBatch(t *testing.T) {
	plan := validDecompositionPlan(decomposition.Selection{})
	digest, err := decomposition.PlanDigest(plan)
	if err != nil {
		t.Fatalf("digest plan: %v", err)
	}
	t.Setenv(executor.InputEnvVar("planFile"), writeTopologyBInput(t, "plan.json", plan))
	t.Setenv(executor.InputEnvVar("validationFile"), writeTopologyBInput(t, "plan-validation.json", validatePlanResult{Valid: true, PlanDigest: digest}))
}

// topologyBStageEnv makes the current stage a topology (b) stage: the demo
// instance's gaggle with ADO code and a GitHub backlog, routed to the ADO
// repository.
func topologyBStageEnv(t *testing.T) string {
	t.Helper()
	root := initDemo(t)
	topologyBFixture(t, root, "example")
	repo := topologyBRouted()
	t.Setenv(executor.RepoProviderEnvVar, string(repo.Provider))
	t.Setenv(executor.RepoOwnerEnvVar, repo.Owner)
	t.Setenv(executor.RepoProjectEnvVar, repo.Project)
	t.Setenv(executor.RepoNameEnvVar, repo.Name)
	t.Setenv("GOOBERS_GAGGLE", "example")
	return root
}

// A (b) stage never builds an Azure DevOps provider from a backlog-family
// credential: the seam refuses before any factory runs, whether the
// credential is named by its capability (the default github:issues:write
// included) or handed over as an explicit token equal to a delivered
// github:issues:* credential. Pull-request credentials, and every gaggle that
// is not (b), are unaffected.
func TestStageProviderRefusesBacklogCredentialOnTopologyBCode(t *testing.T) {
	root := topologyBStageEnv(t)
	t.Setenv(executor.CredentialEnvVar(string(capability.GitHubIssuesWrite)), "backlog-issues-token")
	t.Setenv(executor.CredentialEnvVar(string(capability.GitHubPRWrite)), "code-pr-token")

	previous := stageProviderFactories[providers.ProviderADO]
	t.Cleanup(func() { stageProviderFactories[providers.ProviderADO] = previous })
	var built []stageProviderConfig
	stageProviderFactories[providers.ProviderADO] = func(cfg stageProviderConfig) (providers.Provider, error) {
		built = append(built, cfg)
		return providers.NewADOProvider(cfg.repo.Owner, cfg.repo.Project, "token"), nil
	}

	code := topologyBRouted()
	for _, tc := range []struct {
		name string
		opts []stageProviderOption
	}{
		{"default issue capability", nil},
		{"github:issues:read", []stageProviderOption{withStageProviderCapability(capability.GitHubIssuesRead)}},
		{"github:milestones:write", []stageProviderOption{withStageProviderCapability(capability.GitHubMilestonesWrite)}},
		{"explicit issue token", []stageProviderOption{withStageProviderToken("backlog-issues-token"), withStageProviderCapability(capability.GitHubPRWrite)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			built = nil
			_, err := newProviderForStage(root, code, false, tc.opts...)
			if err == nil || !strings.Contains(err.Error(), "refusing to open Azure DevOps repository") {
				t.Fatalf("newProviderForStage error = %v, want the topology (b) refusal", err)
			}
			if len(built) != 0 {
				t.Fatalf("an ADO provider was built from a backlog credential: %+v", built)
			}
		})
	}

	for _, tc := range []struct {
		name string
		opts []stageProviderOption
	}{
		{"github:pr:write", []stageProviderOption{withStageProviderCapability(capability.GitHubPRWrite)}},
		{"explicit pull-request token", []stageProviderOption{withStageProviderToken("code-pr-token")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			built = nil
			if _, err := newProviderForStage(root, code, false, tc.opts...); err != nil {
				t.Fatalf("newProviderForStage: %v", err)
			}
			if len(built) != 1 {
				t.Fatalf("built %d ADO providers, want 1", len(built))
			}
		})
	}

	// The backlog provider itself is opened with the backlog credential.
	previousGitHub := stageProviderFactories[providers.ProviderGitHub]
	t.Cleanup(func() { stageProviderFactories[providers.ProviderGitHub] = previousGitHub })
	stageProviderFactories[providers.ProviderGitHub] = func(stageProviderConfig) (providers.Provider, error) {
		return providers.NewGitHubProvider("token"), nil
	}
	if _, err := newProviderForStage(root, topologyBBacklogRef(), false); err != nil {
		t.Fatalf("open the GitHub backlog with github:issues:write: %v", err)
	}

	// Outside a (b) gaggle the ADO provider keeps accepting github:issues:*,
	// the DSL 2.0 rebinding rule (§3.1).
	t.Setenv("GOOBERS_GAGGLE", "")
	built = nil
	if _, err := newProviderForStage(root, code, false); err != nil || len(built) != 1 {
		t.Fatalf("same-provider ADO stage: err = %v, built = %d; want one provider", err, len(built))
	}
}

// decompositionIssueRepo addresses the backlog repository in (b) and the
// routed repository everywhere else.
func TestDecompositionIssueRepoTopologyB(t *testing.T) {
	root := topologyBStageEnv(t)
	got, err := decompositionIssueRepo(root)
	if err != nil {
		t.Fatalf("decompositionIssueRepo: %v", err)
	}
	if got != topologyBBacklogRef() {
		t.Fatalf("decompositionIssueRepo = %+v, want the GitHub backlog %+v", got, topologyBBacklogRef())
	}

	t.Setenv("GOOBERS_GAGGLE", "")
	got, err = decompositionIssueRepo(root)
	if err != nil {
		t.Fatalf("decompositionIssueRepo without a gaggle: %v", err)
	}
	if got.Provider != providers.ProviderADO || got.Project != topologyBCodeProject || got.Name != topologyBCodeRepo {
		t.Fatalf("decompositionIssueRepo without a gaggle = %+v, want the routed ADO repository", got)
	}
}
