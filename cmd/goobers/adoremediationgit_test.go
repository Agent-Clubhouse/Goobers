package main

import (
	"encoding/base64"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/providers"
)

// TestADORemediationGitAuthEnvironmentUsesDeliveredRepoPush pins that
// remediation Git auth on Azure DevOps is built from the repo:push credential
// the stage was delivered, in the scheme the daemon stated beside it, and not
// from the repository's configured auth: the instance config here names Azure
// CLI auth, and the stage must still send exactly the delivered value.
func TestADORemediationGitAuthEnvironmentUsesDeliveredRepoPush(t *testing.T) {
	root, _ := providerDispatchFixture(t, providers.ProviderADO)
	cfg, err := instance.LoadConfig(layoutFor(root).ConfigFile())
	if err != nil {
		t.Fatal(err)
	}
	cfg.Repos[0].Auth = &instance.RepoAuthConfig{Kind: instance.ADOAuthAzureCLI}
	cfg.Repos[0].Token = instance.TokenRef{}
	if err := instance.WriteConfig(layoutFor(root).ConfigFile(), cfg); err != nil {
		t.Fatal(err)
	}
	const remote = "https://dev.azure.com/acme/project/_git/web"
	for _, tc := range []struct {
		name       string
		scheme     string
		wantHeader string
		wantMSA    bool
	}{
		{name: "bearer", scheme: "bearer", wantHeader: "AUTHORIZATION: Bearer delivered-push-token", wantMSA: true},
		{name: "basic", scheme: "basic", wantHeader: "AUTHORIZATION: Basic " + base64.StdEncoding.EncodeToString([]byte("goobers:delivered-push-token"))},
		{name: "no scheme defaults to basic", wantHeader: "AUTHORIZATION: Basic " + base64.StdEncoding.EncodeToString([]byte("goobers:delivered-push-token"))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(executor.CredentialEnvVar("repo:push"), "delivered-push-token")
			t.Setenv(executor.RepoAuthSchemeEnvVar, tc.scheme)
			resolve, err := adoRemediationGitAuthEnvironment()
			if err != nil {
				t.Fatal(err)
			}
			env, err := resolve(t.Context(), remote)
			if err != nil {
				t.Fatal(err)
			}
			joined := strings.Join(env, "\n")
			if !strings.Contains(joined, "GIT_CONFIG_VALUE_1="+tc.wantHeader+"\n") {
				t.Fatalf("git environment does not carry %q: %q", tc.wantHeader, joined)
			}
			if got := strings.Contains(joined, "X-VSS-ForceMsaPassThrough: true"); got != tc.wantMSA {
				t.Fatalf("MSA passthrough header present = %v, want %v", got, tc.wantMSA)
			}
		})
	}
}

// TestADORemediationGitAuthEnvironmentRequiresDeclaredRepoPush pins "an
// undeclared capability means no credential" on Azure DevOps: with no
// delivered repo:push there is no Git credential, whatever the instance
// config says.
func TestADORemediationGitAuthEnvironmentRequiresDeclaredRepoPush(t *testing.T) {
	t.Setenv(executor.CredentialEnvVar("repo:push"), "")
	if _, err := adoRemediationGitAuthEnvironment(); err == nil || !strings.Contains(err.Error(), "GOOBERS_CRED_REPO_PUSH") {
		t.Fatalf("error = %v, want a missing GOOBERS_CRED_REPO_PUSH failure", err)
	}
}

func TestADORemediationGitAuthEnvironmentRejectsUnknownScheme(t *testing.T) {
	t.Setenv(executor.CredentialEnvVar("repo:push"), "delivered-push-token")
	t.Setenv(executor.RepoAuthSchemeEnvVar, "digest")
	_, err := adoRemediationGitAuthEnvironment()
	if err == nil || !strings.Contains(err.Error(), executor.RepoAuthSchemeEnvVar) || strings.Contains(err.Error(), "delivered-push-token") {
		t.Fatalf("error = %v, want an unsupported-scheme failure that does not echo the token", err)
	}
}
