package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/credentialoverride"
	"github.com/goobers/goobers/internal/credentials"
	"github.com/goobers/goobers/internal/instance"
)

func credentialOverrideGaggle(name string, project apiv1.RepoRef, backlog apiv1.BacklogRef) apiv1.Gaggle {
	var g apiv1.Gaggle
	g.Name = name
	g.Spec.Project = project
	g.Spec.Backlog = backlog
	return g
}

func githubOverrideGaggle(name, repo string) apiv1.Gaggle {
	return credentialOverrideGaggle(name,
		apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: repo},
		apiv1.BacklogRef{Provider: "github", Project: "acme/" + repo})
}

func credentialOverrideFixture() (*instance.Config, *instance.ConfigSet) {
	cfg := &instance.Config{
		Repos: []instance.RepoRef{
			{Provider: "github", Owner: "acme", Name: "alpha", Token: instance.TokenRef{Env: "ALPHA_TOKEN"}},
			{Provider: "github", Owner: "acme", Name: "beta", Token: instance.TokenRef{Env: "BETA_TOKEN"}},
		},
		Credentials: []instance.CredentialGrant{
			{Capability: "repo:push", Token: instance.TokenRef{Env: "OVERRIDE_TOKEN"}},
			// agent:model never acts on the target repository: not probed.
			{Capability: "agent:model", Token: instance.TokenRef{Env: "MODEL_TOKEN"}},
			{Capability: "github:pr:write", Token: instance.TokenRef{Env: "OVERRIDE_TOKEN"}},
		},
	}
	set := &instance.ConfigSet{Gaggles: []apiv1.Gaggle{githubOverrideGaggle("one", "alpha"), githubOverrideGaggle("two", "beta")}}
	return cfg, set
}

func overrideProbeRepoName(repo instance.RepoRef) string {
	if repo.Project != "" {
		return repo.Owner + "/" + repo.Project + "/" + repo.Name
	}
	return repo.Owner + "/" + repo.Name
}

func summarizeOverrideProbes(probes []credentialoverride.Probe) []string {
	var summary []string
	for _, probe := range probes {
		kind := "git"
		if probe.Metadata {
			kind = "metadata"
		}
		summary = append(summary, fmt.Sprintf("%s credentials[%d] %s -> %s (%s)", probe.Gaggle, probe.Credential, probe.Capability, overrideProbeRepoName(probe.Repo), kind))
	}
	return summary
}

// TestGaggleCredentialOverrideProbesMaterializesPerGaggle is #2744's
// enumeration half: an unqualified credentials: override lands in EVERY
// gaggle's grants, so each gaggle gets its own probe against its own repo.
// Only repo:push is held to git access; other GitHub capabilities need only
// repository metadata access.
func TestGaggleCredentialOverrideProbesMaterializesPerGaggle(t *testing.T) {
	cfg, set := credentialOverrideFixture()
	got := summarizeOverrideProbes(gaggleCredentialOverrideProbes(cfg, set))
	want := []string{
		"one credentials[0] repo:push -> acme/alpha (git)",
		"one credentials[2] github:pr:write -> acme/alpha (metadata)",
		"two credentials[0] repo:push -> acme/beta (git)",
		"two credentials[2] github:pr:write -> acme/beta (metadata)",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("probes =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

func TestGaggleCredentialOverrideProbesNoOverrides(t *testing.T) {
	cfg, set := credentialOverrideFixture()
	cfg.Credentials = cfg.Credentials[1:2] // agent:model only
	if got := gaggleCredentialOverrideProbes(cfg, set); len(got) != 0 {
		t.Fatalf("probes = %+v, want none for a non-repository override", got)
	}
}

// TestGaggleCredentialOverrideProbesHarnessScoped: a harness-scoped entry is
// a distinct runner-owned source (#5148) and is probed like an unscoped one.
func TestGaggleCredentialOverrideProbesHarnessScoped(t *testing.T) {
	cfg, set := credentialOverrideFixture()
	cfg.Credentials = []instance.CredentialGrant{
		{Capability: "repo:push", Token: instance.TokenRef{Env: "OVERRIDE_TOKEN"}},
		{Capability: "repo:push", Harness: "claude-code", Token: instance.TokenRef{Env: "SCOPED_TOKEN"}},
	}
	set.Gaggles = set.Gaggles[:1]
	got := summarizeOverrideProbes(gaggleCredentialOverrideProbes(cfg, set))
	want := []string{
		"one credentials[0] repo:push -> acme/alpha (git)",
		"one credentials[1] repo:push -> acme/alpha (git)",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("probes = %v, want %v", got, want)
	}
}

// TestGaggleCredentialOverrideProbesSkipsOtherProviderCapabilities: a
// provider-specific override is never exercised against another provider's
// repository, and ado:work-items:write acts on Boards, not the repository —
// probing either would fail a valid mixed-provider config.
func TestGaggleCredentialOverrideProbesSkipsOtherProviderCapabilities(t *testing.T) {
	cfg := &instance.Config{
		Repos: []instance.RepoRef{
			{Provider: "github", Owner: "acme", Name: "alpha", Token: instance.TokenRef{Env: "ALPHA_TOKEN"}},
			{Provider: "ado", Owner: "org", Project: "proj", Name: "code", Token: instance.TokenRef{Env: "ADO_TOKEN"}},
		},
		Credentials: []instance.CredentialGrant{
			{Capability: "ado:pr:complete", Token: instance.TokenRef{Env: "ADO_OVERRIDE"}},
			{Capability: "ado:work-items:write", Token: instance.TokenRef{Env: "ADO_OVERRIDE"}},
			{Capability: "github:issues:write", Token: instance.TokenRef{Env: "GH_OVERRIDE"}},
		},
	}
	set := &instance.ConfigSet{Gaggles: []apiv1.Gaggle{
		githubOverrideGaggle("gh", "alpha"),
		credentialOverrideGaggle("az",
			apiv1.RepoRef{Provider: apiv1.ProviderADO, Owner: "org", Project: "proj", Name: "code"},
			apiv1.BacklogRef{Provider: "ado", Project: "proj"}),
	}}
	got := summarizeOverrideProbes(gaggleCredentialOverrideProbes(cfg, set))
	want := []string{
		"gh credentials[2] github:issues:write -> acme/alpha (metadata)",
		"az credentials[0] ado:pr:complete -> org/proj/code (git)",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("probes =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// TestGaggleCredentialOverrideProbesRoutesBacklogRole: with a cross-provider
// backlog, a backlog-family override acts on the backlog repository, not the
// Azure DevOps project repository.
func TestGaggleCredentialOverrideProbesRoutesBacklogRole(t *testing.T) {
	cfg := &instance.Config{
		Repos: []instance.RepoRef{
			{Provider: "ado", Owner: "org", Project: "proj", Name: "code", Token: instance.TokenRef{Env: "ADO_TOKEN"}},
			{Provider: "github", Owner: "acme", Name: "tracker", Token: instance.TokenRef{Env: "GH_TOKEN"}},
		},
		Credentials: []instance.CredentialGrant{
			{Capability: "github:issues:write", Token: instance.TokenRef{Env: "OVERRIDE_TOKEN"}},
		},
	}
	set := &instance.ConfigSet{Gaggles: []apiv1.Gaggle{credentialOverrideGaggle("cross",
		apiv1.RepoRef{Provider: apiv1.ProviderADO, Owner: "org", Project: "proj", Name: "code"},
		apiv1.BacklogRef{Provider: "github", Project: "acme/tracker"})}}
	got := summarizeOverrideProbes(gaggleCredentialOverrideProbes(cfg, set))
	if want := []string{"cross credentials[0] github:issues:write -> acme/tracker (metadata)"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("probes = %v, want %v", got, want)
	}
}

func stubOverrideProbes(t *testing.T, reachable func(instance.RepoRef, string) error, metadata func(instance.RepoRef, string) error) {
	t.Helper()
	originalReachable := targetRepositoryReachable
	originalSize := targetRepositorySize
	t.Cleanup(func() {
		targetRepositoryReachable = originalReachable
		targetRepositorySize = originalSize
	})
	targetRepositoryReachable = func(_ context.Context, repo instance.RepoRef, token string, _ credentials.StoreResolver) error {
		return reachable(repo, token)
	}
	targetRepositorySize = func(_ context.Context, repo instance.RepoRef, token string) (int64, error) {
		return 1, metadata(repo, token)
	}
}

// TestCheckGaggleCredentialOverrideAccessFailsOnUnreachableGaggleRepo is the
// issue's failure mode: the override reaches gaggle one's repo but not gaggle
// two's, which must be a validation error naming the credentials: entry.
func TestCheckGaggleCredentialOverrideAccessFailsOnUnreachableGaggleRepo(t *testing.T) {
	cfg, set := credentialOverrideFixture()
	t.Setenv("OVERRIDE_TOKEN", "override-secret")
	calls := map[string]int{}
	probe := func(kind string) func(instance.RepoRef, string) error {
		return func(repo instance.RepoRef, token string) error {
			calls[kind+" "+repo.Owner+"/"+repo.Name]++
			if token != "override-secret" || repo.Token.Env != "OVERRIDE_TOKEN" || repo.Auth != nil {
				t.Errorf("%s probe of %s/%s used token %q / ref %+v, want the override", kind, repo.Owner, repo.Name, token, repo.Token)
			}
			if repo.Name == "beta" {
				return errors.New("404 not found for override-secret")
			}
			return nil
		}
	}
	stubOverrideProbes(t, probe("git"), probe("metadata"))
	var stdout bytes.Buffer
	diagnostics := &diagnosticCollector{}
	if checkGaggleCredentialOverrideAccess("", "instance.yaml", cfg, set, nil, &stdout, diagnostics) {
		t.Fatalf("check passed; want failure\n%s", stdout.String())
	}
	out := stdout.String()
	for _, want := range []string{
		"CREDENTIAL credentials[0] (repo:push) for Gaggle/one: reaches acme/alpha",
		"CREDENTIAL credentials[0] (repo:push) for Gaggle/two: token cannot reach target repository acme/beta",
		"CREDENTIAL credentials[2] (github:pr:write) for Gaggle/two: token cannot reach target repository acme/beta",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "override-secret") {
		t.Fatalf("output leaked the override token:\n%s", out)
	}
	wantCalls := map[string]int{"git acme/alpha": 1, "git acme/beta": 1, "metadata acme/alpha": 1, "metadata acme/beta": 1}
	if !reflect.DeepEqual(calls, wantCalls) {
		t.Fatalf("probe calls = %v, want %v", calls, wantCalls)
	}
	var codes []string
	for _, finding := range diagnostics.findings {
		codes = append(codes, finding.Code+" "+finding.Path)
	}
	if want := []string{"REPO004 /credentials/0", "REPO004 /credentials/2"}; !reflect.DeepEqual(codes, want) {
		t.Fatalf("diagnostics = %v, want %v", codes, want)
	}
}

// TestCheckGaggleCredentialOverrideAccessProbesSharedRepoOnce: two gaggles on
// one repository each report, but the network probe runs once.
func TestCheckGaggleCredentialOverrideAccessProbesSharedRepoOnce(t *testing.T) {
	cfg, set := credentialOverrideFixture()
	cfg.Credentials = cfg.Credentials[:1]
	set.Gaggles = []apiv1.Gaggle{githubOverrideGaggle("one", "alpha"), githubOverrideGaggle("again", "alpha")}
	t.Setenv("OVERRIDE_TOKEN", "override-secret")
	calls := 0
	stubOverrideProbes(t, func(instance.RepoRef, string) error { calls++; return nil }, func(instance.RepoRef, string) error {
		t.Fatal("unexpected metadata probe")
		return nil
	})
	var stdout bytes.Buffer
	if !checkGaggleCredentialOverrideAccess("", "instance.yaml", cfg, set, nil, &stdout, &diagnosticCollector{}) {
		t.Fatalf("check failed:\n%s", stdout.String())
	}
	if calls != 1 || strings.Count(stdout.String(), "reaches acme/alpha") != 2 {
		t.Fatalf("calls=%d output=\n%s", calls, stdout.String())
	}
}

// TestCheckGaggleCredentialOverrideAccessADOUsesOverrideAsPAT: an ADO
// repository's own auth (here azure-cli) is replaced by the override token
// used as a PAT, which is what a stage holding the grant receives.
func TestCheckGaggleCredentialOverrideAccessADOUsesOverrideAsPAT(t *testing.T) {
	cfg := &instance.Config{
		Repos: []instance.RepoRef{{Provider: "ado", Owner: "org", Project: "proj", Name: "code",
			Auth: &instance.RepoAuthConfig{Kind: instance.ADOAuthAzureCLI}}},
		Credentials: []instance.CredentialGrant{{Capability: "ado:pr:complete", Token: instance.TokenRef{Env: "ADO_OVERRIDE"}}},
	}
	set := &instance.ConfigSet{Gaggles: []apiv1.Gaggle{credentialOverrideGaggle("az",
		apiv1.RepoRef{Provider: apiv1.ProviderADO, Owner: "org", Project: "proj", Name: "code"},
		apiv1.BacklogRef{Provider: "ado", Project: "proj"})}}
	t.Setenv("ADO_OVERRIDE", "ado-pat")
	var probed instance.RepoRef
	stubOverrideProbes(t, func(repo instance.RepoRef, _ string) error { probed = repo; return nil }, func(instance.RepoRef, string) error {
		t.Fatal("unexpected metadata probe for an ADO repository")
		return nil
	})
	var stdout bytes.Buffer
	if !checkGaggleCredentialOverrideAccess("", "instance.yaml", cfg, set, nil, &stdout, &diagnosticCollector{}) {
		t.Fatalf("check failed:\n%s", stdout.String())
	}
	if probed.Auth != nil || probed.Token.Env != "ADO_OVERRIDE" || probed.Project != "proj" {
		t.Fatalf("probed repo = %+v, want the override token as a PAT", probed)
	}
}

func TestCheckGaggleCredentialOverrideAccessUnresolvableToken(t *testing.T) {
	cfg, set := credentialOverrideFixture()
	cfg.Credentials = cfg.Credentials[:1]
	t.Setenv("OVERRIDE_TOKEN", "")
	fail := func(instance.RepoRef, string) error {
		t.Fatal("probe ran without a resolved token")
		return nil
	}
	stubOverrideProbes(t, fail, fail)
	var stdout bytes.Buffer
	if checkGaggleCredentialOverrideAccess("", "instance.yaml", cfg, set, nil, &stdout, &diagnosticCollector{}) {
		t.Fatalf("check passed with an unresolvable override token:\n%s", stdout.String())
	}
	if !strings.Contains(stdout.String(), "resolve token") {
		t.Fatalf("output = %q, want a token resolution failure", stdout.String())
	}
}

// TestValidateCheckReposPreflightsCredentialOverride drives the real command:
// repos[] reach their own repos, yet the override cannot reach the gaggle's
// repo, so --check-repos must exit 1 rather than certify clean.
func TestValidateCheckReposPreflightsCredentialOverride(t *testing.T) {
	root := filepath.Join(t.TempDir(), "override")
	if code, _, stderr := runArgs(t, "init", root); code != 0 {
		t.Fatalf("init: code=%d stderr=%q", code, stderr)
	}
	instancePath := filepath.Join(root, "instance.yaml")
	data, err := os.ReadFile(instancePath)
	if err != nil {
		t.Fatal(err)
	}
	data = append(data, []byte("credentials:\n  - capability: repo:push\n    token:\n      env: GOOBERS_TEST_OVERRIDE_TOKEN\n")...)
	if err := os.WriteFile(instancePath, data, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GOOBERS_GITHUB_TOKEN", "repo-token")
	t.Setenv("GOOBERS_TEST_OVERRIDE_TOKEN", "override-token")
	stubRepositoryRealityChecks(t, []string{"goobers", "goobers:claimed"}, 1, 1)
	overrideReaches := true
	stubOverrideProbes(t, func(_ instance.RepoRef, token string) error {
		if token == "override-token" && !overrideReaches {
			return errors.New("repository not found")
		}
		return nil
	}, func(instance.RepoRef, string) error { return nil })

	code, stdout, stderr := runArgs(t, "validate", "--check-repos", root)
	if code != 0 || !strings.Contains(stdout, "CREDENTIAL credentials[0] (repo:push) for Gaggle/") {
		t.Fatalf("reachable override: code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}

	overrideReaches = false
	code, stdout, stderr = runArgs(t, "validate", "--check-repos", root)
	if code != 1 || !strings.Contains(stdout, "token cannot reach target repository your-org/your-repo") {
		t.Fatalf("unreachable override: code=%d, want 1; stdout=%q stderr=%q", code, stdout, stderr)
	}
}
