package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/claimrelease"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/providers"
)

// continuationTopologyBCode is the Azure DevOps code provider of a topology
// (b) continuation: it serves the source branch and pull-request polls, and
// fails the test if it is asked for a work item, which lives on the GitHub
// backlog.
type continuationTopologyBCode struct {
	providers.Provider
	t          *testing.T
	branch     string
	sha        string
	pullPolled []string
}

func (c *continuationTopologyBCode) GetBranch(_ context.Context, repo providers.RepositoryRef, branch string) (providers.BranchSummary, bool, error) {
	if repo.Provider != providers.ProviderADO || branch != c.branch {
		c.t.Errorf("GetBranch(%+v, %q), want the ADO repository's %q", repo, branch, c.branch)
		return providers.BranchSummary{}, false, nil
	}
	return providers.BranchSummary{Name: branch, SHA: c.sha}, true, nil
}

func (c *continuationTopologyBCode) ListBranches(context.Context, providers.ListBranchesRequest) ([]providers.BranchSummary, error) {
	return nil, nil
}

func (c *continuationTopologyBCode) FindPullRequestByBranch(context.Context, providers.RepositoryRef, string, string) (providers.PullRequestResult, bool, error) {
	return providers.PullRequestResult{}, false, nil
}

func (c *continuationTopologyBCode) DeleteBranch(context.Context, providers.DeleteBranchRequest) (providers.DeleteBranchResult, error) {
	return providers.DeleteBranchResult{}, nil
}

func (c *continuationTopologyBCode) PollPullRequest(_ context.Context, req providers.PullRequestPollRequest) (providers.PullRequestPollResult, error) {
	if req.Repository.Provider != providers.ProviderADO {
		c.t.Errorf("PollPullRequest addressed %+v, want the ADO repository", req.Repository)
	}
	c.pullPolled = append(c.pullPolled, req.PullID)
	return providers.PullRequestPollResult{State: "open"}, nil
}

func (c *continuationTopologyBCode) GetWorkItem(_ context.Context, repo providers.RepositoryRef, id string) (providers.WorkItem, error) {
	c.t.Errorf("GetWorkItem(%+v, %q) on Azure DevOps: a topology (b) claim is a GitHub issue", repo, id)
	return providers.WorkItem{}, errors.New("not on Azure DevOps")
}

// TestRunRunContinueTopologyBRevalidatesTheGitHubClaim continues a topology (b)
// run (Azure DevOps code, GitHub backlog) that claimed a GitHub issue and an
// Azure DevOps pull request. The issue claim, keyed by the backlog provider, is
// re-read from the GitHub backlog repository; the pull-request claim, keyed by
// the code provider, from Azure DevOps. Both are reacquired by the
// continuation. Before this was routed by role the continuation stopped with a
// provider-mismatch error.
func TestRunRunContinueTopologyBRevalidatesTheGitHubClaim(t *testing.T) {
	root := initDeterministicDemo(t)
	topologyBFixture(t, root, "example")
	backlog := newFakeGitHubServer(t, "example-org", "example-backlog")
	backlog.addIssue(7, "Continuation claim", "goobers", "goobers:ready")
	providerCmdEnv(t, backlog, "GOOBERS_CRED_GITHUB_ISSUES_READ", "continuation-topology-b")

	code := &continuationTopologyBCode{t: t, branch: "goobers/continuation-b", sha: "b0b0b0b"}
	previous := stageProviderFactories[providers.ProviderADO]
	t.Cleanup(func() { stageProviderFactories[providers.ProviderADO] = previous })
	stageProviderFactories[providers.ProviderADO] = func(cfg stageProviderConfig) (providers.Provider, error) {
		if cfg.repo != topologyBRouted() {
			t.Errorf("ADO provider opened for %+v, want the code repository", cfg.repo)
		}
		return code, nil
	}

	const sourceID = "historical-source-topology-b"
	machine := seedTopologyBContinuationSource(t, root, sourceID, code.branch, code.sha)
	seedReleasedTopologyBClaim(t, root, sourceID, machine, "github", "7")
	seedReleasedTopologyBClaim(t, root, sourceID, machine, "ado", pullRequestClaimKey(5))

	var stdout, stderr bytes.Buffer
	exitCode := runRunContinue([]string{
		"--from", sourceID, "--terminal-seq", "2", "--target", "local-ci",
		"--operator", "operator@example.test", root,
	}, &stdout, &stderr)
	if exitCode != 0 {
		t.Fatalf("run continue failed: code=%d stderr=%s", exitCode, stderr.String())
	}
	if len(code.pullPolled) != 1 || code.pullPolled[0] != "5" {
		t.Fatalf("ADO pull requests polled = %v, want [5]", code.pullPolled)
	}
	ledger, err := localscheduler.OpenClaimLedger(filepath.Join(instance.NewLayout(root).SchedulerDir(), claimLedgerFileName))
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, claim := range ledger.ForRunAll(strings.TrimSpace(stdout.String())) {
		got[claim.ExternalID] = claim.Provider
	}
	want := map[string]string{"7": "github", pullRequestClaimKey(5): "ado"}
	if len(got) != len(want) || got["7"] != want["7"] || got[pullRequestClaimKey(5)] != want[pullRequestClaimKey(5)] {
		t.Fatalf("continuation claims = %v, want %v", got, want)
	}
}

// TestRunRunContinueTopologyBRejectsClosedGitHubClaim is the fail-closed half:
// a GitHub issue closed since the source run stopped is not reacquired.
func TestRunRunContinueTopologyBRejectsClosedGitHubClaim(t *testing.T) {
	root := initDeterministicDemo(t)
	topologyBFixture(t, root, "example")
	backlog := newFakeGitHubServer(t, "example-org", "example-backlog")
	backlog.addIssue(7, "Closed claim", "goobers", "goobers:ready")
	backlog.closeIssue(7)
	providerCmdEnv(t, backlog, "GOOBERS_CRED_GITHUB_ISSUES_READ", "continuation-topology-b-closed")

	code := &continuationTopologyBCode{t: t, branch: "goobers/continuation-b", sha: "b0b0b0b"}
	previous := stageProviderFactories[providers.ProviderADO]
	t.Cleanup(func() { stageProviderFactories[providers.ProviderADO] = previous })
	stageProviderFactories[providers.ProviderADO] = func(stageProviderConfig) (providers.Provider, error) { return code, nil }

	const sourceID = "historical-source-topology-b-closed"
	machine := seedTopologyBContinuationSource(t, root, sourceID, code.branch, code.sha)
	seedReleasedTopologyBClaim(t, root, sourceID, machine, "github", "7")

	var stdout, stderr bytes.Buffer
	exitCode := runRunContinue([]string{
		"--from", sourceID, "--terminal-seq", "2", "--target", "local-ci",
		"--operator", "operator@example.test", root,
	}, &stdout, &stderr)
	if exitCode == 0 {
		t.Fatalf("run continue unexpectedly succeeded: stdout=%s", stdout.String())
	}
	if !strings.Contains(stderr.String(), `source claim "7" is no longer open (state "closed")`) {
		t.Fatalf("stderr = %s", stderr.String())
	}
}

// A claim keyed by the wrong provider for its kind is refused rather than
// re-read from either repository: a work-item claim keyed by Azure DevOps is
// checked against the GitHub backlog, and a pull-request claim keyed by
// GitHub against the Azure DevOps repository.
func TestRunRunContinueTopologyBRefusesClaimsKeyedByTheWrongProvider(t *testing.T) {
	for _, tc := range []struct {
		name, provider, claim, want string
	}{
		{"issue claim keyed by ADO", "ado", "7", `source claim "7" uses provider "ado", configured repository uses "github"`},
		{"pull-request claim keyed by GitHub", "github", pullRequestClaimKey(5), `source claim "pr/5" uses provider "github", configured repository uses "ado"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := initDeterministicDemo(t)
			topologyBFixture(t, root, "example")
			backlog := newFakeGitHubServer(t, "example-org", "example-backlog")
			backlog.addIssue(7, "Continuation claim", "goobers", "goobers:ready")
			providerCmdEnv(t, backlog, "GOOBERS_CRED_GITHUB_ISSUES_READ", "continuation-topology-b-spoof")
			code := stubTopologyBCode(t)

			const sourceID = "historical-source-topology-b-spoof"
			machine := seedTopologyBContinuationSource(t, root, sourceID, code.branch, code.sha)
			seedReleasedTopologyBClaim(t, root, sourceID, machine, tc.provider, tc.claim)

			exitCode, stdout, stderr := runTopologyBContinue(root, sourceID)
			if exitCode == 0 {
				t.Fatalf("run continue unexpectedly succeeded: stdout=%s", stdout)
			}
			if !strings.Contains(stderr, tc.want) {
				t.Fatalf("stderr = %s, want %q", stderr, tc.want)
			}
			if len(code.pullPolled) != 0 {
				t.Fatalf("ADO pull requests polled = %v, want none", code.pullPolled)
			}
		})
	}
}

// A Gitea backlog for Azure DevOps code is topology (b) too: the claimed issue
// is re-read from the Gitea backlog repository, through the Gitea provider
// that repository's repos[] entry addresses.
func TestRunRunContinueTopologyBRevalidatesAGiteaClaim(t *testing.T) {
	root := initDeterministicDemo(t)
	topologyBFixture(t, root, "example")
	var reads int
	gitea := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/v1/repos/example-org/example-backlog/issues/7" {
			t.Errorf("unexpected Gitea call %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
			return
		}
		reads++
		writeJSONResp(t, w, map[string]any{"number": 7, "title": "Continuation claim", "state": "open",
			"labels": []map[string]any{{"name": "goobers"}, {"name": "goobers:ready"}}})
	}))
	t.Cleanup(gitea.Close)
	replaceInFile(t, filepath.Join(root, "config", "gaggles", "example", "gaggle.yaml"),
		"  backlog:\n    provider: github\n", "  backlog:\n    provider: gitea\n    baseUrl: "+gitea.URL+"\n")
	cfg, err := instance.LoadConfig(instance.NewLayout(root).ConfigFile())
	if err != nil {
		t.Fatal(err)
	}
	cfg.Repos = append(cfg.Repos, instance.RepoRef{Provider: "gitea", Owner: "example-org", Name: "example-backlog",
		BaseURL: gitea.URL, Token: instance.TokenRef{Env: "EXAMPLE_GITEA_TOKEN"}})
	if err := instance.WriteConfig(instance.NewLayout(root).ConfigFile(), cfg); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GOOBERS_CRED_GITHUB_ISSUES_READ", "gitea-read-token")
	code := stubTopologyBCode(t)

	const sourceID = "historical-source-topology-b-gitea"
	machine := seedTopologyBContinuationSource(t, root, sourceID, code.branch, code.sha)
	seedReleasedTopologyBClaim(t, root, sourceID, machine, "gitea", "7")

	exitCode, stdout, stderr := runTopologyBContinue(root, sourceID)
	if exitCode != 0 {
		t.Fatalf("run continue failed: code=%d stderr=%s", exitCode, stderr)
	}
	if reads != 1 {
		t.Fatalf("Gitea issue reads = %d, want 1", reads)
	}
	ledger, err := localscheduler.OpenClaimLedger(filepath.Join(instance.NewLayout(root).SchedulerDir(), claimLedgerFileName))
	if err != nil {
		t.Fatal(err)
	}
	claims := ledger.ForRunAll(strings.TrimSpace(stdout))
	if len(claims) != 1 || claims[0].ExternalID != "7" || claims[0].Provider != "gitea" {
		t.Fatalf("continuation claims = %+v, want the Gitea issue 7", claims)
	}
}

// stubTopologyBCode replaces the Azure DevOps stage provider with the fake
// code provider for the test's duration.
func stubTopologyBCode(t *testing.T) *continuationTopologyBCode {
	t.Helper()
	code := &continuationTopologyBCode{t: t, branch: "goobers/continuation-b", sha: "b0b0b0b"}
	previous := stageProviderFactories[providers.ProviderADO]
	t.Cleanup(func() { stageProviderFactories[providers.ProviderADO] = previous })
	stageProviderFactories[providers.ProviderADO] = func(stageProviderConfig) (providers.Provider, error) { return code, nil }
	return code
}

func runTopologyBContinue(root, sourceID string) (int, string, string) {
	var stdout, stderr bytes.Buffer
	exitCode := runRunContinue([]string{
		"--from", sourceID, "--terminal-seq", "2", "--target", "local-ci",
		"--operator", "operator@example.test", root,
	}, &stdout, &stderr)
	return exitCode, stdout.String(), stderr.String()
}

func seedTopologyBContinuationSource(t *testing.T, root, sourceID, branch, sha string) string {
	t.Helper()
	machine, err := currentWorkflowMachine(root, journal.RunIdentity{
		Workflow: "default-implement", WorkflowVersion: 1, Gaggle: "example",
	})
	if err != nil {
		t.Fatalf("currentWorkflowMachine: %v", err)
	}
	definition, err := json.Marshal(machine.Def)
	if err != nil {
		t.Fatal(err)
	}
	routed := topologyBRouted()
	source, err := journal.Create(instance.NewLayout(root).RunsDir(), journal.RunIdentity{
		RunID: sourceID, Workflow: machine.Def.Name, WorkflowVersion: machine.Def.Version,
		WorkflowDigest: machine.Digest(), Gaggle: "example",
		WorkspaceBranch:    branch,
		WorkspaceBranchSHA: sha,
		WorkspaceRepository: &apiv1.RepoRef{
			Provider: apiv1.ProviderADO, Owner: routed.Owner, Project: routed.Project, Name: routed.Name,
		},
		Trigger: journal.Trigger{Kind: journal.TriggerManual},
	}, map[string][]byte{journal.PinnedWorkflowDefinitionInputName: definition},
		journal.WithInputIntegrity(map[string]apiv1.Integrity{
			journal.PinnedWorkflowDefinitionInputName: apiv1.IntegrityTrusted,
		}))
	if err != nil {
		t.Fatal(err)
	}
	if err := source.Append(journal.Event{Type: journal.EventRunFinished, Status: string(journal.PhaseEscalated)}); err != nil {
		t.Fatal(err)
	}
	if err := source.Close(); err != nil {
		t.Fatal(err)
	}
	return machine.Def.Name
}

func seedReleasedTopologyBClaim(t *testing.T, root, sourceID, workflowName, provider, externalID string) {
	t.Helper()
	ledger, err := localscheduler.OpenClaimLedger(filepath.Join(instance.NewLayout(root).SchedulerDir(), claimLedgerFileName))
	if err != nil {
		t.Fatal(err)
	}
	key := localscheduler.ClaimKey{Gaggle: "example", Provider: provider, ExternalID: externalID}
	if ok, _, err := ledger.ClaimScoped(key, sourceID, workflowName, time.Hour); err != nil || !ok {
		t.Fatalf("seed source claim: ok=%v err=%v", ok, err)
	}
	if err := ledger.ReleaseScoped(key, sourceID, claimrelease.Completed.Reason()); err != nil {
		t.Fatalf("release source claim: %v", err)
	}
}
