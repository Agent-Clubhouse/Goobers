//go:build integration

package main

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/credentials"
	"github.com/goobers/goobers/internal/dispatcher"
	"github.com/goobers/goobers/internal/engine"
	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/workerhost"
	"github.com/goobers/goobers/internal/workspacebranch"
	"github.com/goobers/goobers/internal/worktree"
)

func ownedBackendFixture(t *testing.T) (deterministicExecutorInput, apiv1.InvocationEnvelope, string, string) {
	t.Helper()
	target := initBareOrigin(t)
	sourceWork := filepath.Join(t.TempDir(), "source")
	podRevisionGit(t, "", "clone", "--branch", "main", target, sourceWork)
	if err := os.WriteFile(filepath.Join(sourceWork, "fork.txt"), []byte("fork only\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, folder := range []string{"included", "excluded"} {
		if err := os.MkdirAll(filepath.Join(sourceWork, folder), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(sourceWork, folder, "source.txt"), []byte(folder), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	podRevisionGit(t, sourceWork, "add", ".")
	podRevisionGit(t, sourceWork, "commit", "-m", "selected fork commit")
	sha := podRevisionGit(t, sourceWork, "rev-parse", "HEAD")
	source := filepath.Join(t.TempDir(), "source.git")
	podRevisionGit(t, "", "clone", "--bare", sourceWork, source)
	for _, repo := range []string{source, target} {
		podRevisionGit(t, repo, "-c", "safe.bareRepository=all", "config", "uploadpack.allowFilter", "true")
	}
	base := apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web", Branch: "main"}
	fork := base
	fork.Owner = "contributor"
	originalURL := repoCloneURL
	originalCheckout := checkoutCloneURL
	clone := func(ref apiv1.RepoRef) (string, error) {
		if ref.Owner == base.Owner {
			return podRevisionFileURL(target), nil
		}
		return podRevisionFileURL(source), nil
	}
	repoCloneURL, checkoutCloneURL = clone, clone
	t.Cleanup(func() { repoCloneURL, checkoutCloneURL = originalURL, originalCheckout })
	t.Setenv("OWNED_BASE_TOKEN", "owned-base-fixture-token")
	t.Setenv("OWNED_SOURCE_TOKEN", "owned-source-fixture-token")
	config := &instance.Config{Repos: []instance.RepoRef{
		{Provider: "github", Owner: base.Owner, Name: base.Name, Token: instance.TokenRef{Env: "OWNED_BASE_TOKEN"}},
		{Provider: "github", Owner: fork.Owner, Name: fork.Name, Token: instance.TokenRef{Env: "OWNED_SOURCE_TOKEN"}},
	}}
	registry := journal.NewRegistryScrubber()
	resolver, grants, err := buildCredentials(config, nil, base.Owner, base.Name, []apiv1.RepoRef{fork}, registry)
	if err != nil {
		t.Fatal(err)
	}
	input := deterministicExecutorInput{Config: config, Resolver: resolver, Grants: grants, SharedRegistry: registry,
		GaggleProject: base, AdditionalRepos: []apiv1.RepoRef{fork}, ScratchDir: t.TempDir(),
		ArtifactRecorder: runnerWiringArtifactRecorder{}, SecretRegistrar: registry}
	env := apiv1.InvocationEnvelope{TaskID: "run:establish", RunID: "run", WorkflowID: "owned", Gaggle: "web",
		RepoRef: base, BaseBranch: "main", BranchNamespace: "ns/", Workspace: t.TempDir(), Capabilities: []string{"repo:push"},
		Inputs:            map[string]any{"kind": workspacebranch.KindEstablish},
		WorkspaceRevision: &apiv1.WorkspaceRevision{Repository: apiv1.RepositoryIdentity{Provider: fork.Provider, Owner: fork.Owner, Name: fork.Name}, CommitSHA: sha, SourceRef: "main"}}
	return input, env, target, source
}

func TestIntegrationOwnedBranchBackendGrantRefusals(t *testing.T) {
	for _, missing := range []string{"source", "target", "declaration", "configured-source", "ambiguous-grant"} {
		t.Run(missing, func(t *testing.T) {
			input, env, target, _ := ownedBackendFixture(t)
			key := credentials.RepoScopedCapability("contents:read", "contributor", "web")
			if missing == "target" {
				key = credentials.RepoScopedCapability("repo:push", "acme", "web")
			}
			if missing == "source" || missing == "target" {
				var grants []credentials.Grant
				for _, grant := range input.Grants {
					if grant.Capability != key {
						grants = append(grants, grant)
					}
				}
				input.Grants = grants
			}
			if missing == "declaration" {
				env.Capabilities = nil
			}
			if missing == "configured-source" {
				input.AdditionalRepos = nil
			}
			if missing == "ambiguous-grant" {
				input.Config.Repos = append(input.Config.Repos, instance.RepoRef{
					Provider: "gitea", BaseURL: "https://other.example.invalid", Owner: "contributor", Name: "web",
				})
			}
			backend, err := buildDeterministicExecutor(input)
			if err != nil {
				t.Fatal(err)
			}
			_, err = backend.Run(context.Background(), env, apiv1.DeterministicRun{Command: []string{"true"}, Workspace: apiv1.WorkspaceScratch})
			if err == nil {
				t.Fatal("missing authority accepted")
			}
			if refs := podRevisionGit(t, target, "-c", "safe.bareRepository=all", "for-each-ref", "--format=%(refname)", "refs/heads/ns/"); refs != "" {
				t.Fatal("refusal created a remote branch")
			}
		})
	}
}

func TestIntegrationOwnedBranchWorkerPodDeltaAndBroker(t *testing.T) {
	for _, policy := range []struct {
		name            string
		partial, sparse bool
	}{{"full", false, false}, {"partial", true, false}, {"full-sparse", false, true}, {"partial-sparse", true, true}} {
		t.Run(policy.name, func(t *testing.T) {
			ctx := context.Background()
			input, env, target, source := ownedBackendFixture(t)
			if policy.sparse {
				input.GaggleProject.Checkout = &apiv1.CheckoutSpec{Sparse: []string{"included"}}
			}
			assertPolicy := func(path string) {
				t.Helper()
				if _, err := os.Stat(filepath.Join(path, "included", "source.txt")); err != nil {
					t.Fatal(err)
				}
				_, err := os.Stat(filepath.Join(path, "excluded", "source.txt"))
				if policy.sparse && !os.IsNotExist(err) || !policy.sparse && err != nil {
					t.Fatalf("owned checkout changed sparse policy: %v", err)
				}
			}
			backend, err := buildDeterministicExecutor(input)
			if err != nil {
				t.Fatal(err)
			}
			result, err := backend.Run(ctx, env, apiv1.DeterministicRun{Command: []string{"true"}, Workspace: apiv1.WorkspaceScratch})
			if err != nil {
				t.Fatal(err)
			}
			binding := result.WorkspaceBranchBinding
			if binding == nil {
				t.Fatal("registered backend failed to return typed ownership")
			}
			endpoint, _ := fakeBlobPlane(t)
			t.Setenv(dispatcher.EnvBlobEndpoint, endpoint)
			t.Setenv(dispatcher.EnvPodToken, "fixture-pod")
			t.Setenv(dispatcher.EnvStageWorkspace, string(apiv1.WorkspaceRepo))
			t.Setenv(dispatcher.EnvRunID, env.RunID)
			t.Setenv(dispatcher.EnvWorkflow, env.WorkflowID)
			t.Setenv(executor.BranchNamespaceEnvVar, env.BranchNamespace)
			t.Setenv(executor.BaseBranchEnvVar, "main")
			t.Setenv(dispatcher.EnvWorkspaceBranch, strings.TrimPrefix(binding.Ref, "refs/heads/"))
			t.Setenv(dispatcher.EnvWorkspaceRevision, "")
			t.Setenv(dispatcher.EnvStageSyncBase, "")
			t.Setenv(dispatcher.EnvCheckoutCapability, "")
			encoded, _ := json.Marshal(binding)
			t.Setenv(dispatcher.EnvWorkspaceBranchBinding, string(encoded))
			transport, _ := json.Marshal(dispatcher.WorkspaceCheckout{Repository: input.GaggleProject, PartialClone: policy.partial})
			t.Setenv(dispatcher.EnvWorkspaceCheckout, string(transport))
			store := podBlobClient()
			newWorker := func() *workerhost.WorktreeWorkspaces {
				options := []worktree.ManagerOption{worktree.WithRunBranchNamespaces(env.BranchNamespace)}
				if policy.partial {
					options = append(options, worktree.WithPartialClone())
				}
				manager, err := worktree.NewManager(t.TempDir(), options...)
				if err != nil {
					t.Fatal(err)
				}
				return &workerhost.WorktreeWorkspaces{Manager: manager, ConfiguredBase: input.GaggleProject, AdditionalRepos: input.AdditionalRepos,
					CloneURL: repoCloneURL, Store: store, Log: io.Discard}
			}
			req := engine.WorkspaceRequest{RunID: env.RunID, Workflow: env.WorkflowID, Gaggle: env.Gaggle, Stage: "author",
				BranchNamespace: env.BranchNamespace, RepoRef: input.GaggleProject, Mode: apiv1.WorkspaceRepo,
				WorkspaceBranch: strings.TrimPrefix(binding.Ref, "refs/heads/"), WorkspaceRevision: env.WorkspaceRevision, WorkspaceBranchBinding: binding}
			worker, err := newWorker().Provision(ctx, req)
			if err != nil {
				t.Fatal(err)
			}
			if got := podRevisionGit(t, worker.Path(), "rev-parse", "HEAD"); got != binding.StartingSHA {
				t.Fatalf("worker root = %s", got)
			}
			assertPolicy(worker.Path())
			if err := os.WriteFile(filepath.Join(worker.Path(), "worker.txt"), []byte("worker work"), 0o600); err != nil {
				t.Fatal(err)
			}
			podRevisionGit(t, worker.Path(), "add", ".")
			podRevisionGit(t, worker.Path(), "commit", "-m", "worker")
			delta, err := worker.(engine.DeltaPublisher).PublishDelta(ctx)
			if err != nil || delta.Digest == "" {
				t.Fatalf("worker delta = %+v, %v", delta, err)
			}
			if err := worker.Remove(ctx); err != nil {
				t.Fatal(err)
			}
			t.Setenv(dispatcher.EnvWorkspaceDelta, delta.Digest)
			pod := t.TempDir()
			read := []dispatcher.MintedCredential{{Capability: dispatcher.WorkspaceBranchCheckoutCapability, Value: "owned-base-fixture-token"}}
			if err := checkoutRepoWorkspace(ctx, pod, io.Discard, read); err != nil {
				t.Fatal(err)
			}
			assertPolicy(pod)
			if head := podRevisionGit(t, pod, "rev-parse", "HEAD"); head != delta.Tip {
				t.Fatal("worker delta did not reach pod")
			}
			if err := os.WriteFile(filepath.Join(pod, "pod.txt"), []byte("pod work"), 0o600); err != nil {
				t.Fatal(err)
			}
			podRevisionGit(t, pod, "add", ".")
			podRevisionGit(t, pod, "commit", "-m", "pod")
			podDelta, err := publishWorkspaceDelta(ctx, pod, io.Discard)
			if err != nil || podDelta.Digest == "" {
				t.Fatalf("pod delta = %+v, %v", podDelta, err)
			}
			req.Stage, req.WorkspaceDelta = "publish", podDelta.Digest
			publisher, err := newWorker().Provision(ctx, req)
			if err != nil {
				t.Fatal(err)
			}
			assertPolicy(publisher.Path())
			defer func() { _ = publisher.Remove(ctx) }()
			if _, err := os.Stat(filepath.Join(publisher.Path(), "worker.txt")); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(filepath.Join(publisher.Path(), "pod.txt")); err != nil {
				t.Fatal(err)
			}
			env.Workspace, env.WorkspaceBranchBinding = publisher.Path(), binding
			env.Inputs["kind"] = workspacebranch.KindPublish
			if _, err := backend.Run(ctx, env, apiv1.DeterministicRun{Command: []string{"true"}, Workspace: apiv1.WorkspaceRepo}); err != nil {
				t.Fatal(err)
			}
			if got := podRevisionGit(t, target, "-c", "safe.bareRepository=all", "rev-parse", binding.Ref); got != podRevisionGit(t, pod, "rev-parse", "HEAD") {
				t.Fatal("broker lost pod work")
			}
			if got := podRevisionGit(t, source, "-c", "safe.bareRepository=all", "rev-parse", "main"); got != binding.StartingSHA {
				t.Fatal("source branch changed")
			}
		})
	}
}
