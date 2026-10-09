//go:build integration

package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/childpublication"
	"github.com/goobers/goobers/internal/childworkflow"
	"github.com/goobers/goobers/internal/credentials"
	"github.com/goobers/goobers/internal/dispatcher"
	"github.com/goobers/goobers/internal/engine"
	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/recovery"
	"github.com/goobers/goobers/internal/runner"
	"github.com/goobers/goobers/internal/triggerqueue"
	"github.com/goobers/goobers/internal/worktree"
	"github.com/goobers/goobers/providers"
	"github.com/goobers/goobers/test/testsupport/testdep"
)

const publicationChildSource = `apiVersion: goobers.dev/v1alpha1
kind: Workflow
dslVersion: "3.1"
metadata: {name: generated-publication}
spec:
  gaggle: example
  triggers: [{type: manual}]
  start: push
  tasks:
    - name: push
      goal: Publish child branch
      type: deterministic
      runsOn: {os: linux, capabilities: [isolated-child]}
      capabilities: [repo:push]
      policyActions: [push-repository-branch]
      run: {command: [goobers, push-branch], workspace: repo}
      next: open
    - name: open
      goal: Open child PR
      type: deterministic
      runsOn: {os: linux, capabilities: [isolated-child]}
      capabilities: [provider:pr:write]
      policyActions: [open-or-update-pr]
      run: {command: [goobers, open-pr], workspace: repo}
`

type hostPublicationGit struct{ remote string }

func (g hostPublicationGit) Head(ctx context.Context, workspace, _, head string) (string, error) {
	return (childpublication.GitCommand{AllowLocal: true}).Head(ctx, workspace, g.remote, head)
}
func (g hostPublicationGit) Create(ctx context.Context, workspace, _, head, sha string) error {
	return (childpublication.GitCommand{AllowLocal: true}).Create(ctx, workspace, g.remote, head, sha)
}

type hostPublicationPR struct {
	requests []providers.PullRequestRequest
}

func (p *hostPublicationPR) FindPullRequestByBranch(context.Context, providers.RepositoryRef, string, string) (providers.PullRequestResult, bool, error) {
	return providers.PullRequestResult{}, false, nil
}
func (p *hostPublicationPR) CreatePullRequest(_ context.Context, r providers.PullRequestRequest) (providers.PullRequestResult, error) {
	p.requests = append(p.requests, r)
	return providers.PullRequestResult{ID: "7", Number: 7, URL: "https://github.com/own/repo/pull/7"}, nil
}

func TestIntegrationProductionChildFactoryPublishesWithHostOnlyCredential(t *testing.T) {
	testdep.Require(t, "git")
	testProductionChildPublication(t, false)
}
func TestIntegrationProductionChildRunnerPreservesDirtyOutputAcrossPublicationStages(t *testing.T) {
	testdep.Require(t, "git")
	testProductionChildPublication(t, true)
}
func testProductionChildPublication(t *testing.T, drive bool) {
	t.Helper()
	testdep.Require(t, "git")
	parent := t.TempDir()
	recoveryCLIGit(t, parent, "init", "--initial-branch=main")
	if err := os.WriteFile(filepath.Join(parent, "file.txt"), []byte("base"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(parent, "private.txt"), []byte("preserved excluded baseline"), 0600); err != nil {
		t.Fatal(err)
	}
	recoveryCLIGit(t, parent, "add", ".")
	recoveryCLIGit(t, parent, "commit", "-m", "base")
	remote := filepath.Join(t.TempDir(), "remote.git")
	recoveryCLIGit(t, parent, "init", "--bare", remote)
	recoveryCLIGit(t, parent, "push", remote, "main")
	var ownedPath string
	var project apiv1.RepoRef
	parentSource := strings.ReplaceAll(childValidationParent, "[agent:model]", "[agent:model, repo:push, provider:pr:write]")
	parentSource = strings.ReplaceAll(parentSource, "[agent:model, repo:push]", "[agent:model, repo:push, provider:pr:write]")
	f := newChildKitFixtureConfigured(t, childKitFixtureOptions{isolated: true, gooberCapabilities: []string{"agent:model", "repo:push", "provider:pr:write"}, parent: parentSource, source: publicationChildSource, workspace: func(s *daemonCredentialService, c triggerqueue.ChildRecord, m *worktree.Manager) *runner.ChildWorkspaceAdmission {
		set, _, err := loadConfigDirectory(s.layout.ConfigDir())
		if err != nil {
			t.Fatal(err)
		}
		project = set.Gaggles[0].Spec.Project
		url, err := childRepoCloneURL(project)
		if err != nil {
			t.Fatal(err)
		}
		coordinator := childworkflow.WorkspaceCoordinator{Queue: s.childQueue, Worktrees: m}
		if err = coordinator.Capture(t.Context(), c, childworkflow.YieldedWorkspace{Path: parent, RepoURL: url, RepositoryKey: childRepoKey(project), Policy: recovery.SnapshotPolicy{ExcludedPaths: []string{"private.txt"}}}); err != nil {
			t.Fatal(err)
		}
		if err = m.WithRecoveryMirror(t.Context(), url, func(mirror string) error { recoveryCLIGit(t, mirror, "fetch", parent, "main"); return nil }); err != nil {
			t.Fatal(err)
		}
		admission, err := coordinator.Prepare(t.Context(), c, url)
		if err != nil {
			t.Fatal(err)
		}
		owned, err := m.AdoptChildFromSnapshot(t.Context(), worktree.ChildOptions{RepoURL: url, RunID: admission.WorkspaceID, OwnerRunID: c.RunID, Gaggle: c.Identity.Gaggle, SnapshotSHA: admission.ForkSHA})
		if err != nil {
			t.Fatal(err)
		}
		ownedPath = owned.Path
		if err = os.WriteFile(filepath.Join(ownedPath, "child.txt"), []byte("owned child output"), 0600); err != nil {
			t.Fatal(err)
		}
		return admission
	}})

	s := f.writer.service
	s.Replace(credentialPlaneDefinitionsFromSet(f.parent.applied))
	log, _, err := journal.OpenInstanceLog(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = log.Close() }()
	s.log = log
	var resolved []string
	s.buildSources = func(credentialGaggleScope) (credentials.Resolver, []credentials.Grant, error) {

		functions := map[string]credentials.ResolveFunc{}
		grants := []credentials.Grant{}
		for _, key := range []string{"repo:push", "provider:pr:write", "agent:model"} {
			functions[key] = func(context.Context) (string, error) {
				resolved = append(resolved, key)
				return "host-only-token-" + key, nil
			}
			grants = append(grants, credentials.Grant{Capability: key, Ref: key})
		}
		resolver, err := credentials.NewResolverWithExpiring(nil, nil, functions, nil)
		return resolver, grants, err
	}
	pr := &hostPublicationPR{}
	s.childPublisher = func(target childpublication.Target, key string, credential httpapi.MintedCredential, _ string) (childpublication.Publisher, error) {
		expected := "host-only-token-" + key

		if credential.Value != expected || target.Child.RunID != f.child.RunID || !strings.HasPrefix(target.Remote, "https://") || target.Base != "main" {
			t.Fatal("host authority changed", target, key)
		}
		return childpublication.Publisher{Queue: s.childQueue, Git: hostPublicationGit{remote}, PRs: pr}, nil
	}
	start, release, err := (&queuedChildLauncher{layout: s.layout, queue: s.childQueue, authority: s.children}).admittedChildIdentity(t.Context(), f.writer.identity)
	if err != nil {
		t.Fatal(err)
	}
	release()
	plane, err := dispatcher.NewSurrenderDir(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	worker := &factoryWorkerClient{execute: func(context.Context, engine.ChildDispatchInput) (engine.ChildDispatchResult, error) {
		return engine.ChildDispatchResult{}, errors.New("publication must not run in a pod")
	}}
	previousKey := s.config.API.PodTokenKeyFile
	s.config.API.PodTokenKeyFile = "host-key"
	s.installChildPodFactories(worker, plane, childFactoryJournals(t, s))
	s.config.API.PodTokenKeyFile = previousKey
	runtime := preparedChildRuntime{executionGenerationRuntime: executionGenerationRuntime{gooberDigest: f.writer.identity.GooberDigest, repoRef: project}, worktrees: f.manager}
	factories, err := s.childExecutors(t.Context(), start, runtime)
	if err != nil {
		t.Fatal(err)
	}
	recorder := f.writer.recorder.(*journal.Run)
	executor, err := factories.NewDeterministic(recorder, journal.NewRegistryScrubber())
	if err != nil {
		t.Fatal(err)
	}
	ctx, err := credentials.WithChildCeiling(t.Context(), start.Proposal.CredentialCeiling())
	if err != nil {
		t.Fatal(err)
	}
	if drive {
		if err = recorder.Close(); err != nil {
			t.Fatal(err)
		}
		driver, err := f.driver.ForChildExecution(f.writer.identity, factories)
		if err != nil {
			t.Fatal(err)
		}
		result, err := driver.Resume(t.Context(), runner.ResumeInput{RepoRef: project, RunID: f.child.RunID, Machine: start.Proposal.Machine, GooberDigest: f.writer.identity.GooberDigest})
		if err != nil || result.Phase != journal.PhaseCompleted {
			t.Fatal("actual child multi-stage execution", result, err)
		}
	} else {
		for _, stage := range []string{"push", "open"} {
			task, _ := start.Proposal.Machine.Task(stage)
			if err = recorder.Append(journal.Event{Type: journal.EventStageStarted, Stage: stage, Attempt: 1}); err != nil {
				t.Fatal(err)
			}
			env := *f.attempt.Envelope
			env.Goober = ""
			env.TaskID = f.writer.identity.RunID + ":" + stage
			env.Workspace = ownedPath
			env.Capabilities = task.Capabilities
			if stage == "open" {
				body := "body containing host-only-token-repo:push"

				env.Inputs = map[string]any{"title": "child title", "body": body, "draft": true}
			}
			attempt, proof := invoke.WithWorkspaceQuiescence(ctx)
			result, err := executor.Run(attempt, env, *task.Run)
			if err != nil || result.Status != apiv1.ResultSuccess || proof.Verify() != nil {
				t.Fatal(stage, result, err, proof.Verify())
			}
			if err = recorder.Append(journal.Event{Type: journal.EventStageFinished, Stage: stage, Attempt: 1, Status: "success"}); err != nil {
				t.Fatal(err)
			}
		}
	}
	if !slices.Equal(resolved, []string{"repo:push", "provider:pr:write"}) || worker.starts != 0 || len(pr.requests) != 1 {
		t.Fatal("credential or execution escaped host", resolved, worker.starts, pr.requests)
	}
	if got := recoveryCLIGit(t, parent, "--git-dir="+remote, "show", "refs/heads/"+pr.requests[0].Head+":child.txt"); got != "owned child output" {
		t.Fatal("dirty child contribution lost across stages", got)
	}
	if got := recoveryCLIGit(t, parent, "--git-dir="+remote, "show", "refs/heads/"+pr.requests[0].Head+":private.txt"); got != "preserved excluded baseline" {
		t.Fatal("excluded baseline deleted by publication", got)
	}

	if strings.Contains(pr.requests[0].Body, "host-only-token") || strings.Contains(pr.requests[0].Body, "human-publication-token") {
		t.Fatal("credential escaped PR body")
	}
	events, err := journal.OpenReadOnly(recorder.Dir())
	if err != nil {
		t.Fatal(err)
	}
	all, err := events.Events()
	if err != nil {
		t.Fatal(err)
	}
	refs := 0
	for _, e := range all {
		if e.Type == journal.EventRefTouched {
			refs++
		}
	}
	if refs < 2 {
		t.Fatal("confirmed provider effects missing from journal", refs)
	}
	if drive {
		return
	}
	if err = s.childQueue.FenceChildParent(t.Context(), f.child.Identity.ChildParent, "operator", time.Now()); err != nil {
		t.Fatal(err)
	}
	if err = recorder.Append(journal.Event{Type: journal.EventStageStarted, Stage: "push", Attempt: 2}); err != nil {
		t.Fatal(err)
	}
	task, _ := start.Proposal.Machine.Task("push")
	env := *f.attempt.Envelope
	env.Goober = ""
	env.TaskID = f.writer.identity.RunID + ":push"
	env.Attempt = 2
	env.Capabilities = task.Capabilities
	env.Workspace = ownedPath
	if _, err = executor.Run(ctx, env, *task.Run); err == nil || len(resolved) != 2 {
		t.Fatal("cancelled child published or resolved credentials", err, resolved)
	}
}
