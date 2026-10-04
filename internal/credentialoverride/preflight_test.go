package credentialoverride

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/credentials"
	"github.com/goobers/goobers/internal/instance"
)

func TestCapabilityActsOnRepo(t *testing.T) {
	github := instance.RepoRef{Provider: string(apiv1.ProviderGitHub)}
	ado := instance.RepoRef{Provider: string(apiv1.ProviderADO)}
	for _, tc := range []struct {
		capability string
		repo       instance.RepoRef
		want       bool
	}{
		{"repo:push", github, true},
		{"repo:push", ado, true},
		{"github:pr:write", github, true},
		{"github:pr:write", ado, false},
		{"ado:pr:complete", ado, true},
		{"ado:pr:complete", github, false},
		{"ado:work-items:write", ado, false},
		{"ado:packaging:read", ado, false},
		{"ado:packaging:read", github, false},
	} {
		if got := capabilityActsOnRepo(tc.capability, tc.repo); got != tc.want {
			t.Errorf("capabilityActsOnRepo(%q, %s) = %t, want %t", tc.capability, tc.repo.Provider, got, tc.want)
		}
	}
}

// TestProbesOnlyRepositoryActingTokenOverrides drives Probes with a minimal
// injected Runner: non-repository, MCP, githubApp and unconfigured entries are
// never probed, and no overrides means no probes.
func TestProbesOnlyRepositoryActingTokenOverrides(t *testing.T) {
	runner := Runner{
		RepoCapabilities: []string{"repo:push"},
		StorageKey:       func(cg instance.CredentialGrant) (string, error) { return cg.Capability, nil },
		BacklogRole:      func(apiv1.RepoRef, apiv1.BacklogRef) *credentials.BacklogRole { return nil },
		FilterGrants:     func(_ []instance.RepoRef, grants []credentials.Grant) []credentials.Grant { return grants },
		ProjectRepo: func(cfg *instance.Config, project apiv1.RepoRef) (instance.RepoRef, bool) {
			for _, repo := range cfg.Repos {
				if repo.Owner == project.Owner && repo.Name == project.Name {
					return repo, true
				}
			}
			return instance.RepoRef{}, false
		},
		MintsCredential: func(instance.RepoRef) bool { return false },
	}
	cfg := &instance.Config{
		Repos: []instance.RepoRef{{Provider: "github", Owner: "acme", Name: "alpha", Token: instance.TokenRef{Env: "ALPHA_TOKEN"}}},
		Credentials: []instance.CredentialGrant{
			{Capability: "agent:model", Token: instance.TokenRef{Env: "MODEL_TOKEN"}},
			{Capability: "repo:push"},
			{Capability: "repo:push", Token: instance.TokenRef{Env: "OVERRIDE_TOKEN"}},
		},
	}
	var gaggle apiv1.Gaggle
	gaggle.Name = "one"
	gaggle.Spec.Project = apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "alpha"}
	set := &instance.ConfigSet{Gaggles: []apiv1.Gaggle{gaggle}}

	got := runner.Probes(cfg, set)
	want := []Probe{{Gaggle: "one", Credential: 2, Capability: "repo:push", Repo: cfg.Repos[0]}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Probes = %+v, want %+v", got, want)
	}
	cfg.Credentials = cfg.Credentials[:2]
	if got := runner.Probes(cfg, set); got != nil {
		t.Fatalf("Probes without a repository override = %+v, want nil", got)
	}
}

func stubProber(reachable, size func(instance.RepoRef, string) error) Prober {
	return Prober{
		Timeout: time.Second,
		Reachable: func(_ context.Context, repo instance.RepoRef, token string, _ credentials.StoreResolver) error {
			return reachable(repo, token)
		},
		Size: func(_ context.Context, repo instance.RepoRef, token string) (int64, error) {
			return 1, size(repo, token)
		},
		Scrub: func(err error, token string) string {
			if token == "" {
				return err.Error()
			}
			return strings.ReplaceAll(err.Error(), token, "[redacted]")
		},
	}
}

// TestCheckProbesEachPairOnceAndReportsFailures: the same (credential,
// repository, kind) is probed once across gaggles, the override replaces the
// repo's own auth, failures are scrubbed and reported at the entry's pointer.
func TestCheckProbesEachPairOnceAndReportsFailures(t *testing.T) {
	t.Setenv("OVERRIDE_TOKEN", "override-secret")
	alpha := instance.RepoRef{Provider: "github", Owner: "acme", Name: "alpha", Auth: &instance.RepoAuthConfig{Kind: instance.GitHubAuthApp}}
	beta := instance.RepoRef{Provider: "ado", Owner: "org", Project: "proj", Name: "beta"}
	cfg := &instance.Config{Credentials: []instance.CredentialGrant{{Capability: "repo:push", Token: instance.TokenRef{Env: "OVERRIDE_TOKEN"}}}}
	probes := []Probe{
		{Gaggle: "one", Capability: "repo:push", Repo: alpha},
		{Gaggle: "again", Capability: "repo:push", Repo: alpha},
		{Gaggle: "two", Capability: "repo:push", Repo: beta},
	}
	calls := map[string]int{}
	prober := stubProber(func(repo instance.RepoRef, token string) error {
		calls[repo.Name]++
		if token != "override-secret" || repo.Token.Env != "OVERRIDE_TOKEN" || repo.Auth != nil {
			t.Errorf("probe of %s used token %q ref %+v auth %+v, want the override as a PAT", repo.Name, token, repo.Token, repo.Auth)
		}
		if repo.Name == "beta" {
			return errors.New("denied for override-secret")
		}
		return nil
	}, func(instance.RepoRef, string) error {
		t.Fatal("unexpected metadata probe")
		return nil
	})
	var stdout bytes.Buffer
	var failures []string
	if prober.Check(cfg, probes, nil, &stdout, func(pointer, message string) { failures = append(failures, pointer+" "+message) }) {
		t.Fatalf("Check passed; want failure\n%s", stdout.String())
	}
	if want := map[string]int{"alpha": 1, "beta": 1}; !reflect.DeepEqual(calls, want) {
		t.Fatalf("calls = %v, want %v", calls, want)
	}
	out := stdout.String()
	if strings.Count(out, "reaches acme/alpha") != 2 || !strings.Contains(out, "for Gaggle/two: token cannot reach target repository org/proj/beta: denied for [redacted]") {
		t.Fatalf("output =\n%s", out)
	}
	if strings.Contains(out, "override-secret") || len(failures) != 1 || !strings.HasPrefix(failures[0], "/credentials/0 ") {
		t.Fatalf("failures = %v output =\n%s", failures, out)
	}
}

func TestCheckUnresolvableTokenSkipsProbe(t *testing.T) {
	t.Setenv("OVERRIDE_TOKEN", "")
	cfg := &instance.Config{Credentials: []instance.CredentialGrant{{Capability: "github:pr:write", Token: instance.TokenRef{Env: "OVERRIDE_TOKEN"}}}}
	fail := func(instance.RepoRef, string) error {
		t.Fatal("probe ran without a resolved token")
		return nil
	}
	var stdout bytes.Buffer
	probes := []Probe{{Gaggle: "one", Capability: "github:pr:write", Repo: instance.RepoRef{Provider: "github", Owner: "acme", Name: "alpha"}, Metadata: true}}
	if stubProber(fail, fail).Check(cfg, probes, nil, &stdout, func(string, string) {}) {
		t.Fatalf("Check passed with an unresolvable token:\n%s", stdout.String())
	}
	if !strings.Contains(stdout.String(), "resolve token") {
		t.Fatalf("output = %q, want a token resolution failure", stdout.String())
	}
}

func TestCheckNoProbesPasses(t *testing.T) {
	var stdout bytes.Buffer
	if !(Prober{}).Check(&instance.Config{}, nil, nil, &stdout, func(string, string) { t.Fatal("unexpected failure") }) || stdout.Len() != 0 {
		t.Fatalf("Check with no probes failed or wrote %q", stdout.String())
	}
}
