package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/capability"
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
	"backlog-assignment":       {test: TestBacklogAssignmentDispatchesFromCommand},
	"backlog-dedupe":           {test: TestBacklogDedupeCommandDispatchesToADO},
	"backlog-health":           {test: TestBacklogHealthCommandRunsWithADO},
	"backlog-query":            {test: TestBacklogQueryDispatchesFromCommand},
	"check-issue-staleness":    {test: TestCheckIssueStalenessADONoPinIsNeverStaleWithoutMutation},
	"elect-lander":             {test: TestElectLanderDispatchesADOAndElectsCandidate},
	"file-issues":              {test: TestFileIssuesRefusesNonGitHubProviders},
	"gather-ci-failures":       {test: TestGatherCIFailuresDispatchesFromCommand},
	"gather-implement-context": {test: TestGatherImplementContextCommandDispatchesToADO},
	"gather-issue-context":     {test: TestGatherIssueContextDispatchesFromCommand},
	"gather-pr-context":        {test: TestGatherPRContextDispatchesFromCommand},
	"gather-review-threads":    {test: TestGatherReviewThreadsDispatchesFromCommand},
	"gather-sibling-context":   {test: TestRunGatherSiblingContextADOEmptySiblingSet},
	"issue-close-out":          {test: TestIssueCloseOutDispatchesFromCommand},
	"merge-pr":                 {test: TestMergePRDispatchesToADOAndLandsWithoutVerdictComment},
	"merge-queue-poll":         {test: TestMergeQueuePollADOReportsMergedWithoutBranchCleanupOrWorkItemWrite},
	"open-pr":                  {test: TestOpenPRRoutesADOThroughExecutorInjectedAuthentication},
	"post-merge":               {test: TestPostMergeADODispatchesAndClosesWorkItem},
	"pr-claim":                 {test: TestPRClaimDispatchesFromCommand},
	"pr-comment-watch":         {test: TestPRCommentWatchGiteaEndToEnd},
	"pr-select":                {test: TestPRSelectDispatchesADOAndSelectsPolicyGreenPR},
	"publish-batch":            {test: TestPublishBatchDispatchesFromCommand},
	"push-branch":              {test: TestPushBranchDispatchesADOOrigin},
	"push-remediated":          {test: TestPushRemediatedDispatchesFromCommand},
	"rebase-pr":                {test: TestRebasePRDispatchesFromCommand},
	"reconcile-post-merge":     {test: TestReconcilePostMergeADOCompletesLedgerAndDoesNotRetry},
	"record-merge-refusal":     {test: TestRecordMergeRefusalDispatchesADOAndRecordsComment},
	"remediation-checkpoint":   {test: TestRemediationCheckpointDispatchesFromCommand},
	"resolve-review-threads":   {test: TestResolveReviewThreadsOnADO},
	"report-pr-status":         {test: TestReportPRStatusDispatchesFromCommand},
	"respond-to-findings":      {test: TestRespondToFindingsDispatchesToGitea},
	"security-alerts-query":    {test: TestSecurityAlertsQueryRefusesNonGitHubProviders},
	"select-source":            {test: TestSelectSourceDispatchesFromCommand},
	"update-behind-pr":         {test: TestUpdateBehindPRDispatchesToGitea},
	"validate-plan":            {test: TestValidatePlanDispatchesFromCommand},
}

var providerDispatchAllowlist = map[string]string{
	"recovery-restore":     "Uses configured Git transport and verified archives, not forge REST dispatch; TestIntegrationRecoveryCommandsUseConfiguredGiteaRepository/record and /http-issue exercise the command against a Gitea identity with an exact local Git URL redirect.",
	"recovery-resume":      "Uses claims-plane identity and configured Git transport, not forge REST dispatch; TestIntegrationRecoveryCommandsUseConfiguredGiteaRepository/resume-issue exercises actual adoption for a Gitea repository.",
	"preflight-repo-write": "Repository-write preflight (#4414) is a GitHub-only capability today (branch ruleset introspection has no ADO/Gitea equivalent); Dispatcher fails closed with ErrUnsupported for other providers.",
	"reconcile-branches":   "This operator command is scoped to GitHub branch reconciliation and requires github:branch:delete.",
	"set-milestone":        "Milestones are GitHub-only; the command help explicitly says GitHub milestone and no ADO milestone capability exists.",
	"telemetry-query":      "Provider access is limited to the optional GitHub-only Tutor live-verification format; ordinary telemetry queries are local.",
}

func TestBlessedTierStageDispatchCoverage(t *testing.T) {
	manifestCommands := providerstage.Commands()
	declared := make(map[string]bool, len(manifestCommands))
	evidenceOwners := make(map[uintptr]string, len(providerDispatchCoverage))
	for _, command := range manifestCommands {
		declared[command] = true
		evidence, covered := providerDispatchCoverage[command]
		reason, allowed := providerDispatchAllowlist[command]
		switch {
		case covered && allowed:
			t.Errorf("%q has both non-GitHub test coverage and an allowlist entry; remove the allowlist entry", command)
		case covered && evidence.test == nil:
			t.Errorf("%q has nil non-GitHub test evidence", command)
		case covered:
			pointer := reflect.ValueOf(evidence.test).Pointer()
			if owner, duplicate := evidenceOwners[pointer]; duplicate {
				t.Errorf("%q and %q use the same non-GitHub test evidence; each command requires a stage-specific test", owner, command)
			}
			evidenceOwners[pointer] = command
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

func TestGatherCIFailuresDispatchesFromCommand(t *testing.T) {
	assertRemediationStageDispatch(t, "gather-ci-failures", func(t *testing.T) string {
		root, _ := runGatherCIFixture(t, remediationBriefFixture(true))
		return root
	})
}

func TestGatherIssueContextDispatchesFromCommand(t *testing.T) {
	assertRemediationStageDispatch(t, "gather-issue-context", func(t *testing.T) string {
		const runID = "dispatch-gather-issue"
		root := initDemo(t)
		seedRemediationBriefRun(t, root, runID, issueContextBrief())
		t.Setenv("GOOBERS_RUN_ID", runID)
		t.Setenv("GOOBERS_WORKFLOW", "pr-remediation")
		return root
	})
}

func TestGatherPRContextDispatchesFromCommand(t *testing.T) {
	assertRemediationStageDispatch(t, "gather-pr-context", initDemo)
}

// TestGatherReviewThreadsDispatchesFromCommand is gather-review-threads'
// non-GitHub dispatch evidence. Since ADO-N20 the stage resolves its provider
// through the narrow reviewThreadReader surface (reviewThreadStageSurface),
// so both non-GitHub arms are pinned at the registered stage-provider seam:
// ADO at newADOProviderForStage, built from the declared github:pr:write
// credential (ADO-N18), and Gitea at its stageProviderFactories entry,
// handed the same github:pr:write token.
func TestGatherReviewThreadsDispatchesFromCommand(t *testing.T) {
	const runID = "dispatch-review-threads"
	t.Run("ado", func(t *testing.T) {
		root, _ := adoReviewThreadsStageFixture(t, runID)
		seedReviewThreadsBrief(t, root, runID, reviewThreadsBrief())
		t.Chdir(t.TempDir())
		original := newADOProviderForStage
		called := false
		newADOProviderForStage = func(routed providers.RepositoryRef, credential providers.ADOCredentialSource) (*providers.ADOProvider, error) {
			called = true
			if routed.Provider != providers.ProviderADO {
				t.Fatalf("provider = %q, want ado", routed.Provider)
			}
			if got := deliveredCapabilityOf(t, credential); got != string(capability.GitHubPRWrite) {
				t.Fatalf("gather-review-threads built its ADO provider from %q, want the declared %q", got, capability.GitHubPRWrite)
			}
			return nil, errors.New(dispatchProbeError)
		}
		t.Cleanup(func() { newADOProviderForStage = original })

		code, _, stderr := runArgs(t, "gather-review-threads", root)
		if code != 1 || !called || !strings.Contains(stderr, dispatchProbeError) {
			t.Fatalf("code = %d, called = %v, stderr = %q; want ADO dispatch probe failure", code, called, stderr)
		}
	})
	t.Run("gitea", func(t *testing.T) {
		root, repo := providerDispatchFixture(t, providers.ProviderGitea)
		t.Setenv(executor.RepoProviderEnvVar, string(repo.Provider))
		t.Setenv(executor.RepoOwnerEnvVar, repo.Owner)
		t.Setenv(executor.RepoNameEnvVar, repo.Name)
		t.Setenv("GOOBERS_RUN_ID", runID)
		t.Setenv("GOOBERS_WORKFLOW", "pr-remediation")
		seedReviewThreadsBrief(t, root, runID, reviewThreadsBrief())
		t.Chdir(t.TempDir())
		previous := stageProviderFactories[providers.ProviderGitea]
		t.Cleanup(func() { stageProviderFactories[providers.ProviderGitea] = previous })
		called := false
		stageProviderFactories[providers.ProviderGitea] = func(cfg stageProviderConfig) (providers.Provider, error) {
			called = true
			if cfg.token != "gitea-pr-token" {
				t.Fatalf("token = %q, want the github:pr:write credential", cfg.token)
			}
			return nil, errors.New(dispatchProbeError)
		}

		code, _, stderr := runArgs(t, "gather-review-threads", root)
		if code != 1 || !called || !strings.Contains(stderr, dispatchProbeError) {
			t.Fatalf("code = %d, called = %v, stderr = %q; want Gitea dispatch probe failure", code, called, stderr)
		}
	})
}

// TestPRClaimDispatchesFromCommand is pr-claim's non-GitHub dispatch
// evidence. pr-claim (ADO-N14, #5655) resolves its provider through the
// narrow prClaimProvider surface (remediationStageSurface), not the broad
// GitHub/Gitea-only remediationStageProvider factory, so both non-GitHub
// arms are pinned at the registered stage-provider seam: ADO at
// newADOProviderForStage, built from the github:pr:write credential its
// manifest row declares (§3.1, ADO-N18), and Gitea at its
// stageProviderFactories entry (handed the same github:pr:write token).
func TestPRClaimDispatchesFromCommand(t *testing.T) {
	t.Run("ado", func(t *testing.T) {
		root, _ := prClaimDispatchFixture(t, providers.ProviderADO)
		deliverEveryADOStageCapability(t)
		original := newADOProviderForStage
		var consumed []string
		newADOProviderForStage = func(routed providers.RepositoryRef, credential providers.ADOCredentialSource) (*providers.ADOProvider, error) {
			if routed.Provider != providers.ProviderADO {
				t.Fatalf("provider = %q, want ado", routed.Provider)
			}
			consumed = append(consumed, deliveredCapabilityOf(t, credential))
			return nil, errors.New(dispatchProbeError)
		}
		t.Cleanup(func() { newADOProviderForStage = original })

		code, _, stderr := runArgs(t, "pr-claim", root)
		if code != 1 || len(consumed) != 1 || !strings.Contains(stderr, dispatchProbeError) {
			t.Fatalf("code = %d, consumed = %v, stderr = %q; want ADO dispatch probe failure", code, consumed, stderr)
		}
		if consumed[0] != string(capability.GitHubPRWrite) {
			t.Fatalf("pr-claim built its ADO provider from %q, want the declared %q", consumed[0], capability.GitHubPRWrite)
		}
		assertManifestDeclares(t, "pr-claim", consumed[0])
	})
	t.Run("gitea", func(t *testing.T) {
		root, _ := prClaimDispatchFixture(t, providers.ProviderGitea)
		previous := stageProviderFactories[providers.ProviderGitea]
		t.Cleanup(func() { stageProviderFactories[providers.ProviderGitea] = previous })
		called := false
		stageProviderFactories[providers.ProviderGitea] = func(cfg stageProviderConfig) (providers.Provider, error) {
			called = true
			if cfg.repo.Provider != providers.ProviderGitea {
				t.Fatalf("provider = %q, want gitea", cfg.repo.Provider)
			}
			if cfg.token != "gitea-pr-token" {
				t.Fatalf("token = %q, want the github:pr:write credential", cfg.token)
			}
			return nil, errors.New(dispatchProbeError)
		}

		code, _, stderr := runArgs(t, "pr-claim", root)
		if code != 1 || !called || !strings.Contains(stderr, dispatchProbeError) {
			t.Fatalf("code = %d, called = %v, stderr = %q; want Gitea dispatch probe failure", code, called, stderr)
		}
	})
}

// prClaimDispatchFixture seeds a pr-claim run for kind with PR #77 claimed.
func prClaimDispatchFixture(t *testing.T, kind providers.ProviderKind) (string, providers.RepositoryRef) {
	t.Helper()
	const runID = "dispatch-pr-claim"
	root, repo := providerDispatchFixture(t, kind)
	t.Setenv(executor.RepoProviderEnvVar, string(repo.Provider))
	t.Setenv(executor.RepoOwnerEnvVar, repo.Owner)
	t.Setenv(executor.RepoProjectEnvVar, repo.Project)
	t.Setenv(executor.RepoNameEnvVar, repo.Name)
	t.Setenv("GOOBERS_RUN_ID", runID)
	t.Setenv("GOOBERS_WORKFLOW", "pr-remediation")
	t.Chdir(t.TempDir())
	if _, err := claimPullRequestInOrder(root, repo, []providers.PullRequestSummary{{Number: 77}}, runID, "pr-remediation", time.Hour); err != nil {
		t.Fatalf("seed PR claim: %v", err)
	}
	return root, repo
}

func TestPushRemediatedDispatchesFromCommand(t *testing.T) {
	assertRemediationStageDispatch(t, "push-remediated", initDemo)
}

func TestRebasePRDispatchesFromCommand(t *testing.T) {
	assertRemediationStageDispatch(t, "rebase-pr", func(t *testing.T) string {
		t.Setenv(executor.InputEnvVar("selectedNumber"), "77")
		t.Setenv(executor.InputEnvVar("head"), "goobers/pr-remediation/dispatch")
		return initDemo(t)
	})
}

func TestRemediationCheckpointDispatchesFromCommand(t *testing.T) {
	assertRemediationStageDispatch(t, "remediation-checkpoint", func(t *testing.T) string {
		t.Setenv(executor.InputEnvVar("selectedNumber"), "77")
		return initDemo(t)
	})
}

func assertRemediationStageDispatch(t *testing.T, command string, setup func(*testing.T) string) {
	t.Helper()
	root := setup(t)
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

	code, _, stderr := runArgs(t, command, root)
	if code != 1 || !called || !strings.Contains(stderr, dispatchProbeError) {
		t.Fatalf("code = %d, called = %v, stderr = %q; want Gitea dispatch probe failure", code, called, stderr)
	}
}

func TestBacklogAssignmentDispatchesFromCommand(t *testing.T) {
	assertADOBacklogStageDispatch(t, "backlog-assignment", nil, capability.GitHubIssuesWrite, func(t *testing.T) {
		t.Setenv(executor.InputEnvVar("trustLabel"), "goobers:approved")
		t.Setenv(executor.InputEnvVar("strategy"), assignmentStrategyConstantCap)
		t.Setenv(executor.InputEnvVar("roster"), `[{"assignee":"goober","maxOpen":1}]`)
	})
}

func TestBacklogQueryDispatchesFromCommand(t *testing.T) {
	assertADOBacklogStageDispatch(t, "backlog-query", []string{"--read-only"}, capability.GitHubIssuesRead, func(t *testing.T) {
		t.Setenv(executor.InputEnvVar("trustLabel"), "goobers:approved")
	})
}

func TestIssueCloseOutDispatchesFromCommand(t *testing.T) {
	assertADOBacklogStageDispatch(t, "issue-close-out", nil, capability.GitHubIssuesWrite, func(*testing.T) {})
}

func TestReportPRStatusDispatchesFromCommand(t *testing.T) {
	assertADOBacklogStageDispatch(t, "report-pr-status", nil, capability.GitHubPRWrite, func(t *testing.T) {
		t.Setenv(executor.InputEnvVar("prNumber"), "77")
	})
}

func TestSetMilestoneDispatchesFromCommand(t *testing.T) {
	assertADOBacklogStageDispatch(t, "set-milestone", []string{"--item", "7", "--milestone", "22"}, capability.GitHubMilestonesWrite, func(*testing.T) {})
}

func assertADOBacklogStageDispatch(t *testing.T, command string, args []string, want capability.Capability, setup func(*testing.T)) {
	t.Helper()
	assertADOStageDispatch(t, command, args, want, setup)
}

func TestBacklogDedupeCommandDispatchesToADO(t *testing.T) {
	assertADOCommandDispatch(t, "backlog-dedupe", capability.GitHubIssuesRead, func(t *testing.T) {
		t.Setenv("GOOBERS_RUN_ID", "dispatch-backlog-dedupe")
		t.Setenv("GOOBERS_WORKFLOW", "backlog-curation")
	})
}

func TestGatherImplementContextCommandDispatchesToADO(t *testing.T) {
	assertADOCommandDispatch(t, "gather-implement-context", capability.GitHubPRWrite, func(t *testing.T) {
		t.Setenv("GOOBERS_GAGGLE", "acme-web")
	})
}

func assertADOCommandDispatch(t *testing.T, command string, want capability.Capability, setup func(*testing.T)) {
	t.Helper()
	assertADOStageDispatch(t, command, nil, want, setup)
}

func TestSelectSourceDispatchesFromCommand(t *testing.T) {
	assertADOCommandDispatch(t, "select-source", capability.GitHubIssuesWrite, func(t *testing.T) {
		t.Setenv(executor.InputEnvVar("trustLabel"), providers.LabelApproved)
	})
}

func TestPublishBatchDispatchesFromCommand(t *testing.T) {
	assertADOCommandDispatch(t, "publish-batch", capability.GitHubIssuesWrite, func(t *testing.T) {
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
	})
}

func TestValidatePlanDispatchesFromCommand(t *testing.T) {
	assertADOCommandDispatch(t, "validate-plan", capability.GitHubIssuesRead, func(t *testing.T) {
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
	})
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
	// gather-issue-context declares github:issues:read, not :write, and exits
	// before provider dispatch when its credential is absent — which reads as
	// "dispatch was never attempted" rather than as a missing credential.
	t.Setenv(executor.CredentialEnvVar("github:issues:read"), "issues-read-token")
	t.Setenv(executor.CredentialEnvVar("repo:push"), "push-token")
	t.Setenv("GOOBERS_INPUT_RESULTFILE", filepath.Join(t.TempDir(), "result.json"))
}

// ADO credential conformance (docs/design/ado-parity-dsl-2-0.md §3.1, ADO-N18).
// On Azure DevOps the declared capability selects the credential through the
// same injector as GitHub: a github:* capability declared on a stage whose
// command dispatches through newProviderForStage authorizes the same operation
// on the provider the stage routes to, and the ADO provider must be built from
// exactly that capability's GOOBERS_CRED_ value. The probes below deliver a
// distinct value for EVERY credentialed capability, so a provider built from
// the wrong one is caught by name.

// everyCredentialedCapability is every capability the daemon can deliver as
// GOOBERS_CRED_<capability> from a repository credential
// (repoCredentialedCapabilityNames, which on ADO includes
// ado:work-items:write), so the probe can tell any of them apart.
func everyCredentialedCapability() []string {
	return repoCredentialedCapabilityNames()
}

// deliverEveryADOStageCapability delivers a distinct value for every
// credentialed capability, with the bearer scheme beside them.
func deliverEveryADOStageCapability(t *testing.T) {
	t.Helper()
	for _, name := range everyCredentialedCapability() {
		t.Setenv(executor.CredentialEnvVar(name), deliveredADOStageToken(name))
	}
	t.Setenv(executor.RepoAuthSchemeEnvVar, "bearer")
}

// deliveredCapabilityOf names the capability whose delivered value credential
// carries, and checks it is sent in the delivered (bearer) scheme.
func deliveredCapabilityOf(t *testing.T, credential providers.ADOCredentialSource) string {
	t.Helper()
	if credential == nil {
		t.Error("ADO stage provider built with no credential")
		return ""
	}
	got, err := credential.Credential(context.Background())
	if err != nil {
		t.Errorf("resolve delivered credential: %v", err)
		return ""
	}
	if got.Kind != providers.ADOCredentialKindBearer {
		t.Errorf("credential kind = %q, want %q from %s", got.Kind, providers.ADOCredentialKindBearer, executor.RepoAuthSchemeEnvVar)
	}
	for _, name := range everyCredentialedCapability() {
		if got.Secret == deliveredADOStageToken(name) {
			return name
		}
	}
	t.Errorf("ADO stage provider built from a value no capability delivered")
	return ""
}

// assertManifestDeclares fails unless command's manifest row names cap: the
// ADO path may only consume a capability the stage can declare for it.
func assertManifestDeclares(t *testing.T, command, cap string) {
	t.Helper()
	entry, ok := providerstage.Lookup(command)
	if !ok {
		t.Fatalf("%q is not a manifest command", command)
	}
	for _, use := range entry.Capabilities {
		if string(use.Capability) == cap {
			return
		}
	}
	t.Errorf("%s: the ADO path consumed %q, which its manifest row does not name", command, cap)
}

// assertADOStageDispatch runs command routed to Azure DevOps and fails its
// first ADO provider construction with dispatchProbeError. It asserts the
// command dispatched to ADO, that the provider was built from want's
// delivered value and no other capability's, and that the manifest names want.
func assertADOStageDispatch(t *testing.T, command string, args []string, want capability.Capability, setup func(*testing.T)) {
	t.Helper()
	root := initDemo(t)
	setup(t)
	setNonGitHubStageEnv(t, providers.ProviderADO)
	deliverEveryADOStageCapability(t)
	previous := newADOProviderForStage
	var consumed []string
	newADOProviderForStage = func(repo providers.RepositoryRef, credential providers.ADOCredentialSource) (*providers.ADOProvider, error) {
		if repo.Provider != providers.ProviderADO {
			t.Errorf("provider = %q, want ado", repo.Provider)
		}
		consumed = append(consumed, deliveredCapabilityOf(t, credential))
		return nil, errors.New(dispatchProbeError)
	}
	t.Cleanup(func() { newADOProviderForStage = previous })

	commandArgs := append([]string{command}, args...)
	commandArgs = append(commandArgs, root)
	code, _, stderr := runArgs(t, commandArgs...)
	if code != 1 || len(consumed) == 0 || !strings.Contains(stderr, dispatchProbeError) {
		t.Fatalf("code = %d, consumed = %v, stderr = %q; want ADO dispatch probe failure", code, consumed, stderr)
	}
	if consumed[0] != string(want) {
		t.Fatalf("%s: the ADO provider was built from %q's credential, want the declared %q", command, consumed[0], want)
	}
	assertManifestDeclares(t, command, consumed[0])
}

// installADOCredentialProbe replaces newADOProviderForStage with a real stage
// provider pointed at a server that refuses every request. The command builds
// every provider it needs and then fails at its first call; the returned
// slice records, in order, the capability each provider was built from.
func installADOCredentialProbe(t *testing.T) *[]string {
	t.Helper()
	deliverEveryADOStageCapability(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, dispatchProbeError, http.StatusBadRequest)
	}))
	t.Cleanup(server.Close)
	var consumed []string
	previous := newADOProviderForStage
	newADOProviderForStage = func(routed providers.RepositoryRef, credential providers.ADOCredentialSource) (*providers.ADOProvider, error) {
		consumed = append(consumed, deliveredCapabilityOf(t, credential))
		provider, err := buildADOProviderForStage(routed, credential)
		if err != nil {
			return nil, err
		}
		provider.BaseURL = server.URL
		return provider, nil
	}
	t.Cleanup(func() { newADOProviderForStage = previous })
	return &consumed
}

// adoStageCredentialCase is one command run on Azure DevOps with every
// credentialed capability delivered, and the capabilities whose values its ADO
// providers must be built from, in order.
type adoStageCredentialCase struct {
	command string
	args    []string
	inputs  map[string]string
	seed    func(t *testing.T, root, runID string)
	want    []string
}

var (
	credPRWrite     = string(capability.GitHubPRWrite)
	credIssuesWrite = string(capability.GitHubIssuesWrite)
	credIssuesRead  = string(capability.GitHubIssuesRead)
)

// adoStageCredentialCases is the table TestADOStageProvidersConsumeTheDeclaredCapability
// runs; TestADOStageConsumesDeclaredCapabilityCredential counts each row as
// its command's credential evidence.
var adoStageCredentialCases = []adoStageCredentialCase{
	{command: "backlog-health", inputs: map[string]string{"trustLabel": providers.LabelApproved}, want: []string{credIssuesRead}},
	{command: "check-issue-staleness", inputs: map[string]string{"pullNumber": "77", "head": "goobers/implementation/run"}, want: []string{credPRWrite, credIssuesWrite}},
	{command: "gather-pr-context", want: []string{credPRWrite}},
	{command: "gather-review-threads", seed: seedADOGatherReviewThreadsRun, want: []string{credPRWrite}},
	{command: "gather-sibling-context", inputs: map[string]string{"selectedNumber": "77"}, want: []string{credPRWrite}},
	{command: "merge-pr", inputs: map[string]string{"pullNumber": "77", "verdict": "pass"}, want: []string{string(capability.ADOPRComplete)}},
	{command: "merge-queue-poll", inputs: map[string]string{"pullNumber": "77"}, want: []string{string(capability.ADOPRComplete)}},
	{command: "open-pr", want: []string{string(capability.ProviderPRWrite)}},
	{command: "post-merge", inputs: map[string]string{"pullNumber": "77"}, want: []string{credPRWrite, credIssuesWrite}},
	{command: "pr-select", inputs: map[string]string{"selfIdentity": "goober"}, want: []string{credPRWrite}},
	{command: "push-remediated", want: []string{credPRWrite}},
	{command: "rebase-pr", inputs: map[string]string{"selectedNumber": "77", "head": "goobers/pr-remediation/run"}, want: []string{credPRWrite}},
	// reconcile-branches builds its provider before it refuses Azure DevOps,
	// so the credential it would use is still the declared one.
	{command: "reconcile-branches", want: []string{string(capability.GitHubBranchDelete)}},
	{command: "reconcile-post-merge", want: []string{credPRWrite, credIssuesWrite}},
	{command: "record-merge-refusal", inputs: map[string]string{"selectedNumber": "77", "selectedHeadSha": "head-sha", "reason": "blocked"}, want: []string{credPRWrite}},
	{command: "remediation-checkpoint", inputs: map[string]string{"selectedNumber": "77"}, want: []string{credPRWrite}},
	{command: "resolve-review-threads", seed: seedADOResolveReviewThreadsRun, want: []string{credPRWrite}},
}

// adoCredentialEvidence names, for each provider-dispatched command not in
// adoStageCredentialCases, the test that runs it on Azure DevOps and asserts
// its ADO provider is built from the declared capability's delivered value
// (through assertADOStageDispatch, deliveredCapabilityOf or
// routeADOStageProvider).
var adoCredentialEvidence = map[string]func(*testing.T){
	"apply-verdict":            TestRunApplyVerdictADOPassPublishesStatusAndDecisionPass,
	"backlog-assignment":       TestBacklogAssignmentDispatchesFromCommand,
	"backlog-dedupe":           TestBacklogDedupeCommandDispatchesToADO,
	"backlog-query":            TestBacklogQueryDispatchesFromCommand,
	"elect-lander":             TestElectLanderDispatchesADOAndElectsCandidate,
	"gather-ci-failures":       TestGatherCIFailuresOnADOWritesPolicyEvidence,
	"gather-implement-context": TestGatherImplementContextCommandDispatchesToADO,
	"issue-close-out":          TestIssueCloseOutDispatchesFromCommand,
	"pr-claim":                 TestPRClaimDispatchesFromCommand,
	"publish-batch":            TestPublishBatchDispatchesFromCommand,
	"report-pr-status":         TestReportPRStatusDispatchesFromCommand,
	"select-source":            TestSelectSourceDispatchesFromCommand,
	"set-milestone":            TestSetMilestoneDispatchesFromCommand,
	"validate-plan":            TestValidatePlanDispatchesFromCommand,
}

// adoCredentialExempt lists the commands whose manifest row names a github:*
// capability but which build no Azure DevOps provider, so no ADO credential
// is consumed at all.
var adoCredentialExempt = map[string]string{
	"file-issues":           "Refuses every non-GitHub provider before building one (TestFileIssuesRefusesNonGitHubProviders).",
	"gather-issue-context":  "Uses the broad remediation provider factory, whose Azure DevOps arm is an error, so no ADO provider is built.",
	"pr-comment-watch":      "Refuses the ado repository provider before building one.",
	"respond-to-findings":   "Uses the broad remediation provider factory, whose Azure DevOps arm is an error, so no ADO provider is built.",
	"security-alerts-query": "Refuses every non-GitHub provider before building one (TestSecurityAlertsQueryRefusesNonGitHubProviders).",
	"telemetry-query":       "Its only provider access is the optional GitHub-only Tutor live-verification format; ordinary telemetry queries are local.",
	"update-behind-pr":      "Reports not-applicable on Azure DevOps and routes to full remediation without building a provider (ADO-N15).",
}

// manifestNamesGitHubCapability reports whether command's manifest row names
// any github:* capability, required or optional.
func manifestNamesGitHubCapability(t *testing.T, command string) bool {
	t.Helper()
	entry, ok := providerstage.Lookup(command)
	if !ok {
		t.Fatalf("%q is not a manifest command", command)
	}
	for _, use := range entry.Capabilities {
		if strings.HasPrefix(string(use.Capability), "github:") {
			return true
		}
	}
	return false
}

// TestADOStageConsumesDeclaredCapabilityCredential is the completeness gate
// for the rebinding rule's credential half (docs/design/ado-parity-dsl-2-0.md
// §3.1, ADO-N24): every manifest row that names a github:* capability must
// either carry a test asserting that its Azure DevOps path builds each
// provider from the declared capability's GOOBERS_CRED_ value and no other,
// or document why it builds no ADO provider. A new provider-dispatched
// command cannot land without one of the two.
func TestADOStageConsumesDeclaredCapabilityCredential(t *testing.T) {
	tabled := make(map[string]bool, len(adoStageCredentialCases))
	for _, tc := range adoStageCredentialCases {
		tabled[tc.command] = true
	}
	for _, command := range providerstage.Commands() {
		if !manifestNamesGitHubCapability(t, command) {
			continue
		}
		evidence, hasEvidence := adoCredentialEvidence[command]
		reason, exempt := adoCredentialExempt[command]
		sources := 0
		for _, present := range []bool{tabled[command], hasEvidence, exempt} {
			if present {
				sources++
			}
		}
		switch {
		case sources == 0:
			t.Errorf("%q names a github:* capability but has no Azure DevOps credential conformance evidence; add it to adoStageCredentialCases or adoCredentialEvidence, or document it in adoCredentialExempt", command)
		case sources > 1:
			t.Errorf("%q is listed more than once across adoStageCredentialCases, adoCredentialEvidence and adoCredentialExempt", command)
		case hasEvidence && evidence == nil:
			t.Errorf("%q has nil Azure DevOps credential evidence", command)
		case exempt && strings.TrimSpace(reason) == "":
			t.Errorf("%q has an undocumented Azure DevOps credential exemption", command)
		}
	}
	for command := range adoCredentialEvidence {
		if _, ok := providerstage.Lookup(command); !ok || !manifestNamesGitHubCapability(t, command) {
			t.Errorf("adoCredentialEvidence lists %q, which is not a manifest command naming a github:* capability", command)
		}
	}
	for command := range adoCredentialExempt {
		if _, ok := providerstage.Lookup(command); !ok || !manifestNamesGitHubCapability(t, command) {
			t.Errorf("adoCredentialExempt lists %q, which is not a manifest command naming a github:* capability", command)
		}
	}
}

// TestADOStageProvidersConsumeTheDeclaredCapability covers the commands whose
// Azure DevOps path builds its own providers (merge review and PR
// remediation). Each must build every ADO provider from the delivered value of
// a capability its manifest row names, and exactly the capabilities listed:
// pull-request work on the project provider from github:pr:write (or
// provider:pr:write), backlog work items from github:issues:*.
//
// merge-pr and merge-queue-poll land with github:pr:merge, or with
// ado:pr:complete when it was delivered, as it is here
// (docs/design/ado-parity-dsl-2-0.md §3.3; the full matrix is
// TestMergePRADOLandingAuthorityMatrix and
// TestMergeQueuePollADOLandingAuthorityMatrix).
// apply-verdict and elect-lander read a journaled verdict before they build a
// provider, so their end-to-end ADO tests (TestRunApplyVerdictADO*,
// TestElectLanderDispatchesADOAndElectsCandidate) assert the same rule.
func TestADOStageProvidersConsumeTheDeclaredCapability(t *testing.T) {
	for _, tc := range adoStageCredentialCases {
		t.Run(tc.command, func(t *testing.T) {
			root := initDemo(t)
			runID := "ado-credential-" + tc.command
			if tc.seed != nil {
				tc.seed(t, root, runID)
			}
			t.Setenv("GOOBERS_RUN_ID", runID)
			t.Setenv("GOOBERS_WORKFLOW", "merge-review")
			setNonGitHubStageEnv(t, providers.ProviderADO)
			for key, value := range tc.inputs {
				t.Setenv(executor.InputEnvVar(key), value)
			}
			consumed := installADOCredentialProbe(t)
			t.Chdir(t.TempDir())

			code, stdout, stderr := runArgs(t, append(append([]string{tc.command}, tc.args...), root)...)
			if !reflect.DeepEqual(*consumed, tc.want) {
				t.Fatalf("%s built ADO providers from %v, want exactly %v (code = %d, stdout = %q, stderr = %q)", tc.command, *consumed, tc.want, code, stdout, stderr)
			}
			for _, cap := range *consumed {
				assertManifestDeclares(t, tc.command, cap)
			}
		})
	}
}

// seedADOGatherReviewThreadsRun seeds the remediation brief
// gather-review-threads reads before it builds its provider.
func seedADOGatherReviewThreadsRun(t *testing.T, root, runID string) {
	t.Helper()
	seedReviewThreadsBrief(t, root, runID, reviewThreadsBrief())
}

// seedADOResolveReviewThreadsRun seeds a published resolve-review-threads run
// with one addressed ADO thread, so the stage reaches provider construction.
func seedADOResolveReviewThreadsRun(t *testing.T, root, runID string) {
	t.Helper()
	seedReviewThreadResolutionRunWithComments(t, root, runID,
		`[{"threadId":"77/5","disposition":"addressed","detail":"fixed"}]`,
		[]apiv1.RemediationInlineComment{{ID: 1, ThreadID: "77/5", Body: "fix", Path: "a.go", Integrity: apiv1.IntegrityUnapproved}})
}

// TestADOStageWithoutDeclaredCapabilityGetsNoCredential is the other half of
// the §3.1 rule: with every other capability delivered, a command whose
// declared capability delivered nothing fails naming the variable it needed,
// and builds no ADO provider at all.
func TestADOStageWithoutDeclaredCapabilityGetsNoCredential(t *testing.T) {
	root := initDemo(t)
	setNonGitHubStageEnv(t, providers.ProviderADO)
	t.Setenv(executor.InputEnvVar("prNumber"), "77")
	consumed := installADOCredentialProbe(t)
	t.Setenv(executor.CredentialEnvVar(string(capability.GitHubPRWrite)), "")

	code, _, stderr := runArgs(t, "report-pr-status", root)
	if code != 1 || !strings.Contains(stderr, "GOOBERS_CRED_GITHUB_PR_WRITE") {
		t.Fatalf("code = %d, stderr = %q; want a missing GOOBERS_CRED_GITHUB_PR_WRITE failure", code, stderr)
	}
	if len(*consumed) != 0 {
		t.Fatalf("built ADO providers from %v with the declared capability undelivered", *consumed)
	}
}
