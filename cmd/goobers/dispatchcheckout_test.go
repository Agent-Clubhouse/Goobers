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
	"github.com/goobers/goobers/internal/testgit"
)

// `git clone <url> .` refuses a non-empty destination, so NOTHING the checkout
// creates for its own use may land in the workspace.
//
// MEASURED, from inside a stage pod, when the askpass helper was written there:
//
//	fatal: destination path '.' already exists and is not an empty directory
//
// The clone failed before contacting the remote at all, which made a working
// credential look like a broken one.
func TestCheckoutAuthMaterialStaysOutOfTheWorkspace(t *testing.T) {
	ws := t.TempDir()
	creds := []dispatcher.MintedCredential{{Capability: "repo:push", Value: "t0ken"}}

	if _, err := checkoutGitAuthEnv(ws, creds); err != nil {
		t.Fatalf("build git auth env: %v", err)
	}

	entries, err := os.ReadDir(ws)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("workspace is not empty after preparing git auth: %v — git clone will refuse it", names)
	}
}

// No repo-shaped capability means an anonymous clone, NOT a substituted
// credential: handing a stage's unrelated token to git would be silent scope
// widening.
func TestCheckoutRefusesToSubstituteAnUnrelatedCredential(t *testing.T) {
	ws := t.TempDir()
	creds := []dispatcher.MintedCredential{{Capability: "github:issues:read", Value: "issues-only"}}

	env, err := checkoutGitAuthEnv(ws, creds)
	if err != nil {
		t.Fatalf("build git auth env: %v", err)
	}
	for _, kv := range env {
		if filepath.Base(kv) == "issues-only" || kv == "GOOBERS_GIT_TOKEN=issues-only" {
			t.Fatal("an unrelated credential was handed to git")
		}
	}
	if len(env) != 0 {
		t.Fatalf("expected no git auth env for a non-repo capability, got %v", env)
	}
}

// newBareRepoWithCommit builds a local bare repository holding one commit on
// baseBranch, so the real clone path can be exercised with no network and no
// credential.
func newBareRepoWithCommit(t *testing.T, baseBranch string) string {
	t.Helper()
	run := func(dir string, args ...string) {
		t.Helper()
		cmd := testgit.Command(args...)
		cmd.Dir = dir
		cmd.Env = append(cmd.Env,
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.invalid",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.invalid",
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	work := t.TempDir()
	run(work, "init", "--quiet", "-b", baseBranch, work)
	if err := os.WriteFile(filepath.Join(work, "README.md"), []byte("probe\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(work, "add", "README.md")
	run(work, "commit", "--quiet", "-m", "seed commit")

	bare := filepath.Join(t.TempDir(), "origin.git")
	run(work, "clone", "--quiet", "--bare", work, bare)
	return bare
}

// The clone that actually runs, against a real git remote. THIS is the coverage
// that was missing: #3733 shipped with an askpass helper written into the
// workspace, and `git clone <url> .` refuses a non-empty destination — a bug no
// amount of shape-testing could catch, and which only a live cluster found.
func TestCheckoutClonesRepoWorkspaceOntoTheRunBranch(t *testing.T) {
	bare := newBareRepoWithCommit(t, "main")
	prev := checkoutCloneURL
	checkoutCloneURL = func(apiv1.RepoRef) (string, error) { return bare, nil }
	t.Cleanup(func() { checkoutCloneURL = prev })

	t.Setenv(dispatcher.EnvStageWorkspace, string(apiv1.WorkspaceRepo))
	t.Setenv(executor.RepoProviderEnvVar, string(apiv1.ProviderGitHub))
	t.Setenv(executor.RepoOwnerEnvVar, "acme")
	t.Setenv(executor.RepoNameEnvVar, "widget")
	t.Setenv(executor.BranchNamespaceEnvVar, "e2e/")
	t.Setenv(executor.BaseBranchEnvVar, "main")
	t.Setenv(dispatcher.EnvWorkflow, "probe")
	t.Setenv(dispatcher.EnvRunID, "run-1")

	ws := t.TempDir()
	var errOut strings.Builder
	// A repo credential is REQUIRED for this test to mean anything: without one
	// the askpass helper is never written, and the empty-destination bug that
	// reached the cluster is invisible. Verified by ablation — with creds nil
	// this test passes against the broken code.
	creds := []dispatcher.MintedCredential{{Capability: "repo:push", Value: "t0ken"}}
	if err := checkoutRepoWorkspace(context.Background(), ws, &errOut, creds); err != nil {
		t.Fatalf("checkout: %v\nstderr: %s", err, errOut.String())
	}

	if _, err := os.Stat(filepath.Join(ws, "README.md")); err != nil {
		t.Fatalf("repo content missing after checkout: %v", err)
	}
	// A repo workspace must land on the RUN BRANCH: that is how work moves
	// between stages. Landing on base would look healthy and silently discard
	// every earlier stage's commits.
	out, err := testgit.Command("-C", ws, "rev-parse", "--abbrev-ref", "HEAD").Output()
	if err != nil {
		t.Fatal(err)
	}
	if got, want := strings.TrimSpace(string(out)), "e2e/probe/run-1"; got != want {
		t.Fatalf("branch = %q, want the derived run branch %q", got, want)
	}
}

// repo-readonly must land DETACHED at base, never on the run branch: a
// read-only research stage reads the pinned base, not what earlier stages
// pushed.
func TestCheckoutRepoReadOnlyDetachesAtBase(t *testing.T) {
	bare := newBareRepoWithCommit(t, "main")
	prev := checkoutCloneURL
	checkoutCloneURL = func(apiv1.RepoRef) (string, error) { return bare, nil }
	t.Cleanup(func() { checkoutCloneURL = prev })

	t.Setenv(dispatcher.EnvStageWorkspace, string(apiv1.WorkspaceRepoReadOnly))
	t.Setenv(executor.RepoProviderEnvVar, string(apiv1.ProviderGitHub))
	t.Setenv(executor.RepoOwnerEnvVar, "acme")
	t.Setenv(executor.RepoNameEnvVar, "widget")
	t.Setenv(executor.BranchNamespaceEnvVar, "e2e/")
	t.Setenv(executor.BaseBranchEnvVar, "main")
	t.Setenv(dispatcher.EnvWorkflow, "probe")
	t.Setenv(dispatcher.EnvRunID, "run-1")

	ws := t.TempDir()
	var errOut strings.Builder
	if err := checkoutRepoWorkspace(context.Background(), ws, &errOut, nil); err != nil {
		t.Fatalf("checkout: %v\nstderr: %s", err, errOut.String())
	}
	out, err := testgit.Command("-C", ws, "rev-parse", "--abbrev-ref", "HEAD").Output()
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(out)); got != "HEAD" {
		t.Fatalf("branch = %q, want detached HEAD for repo-readonly", got)
	}
}

// scratch must remain a no-op: every stage that ran before pod-side checkout
// existed has to behave identically.
func TestCheckoutScratchLeavesWorkspaceUntouched(t *testing.T) {
	t.Setenv(dispatcher.EnvStageWorkspace, string(apiv1.WorkspaceScratch))
	ws := t.TempDir()
	var errOut strings.Builder
	if err := checkoutRepoWorkspace(context.Background(), ws, &errOut, nil); err != nil {
		t.Fatalf("scratch must be a no-op: %v", err)
	}
	entries, err := os.ReadDir(ws)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("scratch workspace was modified: %d entries", len(entries))
	}
}

// The run-branch attempt can leave partial state behind: git may populate the
// destination and only then discover the branch is missing. A second
// `clone <url> .` into a non-empty directory is refused outright, so the
// fallback must not depend on how far the first attempt got — behaviour that
// differs between a local file:// remote and an authenticated HTTPS one.
func TestCheckoutFallbackSurvivesDirtyWorkspace(t *testing.T) {
	bare := newBareRepoWithCommit(t, "main")
	prev := checkoutCloneURL
	checkoutCloneURL = func(apiv1.RepoRef) (string, error) { return bare, nil }
	t.Cleanup(func() { checkoutCloneURL = prev })

	t.Setenv(dispatcher.EnvStageWorkspace, string(apiv1.WorkspaceRepo))
	t.Setenv(executor.RepoProviderEnvVar, string(apiv1.ProviderGitHub))
	t.Setenv(executor.RepoOwnerEnvVar, "acme")
	t.Setenv(executor.RepoNameEnvVar, "widget")
	t.Setenv(executor.BranchNamespaceEnvVar, "e2e/")
	t.Setenv(executor.BaseBranchEnvVar, "main")
	t.Setenv(dispatcher.EnvWorkflow, "probe")
	t.Setenv(dispatcher.EnvRunID, "run-dirty")

	ws := t.TempDir()
	// Exactly what a partially-completed clone leaves behind.
	if err := os.MkdirAll(filepath.Join(ws, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "leftover"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	var errOut strings.Builder
	creds := []dispatcher.MintedCredential{{Capability: "repo:push", Value: "t0ken"}}
	if err := checkoutRepoWorkspace(context.Background(), ws, &errOut, creds); err != nil {
		t.Fatalf("fallback must clear and re-clone: %v\nstderr: %s", err, errOut.String())
	}
	if _, err := os.Stat(filepath.Join(ws, "README.md")); err != nil {
		t.Fatalf("repo content missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(ws, "leftover")); !os.IsNotExist(err) {
		t.Fatal("stale content from the failed attempt survived into the workspace")
	}
}

// The safe.directory exemption must name the workspace PATH and never the "*"
// wildcard: the protection exists to stop git trusting a repository someone
// else planted, and a pod that disabled it globally would trust anything
// mounted into it.
func TestWorkspaceGitEnvScopesSafeDirectoryToThePath(t *testing.T) {
	ws := t.TempDir()
	env := workspaceGitEnv(ws).Env()

	var key, value string
	for _, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		switch k {
		case "GIT_CONFIG_KEY_0":
			key = v
		case "GIT_CONFIG_VALUE_0":
			value = v
		}
	}
	if key != "safe.directory" {
		t.Fatalf("key = %q, want safe.directory", key)
	}
	if value == "*" {
		t.Fatal("safe.directory must not be the wildcard: that trusts every repository in the pod")
	}
	if !filepath.IsAbs(value) {
		t.Fatalf("safe.directory = %q, want an absolute path", value)
	}
	if value != ws {
		t.Fatalf("safe.directory = %q, want the workspace %q", value, ws)
	}
}

// The safe.directory exemption must SURVIVE composition with the credential
// environment.
//
// credentials.GitAuthEnvironment returns a COMPLETE environment and strips
// foreign GIT_CONFIG_* before installing its own (COUNT=1,
// KEY_0=credential.helper). Appending it after a safe.directory setting erased
// that setting — MEASURED on a live cluster: the clone succeeded and every
// later git command still failed with "detected dubious ownership".
//
// The previous test passed throughout, because it checked the helper in
// isolation and never composed it with auth. This one composes.
func TestComposedGitEnvKeepsBothConfigSlots(t *testing.T) {
	ws := t.TempDir()
	auth := []string{
		"PATH=/usr/bin",
		"GIT_CONFIG_COUNT=1",
		"GIT_CONFIG_KEY_0=credential.helper",
		"GIT_CONFIG_VALUE_0=",
		"GIT_ASKPASS=/tmp/askpass",
	}
	env := composeGitEnv(ws, auth)

	// Later entries win, so read the EFFECTIVE value of each name.
	eff := map[string]string{}
	for _, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		eff[k] = v
	}
	if eff["GIT_CONFIG_COUNT"] != "2" {
		t.Fatalf("GIT_CONFIG_COUNT = %q, want 2 — git reads only the first COUNT entries", eff["GIT_CONFIG_COUNT"])
	}
	if eff["GIT_CONFIG_KEY_0"] != "credential.helper" {
		t.Fatalf("slot 0 = %q, want credential.helper preserved", eff["GIT_CONFIG_KEY_0"])
	}
	if eff["GIT_CONFIG_KEY_1"] != "safe.directory" {
		t.Fatalf("slot 1 = %q, want safe.directory", eff["GIT_CONFIG_KEY_1"])
	}
	if eff["GIT_CONFIG_VALUE_1"] != ws {
		t.Fatalf("safe.directory = %q, want the workspace %q", eff["GIT_CONFIG_VALUE_1"], ws)
	}
	if eff["GIT_ASKPASS"] != "/tmp/askpass" {
		t.Fatal("the credential helper must survive composition")
	}
}

// Without a credential the exemption takes slot 0, and any inherited
// GIT_CONFIG_* must be cleared so the indices are unambiguous.
func TestComposedGitEnvWithoutAuthClaimsSlotZero(t *testing.T) {
	ws := t.TempDir()
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "inherited.key")
	t.Setenv("GIT_CONFIG_VALUE_0", "inherited")

	eff := map[string]string{}
	for _, kv := range composeGitEnv(ws, nil) {
		k, v, _ := strings.Cut(kv, "=")
		eff[k] = v
	}
	if eff["GIT_CONFIG_KEY_0"] != "safe.directory" {
		t.Fatalf("slot 0 = %q, want safe.directory to replace the inherited entry", eff["GIT_CONFIG_KEY_0"])
	}
	if eff["GIT_CONFIG_COUNT"] != "1" {
		t.Fatalf("COUNT = %q, want 1", eff["GIT_CONFIG_COUNT"])
	}
}
