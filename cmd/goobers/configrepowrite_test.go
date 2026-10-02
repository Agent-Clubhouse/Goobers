package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/capability"
	"github.com/goobers/goobers/internal/credentials"
	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/internal/instance"
)

// TUT-A8 (#1220): the configrepo:write capability and the config-repo target of
// push-branch / open-pr. These tests pin the three properties the design asks
// for: credential isolation in both directions, target resolution from
// workflowSource, and fail-closed behavior everywhere a credential or target
// is missing.

func configWriteAppSource() *instance.WorkflowSource {
	return &instance.WorkflowSource{
		Kind: instance.WorkflowSourceKindGit,
		URL:  "https://github.com/acme/workflows.git",
		Ref:  "release",
		Auth: &instance.RepoAuthConfig{
			Kind: instance.GitHubAuthApp, AppID: "123456", InstallationID: "42",
			PrivateKey: &instance.TokenRef{File: "/run/secrets/app.pem"},
		},
	}
}

func stubWorkflowSourceWriteMint(t *testing.T) *int {
	t.Helper()
	prev := newWorkflowSourceWriteTokenSource
	mints := 0
	newWorkflowSourceWriteTokenSource = func(source instance.WorkflowSource, _ credentials.SecretRegistrar, _ credentials.StoreResolver) (credentials.ExpiringResolveFunc, error) {
		if repo, err := workflowSourceRepositoryName(source.URL); err != nil || repo != "workflows" {
			t.Fatalf("write mint scoped to %q (%v), want the config repository", repo, err)
		}
		return func(context.Context) (string, time.Time, error) {
			mints++
			return "minted-config-write", time.Time{}, nil
		}, nil
	}
	t.Cleanup(func() { newWorkflowSourceWriteTokenSource = prev })
	return &mints
}

type noopRegistrar struct{}

func (noopRegistrar) Register([]byte) {}

func TestBuildCredentialsConfigRepoWriteIsIsolatedFromProductRepo(t *testing.T) {
	stubWorkflowSourceWriteMint(t)
	t.Setenv("PRODUCT_TOKEN", "product-token")
	cfg := &instance.Config{
		Repos:          []instance.RepoRef{{Provider: "github", Owner: "acme", Name: "web", Token: instance.TokenRef{Env: "PRODUCT_TOKEN"}}},
		WorkflowSource: configWriteAppSource(),
	}
	resolver, grants, err := buildCredentials(cfg, nil, "acme", "web", nil, nil)
	if err != nil {
		t.Fatalf("buildCredentials: %v", err)
	}
	got := resolveGrants(t, resolver, grants)
	if got[string(capability.ConfigRepoWrite)] != "minted-config-write" {
		t.Fatalf("configrepo:write = %q, want the workflowSource-minted token", got[string(capability.ConfigRepoWrite)])
	}
	if got[string(capability.RepoPush)] != "product-token" {
		t.Fatalf("repo:push = %q, want the product-repo token", got[string(capability.RepoPush)])
	}
	for capName, token := range got {
		if capName != string(capability.ConfigRepoWrite) && token == got[string(capability.ConfigRepoWrite)] {
			t.Fatalf("capability %s shares the config-repo write token", capName)
		}
	}

	// A stage declaring only the product capability holds no config credential,
	// and a stage declaring only the config capability holds no product one.
	injector, err := credentials.NewInjector(resolver, grants, noopRegistrar{})
	if err != nil {
		t.Fatalf("NewInjector: %v", err)
	}
	productStage, err := injector.Materialize(context.Background(), []string{string(capability.RepoPush)})
	if err != nil {
		t.Fatalf("Materialize product: %v", err)
	}
	if _, err := productStage.Token(context.Background(), string(capability.ConfigRepoWrite)); !errors.Is(err, credentials.ErrUndeclaredCapability) {
		t.Fatalf("product stage read configrepo:write: err = %v, want ErrUndeclaredCapability", err)
	}
	configStage, err := injector.Materialize(context.Background(), []string{string(capability.ConfigRepoWrite)})
	if err != nil {
		t.Fatalf("Materialize config: %v", err)
	}
	if _, err := configStage.Token(context.Background(), string(capability.RepoPush)); !errors.Is(err, credentials.ErrUndeclaredCapability) {
		t.Fatalf("config stage read repo:push: err = %v, want ErrUndeclaredCapability", err)
	}
	if tok, err := configStage.Token(context.Background(), string(capability.ConfigRepoWrite)); err != nil || tok != "minted-config-write" {
		t.Fatalf("config stage token = %q, %v", tok, err)
	}
}

func TestBuildCredentialsConfigRepoWriteUnavailableWithoutAppAuth(t *testing.T) {
	stubWorkflowSourceWriteMint(t)
	t.Setenv("PRODUCT_TOKEN", "product-token")
	t.Setenv("CONFIG_READ_TOKEN", "config-read-token")
	repos := []instance.RepoRef{{Provider: "github", Owner: "acme", Name: "web", Token: instance.TokenRef{Env: "PRODUCT_TOKEN"}}}
	tests := map[string]*instance.WorkflowSource{
		"no workflowSource": nil,
		"local dir":         {Kind: instance.WorkflowSourceKindLocalDir, Path: "/srv/config"},
		"token authed": {
			Kind: instance.WorkflowSourceKindGit, URL: "https://github.com/acme/workflows",
			Token: &instance.TokenRef{Env: "CONFIG_READ_TOKEN"},
		},
	}
	for name, source := range tests {
		t.Run(name, func(t *testing.T) {
			resolver, grants, err := buildCredentials(&instance.Config{Repos: repos, WorkflowSource: source}, nil, "acme", "web", nil, nil)
			if err != nil {
				t.Fatalf("buildCredentials: %v", err)
			}
			got := resolveGrants(t, resolver, grants)
			if tok, ok := got[string(capability.ConfigRepoWrite)]; ok {
				t.Fatalf("configrepo:write granted (%q) without a github-app workflowSource; the read token and product token must never back it", tok)
			}
		})
	}
}

func TestBuildCredentialsConfigRepoWriteExplicitEntryWins(t *testing.T) {
	stubWorkflowSourceWriteMint(t)
	t.Setenv("PRODUCT_TOKEN", "product-token")
	t.Setenv("CONFIG_WRITE_PAT", "config-write-pat")
	cfg := &instance.Config{
		Repos:          []instance.RepoRef{{Provider: "github", Owner: "acme", Name: "web", Token: instance.TokenRef{Env: "PRODUCT_TOKEN"}}},
		WorkflowSource: configWriteAppSource(),
		Credentials: []instance.CredentialGrant{{
			Capability: string(capability.ConfigRepoWrite), Token: instance.TokenRef{Env: "CONFIG_WRITE_PAT"},
		}},
	}
	resolver, grants, err := buildCredentials(cfg, nil, "acme", "web", nil, nil)
	if err != nil {
		t.Fatalf("buildCredentials: %v", err)
	}
	if got := resolveGrants(t, resolver, grants)[string(capability.ConfigRepoWrite)]; got != "config-write-pat" {
		t.Fatalf("configrepo:write = %q, want the explicit credentials: entry", got)
	}
}

func TestWorkflowSourceWritePermissionsAreMinimal(t *testing.T) {
	want := map[string]string{"contents": "write", "pull_requests": "write", "metadata": "read"}
	if len(workflowSourceWritePermissions) != len(want) {
		t.Fatalf("permissions = %v, want exactly %v", workflowSourceWritePermissions, want)
	}
	for k, v := range want {
		if workflowSourceWritePermissions[k] != v {
			t.Fatalf("permissions[%s] = %q, want %q", k, workflowSourceWritePermissions[k], v)
		}
	}
	if _, err := newWorkflowSourceWriteTokenSource(*configWriteAppSource(), nil, nil); err != nil {
		t.Fatalf("real constructor: %v", err)
	}
	token := configWriteAppSource()
	token.Auth, token.Token = nil, &instance.TokenRef{Env: "X"}
	if _, err := newWorkflowSourceWriteTokenSource(*token, nil, nil); err == nil {
		t.Fatal("non-app workflowSource must not build a write minter")
	}
}

func TestConfigRepoTargetFromSource(t *testing.T) {
	target, err := configRepoTargetFromSource(configWriteAppSource())
	if err != nil {
		t.Fatalf("configRepoTargetFromSource: %v", err)
	}
	if target.Repo.Owner != "acme" || target.Repo.Name != "workflows" || target.Base != "release" {
		t.Fatalf("target = %+v", target)
	}
	if target.CloneURL() != "https://github.com/acme/workflows.git" {
		t.Fatalf("CloneURL = %q", target.CloneURL())
	}
	def := configWriteAppSource()
	def.Ref = ""
	if target, _ = configRepoTargetFromSource(def); target.Base != instance.DefaultWorkflowSourceRef {
		t.Fatalf("default base = %q", target.Base)
	}
	for name, source := range map[string]*instance.WorkflowSource{
		"nil":      nil,
		"local":    {Kind: instance.WorkflowSourceKindLocalDir, Path: "/x"},
		"non-gh":   {Kind: instance.WorkflowSourceKindGit, URL: "https://git.example.com/acme/workflows"},
		"no-owner": {Kind: instance.WorkflowSourceKindGit, URL: "https://github.com/workflows"},
	} {
		if _, err := configRepoTargetFromSource(source); err == nil {
			t.Fatalf("%s: want an error", name)
		}
	}
}

func writeConfigRepoInstance(t *testing.T, workflowSource string) string {
	t.Helper()
	root := t.TempDir()
	body := "apiVersion: goobers.dev/v1alpha1\nkind: Instance\n"
	if workflowSource != "" {
		body += "workflowSource:\n" + workflowSource
	}
	if err := os.WriteFile(instance.NewLayout(root).ConfigFile(), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return root
}

func TestResolveConfigRepoTarget(t *testing.T) {
	root := writeConfigRepoInstance(t, "  kind: git\n  url: https://github.com/acme/workflows.git\n  ref: stable\n  token:\n    env: CFG_READ\n")
	target, err := resolveConfigRepoTarget(root)
	if err != nil || target.Repo.Owner != "acme" || target.Repo.Name != "workflows" || target.Base != "stable" {
		t.Fatalf("target = %+v, err = %v", target, err)
	}

	// Inputs that agree are accepted; inputs that disagree are refused, so a
	// workflow cannot redirect the config credential at another repository.
	t.Setenv(executor.InputEnvVar(configRepoInput), "ACME/workflows")
	if _, err := resolveConfigRepoTarget(root); err != nil {
		t.Fatalf("agreeing configRepo input: %v", err)
	}
	t.Setenv(executor.InputEnvVar(configRepoInput), "acme/web")
	if _, err := resolveConfigRepoTarget(root); err == nil || !strings.Contains(err.Error(), "does not match workflowSource") {
		t.Fatalf("mismatched configRepo input: err = %v", err)
	}
	t.Setenv(executor.InputEnvVar(configRepoInput), "")
	t.Setenv(executor.InputEnvVar(configRepoBaseInput), "main")
	if _, err := resolveConfigRepoTarget(root); err == nil || !strings.Contains(err.Error(), "does not match workflowSource ref") {
		t.Fatalf("mismatched configRepoBase input: err = %v", err)
	}
}

func TestResolveConfigRepoTargetFailsClosedWithoutWorkflowSource(t *testing.T) {
	root := writeConfigRepoInstance(t, "")
	// Even naming the repository by input must not bypass a readable instance
	// config that has no workflowSource.
	t.Setenv(executor.InputEnvVar(configRepoInput), "acme/workflows")
	if _, err := resolveConfigRepoTarget(root); err == nil || !strings.Contains(err.Error(), "no workflowSource") {
		t.Fatalf("err = %v, want a no-workflowSource error", err)
	}
}

func TestResolveConfigRepoTargetPodFallbackFromInputs(t *testing.T) {
	root := t.TempDir() // no instance config, as in a stage pod
	if _, err := resolveConfigRepoTarget(root); err == nil {
		t.Fatal("no config and no inputs must fail closed")
	}
	t.Setenv(executor.InputEnvVar(configRepoInput), "not-a-slug")
	if _, err := resolveConfigRepoTarget(root); err == nil {
		t.Fatal("malformed configRepo input must fail closed")
	}
	t.Setenv(executor.InputEnvVar(configRepoInput), "acme/workflows")
	target, err := resolveConfigRepoTarget(root)
	if err != nil || target.Repo.Name != "workflows" || target.Base != instance.DefaultWorkflowSourceRef {
		t.Fatalf("target = %+v, err = %v", target, err)
	}
	t.Setenv(executor.InputEnvVar(configRepoBaseInput), "dev")
	if target, _ = resolveConfigRepoTarget(root); target.Base != "dev" {
		t.Fatalf("base = %q", target.Base)
	}
}

func TestProviderTargetRejectsUnknownValue(t *testing.T) {
	if ok, err := providerTargetIsConfigRepo(); ok || err != nil {
		t.Fatalf("unset target = %v, %v", ok, err)
	}
	t.Setenv(executor.InputEnvVar(configRepoTargetInput), "config-repo")
	if ok, err := providerTargetIsConfigRepo(); !ok || err != nil {
		t.Fatalf("config-repo target = %v, %v", ok, err)
	}
	t.Setenv(executor.InputEnvVar(configRepoTargetInput), "config")
	if _, err := providerTargetIsConfigRepo(); err == nil {
		t.Fatal("a typo'd target must fail rather than fall back to the product repository")
	}
}

func TestConfigRepoWriteTokenReadsOnlyItsOwnCapability(t *testing.T) {
	// Product-repo credentials present, config credential absent: fail closed.
	t.Setenv(executor.CredentialEnvVar(string(capability.RepoPush)), "product-push")
	t.Setenv(executor.CredentialEnvVar(string(capability.ProviderPRWrite)), "product-pr")
	if _, err := configRepoWriteToken(); err == nil || !strings.Contains(err.Error(), "configrepo:write") {
		t.Fatalf("err = %v, want a configrepo:write error", err)
	}
	t.Setenv(executor.CredentialEnvVar(string(capability.ConfigRepoWrite)), "config-write")
	if tok, err := configRepoWriteToken(); err != nil || tok != "config-write" {
		t.Fatalf("token = %q, %v", tok, err)
	}
}

func configRepoCheckoutFixture(t *testing.T) (dir string) {
	t.Helper()
	origin := initBareOrigin(t)
	dir = filepath.Join(t.TempDir(), "config-repo")
	runGitT(t, filepath.Dir(dir), "clone", origin, dir)
	runGitT(t, dir, "config", "user.name", "t")
	runGitT(t, dir, "config", "user.email", "t@example.com")
	runGitT(t, dir, "checkout", "-b", "goobers/tutor/run-1")
	if err := os.MkdirAll(filepath.Join(dir, "gaggles", "alpha"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "gaggles", "alpha", "change.yaml"), []byte("x: 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitT(t, dir, "add", ".")
	runGitT(t, dir, "commit", "-m", "change")
	// Point origin at the config repository's canonical URL and let git rewrite
	// it to the local bare repo, so the origin check sees the real shape.
	runGitT(t, dir, "config", "url."+origin+".pushInsteadOf", "https://github.com/acme/workflows.git")
	runGitT(t, dir, "remote", "set-url", "origin", "https://github.com/acme/workflows.git")
	return dir
}

func configRepoPushEnv(t *testing.T, workDir string) {
	t.Helper()
	t.Setenv(executor.InputEnvVar(configRepoInput), "acme/workflows")
	t.Setenv(executor.InputEnvVar(configRepoTargetInput), configRepoTargetValue)
	t.Setenv(executor.InputEnvVar(configRepoDirInput), "config-repo")
	t.Setenv("GOOBERS_INSTANCE_ROOT", t.TempDir())
	t.Chdir(workDir)
}

func TestPushBranchConfigTargetPushesConfigCheckout(t *testing.T) {
	dir := configRepoCheckoutFixture(t)
	configRepoPushEnv(t, filepath.Dir(dir))
	t.Setenv(executor.CredentialEnvVar(string(capability.ConfigRepoWrite)), "config-write")

	code, stdout, stderr := runArgs(t, "push-branch")
	if code != 0 {
		t.Fatalf("push-branch: code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	bare := strings.Fields(runGitOutputT(t, dir, "config", "--get-regexp", `^url\..*\.pushinsteadof$`))[0]
	bare = strings.TrimSuffix(strings.TrimPrefix(bare, "url."), ".pushinsteadof")
	if !branchExistsOnOrigin(t, bare, "goobers/tutor/run-1") {
		t.Fatal("branch did not reach the config origin")
	}
}

func TestPushBranchConfigTargetFailsClosedWithoutConfigCredential(t *testing.T) {
	dir := configRepoCheckoutFixture(t)
	configRepoPushEnv(t, filepath.Dir(dir))
	// The product-repo push credential must not be accepted for the config repo.
	t.Setenv(executor.CredentialEnvVar(string(capability.RepoPush)), "product-push")

	code, _, stderr := runArgs(t, "push-branch")
	if code == 0 || !strings.Contains(stderr, "configrepo:write") {
		t.Fatalf("push-branch: code = %d, stderr = %q, want a configrepo:write failure", code, stderr)
	}
}

func TestPushBranchConfigTargetRefusesNonConfigOrigin(t *testing.T) {
	dir := configRepoCheckoutFixture(t)
	runGitT(t, dir, "remote", "set-url", "origin", "https://github.com/acme/web.git")
	configRepoPushEnv(t, filepath.Dir(dir))
	t.Setenv(executor.CredentialEnvVar(string(capability.ConfigRepoWrite)), "config-write")

	code, _, stderr := runArgs(t, "push-branch")
	if code == 0 || !strings.Contains(stderr, "not the config repository") {
		t.Fatalf("push-branch: code = %d, stderr = %q, want an origin refusal", code, stderr)
	}
}

func TestPushBranchDefaultTargetIgnoresConfigCredential(t *testing.T) {
	// The default (product-repo) target never reads configrepo:write: with only
	// that credential delivered it still fails closed on repo:push.
	origin := initBareOrigin(t)
	wt := filepath.Join(t.TempDir(), "wt")
	runGitT(t, filepath.Dir(wt), "clone", origin, wt)
	runGitT(t, wt, "checkout", "-b", "goobers/implementation/run-1")
	t.Chdir(wt)
	t.Setenv(executor.CredentialEnvVar(string(capability.ConfigRepoWrite)), "config-write")
	t.Setenv(executor.CredentialEnvVar(string(capability.RepoPush)), "")
	code, _, stderr := runArgs(t, "push-branch")
	if code == 0 || !strings.Contains(stderr, "GOOBERS_CRED_REPO_PUSH") {
		t.Fatalf("push-branch: code = %d, stderr = %q, want a repo:push failure", code, stderr)
	}
}

// TestOpenPRConfigTargetOpensInConfigRepoWithConfigCredential is the open-pr
// half: with target config-repo the PR is opened in the config repository (not
// the gaggle's routed repo), authenticated by configrepo:write alone, based on
// the configured base, with write-boundary paths relative to the config root.
func TestOpenPRConfigTargetOpensInConfigRepoWithConfigCredential(t *testing.T) {
	dir := configRepoCheckoutFixture(t)
	server := newFakeGitHubServer(t, "acme", "workflows")
	providerCmdEnv(t, server, "", "run-1")
	// The gaggle's routed (product) repository is a different one entirely.
	t.Setenv(executor.RepoOwnerEnvVar, "acme")
	t.Setenv(executor.RepoNameEnvVar, "web")
	configRepoPushEnv(t, filepath.Dir(dir))
	t.Setenv(executor.InputEnvVar("head"), "goobers/tutor/run-1")
	t.Setenv(executor.InputEnvVar("confineToActionRoots"), "true")
	t.Setenv(executor.InputEnvVar("actionRoots"), "gaggles/alpha,skills")
	t.Setenv(executor.CredentialEnvVar(string(capability.ConfigRepoWrite)), "config-write")

	code, stdout, stderr := runArgs(t, "open-pr", t.TempDir())
	if code != 0 {
		t.Fatalf("open-pr: code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	server.mu.Lock()
	defer server.mu.Unlock()
	if len(server.prs) != 1 {
		t.Fatalf("PRs opened in the config repository = %d, want 1", len(server.prs))
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(dir), "pr-result.json")); err != nil {
		t.Fatalf("result file must land in the stage workspace: %v", err)
	}
}

func TestOpenPRConfigTargetEnforcesBoundaryInConfigCheckout(t *testing.T) {
	dir := configRepoCheckoutFixture(t)
	server := newFakeGitHubServer(t, "acme", "workflows")
	providerCmdEnv(t, server, "", "run-1")
	configRepoPushEnv(t, filepath.Dir(dir))
	t.Setenv(executor.InputEnvVar("head"), "goobers/tutor/run-1")
	t.Setenv(executor.InputEnvVar("confineToActionRoots"), "true")
	t.Setenv(executor.InputEnvVar("actionRoots"), "reference-workflows") // product-repo layout: not the config tree
	t.Setenv(executor.CredentialEnvVar(string(capability.ConfigRepoWrite)), "config-write")

	code, _, stderr := runArgs(t, "open-pr", t.TempDir())
	if code == 0 || !strings.Contains(stderr, "write-boundary") {
		t.Fatalf("open-pr: code = %d, stderr = %q, want a write-boundary refusal", code, stderr)
	}
	server.mu.Lock()
	defer server.mu.Unlock()
	if len(server.prs) != 0 {
		t.Fatal("a PR was opened despite the boundary refusal")
	}
}

func TestOpenPRConfigTargetFailsClosedWithProductCredentialOnly(t *testing.T) {
	dir := configRepoCheckoutFixture(t)
	server := newFakeGitHubServer(t, "acme", "workflows")
	providerCmdEnv(t, server, executor.CredentialEnvVar(string(capability.ProviderPRWrite)), "run-1")
	configRepoPushEnv(t, filepath.Dir(dir))
	t.Setenv(executor.InputEnvVar("head"), "goobers/tutor/run-1")

	code, _, stderr := runArgs(t, "open-pr", t.TempDir())
	if code == 0 || !strings.Contains(stderr, "configrepo:write") {
		t.Fatalf("open-pr: code = %d, stderr = %q, want a configrepo:write failure", code, stderr)
	}
	server.mu.Lock()
	defer server.mu.Unlock()
	if len(server.prs) != 0 {
		t.Fatal("a PR was opened with a product-repo credential")
	}
}

func TestOpenPRDefaultTargetIgnoresConfigCredential(t *testing.T) {
	root := initDemo(t)
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	providerCmdEnv(t, server, "", "run-1")
	t.Setenv(executor.CredentialEnvVar(string(capability.ConfigRepoWrite)), "config-write")
	t.Chdir(t.TempDir())
	code, _, stderr := runArgs(t, "open-pr", root)
	if code == 0 || !strings.Contains(stderr, executor.CredentialEnvVar(string(capability.ProviderPRWrite))) {
		t.Fatalf("open-pr: code = %d, stderr = %q, want a provider:pr:write failure", code, stderr)
	}
}

func TestConfigCheckoutClonesBaseAndCreatesRunBranch(t *testing.T) {
	origin := initBareOrigin(t)
	work := t.TempDir()
	t.Chdir(work)
	t.Setenv("GOOBERS_RUN_ID", "run-9")
	t.Setenv("GOOBERS_WORKFLOW", "tutor")
	t.Setenv(executor.InputEnvVar(configRepoInput), "acme/workflows")
	t.Setenv("GOOBERS_INSTANCE_ROOT", t.TempDir())
	t.Setenv(executor.CredentialEnvVar(string(capability.ConfigRepoWrite)), "config-write")
	// Route the canonical https URL at the local bare origin.
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(work, "gitconfig"))
	if err := os.WriteFile(filepath.Join(work, "gitconfig"), []byte("[url \""+origin+"\"]\n\tinsteadOf = https://github.com/acme/workflows.git\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	code, stdout, stderr := runArgs(t, "config-checkout")
	if code != 0 {
		t.Fatalf("config-checkout: code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	branch := strings.TrimSpace(runGitOutputT(t, filepath.Join(work, "config-repo"), "symbolic-ref", "--short", "HEAD"))
	if !strings.Contains(branch, "tutor") || !strings.Contains(branch, "run-9") {
		t.Fatalf("branch = %q, want the run's stable tutor branch", branch)
	}
	if _, err := os.Stat(filepath.Join(work, "config-repo", "README.md")); err != nil {
		t.Fatalf("config tree not checked out: %v", err)
	}
	if cfg := runGitOutputT(t, filepath.Join(work, "config-repo"), "config", "--local", "--list"); strings.Contains(cfg, "config-write") {
		t.Fatal("the credential leaked into .git/config")
	}
	if code, _, _ := runArgs(t, "config-checkout"); code == 0 {
		t.Fatal("a second checkout into a non-empty directory must be refused")
	}
}

func TestConfigCheckoutFailsClosedWithoutConfigCredential(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("GOOBERS_RUN_ID", "run-9")
	t.Setenv("GOOBERS_WORKFLOW", "tutor")
	t.Setenv(executor.InputEnvVar(configRepoInput), "acme/workflows")
	t.Setenv("GOOBERS_INSTANCE_ROOT", t.TempDir())
	t.Setenv(executor.CredentialEnvVar(string(capability.RepoPush)), "product-push")
	code, _, stderr := runArgs(t, "config-checkout")
	if code == 0 || !strings.Contains(stderr, "configrepo:write") {
		t.Fatalf("config-checkout: code = %d, stderr = %q", code, stderr)
	}
}
