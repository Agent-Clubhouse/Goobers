package main

import (
	"os"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/providerstage"
	"github.com/goobers/goobers/providers"
)

type providerDispatchEvidence struct {
	test func(*testing.T)
}

// CONF-8 (#2497) and CONF-9 (#2498) landed before this gate, so their fixed
// commands are coverage entries rather than stale allowlist entries.
var providerDispatchCoverage = map[string]providerDispatchEvidence{
	"apply-verdict":            {test: TestCloseMootPullRequestDispatchesToADO},
	"backlog-assignment":       {test: TestADOBacklogStageCallSitesUseProviderDispatch},
	"backlog-dedupe":           {test: TestBacklogDedupeProviderDispatchesADOAndGitea},
	"backlog-health":           {test: TestBacklogHealthCommandRunsWithADO},
	"backlog-query":            {test: TestADOBacklogStageCallSitesUseProviderDispatch},
	"gather-ci-failures":       {test: TestRemediationStageCallSitesUseProviderDispatch},
	"gather-implement-context": {test: TestImplementationContextProviderDispatchesADOAndGitea},
	"gather-issue-context":     {test: TestRemediationStageCallSitesUseProviderDispatch},
	"gather-pr-context":        {test: TestRemediationStageCallSitesUseProviderDispatch},
	"gather-review-threads":    {test: TestRemediationStageCallSitesUseProviderDispatch},
	"issue-close-out":          {test: TestADOBacklogStageCallSitesUseProviderDispatch},
	"open-pr":                  {test: TestOpenPRRoutesADOThroughExecutorInjectedAuthentication},
	"pr-claim":                 {test: TestRemediationStageCallSitesUseProviderDispatch},
	"push-branch":              {test: TestADORepoForOriginRequiresExactConfiguredRemote},
	"push-remediated":          {test: TestRemediationStageCallSitesUseProviderDispatch},
	"rebase-pr":                {test: TestRemediationStageCallSitesUseProviderDispatch},
	"remediation-checkpoint":   {test: TestRemediationStageCallSitesUseProviderDispatch},
	"report-pr-status":         {test: TestReportPRStatusRequiresPRNumber},
	"respond-to-findings":      {test: TestRespondToFindingsDispatchesToGitea},
	"update-behind-pr":         {test: TestUpdateBehindPRDispatchesToGitea},
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
	"select-source":          "This decomposition command is scoped to GitHub parent issues and requires github:issues:write.",
	"set-milestone":          "Milestones are GitHub-only; the command help explicitly says GitHub milestone and no ADO milestone capability exists.",
	"telemetry-query":        "Provider access is limited to the optional GitHub-only Tutor live-verification format; ordinary telemetry queries are local.",
	"validate-plan":          "This decomposition command validates a live GitHub parent issue and requires github:issues:write.",
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

func TestRemediationStageCallSitesUseProviderDispatch(t *testing.T) {
	root, repo := providerDispatchFixture(t, providers.ProviderGitea)
	provider, err := remediationStageProvider(root, repo, "gitea-token", false)
	if err != nil {
		t.Fatalf("remediationStageProvider: %v", err)
	}
	assertDispatchedProviderKind(t, provider, providers.ProviderGitea)

	assertStageFilesUseProviderDispatch(t, "remediationStageProvider(", []string{
		"gathercifailures.go",
		"gatherissuecontext.go",
		"gatherprcontext.go",
		"gatherreviewthreads.go",
		"prremediationlifecycle.go",
		"pushremediated.go",
		"rebasepr.go",
		"remediationcheckpoint.go",
	})
}

func TestADOBacklogStageCallSitesUseProviderDispatch(t *testing.T) {
	root, repo := providerDispatchFixture(t, providers.ProviderADO)
	provider, err := assignmentProvider(root, repo)
	if err != nil {
		t.Fatalf("assignmentProvider: %v", err)
	}
	assertDispatchedProviderKind(t, provider, providers.ProviderADO)

	assertStageFilesUseProviderDispatch(t, "newADOProviderForStage(", []string{
		"backlogassignment.go",
		"backlogquery.go",
		"issuecloseout.go",
	})
}

func assertStageFilesUseProviderDispatch(t *testing.T, constructor string, files []string) {
	t.Helper()
	for _, file := range files {
		t.Run(file, func(t *testing.T) {
			source, err := os.ReadFile(file)
			if err != nil {
				t.Fatalf("read %s: %v", file, err)
			}
			if !strings.Contains(string(source), constructor) {
				t.Fatalf("%s does not dispatch through %s", file, strings.TrimSuffix(constructor, "("))
			}
		})
	}
}
