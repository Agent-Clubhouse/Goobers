package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/interactiveaccess"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/runner"
	"github.com/goobers/goobers/internal/worktree"
)

func interactiveExecutionFixture(t *testing.T) (*schedulerSetup, pinnedChildFixture) {
	t.Helper()
	f := newPinnedChildFixture(t, func(root string) {
		file := filepath.Join(root, "config/gaggles/example/workflows/default-implement.yaml")
		workflow := strings.Split(childValidationParent, "      childWorkflows:")[0] + "      workspace: scratch\n"
		if err := os.WriteFile(file, []byte(workflow), 0600); err != nil {
			t.Fatal(err)
		}
		file = filepath.Join(root, "config/gaggles/example/goobers/coder/goober.yaml")
		raw, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		raw = []byte(strings.ReplaceAll(string(raw), "harness: copilot", "harness: claude-code"))
		if err := os.WriteFile(file, raw, 0600); err != nil {
			t.Fatal(err)
		}
	})
	f.cfg.Sandbox = &instance.SandboxConfig{Agentic: "enforced"}
	f.cfg.Credentials = []instance.CredentialGrant{{Capability: "agent:model", Harness: "claude-code", Token: instance.TokenRef{Env: "HUMAN_MODEL"}}}
	shared := journal.NewRegistryScrubber()
	gaggle := f.applied.Gaggles[0].DeepCopy()
	gaggle.Spec.InteractiveAccess = &apiv1.InteractiveAccessPolicy{Humans: apiv1.InteractiveHumanGrants{Operators: []apiv1.InteractiveHumanGrant{{Issuer: "issuer", Subject: "human"}}}, Actions: []apiv1.InteractiveAction{"run.restartStage", "repository.read", "pr.repair", "backlog.read"}, Credentials: apiv1.InteractiveCredentialBindings{Backlog: "backlog", Repositories: []apiv1.InteractiveRepositoryCredential{{Repository: interactiveRepository(gaggle.Spec.Project), CredentialRef: "repo"}}}}
	repo := gaggle.Spec.Project
	sources := []instance.InteractiveCredential{{Name: "repo", Provider: "github", Owner: repo.Owner, Repository: repo.Name, Token: instance.TokenRef{Env: "HUMAN_REPO"}}, {Name: "backlog", Provider: "github", Owner: repo.Owner, Repository: repo.Name, Token: instance.TokenRef{Env: "HUMAN_BACKLOG"}}}
	access, err := interactiveaccess.New([]apiv1.Gaggle{*gaggle}, sources, interactiveaccess.Dependencies{Registrar: shared})
	if err != nil {
		t.Fatal(err)
	}
	manager, err := worktree.NewManager(f.layout.ForGaggle("example").WorkcopiesDir())
	if err != nil {
		t.Fatal(err)
	}
	base, err := runner.New(runner.Config{RunsDir: f.layout.ForGaggle("example").RunsDir(), ScratchDir: t.TempDir(), Worktrees: manager})
	if err != nil {
		t.Fatal(err)
	}
	setup := &schedulerSetup{Root: f.layout.Root, Config: f.cfg, InteractiveAccess: access, SharedRegistry: shared, Runners: map[string]*runner.Runner{"example": base}}
	return setup, f
}

func TestInteractiveRestartBuilderUsesArchiveAndRemainsLazyUnderPolicyLease(t *testing.T) {
	setup, f := interactiveExecutionFixture(t)
	if err := os.WriteFile(f.sourcePath, []byte("invalid: ["), 0600); err != nil {
		t.Fatal(err)
	}
	// The builder executes inside the same acceptance RLock as production. No
	// credentials exist: any eager resolution or nested authorization fails.
	p := httpapi.Principal{Issuer: "issuer", Subject: "human", Roles: []httpapi.Role{httpapi.RoleOperate}}
	err := setup.InteractiveAccess.WithAuthorization(t.Context(), p, "example", "run.restartStage", func(ctx context.Context) error {
		execution, err := setup.buildInteractiveRestartExecution(ctx, runner.StageRestartPlan{Source: f.parent, Continuation: journal.ContinuationRequest{RunID: "human-epoch"}})
		if err != nil {
			return err
		}
		if execution.Machine.Digest() != f.parent.WorkflowDigest || execution.GooberDigest != f.parent.GooberDigest || execution.RepoRef != f.applied.Gaggles[0].Spec.Project {
			t.Fatal("execution pins changed")
		}
		if _, err := execution.Runner.Start(ctx, runner.StartInput{RunID: "unrelated"}); err == nil {
			t.Fatal("human driver started automation")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	setup.Config.Sandbox = nil
	if _, err := setup.buildInteractiveRestartExecution(t.Context(), runner.StageRestartPlan{Source: f.parent, Continuation: journal.ContinuationRequest{RunID: "human-epoch"}}); err == nil || !strings.Contains(err.Error(), "sandbox") {
		t.Fatalf("disabled sandbox accepted: %v", err)
	}
}

func TestInteractiveExecutionCredentialAndGitPathsKeepHumanIdentity(t *testing.T) {
	setup, f := interactiveExecutionFixture(t)
	t.Setenv("HUMAN_MODEL", "sk-ant-api03-human-model-canary")
	t.Setenv("HUMAN_REPO", "human-repository-canary")
	t.Setenv("HUMAN_BACKLOG", "human-backlog-canary")
	t.Setenv("GH_TOKEN", "automation-canary")
	execution, err := setup.loadInteractiveRestartExecution(t.Context(), f.parent)
	if err != nil {
		t.Fatal(err)
	}
	p := httpapi.Principal{Issuer: "issuer", Subject: "human", Roles: []httpapi.Role{httpapi.RoleOperate}}
	lease, err := setup.InteractiveAccess.BeginExecution(t.Context(), p, "example")
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	reg := journal.NewRegistryScrubber()
	runtime := &interactiveRestartContext{execution: execution, lease: lease, home: t.TempDir(), registrar: teeRegistrar{run: reg, shared: setup.SharedRegistry}}
	ctx := context.WithValue(lease.Context(), interactiveRestartContextKey{}, runtime)
	ctx = context.WithValue(ctx, interactiveRepositoryKey{}, execution.gaggle.Spec.Project)
	resolver := interactiveCredentialResolver{execution: execution, goober: "coder", registrar: runtime.registrar}
	for cap, want := range map[string]string{"agent:model": "sk-ant-api03-human-model-canary", "repo:push": "human-repository-canary", "github:issues:read": "human-backlog-canary"} {
		got, err := resolver.Resolve(ctx, cap)
		if err != nil || got != want {
			t.Fatalf("%s identity err=%v", cap, err)
		}
	}
	remote, err := runner.DefaultRepoCloneURL(execution.gaggle.Spec.Project)
	if err != nil {
		t.Fatal(err)
	}
	provided, err := runtime.gitEnvironment(ctx, remote)
	if err != nil {
		t.Fatal(err)
	}
	_, env, release, err := runtime.prepareGit(context.WithoutCancel(ctx), provided)
	if err != nil {
		t.Fatal(err)
	}
	release()
	if strings.Contains(strings.Join(env, "\n"), "automation-canary") {
		t.Fatal("Git inherited automation credentials")
	}
	for _, scrubber := range []*journal.RegistryScrubber{reg, setup.SharedRegistry} {
		for _, secret := range []string{"human-repository-canary", "human-backlog-canary", "sk-ant-api03-human-model-canary"} {
			if strings.Contains(string(scrubber.Scrub([]byte(secret))), secret) {
				t.Fatal("credential missing from run or shared scrubber")
			}
		}
	}
	if _, err := runtime.gitEnvironment(ctx, "https://github.com/other/repo.git"); err == nil {
		t.Fatal("unmatched Git target accepted")
	}
	lease.Close()
	if _, err := resolver.Resolve(context.WithoutCancel(ctx), "repo:push"); !errors.Is(err, context.Canceled) {
		t.Fatalf("detached context retained credential access: %v", err)
	}
}
