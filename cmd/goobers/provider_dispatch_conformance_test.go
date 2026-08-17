package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/decomposition"
	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/internal/providerstage"
	"github.com/goobers/goobers/providers"
)

type providerDispatchEvidence struct {
	test func(*testing.T)
}

// CONF-8 (#2497) and CONF-9 (#2498) landed before this gate, so their fixed
// commands are coverage entries rather than stale allowlist entries.
var providerDispatchCoverage = map[string]providerDispatchEvidence{
	"apply-verdict":            {test: TestRunApplyVerdictADOPassPublishesStatusAndDecisionPass},
	"backlog-assignment":       {test: TestADOBacklogStagesDispatchFromCommands},
	"backlog-dedupe":           {test: TestBacklogDedupeCommandDispatchesToADO},
	"backlog-health":           {test: TestBacklogHealthCommandRunsWithADO},
	"backlog-query":            {test: TestADOBacklogStagesDispatchFromCommands},
	"gather-ci-failures":       {test: TestRemediationStagesDispatchFromCommands},
	"gather-implement-context": {test: TestGatherImplementContextCommandDispatchesToADO},
	"gather-issue-context":     {test: TestRemediationStagesDispatchFromCommands},
	"gather-pr-context":        {test: TestRemediationStagesDispatchFromCommands},
	"gather-review-threads":    {test: TestRemediationStagesDispatchFromCommands},
	"issue-close-out":          {test: TestADOBacklogStagesDispatchFromCommands},
	"open-pr":                  {test: TestOpenPRRoutesADOThroughExecutorInjectedAuthentication},
	"pr-claim":                 {test: TestRemediationStagesDispatchFromCommands},
	"publish-batch":            {test: TestDecompositionStagesDispatchFromCommands},
	"push-branch":              {test: TestPushBranchDispatchesADOOrigin},
	"push-remediated":          {test: TestRemediationStagesDispatchFromCommands},
	"rebase-pr":                {test: TestRemediationStagesDispatchFromCommands},
	"remediation-checkpoint":   {test: TestRemediationStagesDispatchFromCommands},
	"report-pr-status":         {test: TestADOBacklogStagesDispatchFromCommands},
	"respond-to-findings":      {test: TestRespondToFindingsDispatchesToGitea},
	"select-source":            {test: TestDecompositionStagesDispatchFromCommands},
	"update-behind-pr":         {test: TestUpdateBehindPRDispatchesToGitea},
	"validate-plan":            {test: TestDecompositionStagesDispatchFromCommands},
}

var providerDispatchAllowlist = map[string]string{
	"check-issue-staleness":  "CONF-7 (#2496): merge-review still constructs GitHub providers directly.",
	"elect-lander":           "CONF-7 (#2496): merge-review still constructs GitHub providers directly.",
	"gather-sibling-context": "CONF-7 (#2496): merge-review still constructs GitHub providers directly.",
	"merge-pr":               "CONF-7 (#2496): merge-review still constructs GitHub providers directly.",
	"merge-queue-poll":       "CONF-7 (#2496): merge-review still constructs GitHub providers directly.",
	"post-merge":             "CONF-7 (#2496): merge-review still constructs GitHub providers directly.",
	"pr-select":              "CONF-7 (#2496): merge-review still constructs GitHub providers directly.",
	"reconcile-post-merge":   "CONF-7 (#2496): merge-review still constructs GitHub providers directly.",
	"record-merge-refusal":   "CONF-7 (#2496): merge-review still constructs GitHub providers directly.",
	"reconcile-branches":     "This operator command is scoped to GitHub branch reconciliation and requires github:branch:delete.",
	"set-milestone":          "Milestones are GitHub-only; the command help explicitly says GitHub milestone and no ADO milestone capability exists.",
	"telemetry-query":        "Provider access is limited to the optional GitHub-only Tutor live-verification format; ordinary telemetry queries are local.",
}

func TestBlessedTierStageDispatchCoverage(t *testing.T) {
	manifestCommands := providerstage.Commands()
	declared := make(map[string]bool, len(manifestCommands))
	for _, command := range manifestCommands {
		declared[command] = true
		evidence, covered := providerDispatchCoverage[command]
		reason, allowed := providerDispatchAllowlist[command]
		switch {
		case covered && allowed:
			t.Errorf("%q has both non-GitHub test coverage and an allowlist entry; remove the allowlist entry", command)
		case covered && evidence.test == nil:
			t.Errorf("%q has nil non-GitHub test evidence", command)
		case allowed && strings.TrimSpace(reason) == "":
			t.Errorf("%q has an undocumented provider-dispatch allowlist entry", command)
		case !covered && !allowed:
			t.Errorf("%q has neither non-GitHub dispatch test coverage nor a documented entry in providerDispatchAllowlist", command)
		}
	}

	for command := range providerDispatchCoverage {
		if !declared[command] {
			t.Errorf("providerDispatchCoverage contains unknown manifest command %q", command)
		}
	}

	for command := range providerDispatchAllowlist {
		if !declared[command] {
			t.Errorf("providerDispatchAllowlist contains unknown manifest command %q", command)
		}
	}
}

const dispatchProbeError = "provider dispatch probe"

func TestRemediationStagesDispatchFromCommands(t *testing.T) {
	tests := []struct {
		command string
		setup   func(*testing.T) string
	}{
		{"gather-ci-failures", func(t *testing.T) string {
			root, _ := runGatherCIFixture(t, remediationBriefFixture(true))
			return root
		}},
		{"gather-issue-context", func(t *testing.T) string {
			const runID = "dispatch-gather-issue"
			root := initDemo(t)
			seedRemediationBriefRun(t, root, runID, issueContextBrief())
			t.Setenv("GOOBERS_RUN_ID", runID)
			t.Setenv("GOOBERS_WORKFLOW", "pr-remediation")
			return root
		}},
		{"gather-pr-context", func(t *testing.T) string { return initDemo(t) }},
		{"gather-review-threads", func(t *testing.T) string {
			const runID = "dispatch-review-threads"
			root := initDemo(t)
			seedReviewThreadsBrief(t, root, runID, reviewThreadsBrief())
			t.Setenv("GOOBERS_RUN_ID", runID)
			t.Setenv("GOOBERS_WORKFLOW", "pr-remediation")
			return root
		}},
		{"pr-claim", func(t *testing.T) string {
			const runID = "dispatch-pr-claim"
			root := initDemo(t)
			t.Setenv("GOOBERS_RUN_ID", runID)
			t.Setenv("GOOBERS_WORKFLOW", "pr-remediation")
			if _, err := claimPullRequestInOrder(root, []providers.PullRequestSummary{{Number: 77}}, runID, "pr-remediation", time.Hour); err != nil {
				t.Fatalf("seed PR claim: %v", err)
			}
			return root
		}},
		{"push-remediated", func(t *testing.T) string { return initDemo(t) }},
		{"rebase-pr", func(t *testing.T) string {
			t.Setenv(executor.InputEnvVar("selectedNumber"), "77")
			t.Setenv(executor.InputEnvVar("head"), "goobers/pr-remediation/dispatch")
			return initDemo(t)
		}},
		{"remediation-checkpoint", func(t *testing.T) string {
			t.Setenv(executor.InputEnvVar("selectedNumber"), "77")
			return initDemo(t)
		}},
	}

	for _, test := range tests {
		t.Run(test.command, func(t *testing.T) {
			root := test.setup(t)
			setNonGitHubStageEnv(t, providers.ProviderGitea)
			previous := remediationStageProvider
			called := false
			remediationStageProvider = func(_ string, repo providers.RepositoryRef, _ string, _ bool) (remediationProvider, error) {
				called = true
				if repo.Provider != providers.ProviderGitea {
					t.Fatalf("provider = %q, want gitea", repo.Provider)
				}
				return nil, errors.New(dispatchProbeError)
			}
			t.Cleanup(func() { remediationStageProvider = previous })

			code, _, stderr := runArgs(t, test.command, root)
			if code != 1 || !called || !strings.Contains(stderr, dispatchProbeError) {
				t.Fatalf("code = %d, called = %v, stderr = %q; want Gitea dispatch probe failure", code, called, stderr)
			}
		})
	}
}

func TestADOBacklogStagesDispatchFromCommands(t *testing.T) {
	tests := []struct {
		command string
		args    []string
		setup   func(*testing.T)
	}{
		{"backlog-assignment", nil, func(t *testing.T) {
			t.Setenv(executor.InputEnvVar("trustLabel"), "goobers:approved")
			t.Setenv(executor.InputEnvVar("strategy"), assignmentStrategyConstantCap)
			t.Setenv(executor.InputEnvVar("roster"), `[{"assignee":"goober","maxOpen":1}]`)
		}},
		{"backlog-query", []string{"--read-only"}, func(t *testing.T) {
			t.Setenv(executor.InputEnvVar("trustLabel"), "goobers:approved")
		}},
		{"issue-close-out", nil, func(*testing.T) {}},
		{"report-pr-status", nil, func(t *testing.T) {
			t.Setenv(executor.InputEnvVar("prNumber"), "77")
		}},
	}

	for _, test := range tests {
		t.Run(test.command, func(t *testing.T) {
			root := initDemo(t)
			test.setup(t)
			setNonGitHubStageEnv(t, providers.ProviderADO)
			previous := newADOProviderForStage
			called := false
			newADOProviderForStage = func(_ string, repo providers.RepositoryRef) (*providers.ADOProvider, error) {
				called = true
				if repo.Provider != providers.ProviderADO {
					t.Fatalf("provider = %q, want ado", repo.Provider)
				}
				return nil, errors.New(dispatchProbeError)
			}
			t.Cleanup(func() { newADOProviderForStage = previous })

			args := append([]string{test.command}, test.args...)
			args = append(args, root)
			code, _, stderr := runArgs(t, args...)
			if code != 1 || !called || !strings.Contains(stderr, dispatchProbeError) {
				t.Fatalf("code = %d, called = %v, stderr = %q; want ADO dispatch probe failure", code, called, stderr)
			}
		})
	}
}

func TestBacklogDedupeCommandDispatchesToADO(t *testing.T) {
	assertADOCommandDispatch(t, "backlog-dedupe", func(t *testing.T) {
		t.Setenv("GOOBERS_RUN_ID", "dispatch-backlog-dedupe")
		t.Setenv("GOOBERS_WORKFLOW", "backlog-curation")
	})
}

func TestGatherImplementContextCommandDispatchesToADO(t *testing.T) {
	assertADOCommandDispatch(t, "gather-implement-context", func(t *testing.T) {
		t.Setenv("GOOBERS_GAGGLE", "acme-web")
	})
}

func assertADOCommandDispatch(t *testing.T, command string, setup func(*testing.T)) {
	t.Helper()
	root := initDemo(t)
	setup(t)
	setNonGitHubStageEnv(t, providers.ProviderADO)
	previous := newADOProviderForStage
	called := false
	newADOProviderForStage = func(_ string, repo providers.RepositoryRef) (*providers.ADOProvider, error) {
		called = true
		if repo.Provider != providers.ProviderADO {
			t.Fatalf("provider = %q, want ado", repo.Provider)
		}
		return nil, errors.New(dispatchProbeError)
	}
	t.Cleanup(func() { newADOProviderForStage = previous })

	code, _, stderr := runArgs(t, command, root)
	if code != 1 || !called || !strings.Contains(stderr, dispatchProbeError) {
		t.Fatalf("code = %d, called = %v, stderr = %q; want ADO dispatch probe failure", code, called, stderr)
	}
}

func TestDecompositionStagesDispatchFromCommands(t *testing.T) {
	tests := []struct {
		command string
		setup   func(*testing.T)
	}{
		{"select-source", func(t *testing.T) {
			t.Setenv(executor.InputEnvVar("trustLabel"), providers.LabelApproved)
		}},
		{"publish-batch", func(t *testing.T) {
			plan := validDecompositionPlan(decomposition.Selection{})
			digest, err := decomposition.PlanDigest(plan)
			if err != nil {
				t.Fatalf("digest plan: %v", err)
			}
			writeJSON := func(name string, value any) string {
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
			t.Setenv(executor.InputEnvVar("planFile"), writeJSON("plan.json", plan))
			t.Setenv(executor.InputEnvVar("validationFile"), writeJSON("plan-validation.json", validatePlanResult{
				Valid:      true,
				PlanDigest: digest,
			}))
		}},
		{"validate-plan", func(t *testing.T) {
			dir := t.TempDir()
			planFile := filepath.Join(dir, "plan.json")
			selectionFile := filepath.Join(dir, "selection.json")
			if err := os.WriteFile(planFile, []byte("{}"), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(selectionFile, []byte("{}"), 0o644); err != nil {
				t.Fatal(err)
			}
			t.Setenv(executor.InputEnvVar("planFile"), planFile)
			t.Setenv(executor.InputEnvVar("selectionFile"), selectionFile)
		}},
	}

	for _, test := range tests {
		t.Run(test.command, func(t *testing.T) {
			root := initDemo(t)
			test.setup(t)
			setNonGitHubStageEnv(t, providers.ProviderADO)
			previous := newADOProviderForStage
			called := false
			newADOProviderForStage = func(_ string, repo providers.RepositoryRef) (*providers.ADOProvider, error) {
				called = true
				if repo.Provider != providers.ProviderADO {
					t.Fatalf("provider = %q, want ado", repo.Provider)
				}
				return nil, errors.New(dispatchProbeError)
			}
			t.Cleanup(func() { newADOProviderForStage = previous })

			code, _, stderr := runArgs(t, test.command, root)
			if code != 1 || !called || !strings.Contains(stderr, dispatchProbeError) {
				t.Fatalf("code = %d, called = %v, stderr = %q; want ADO dispatch probe failure", code, called, stderr)
			}
		})
	}
}

func TestPushBranchDispatchesADOOrigin(t *testing.T) {
	root := initDemo(t)
	repo := t.TempDir()
	runGitT(t, repo, "init", "-b", "dispatch")
	runGitT(t, repo, "remote", "add", "origin", "https://dev.azure.com/acme/project/_git/repo")
	t.Setenv("GOOBERS_INSTANCE_ROOT", root)

	code, _, stderr := runArgs(t, "push-branch", repo)
	if code != 1 || !strings.Contains(stderr, "ADO origin") || !strings.Contains(stderr, "does not match any configured repository") {
		t.Fatalf("code = %d, stderr = %q; want configured ADO dispatch failure", code, stderr)
	}
}

func setNonGitHubStageEnv(t *testing.T, kind providers.ProviderKind) {
	t.Helper()
	t.Setenv(executor.RepoProviderEnvVar, string(kind))
	t.Setenv(executor.RepoOwnerEnvVar, "acme")
	t.Setenv(executor.RepoNameEnvVar, "web")
	t.Setenv(executor.RepoProjectEnvVar, "project")
	t.Setenv(executor.CredentialEnvVar("github:pr:write"), "pr-token")
	t.Setenv(executor.CredentialEnvVar("github:issues:write"), "issues-token")
	t.Setenv(executor.CredentialEnvVar("repo:push"), "push-token")
	t.Setenv("GOOBERS_INPUT_RESULTFILE", filepath.Join(t.TempDir(), "result.json"))
}
