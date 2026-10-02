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

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/capability"
	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/providers"
)

// Topology (b) (docs/design/ado-parity-dsl-2-0.md §7.2, ADO-N31): a GitHub
// backlog for Azure DevOps code. Neutral placeholders throughout.
const (
	topologyBOrg         = "example-org"
	topologyBCodeProject = "example-project"
	topologyBCodeRepo    = "service"
	topologyBBacklog     = "example-org/example-backlog"
	topologyBIssueURL    = "https://github.com/example-org/example-backlog/issues/"
)

func topologyBRouted() providers.RepositoryRef {
	return providers.RepositoryRef{Provider: providers.ProviderADO, Owner: topologyBOrg, Project: topologyBCodeProject, Name: topologyBCodeRepo}
}

func topologyBBacklogRef() providers.RepositoryRef {
	return providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "example-org", Name: "example-backlog"}
}

// open-pr never writes a bare "#<id>" into an ADO pull request whose item is
// a GitHub issue: the item's full URL replaces it, in the generic body and in
// the structured one.
func TestPRIssueReferenceTopologyB(t *testing.T) {
	backlog := topologyBBacklogRef()
	if got, want := backlogIssueURL(backlog, "42"), topologyBIssueURL+"42"; got != want {
		t.Fatalf("backlogIssueURL = %q, want %q", got, want)
	}
	gitea := providers.RepositoryRef{Provider: providers.ProviderGitea, Owner: "example-org", Name: "example-backlog", URL: "https://gitea.example.com/"}
	if got, want := backlogIssueURL(gitea, "42"), "https://gitea.example.com/example-org/example-backlog/issues/42"; got != want {
		t.Fatalf("backlogIssueURL(gitea) = %q, want %q", got, want)
	}

	body := formatStructuredPRBody("42", backlogIssueURL(backlog, "42"), "Add the widget", "", "", "sha256:abc", nil, nil, nil)
	if strings.Contains(body, "#42") {
		t.Fatalf("structured body carries a bare #42 in topology (b):\n%s", body)
	}
	for _, want := range []string{"Implements " + topologyBIssueURL + "42: **Add the widget**.", "Fixes " + topologyBIssueURL + "42"} {
		if !strings.Contains(body, want) {
			t.Fatalf("structured body missing %q:\n%s", want, body)
		}
	}

	// Same-provider gaggles keep "#<id>" byte-identical.
	same := formatStructuredPRBody("42", "#42", "Add the widget", "", "", "sha256:abc", nil, nil, nil)
	for _, want := range []string{"Implements #42: **Add the widget**.", "Fixes #42"} {
		if !strings.Contains(same, want) {
			t.Fatalf("same-provider structured body missing %q:\n%s", want, same)
		}
	}
}

// Post-merge in topology (b) closes only the full-URL references into the
// backlog; a bare "#<id>" there names an ADO work item and is never taken for
// the GitHub issue with that number.
func TestPostMergeClosingIDsTopologyB(t *testing.T) {
	routed, backlog := topologyBRouted(), topologyBBacklogRef()
	body := "Fixes #7\n\nFixes " + topologyBIssueURL + "42\nCloses https://github.com/example-org/other-repo/issues/9\nresolves " + topologyBIssueURL + "43"
	if got, want := postMergeClosingIDs(body, routed, backlog), []string{"42", "43"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("postMergeClosingIDs = %v, want %v", got, want)
	}
	// The ADO project split keeps the "#<id>" grammar.
	split := routed
	split.Project = "example-backlog-project"
	if got, want := postMergeClosingIDs(body, routed, split), []string{"7"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("same-provider postMergeClosingIDs = %v, want %v", got, want)
	}
}

// The ADO squash message carries "Closes <issue URL>" in topology (b), never
// "Closes #<id>", which ADO would resolve to one of its own work items.
func TestADOMergeCommitMessageTopologyB(t *testing.T) {
	poll := providers.PullRequestPollResult{
		Title: "Add the widget",
		Body:  "Automated PR.\n\nFixes " + topologyBIssueURL + "42",
	}
	_, message, err := adoMergeCommitMessage(poll, nil, topologyBRouted(), topologyBBacklogRef())
	if err != nil {
		t.Fatalf("adoMergeCommitMessage: %v", err)
	}
	if strings.Contains(message, "#") {
		t.Fatalf("squash message carries a bare # reference in topology (b): %q", message)
	}
	if want := "Closes " + topologyBIssueURL + "42"; message != want {
		t.Fatalf("squash message = %q, want %q", message, want)
	}

	poll.Body = "Automated PR.\n\nFixes #42"
	_, message, err = adoMergeCommitMessage(poll, nil, topologyBRouted(), topologyBRouted())
	if err != nil {
		t.Fatalf("adoMergeCommitMessage: %v", err)
	}
	if message != "Closes #42" {
		t.Fatalf("same-provider squash message = %q, want %q", message, "Closes #42")
	}
}

// topologyBCloser records every backlog call post-merge makes.
type topologyBCloser struct {
	repos    []providers.RepositoryRef
	ids      []string
	comments []string
}

func (c *topologyBCloser) GetWorkItem(_ context.Context, repo providers.RepositoryRef, id string) (providers.WorkItem, error) {
	c.repos, c.ids = append(c.repos, repo), append(c.ids, id)
	return providers.WorkItem{ID: id, State: "open"}, nil
}

func (c *topologyBCloser) ListComments(context.Context, providers.RepositoryRef, string) ([]providers.Comment, error) {
	return nil, nil
}

func (c *topologyBCloser) UpdateWorkItem(_ context.Context, req providers.UpdateWorkItemRequest) (providers.WorkItem, error) {
	c.repos = append(c.repos, req.Repository)
	if req.Comment != "" {
		c.comments = append(c.comments, req.Comment)
	}
	return providers.WorkItem{ID: req.ID}, nil
}

func (c *topologyBCloser) UpdateWorkItemStatus(_ context.Context, req providers.UpdateWorkItemStatusRequest) (providers.WorkItem, error) {
	c.repos = append(c.repos, req.Repository)
	return providers.WorkItem{ID: req.ID, State: "closed"}, nil
}

func TestPostMergeADOTopologyBClosesTheGitHubIssue(t *testing.T) {
	closer := &topologyBCloser{}
	const pullURL = "https://dev.azure.com/example-org/example-project/_git/service/pullrequest/5"
	poll := providers.PullRequestPollResult{Merged: true, URL: pullURL, Body: "Fixes #7\n\nFixes " + topologyBIssueURL + "42"}
	var stdout, stderr strings.Builder
	errs := performPostMergeADOWithOrigin(context.Background(), closer, nil, topologyBBacklogRef(), poll, "5", t.TempDir(), "example", topologyBRouted(), &stdout, &stderr)
	if len(errs) != 0 {
		t.Fatalf("post-merge errors: %v", errs)
	}
	if !reflect.DeepEqual(closer.ids, []string{"42"}) {
		t.Fatalf("post-merge read items %v, want only the GitHub issue 42 (never ADO work item 7)", closer.ids)
	}
	for _, repo := range closer.repos {
		if repo != topologyBBacklogRef() {
			t.Fatalf("post-merge addressed %+v, want only the GitHub backlog", repo)
		}
	}
	if !strings.Contains(stdout.String(), "closed 1 work item(s)") {
		t.Fatalf("stdout = %q, want one closed item", stdout.String())
	}
	// The GitHub issue names the ADO pull request by URL: "#5" there would
	// link issue or pull request 5 of the backlog repository.
	if want := []string{"Merged in pull request " + pullURL + "."}; !reflect.DeepEqual(closer.comments, want) {
		t.Fatalf("close-out comments = %q, want %q", closer.comments, want)
	}
}

// postMergePullRequestRef keeps "#<n>" on a same-provider gaggle and never
// writes a bare "#<n>" onto a (b) backlog issue.
func TestPostMergePullRequestRefTopologyB(t *testing.T) {
	routed, backlog := topologyBRouted(), topologyBBacklogRef()
	if got := postMergePullRequestRef("5", "https://example.test/pr/5", routed, routed); got != "#5" {
		t.Fatalf("same-provider ref = %q, want #5", got)
	}
	if got := postMergePullRequestRef("5", " https://example.test/pr/5 ", routed, backlog); got != "https://example.test/pr/5" {
		t.Fatalf("topology (b) ref = %q, want the pull request URL", got)
	}
	if got := postMergePullRequestRef("5", "", routed, backlog); strings.Contains(got, "#") {
		t.Fatalf("topology (b) ref without a URL = %q, want no bare #", got)
	}
}

// Issue text open-pr embeds in a (b) pull request description names the
// backlog's issues by URL, never by a bare "#<n>" that Azure DevOps would read
// as one of its work items. Same-provider gaggles embed it unchanged.
func TestCrossProviderIssueTextTopologyB(t *testing.T) {
	const text = "- [ ] depends on #7 (see #8, #9)\n## Heading\nit&#39;s ok, page#3 and /x#4 stay"
	ref := backlogIssueURL(topologyBBacklogRef(), "42")
	want := "- [ ] depends on " + topologyBIssueURL + "7 (see " + topologyBIssueURL + "8, " + topologyBIssueURL + "9)\n## Heading\nit&#39;s ok, page#3 and /x#4 stay"
	if got := crossProviderIssueText(text, "42", ref); got != want {
		t.Fatalf("crossProviderIssueText =\n%q\nwant\n%q", got, want)
	}
	if got := crossProviderIssueText(text, "42", "#42"); got != text {
		t.Fatalf("same-provider text rewritten: %q", got)
	}

	body := formatStructuredPRBody("42", ref, "Add the widget", "## Acceptance criteria\n\n- [ ] after #7 lands\n", "", "sha256:abc", nil, nil, nil)
	if strings.Contains(body, "#7") || !strings.Contains(body, "after "+topologyBIssueURL+"7 lands") {
		t.Fatalf("structured (b) body embeds a bare #7:\n%s", body)
	}

	// The issue title and the agent-authored verdict text are rewritten too.
	reviews := []prBodyReview{{verdict: apiv1.Verdict{
		Decision:  apiv1.VerdictPass,
		Summary:   "closes the gap from #11",
		Rationale: "matches #12",
		Findings:  []apiv1.Finding{{Severity: apiv1.SeverityInfo, Message: "revisit #13"}},
	}}}
	body = formatStructuredPRBody("42", ref, "Follow-up to #10", "", "", "sha256:abc", reviews, nil, nil)
	for _, n := range []string{"10", "11", "12", "13"} {
		if strings.Contains(body, "#"+n) || !strings.Contains(body, topologyBIssueURL+n) {
			t.Fatalf("structured (b) body embeds a bare #%s:\n%s", n, body)
		}
	}
	if reviews[0].verdict.Summary != "closes the gap from #11" || reviews[0].verdict.Findings[0].Message != "revisit #13" {
		t.Fatalf("the caller's verdict was mutated: %+v", reviews[0].verdict)
	}
	same := formatStructuredPRBody("42", "#42", "Follow-up to #10", "", "", "sha256:abc", reviews, nil, nil)
	for _, n := range []string{"10", "11", "12", "13"} {
		if !strings.Contains(same, "#"+n) {
			t.Fatalf("same-provider body lost #%s:\n%s", n, same)
		}
	}
}

// The role-aware credential binding (§7.2 step 2): in a (b) gaggle the
// backlog-family capabilities are backed by the GitHub backlog repository's
// credential and every other capability by the ADO project's.
func TestBuildGaggleCredentialsTopologyB(t *testing.T) {
	cfg := &instance.Config{Repos: []instance.RepoRef{
		{Provider: "ado", Owner: topologyBOrg, Project: topologyBCodeProject, Name: topologyBCodeRepo, Token: instance.TokenRef{Env: "TOPOLOGY_B_ADO_PAT"}},
		{Provider: "github", Owner: "example-org", Name: "example-backlog", Token: instance.TokenRef{Env: "TOPOLOGY_B_GITHUB_TOKEN"}},
	}}
	t.Setenv("TOPOLOGY_B_ADO_PAT", "ado-pat")
	t.Setenv("TOPOLOGY_B_GITHUB_TOKEN", "github-token")
	project := apiv1.RepoRef{Provider: apiv1.ProviderADO, Owner: topologyBOrg, Project: topologyBCodeProject, Name: topologyBCodeRepo}
	backlog := apiv1.BacklogRef{Provider: apiv1.ProviderGitHub, Project: topologyBBacklog}

	_, grants, err := buildGaggleCredentials(cfg, nil, project, backlog, nil, nil)
	if err != nil {
		t.Fatalf("buildGaggleCredentials: %v", err)
	}
	refs := map[string]string{}
	for _, grant := range grants {
		refs[grant.Capability] = grant.Ref
	}
	const adoRef, githubRef = "example-org/example-project/service", "example-org/example-backlog"
	for capabilityName, want := range map[capability.Capability]string{
		capability.GitHubIssuesRead:      githubRef,
		capability.GitHubIssuesWrite:     githubRef,
		capability.GitHubIssuesApprove:   githubRef,
		capability.GitHubMilestonesWrite: githubRef,
		capability.GitHubPRWrite:         adoRef,
		capability.GitHubPRMerge:         adoRef,
		capability.ProviderPRWrite:       adoRef,
		capability.RepoPush:              adoRef,
	} {
		if got := refs[string(capabilityName)]; got != want {
			t.Errorf("%s backed by %q, want %q", capabilityName, got, want)
		}
	}

	// A daemon identity is a GitHub identity: in (b) it backs only the
	// backlog-family part of the daemon-mutation set, and every pull-request
	// and repository capability keeps the ADO repository's credential.
	withIdentity := *cfg
	withIdentity.DaemonIdentity = &instance.DaemonIdentityConfig{Kind: instance.GitHubAuthPAT, Token: &instance.TokenRef{Env: "TOPOLOGY_B_DAEMON_PAT"}}
	t.Setenv("TOPOLOGY_B_DAEMON_PAT", "daemon-token")
	_, identityGrants, err := buildGaggleCredentials(&withIdentity, nil, project, backlog, nil, nil)
	if err != nil {
		t.Fatalf("buildGaggleCredentials with a daemon identity: %v", err)
	}
	identityRefs := map[string]string{}
	for _, grant := range identityGrants {
		identityRefs[grant.Capability] = grant.Ref
	}
	for capabilityName, want := range map[capability.Capability]string{
		capability.GitHubIssuesWrite:  daemonIdentityRefName,
		capability.GitHubIssuesRead:   githubRef,
		capability.GitHubPRWrite:      adoRef,
		capability.GitHubPRMerge:      adoRef,
		capability.GitHubBranchDelete: adoRef,
		capability.RepoPush:           adoRef,
		capability.ProviderCICancel:   adoRef,
	} {
		if got := identityRefs[string(capabilityName)]; got != want {
			t.Errorf("with a daemon identity %s backed by %q, want %q", capabilityName, got, want)
		}
	}

	// Same-provider gaggles keep the single project binding.
	_, same, err := buildGaggleCredentials(cfg, nil, project, apiv1.BacklogRef{Provider: apiv1.ProviderADO, Project: "example-backlog-project"}, nil, nil)
	if err != nil {
		t.Fatalf("buildGaggleCredentials(same provider): %v", err)
	}
	for _, grant := range same {
		if grant.Ref == githubRef {
			t.Fatalf("same-provider gaggle bound %s to the GitHub repository", grant.Capability)
		}
	}
}

// topologyBFixture turns the demo instance's gaggle into topology (b): code in
// an ADO project, backlog in a GitHub repository.
func topologyBFixture(t *testing.T, root, gaggle string) {
	t.Helper()
	gagglePath := filepath.Join(root, "config", "gaggles", gaggle, "gaggle.yaml")
	raw, err := os.ReadFile(gagglePath)
	if err != nil {
		t.Fatalf("read gaggle: %v", err)
	}
	updated := string(raw)
	for _, sub := range []struct{ old, new string }{
		{
			"  project:\n    provider: github\n    owner: your-org\n    name: your-repo\n",
			"  project:\n    provider: ado\n    owner: " + topologyBOrg + "\n    project: " + topologyBCodeProject + "\n    name: " + topologyBCodeRepo + "\n",
		},
		{
			"  backlog:\n    provider: github\n    project: your-org/your-repo\n",
			"  backlog:\n    provider: github\n    project: " + topologyBBacklog + "\n",
		},
	} {
		next := strings.Replace(updated, sub.old, sub.new, 1)
		if next == updated {
			t.Fatalf("starter gaggle did not contain %q:\n%s", sub.old, raw)
		}
		updated = next
	}
	if err := os.WriteFile(gagglePath, []byte(updated), 0o644); err != nil {
		t.Fatalf("write gaggle: %v", err)
	}
}

// TestOpenPRTopologyBLinksTheIssueByURL drives open-pr for a (b) gaggle: the
// ADO pull request's description names the GitHub issue by URL, never "#42",
// and no ADO work item is read or linked.
func TestOpenPRTopologyBLinksTheIssueByURL(t *testing.T) {
	const runID = "run-topology-b"
	root := initDemo(t)
	topologyBFixture(t, root, "example")
	recordClaimedIssue(t, root, runID, "42", "Follow-up to #123")

	repo := topologyBRouted()
	t.Setenv(executor.RepoProviderEnvVar, string(repo.Provider))
	t.Setenv(executor.RepoOwnerEnvVar, repo.Owner)
	t.Setenv(executor.RepoProjectEnvVar, repo.Project)
	t.Setenv(executor.RepoNameEnvVar, repo.Name)
	t.Setenv("GOOBERS_GAGGLE", "example")
	t.Setenv("GOOBERS_RUN_ID", runID)
	t.Setenv("GOOBERS_WORKFLOW", "implementation")

	var description, title string
	mux := http.NewServeMux()
	mux.HandleFunc("/"+topologyBOrg+"/"+topologyBCodeProject+"/_apis/git/repositories/"+topologyBCodeRepo+"/pullrequests", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			writeJSONResp(t, w, map[string]interface{}{"value": []interface{}{}})
		case http.MethodPost:
			var posted struct {
				Title       string `json:"title"`
				Description string `json:"description"`
			}
			if err := json.NewDecoder(r.Body).Decode(&posted); err != nil {
				t.Errorf("decode pull request: %v", err)
			}
			title, description = posted.Title, posted.Description
			writeJSONResp(t, w, map[string]interface{}{
				"pullRequestId": 5,
				"_links":        map[string]interface{}{"web": map[string]string{"href": "https://dev.azure.com/example-org/example-project/_git/service/pullrequest/5"}},
			})
		default:
			t.Errorf("unexpected %s pull request call", r.Method)
		}
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected ADO call %s %s: a GitHub backlog item is never read or linked on ADO", r.Method, r.URL.Path)
		http.NotFound(w, r)
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	t.Setenv(executor.CredentialEnvVar("provider:pr:write"), "pr-write-token")
	previous := newADOProviderForStage
	newADOProviderForStage = func(routed providers.RepositoryRef, _ providers.ADOCredentialSource) (*providers.ADOProvider, error) {
		return providers.NewADOProvider(routed.Owner, routed.Project, "token",
			func(p *providers.ADOProvider) { p.BaseURL = server.URL }), nil
	}
	t.Cleanup(func() { newADOProviderForStage = previous })

	workDir := t.TempDir()
	t.Chdir(workDir)
	code, stdout, stderr := runArgs(t, "open-pr", root)
	if code != 0 {
		t.Fatalf("open-pr: code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	if !strings.Contains(description, "Fixes "+topologyBIssueURL+"42") {
		t.Fatalf("pull request description = %q, want the GitHub issue URL", description)
	}
	if strings.Contains(description, "#42") {
		t.Fatalf("pull request description carries a bare #42: %q", description)
	}
	// The PR title defaults to the issue title and becomes the ADO squash
	// commit title, so a "#123" in it would name ADO work item 123.
	if title != "Follow-up to "+topologyBIssueURL+"123" {
		t.Fatalf("pull request title = %q, want the bare #123 rewritten to the backlog issue URL", title)
	}
	if !strings.Contains(stderr, "github:issues:read") {
		t.Fatalf("stderr = %q, want the skipped staleness re-check to name the missing capability", stderr)
	}
}

// issue-close-out in topology (b) splits by role: work items go to the GitHub
// backlog provider and the pull-request lookup to the ADO provider, which is
// skipped when the stage holds no pull-request credential.
func TestIssueCloseOutProviderSplitsTopologyB(t *testing.T) {
	var opened []providers.RepositoryRef
	for _, kind := range []providers.ProviderKind{providers.ProviderADO, providers.ProviderGitHub} {
		previous := stageProviderFactories[kind]
		t.Cleanup(func() { stageProviderFactories[kind] = previous })
	}
	stageProviderFactories[providers.ProviderADO] = func(cfg stageProviderConfig) (providers.Provider, error) {
		opened = append(opened, cfg.repo)
		return providers.NewADOProvider(cfg.repo.Owner, cfg.repo.Project, "ado-token"), nil
	}
	stageProviderFactories[providers.ProviderGitHub] = func(cfg stageProviderConfig) (providers.Provider, error) {
		opened = append(opened, cfg.repo)
		if cfg.capability != capability.GitHubIssuesWrite {
			t.Errorf("backlog provider opened with %s, want github:issues:write", cfg.capability)
		}
		return providers.NewGitHubProvider("github-token"), nil
	}

	var stderr strings.Builder
	provider, err := openIssueCloseOutProvider(t.TempDir(), topologyBRouted(), topologyBBacklogRef(), &stderr)
	if err != nil {
		t.Fatalf("openIssueCloseOutProvider: %v", err)
	}
	split, ok := provider.(splitIssueCloseOutProvider)
	if !ok {
		t.Fatalf("provider = %T, want the topology (b) split", provider)
	}
	if !reflect.DeepEqual(opened, []providers.RepositoryRef{topologyBBacklogRef()}) {
		t.Fatalf("opened %v, want only the GitHub backlog (no pull-request credential declared)", opened)
	}
	if _, found, err := split.FindPullRequestByBranch(context.Background(), topologyBRouted(), "head", "main"); err != nil || found {
		t.Fatalf("FindPullRequestByBranch without a PR credential = (%v, %v), want not found", found, err)
	}
	if !strings.Contains(stderr.String(), "github:pr:write") {
		t.Fatalf("stderr = %q, want the skipped lookup to name github:pr:write", stderr.String())
	}

	t.Setenv(executor.CredentialEnvVar(string(capability.GitHubPRWrite)), "ado-pr-token")
	opened = nil
	if _, err := openIssueCloseOutProvider(t.TempDir(), topologyBRouted(), topologyBBacklogRef(), &stderr); err != nil {
		t.Fatalf("openIssueCloseOutProvider with a PR credential: %v", err)
	}
	if !reflect.DeepEqual(opened, []providers.RepositoryRef{topologyBBacklogRef(), topologyBRouted()}) {
		t.Fatalf("opened %v, want the GitHub backlog then the ADO project", opened)
	}
}

// TestTopologyBBacklogStagesDispatchToTheBacklogProvider is the topology (b)
// row set of the provider dispatch conformance gate: every backlog stage of a
// (b) gaggle builds its issue provider on GitHub, for the backlog repository,
// from the github:issues:* capability its manifest row declares, and builds no
// Azure DevOps provider for that work.
func TestTopologyBBacklogStagesDispatchToTheBacklogProvider(t *testing.T) {
	for _, tc := range []struct {
		command string
		args    []string
		want    capability.Capability
		setup   func(*testing.T)
	}{
		{"backlog-query", []string{"--read-only"}, capability.GitHubIssuesRead, func(t *testing.T) {
			t.Setenv(executor.InputEnvVar("trustLabel"), "goobers:approved")
		}},
		{"backlog-assignment", nil, capability.GitHubIssuesWrite, func(t *testing.T) {
			t.Setenv(executor.InputEnvVar("trustLabel"), "goobers:approved")
			t.Setenv(executor.InputEnvVar("strategy"), assignmentStrategyConstantCap)
			t.Setenv(executor.InputEnvVar("roster"), `[{"assignee":"goober","maxOpen":1}]`)
		}},
		{"backlog-dedupe", nil, capability.GitHubIssuesRead, func(t *testing.T) {
			t.Setenv("GOOBERS_RUN_ID", "dispatch-backlog-dedupe")
			t.Setenv("GOOBERS_WORKFLOW", "backlog-curation")
		}},
		// The decomposition stages address the backlog too: the parent issue
		// and its children live there, never on the ADO code provider.
		{"select-source", nil, capability.GitHubIssuesWrite, func(t *testing.T) {
			t.Setenv(executor.InputEnvVar("trustLabel"), providers.LabelApproved)
		}},
		{"validate-plan", nil, capability.GitHubIssuesRead, setupTopologyBValidatePlan},
		{"publish-batch", nil, capability.GitHubIssuesWrite, setupTopologyBPublishBatch},
		// Milestones live with the backlog issues, and
		// github:milestones:write is bound to the backlog credential.
		{"set-milestone", []string{"--item", "7", "--milestone", "22"}, capability.GitHubMilestonesWrite, func(*testing.T) {}},
	} {
		t.Run(tc.command, func(t *testing.T) {
			root := initDemo(t)
			topologyBFixture(t, root, "example")
			tc.setup(t)
			repo := topologyBRouted()
			t.Setenv(executor.RepoProviderEnvVar, string(repo.Provider))
			t.Setenv(executor.RepoOwnerEnvVar, repo.Owner)
			t.Setenv(executor.RepoProjectEnvVar, repo.Project)
			t.Setenv(executor.RepoNameEnvVar, repo.Name)
			t.Setenv("GOOBERS_GAGGLE", "example")
			t.Setenv("GOOBERS_INPUT_RESULTFILE", filepath.Join(t.TempDir(), "result.json"))
			deliverEveryADOStageCapability(t)

			previousADO := newADOProviderForStage
			newADOProviderForStage = func(routed providers.RepositoryRef, _ providers.ADOCredentialSource) (*providers.ADOProvider, error) {
				t.Errorf("%s built an Azure DevOps provider for %+v; backlog work belongs to the GitHub backlog", tc.command, routed)
				return nil, errors.New(dispatchProbeError)
			}
			t.Cleanup(func() { newADOProviderForStage = previousADO })
			previousGitHub := stageProviderFactories[providers.ProviderGitHub]
			t.Cleanup(func() { stageProviderFactories[providers.ProviderGitHub] = previousGitHub })
			var built []stageProviderConfig
			stageProviderFactories[providers.ProviderGitHub] = func(cfg stageProviderConfig) (providers.Provider, error) {
				built = append(built, cfg)
				return nil, errors.New(dispatchProbeError)
			}

			args := append(append([]string{tc.command}, tc.args...), root)
			code, _, stderr := runArgs(t, args...)
			if code != 1 || len(built) == 0 || !strings.Contains(stderr, dispatchProbeError) {
				t.Fatalf("code = %d, built = %d, stderr = %q; want the GitHub dispatch probe failure", code, len(built), stderr)
			}
			if built[0].repo != topologyBBacklogRef() {
				t.Fatalf("%s opened %+v, want the GitHub backlog %+v", tc.command, built[0].repo, topologyBBacklogRef())
			}
			if built[0].capability != tc.want {
				t.Fatalf("%s built its backlog provider from %q, want %q", tc.command, built[0].capability, tc.want)
			}
			assertManifestDeclares(t, tc.command, string(tc.want))
		})
	}
}
