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
	"file-issues":              {test: TestFileIssuesRefusesNonGitHubProviders},
	"backlog-health":           {test: TestBacklogHealthCommandRunsWithADO},
	"backlog-query":            {test: TestBacklogQueryDispatchesFromCommand},
	"check-issue-staleness":    {test: TestCheckIssueStalenessADONoPinIsNeverStaleWithoutMutation},
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
	"pr-claim":                 {test: TestPRClaimDispatchesFromCommand},
	"pr-comment-watch":         {test: TestPRCommentWatchGiteaEndToEnd},
	"pr-select":                {test: TestPRSelectDispatchesADOAndSelectsPolicyGreenPR},
	"publish-batch":            {test: TestPublishBatchDispatchesFromCommand},
	"push-branch":              {test: TestPushBranchDispatchesADOOrigin},
	"push-remediated":          {test: TestPushRemediatedDispatchesFromCommand},
	"rebase-pr":                {test: TestRebasePRDispatchesFromCommand},
	"remediation-checkpoint":   {test: TestRemediationCheckpointDispatchesFromCommand},
	"report-pr-status":         {test: TestReportPRStatusDispatchesFromCommand},
	"respond-to-findings":      {test: TestRespondToFindingsDispatchesToGitea},
	"security-alerts-query":    {test: TestSecurityAlertsQueryRefusesNonGitHubProviders},
	"select-source":            {test: TestSelectSourceDispatchesFromCommand},
	"update-behind-pr":         {test: TestUpdateBehindPRDispatchesToGitea},
	"validate-plan":            {test: TestValidatePlanDispatchesFromCommand},
}

var providerDispatchAllowlist = map[string]string{
	"recovery-restore":       "Uses configured Git transport and verified archives, not forge REST dispatch; TestIntegrationRecoveryCommandsUseConfiguredGiteaRepository/record and /http-issue exercise the command against a Gitea identity with an exact local Git URL redirect.",
	"recovery-resume":        "Uses claims-plane identity and configured Git transport, not forge REST dispatch; TestIntegrationRecoveryCommandsUseConfiguredGiteaRepository/resume-issue exercises actual adoption for a Gitea repository.",
	"elect-lander":           "CONF-7 (#2496): merge-review still constructs GitHub providers directly.",
	"post-merge":             "CONF-7 (#2496): merge-review still constructs GitHub providers directly.",
	"preflight-repo-write":   "Repository-write preflight (#4414) is a GitHub-only capability today (branch ruleset introspection has no ADO/Gitea equivalent); Dispatcher fails closed with ErrUnsupported for other providers.",
	"reconcile-post-merge":   "CONF-7 (#2496): merge-review still constructs GitHub providers directly.",
	"record-merge-refusal":   "CONF-7 (#2496): merge-review still constructs GitHub providers directly.",
	"reconcile-branches":     "This operator command is scoped to GitHub branch reconciliation and requires github:branch:delete.",
	"resolve-review-threads": "Native review-thread replies and resolution are GitHub-only; no equivalent ADO or Gitea capability exists.",
	"set-milestone":          "Milestones are GitHub-only; the command help explicitly says GitHub milestone and no ADO milestone capability exists.",
	"telemetry-query":        "Provider access is limited to the optional GitHub-only Tutor live-verification format; ordinary telemetry queries are local.",
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

func TestGatherReviewThreadsDispatchesFromCommand(t *testing.T) {
	assertRemediationStageDispatch(t, "gather-review-threads", func(t *testing.T) string {
		const runID = "dispatch-review-threads"
		root := initDemo(t)
		seedReviewThreadsBrief(t, root, runID, reviewThreadsBrief())
		t.Setenv("GOOBERS_RUN_ID", runID)
		t.Setenv("GOOBERS_WORKFLOW", "pr-remediation")
		return root
	})
}

func TestPRClaimDispatchesFromCommand(t *testing.T) {
	assertRemediationStageDispatch(t, "pr-claim", func(t *testing.T) string {
		const runID = "dispatch-pr-claim"
		root := initDemo(t)
		t.Setenv("GOOBERS_RUN_ID", runID)
		t.Setenv("GOOBERS_WORKFLOW", "pr-remediation")
		if _, err := claimPullRequestInOrder(root, prClaimTestRepo(), []providers.PullRequestSummary{{Number: 77}}, runID, "pr-remediation", time.Hour); err != nil {
			t.Fatalf("seed PR claim: %v", err)
		}
		return root
	})
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
// GOOBERS_CRED_<capability> (credentialedCapabilities), so the probe can tell
// any of them apart.
func everyCredentialedCapability() []string {
	names := make([]string, 0, len(credentialedCapabilities))
	for _, c := range credentialedCapabilities {
		names = append(names, string(c))
	}
	return names
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

// TestADOStageProvidersConsumeTheDeclaredCapability covers the commands whose
// Azure DevOps path builds its own providers (merge review and PR
// remediation). Each must build every ADO provider from the delivered value of
// a capability its manifest row names, and exactly the capabilities listed:
// pull-request work on the project provider from github:pr:write (or
// provider:pr:write), backlog work items from github:issues:*.
//
// merge-pr and merge-queue-poll consume ado:pr:complete for completion today;
// ADO-N2 moves landing authority onto github:pr:merge through this seam.
// apply-verdict and elect-lander read a journaled verdict before they build a
// provider, so their end-to-end ADO tests (TestRunApplyVerdictADO*,
// TestElectLanderDispatchesADOAndElectsCandidate) assert the same rule.
func TestADOStageProvidersConsumeTheDeclaredCapability(t *testing.T) {
	prWrite, issuesWrite, issuesRead := string(capability.GitHubPRWrite), string(capability.GitHubIssuesWrite), string(capability.GitHubIssuesRead)
	for _, tc := range []struct {
		command string
		args    []string
		inputs  map[string]string
		want    []string
	}{
		{command: "backlog-health", inputs: map[string]string{"trustLabel": providers.LabelApproved}, want: []string{issuesRead}},
		{command: "check-issue-staleness", inputs: map[string]string{"pullNumber": "77", "head": "goobers/implementation/run"}, want: []string{prWrite, issuesWrite}},
		{command: "gather-pr-context", want: []string{prWrite}},
		{command: "gather-sibling-context", inputs: map[string]string{"selectedNumber": "77"}, want: []string{prWrite}},
		{command: "merge-pr", inputs: map[string]string{"pullNumber": "77", "verdict": "pass"}, want: []string{string(capability.ADOPRComplete)}},
		{command: "merge-queue-poll", inputs: map[string]string{"pullNumber": "77"}, want: []string{string(capability.ADOPRComplete)}},
		{command: "open-pr", want: []string{string(capability.ProviderPRWrite)}},
		{command: "post-merge", inputs: map[string]string{"pullNumber": "77"}, want: []string{prWrite, issuesWrite}},
		{command: "pr-select", inputs: map[string]string{"selfIdentity": "goober"}, want: []string{prWrite}},
		{command: "push-remediated", want: []string{prWrite}},
		{command: "rebase-pr", inputs: map[string]string{"selectedNumber": "77", "head": "goobers/pr-remediation/run"}, want: []string{prWrite}},
		{command: "reconcile-post-merge", want: []string{prWrite, issuesWrite}},
		{command: "record-merge-refusal", inputs: map[string]string{"selectedNumber": "77", "selectedHeadSha": "head-sha", "reason": "blocked"}, want: []string{prWrite}},
		{command: "remediation-checkpoint", inputs: map[string]string{"selectedNumber": "77"}, want: []string{prWrite}},
	} {
		t.Run(tc.command, func(t *testing.T) {
			root := initDemo(t)
			t.Setenv("GOOBERS_RUN_ID", "ado-credential-"+tc.command)
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
