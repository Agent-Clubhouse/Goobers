package main

import (
	"context"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/interactiveaccess"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/runner"
	"github.com/goobers/goobers/providers"
)

type restartReadFake struct {
	items, prs []string
	head       string
	state      string
	repos      []providers.RepositoryRef
}

func (f *restartReadFake) GetBranch(context.Context, providers.RepositoryRef, string) (providers.BranchSummary, bool, error) {
	return providers.BranchSummary{SHA: f.head}, f.head != "", nil
}
func (f *restartReadFake) GetWorkItem(_ context.Context, r providers.RepositoryRef, id string) (providers.WorkItem, error) {
	f.items = append(f.items, id)
	f.repos = append(f.repos, r)
	return providers.WorkItem{State: f.state}, nil
}
func (f *restartReadFake) PollPullRequest(_ context.Context, r providers.PullRequestPollRequest) (providers.PullRequestPollResult, error) {
	f.prs = append(f.prs, r.PullID)
	f.repos = append(f.repos, r.Repository)
	return providers.PullRequestPollResult{State: f.state}, nil
}

func TestInteractiveRestartReadsCodeAndBacklogClaimsWithSeparateProviders(t *testing.T) {
	source := journal.RunIdentity{RunID: "source", Gaggle: "web", Workflow: "implement"}
	codeRepo := providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "acme", Name: "code"}
	backlogRepo := providers.RepositoryRef{Provider: providers.ProviderADO, Owner: "org", Project: "work"}
	claims := []localscheduler.ClaimEntry{{RunID: "source", Gaggle: "web", Workflow: "implement", Provider: "github", ExternalID: pullRequestClaimPrefix + "17"}, {RunID: "source", Gaggle: "web", Workflow: "implement", Provider: "ado", ExternalID: "42"}}
	code, backlog := &restartReadFake{state: "open"}, &restartReadFake{state: "open"}
	if err := verifyRestartClaims(t.Context(), source, claims, code, codeRepo, backlog, backlogRepo, nil); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(code.prs, []string{"17"}) || len(code.items) != 0 || !reflect.DeepEqual(backlog.items, []string{"42"}) || len(backlog.prs) != 0 || code.repos[0] != codeRepo || backlog.repos[0] != backlogRepo {
		t.Fatal("provider roles crossed")
	}
	for name, change := range map[string]func(*localscheduler.ClaimEntry){
		"unknown provider":  func(c *localscheduler.ClaimEntry) { c.Provider = "" },
		"wrong provider":    func(c *localscheduler.ClaimEntry) { c.Provider = "github" },
		"foreign gaggle":    func(c *localscheduler.ClaimEntry) { c.Gaggle = "other" },
		"foreign source":    func(c *localscheduler.ClaimEntry) { c.RunID = "other" },
		"legacy missing ID": func(c *localscheduler.ClaimEntry) { c.ExternalID = "" },
		"shared revoked":    func(c *localscheduler.ClaimEntry) { c.SharedRevoked = true },
	} {
		t.Run(name, func(t *testing.T) {
			bad := claims[1]
			change(&bad)
			if err := verifyRestartClaims(t.Context(), source, []localscheduler.ClaimEntry{bad}, code, codeRepo, backlog, backlogRepo, nil); err == nil {
				t.Fatal("ineligible claim accepted")
			}
		})
	}
	backlog.state = "closed"
	if err := verifyRestartClaims(t.Context(), source, claims, code, codeRepo, backlog, backlogRepo, nil); err == nil {
		t.Fatal("closed backlog item accepted")
	}
}

func TestInteractiveRestartAuthorityReplayKeepsOriginalClaimsAndRefusesIdentityCollision(t *testing.T) {
	layout := instance.NewLayout(t.TempDir())
	source := journal.RunIdentity{RunID: "source", Gaggle: "web", Workflow: "implement", WorkflowDigest: journal.Digest([]byte("workflow")), GooberDigest: journal.Digest([]byte("goobers"))}
	runs := layout.ForGaggle("web").RunsDir()
	run, err := journal.Create(runs, source, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := run.Append(journal.Event{Type: journal.EventRunFinished, Status: string(journal.PhaseEscalated)}); err != nil {
		t.Fatal(err)
	}
	if err := run.Close(); err != nil {
		t.Fatal(err)
	}
	reader, err := journal.OpenReadOnly(filepath.Join(runs, source.RunID))
	if err != nil {
		t.Fatal(err)
	}
	events, err := reader.Events()
	if err != nil {
		t.Fatal(err)
	}
	p := httpapi.Principal{Issuer: "https://identity.test", Subject: "team:alice", Roles: []httpapi.Role{httpapi.RoleOperate}, Groups: []string{"original-group"}}
	plan := runner.StageRestartPlan{Source: source, Continuation: journal.ContinuationRequest{RunID: "epoch", SourceRunID: source.RunID, ExpectedTerminalSeq: events[len(events)-1].Seq, Operator: p.Issuer + ":" + p.Subject, Target: "implement"}}
	if err := stampRestartAuthority(layout, p, &plan); err != nil {
		t.Fatal(err)
	}
	original := string(plan.Continuation.Inputs[interactiveaccess.RestartAuthorityInputName])
	run, err = journal.CreateContinuation(runs, plan.Continuation)
	if err != nil {
		t.Fatal(err)
	}
	if err := run.Close(); err != nil {
		t.Fatal(err)
	}
	p.Groups = []string{"current-group"}
	if err := stampRestartAuthority(layout, p, &plan); err != nil {
		t.Fatal(err)
	}
	if string(plan.Continuation.Inputs[interactiveaccess.RestartAuthorityInputName]) != original {
		t.Fatal("retry replaced original verified claims")
	}
	// Same concatenated display reference, different authenticated identity.
	p.Issuer = "https://identity.test:team"
	p.Subject = "alice"
	if err := stampRestartAuthority(layout, p, &plan); err == nil {
		t.Fatal("concatenated principal collision accepted")
	}
}

func TestInteractiveRestartRefusesChangedPublishedBranch(t *testing.T) {
	runs := t.TempDir()
	repo := apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "repo"}
	old, latest := strings.Repeat("a", 40), strings.Repeat("b", 40)
	source := journal.RunIdentity{RunID: "source", Workflow: "wf", WorkspaceBranch: "work", WorkspaceBranchSHA: old, WorkspaceRepository: &repo}
	run, err := journal.Create(runs, source, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := run.Append(journal.Event{Type: journal.EventRefTouched, ExternalRef: &journal.ExternalRef{Kind: "branch", ID: "work", CommitSHA: latest}}); err != nil {
		t.Fatal(err)
	}
	if err := run.Close(); err != nil {
		t.Fatal(err)
	}
	plan := runner.StageRestartPlan{Source: source}
	provider := &restartReadFake{head: latest}
	if err := prepareRestartBranch(t.Context(), runs, &plan, repo, provider); err != nil {
		t.Fatal(err)
	}
	if plan.Continuation.ExpectedSourceSHA != latest {
		t.Fatal("restart selected stale initial branch head")
	}
	provider.head = old
	if err := plan.Continuation.VerifySourceBranch("work", latest); err == nil {
		t.Fatal("branch race accepted")
	}
}

func TestInteractiveRestartSourceScopeCannotDrift(t *testing.T) {
	project := apiv1.RepoRef{Provider: apiv1.ProviderADO, Owner: "org", Project: "code", Name: "repo"}
	scope := credentialGaggleScope{Project: project, Backlog: apiv1.BacklogRef{Provider: apiv1.ProviderADO, Project: "work"}}
	g := &apiv1.Gaggle{Spec: apiv1.GaggleSpec{Project: project, Backlog: scope.Backlog}}
	if !sameRestartBacklog(scope, g) || !restartRepositoryConfigured(project, g) {
		t.Fatal("same source refused")
	}
	g.Spec.Backlog.Project = "other"
	if sameRestartBacklog(scope, g) {
		t.Fatal("foreign backlog accepted")
	}
	g.Spec.Project.BaseURL = "https://other.test"
	if restartRepositoryConfigured(project, g) {
		t.Fatal("foreign repository host accepted")
	}
}
