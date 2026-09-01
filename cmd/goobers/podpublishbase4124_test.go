package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/dispatcher"
	"github.com/goobers/goobers/internal/executor"
)

// #4124: a pod must publish its OWN commits and nothing else.
//
// The failure this pins was live, not theoretical. Run 187c9f340bf9
// (pr-remediation on PR #3900) recorded one digest published three times:
// gather-pr-context cut it, then rebase-pr — which aborted a conflicted
// rebase and committed nothing — re-published the identical bytes, and then
// remediation-checkpoint — which only writes a sticky comment — did it again.
// The engine's continuity record therefore named a non-producer as its newest
// entry, and implement (repoFrom: [rebase-pr]) was refused under the WF022
// runtime arm for building on commits from a stage it had not declared.
//
// The cause was that the pod measured "did this stage commit" against BASE
// rather than against the HEAD it was handed. Every stage that inherits a
// delta, and every stage standing on a rebound PR head, is ahead of base
// before it runs — so the base-only test could never answer no.
//
// These tests go through the real seam in both halves: checkoutRepoWorkspace
// provisions (and records the baseline), publishWorkspaceDelta decides. A test
// that called the helpers directly would not have caught the original bug,
// because the bug was that provisioning recorded nothing at all.
func TestPodPublishesOnlyItsOwnCommits(t *testing.T) {
	origin := initBareOrigin(t)
	endpoint, _ := fakeBlobPlane(t)
	prev := checkoutCloneURL
	checkoutCloneURL = func(apiv1.RepoRef) (string, error) { return origin, nil }
	t.Cleanup(func() { checkoutCloneURL = prev })

	stampEnv := func(t *testing.T, runID, digest string) {
		t.Helper()
		t.Setenv(dispatcher.EnvBlobEndpoint, endpoint)
		t.Setenv(dispatcher.EnvPodToken, "pod-token")
		t.Setenv(dispatcher.EnvStageWorkspace, string(apiv1.WorkspaceRepo))
		t.Setenv(executor.RepoProviderEnvVar, string(apiv1.ProviderGitHub))
		t.Setenv(executor.RepoOwnerEnvVar, "acme")
		t.Setenv(executor.RepoNameEnvVar, "widget")
		t.Setenv(executor.BranchNamespaceEnvVar, "e2e/")
		t.Setenv(executor.BaseBranchEnvVar, "main")
		t.Setenv(dispatcher.EnvWorkflow, "seam")
		t.Setenv(dispatcher.EnvRunID, runID)
		t.Setenv(dispatcher.EnvWorkspaceDelta, digest)
	}

	// The exact shape of the live failure: this pod inherits a predecessor's
	// delta, applies it, and does nothing of its own.
	t.Run("a stage that inherits a delta and commits nothing publishes nothing", func(t *testing.T) {
		t.Setenv(dispatcher.EnvBlobEndpoint, endpoint)
		t.Setenv(dispatcher.EnvPodToken, "pod-token")
		t.Setenv(dispatcher.EnvStageWorkspace, string(apiv1.WorkspaceRepo))

		const branch = "e2e/seam/run-4124-quiet"
		pub := filepath.Join(t.TempDir(), "pub")
		runGitT(t, filepath.Dir(pub), "clone", "--branch", "main", origin, pub)
		digest, deltaHead := publishDeltaFrom(t, pub, branch, "carried.txt", "work\n")

		stampEnv(t, "run-4124-quiet", digest)
		ws := t.TempDir()
		var errOut strings.Builder
		if err := checkoutRepoWorkspace(context.Background(), ws, &errOut, nil); err != nil {
			t.Fatalf("checkout: %v\nstderr: %s", err, errOut.String())
		}
		if got := strings.TrimSpace(runGitOutputT(t, ws, "rev-parse", "HEAD")); got != deltaHead {
			t.Fatalf("HEAD = %s, want the inherited delta tip %s", got, deltaHead)
		}
		// The branch really is ahead of base here — which is exactly why the
		// base-only test said "publish" and why this assertion is the fix.
		empty, err := branchHasNoCommitsBeyondBase(ws, branch)
		if err != nil {
			t.Fatalf("branchHasNoCommitsBeyondBase: %v", err)
		}
		if empty {
			t.Fatal("the inherited branch is not ahead of base; this case no longer reproduces #4124")
		}

		published, err := publishWorkspaceDelta(context.Background(), ws, &errOut)
		if err != nil {
			t.Fatalf("publishWorkspaceDelta: %v\nstderr: %s", err, errOut.String())
		}
		if published.Digest != "" {
			t.Fatalf("a stage that committed nothing published %s — the continuity record now names a non-producer (#4124)", published.Digest)
		}
		if !published.Unchanged {
			t.Fatal("publication was skipped without reporting Unchanged; the engine cannot tell that from a scratch stage")
		}
	})

	// The other direction, on the same seam: the fix must not silence a stage
	// that really did commit on top of what it inherited.
	t.Run("a stage that commits on top of an inherited delta still publishes", func(t *testing.T) {
		t.Setenv(dispatcher.EnvBlobEndpoint, endpoint)
		t.Setenv(dispatcher.EnvPodToken, "pod-token")
		t.Setenv(dispatcher.EnvStageWorkspace, string(apiv1.WorkspaceRepo))

		const branch = "e2e/seam/run-4124-busy"
		pub := filepath.Join(t.TempDir(), "pub")
		runGitT(t, filepath.Dir(pub), "clone", "--branch", "main", origin, pub)
		digest, _ := publishDeltaFrom(t, pub, branch, "carried.txt", "work\n")

		stampEnv(t, "run-4124-busy", digest)
		ws := t.TempDir()
		var errOut strings.Builder
		if err := checkoutRepoWorkspace(context.Background(), ws, &errOut, nil); err != nil {
			t.Fatalf("checkout: %v\nstderr: %s", err, errOut.String())
		}
		runGitT(t, ws, "config", "user.name", "stage")
		runGitT(t, ws, "config", "user.email", "stage@example.com")
		if err := os.WriteFile(filepath.Join(ws, "mine.txt"), []byte("mine\n"), 0o644); err != nil {
			t.Fatalf("write mine.txt: %v", err)
		}
		runGitT(t, ws, "add", "mine.txt")
		runGitT(t, ws, "commit", "-m", "this stage's own work")
		want := strings.TrimSpace(runGitOutputT(t, ws, "rev-parse", "HEAD"))

		published, err := publishWorkspaceDelta(context.Background(), ws, &errOut)
		if err != nil {
			t.Fatalf("publishWorkspaceDelta: %v\nstderr: %s", err, errOut.String())
		}
		if published.Digest == "" {
			t.Fatal("a stage that committed published nothing — its work cannot reach the next pod")
		}
		if published.Tip != want {
			t.Fatalf("published tip = %s, want this stage's commit %s", published.Tip, want)
		}
	})

	// Fail-open. An image whose dispatch-checkout predates this change leaves
	// no baseline, and the pod must keep bundling: a redundant delta costs
	// bytes, a skipped one loses a stage's only copy of its work.
	t.Run("no recorded baseline falls back to the base-range test", func(t *testing.T) {
		t.Setenv(dispatcher.EnvBlobEndpoint, endpoint)
		t.Setenv(dispatcher.EnvPodToken, "pod-token")
		t.Setenv(dispatcher.EnvStageWorkspace, string(apiv1.WorkspaceRepo))

		const branch = "e2e/seam/run-4124-legacy"
		pub := filepath.Join(t.TempDir(), "pub")
		runGitT(t, filepath.Dir(pub), "clone", "--branch", "main", origin, pub)
		digest, _ := publishDeltaFrom(t, pub, branch, "carried.txt", "work\n")

		stampEnv(t, "run-4124-legacy", digest)
		ws := t.TempDir()
		var errOut strings.Builder
		if err := checkoutRepoWorkspace(context.Background(), ws, &errOut, nil); err != nil {
			t.Fatalf("checkout: %v\nstderr: %s", err, errOut.String())
		}
		path, err := publishBasePath(ws)
		if err != nil {
			t.Fatalf("publishBasePath: %v", err)
		}
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("provisioning left no baseline at %s: %v", path, err)
		}
		if err := os.Remove(path); err != nil {
			t.Fatalf("remove baseline: %v", err)
		}

		published, err := publishWorkspaceDelta(context.Background(), ws, &errOut)
		if err != nil {
			t.Fatalf("publishWorkspaceDelta: %v\nstderr: %s", err, errOut.String())
		}
		if published.Digest == "" {
			t.Fatal("without a baseline the pod must bundle rather than assume unchanged")
		}
	})
}

// The baseline is state the stage must never see: it sits in the workspace the
// stage runs in, and a file that shows up in `git status` would be committed by
// an agentic stage, bundled by this very mechanism, and eventually pushed onto
// a PR.
func TestThePublishBaselineIsInvisibleToTheStage(t *testing.T) {
	origin := initBareOrigin(t)
	endpoint, _ := fakeBlobPlane(t)
	prev := checkoutCloneURL
	checkoutCloneURL = func(apiv1.RepoRef) (string, error) { return origin, nil }
	t.Cleanup(func() { checkoutCloneURL = prev })

	t.Setenv(dispatcher.EnvBlobEndpoint, endpoint)
	t.Setenv(dispatcher.EnvPodToken, "pod-token")
	t.Setenv(dispatcher.EnvStageWorkspace, string(apiv1.WorkspaceRepo))
	t.Setenv(executor.RepoProviderEnvVar, string(apiv1.ProviderGitHub))
	t.Setenv(executor.RepoOwnerEnvVar, "acme")
	t.Setenv(executor.RepoNameEnvVar, "widget")
	t.Setenv(executor.BranchNamespaceEnvVar, "e2e/")
	t.Setenv(executor.BaseBranchEnvVar, "main")
	t.Setenv(dispatcher.EnvWorkflow, "seam")
	t.Setenv(dispatcher.EnvRunID, "run-4124-invisible")

	ws := t.TempDir()
	var errOut strings.Builder
	if err := checkoutRepoWorkspace(context.Background(), ws, &errOut, nil); err != nil {
		t.Fatalf("checkout: %v\nstderr: %s", err, errOut.String())
	}
	if out := strings.TrimSpace(runGitOutputT(t, ws, "status", "--porcelain")); out != "" {
		t.Fatalf("provisioning left the workspace dirty:\n%s", out)
	}
	path, err := publishBasePath(ws)
	if err != nil {
		t.Fatalf("publishBasePath: %v", err)
	}
	recorded, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read baseline: %v", err)
	}
	head := strings.TrimSpace(runGitOutputT(t, ws, "rev-parse", "HEAD"))
	if strings.TrimSpace(string(recorded)) != head {
		t.Fatalf("baseline = %q, want provisioned HEAD %s", strings.TrimSpace(string(recorded)), head)
	}
}
