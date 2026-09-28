package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/apicontract"
	"github.com/goobers/goobers/internal/dispatcher"
	"github.com/goobers/goobers/internal/executor"
)

const podADOCloneURL = "https://dev.azure.com/example-org/example-project/_git/example-repo"

// gitEnvMap reads a child environment the way exec does: the last value of a
// repeated name wins.
func gitEnvMap(env []string) map[string]string {
	out := make(map[string]string, len(env))
	for _, entry := range env {
		name, value, _ := strings.Cut(entry, "=")
		out[name] = value
	}
	return out
}

// A pod's workspace checkout on Azure DevOps sends the delivered credential
// in the scheme the credential plane stated, exactly as push-branch and the
// other in-pod ADO git operations do: a Microsoft Entra token as Bearer plus
// the MSA passthrough header, never as an askpass Basic password.
func TestPodCheckoutSendsADOBearerCredentialWithPassthrough(t *testing.T) {
	creds := []dispatcher.MintedCredential{{Capability: "repo:push", Value: "entra-access-token"}}
	ado := adoCheckoutAuthFor(apiv1.RepoRef{Provider: apiv1.ProviderADO}, "bearer", podADOCloneURL)

	env, err := checkoutGitAuthEnv(context.Background(), t.TempDir(), creds, ado)
	if err != nil {
		t.Fatalf("checkoutGitAuthEnv: %v", err)
	}
	eff := gitEnvMap(composeGitEnv(t.TempDir(), env))
	if _, askpass := eff["GIT_ASKPASS"]; askpass {
		t.Fatal("ADO bearer checkout still installs the askpass helper (Basic x-access-token)")
	}
	scoped := "http." + podADOCloneURL + "/.extraheader"
	if eff["GIT_CONFIG_KEY_1"] != scoped || eff["GIT_CONFIG_VALUE_1"] != "AUTHORIZATION: Bearer entra-access-token" {
		t.Fatalf("authorization slot = %q/%q, want the scoped Bearer header", eff["GIT_CONFIG_KEY_1"], eff["GIT_CONFIG_VALUE_1"])
	}
	if eff["GIT_CONFIG_KEY_2"] != scoped || eff["GIT_CONFIG_VALUE_2"] != "X-VSS-ForceMsaPassThrough: true" {
		t.Fatalf("passthrough slot = %q/%q, want the MSA passthrough header", eff["GIT_CONFIG_KEY_2"], eff["GIT_CONFIG_VALUE_2"])
	}
	if eff["GIT_CONFIG_KEY_3"] != "safe.directory" || eff["GIT_CONFIG_COUNT"] != "4" {
		t.Fatalf("safe.directory slot = %q (count %q), want slot 3 of 4", eff["GIT_CONFIG_KEY_3"], eff["GIT_CONFIG_COUNT"])
	}
}

func TestPodCheckoutSendsADOPATAsBasicHeader(t *testing.T) {
	creds := []dispatcher.MintedCredential{{Capability: "repo:push", Value: "example-pat-value"}}
	ado := adoCheckoutAuthFor(apiv1.RepoRef{Provider: apiv1.ProviderADO}, "basic", podADOCloneURL)

	env, err := checkoutGitAuthEnv(context.Background(), t.TempDir(), creds, ado)
	if err != nil {
		t.Fatalf("checkoutGitAuthEnv: %v", err)
	}
	eff := gitEnvMap(env)
	want := "AUTHORIZATION: Basic " + base64.StdEncoding.EncodeToString([]byte("goobers:example-pat-value"))
	if eff["GIT_CONFIG_VALUE_1"] != want || eff["GIT_CONFIG_COUNT"] != "2" {
		t.Fatalf("PAT checkout header = %q (count %q), want %q in a two-slot layout", eff["GIT_CONFIG_VALUE_1"], eff["GIT_CONFIG_COUNT"], want)
	}
	for _, entry := range env {
		if strings.HasPrefix(entry, "GIT_CONFIG_VALUE_2=") {
			t.Fatalf("PAT checkout sent a passthrough slot: %q", entry)
		}
	}
}

// GitHub and Gitea never get a scheme, and an ADO repository whose plane
// stated none keeps the askpass helper: the scheme is never guessed.
func TestPodCheckoutKeepsAskpassWithoutAStatedADOScheme(t *testing.T) {
	creds := []dispatcher.MintedCredential{{Capability: "repo:push", Value: "t0ken"}}
	for name, ado := range map[string]adoCheckoutAuth{
		"github":             adoCheckoutAuthFor(apiv1.RepoRef{Provider: apiv1.ProviderGitHub}, "bearer", "https://github.com/example-org/example-repo"),
		"ado without scheme": adoCheckoutAuthFor(apiv1.RepoRef{Provider: apiv1.ProviderADO}, "", podADOCloneURL),
	} {
		t.Run(name, func(t *testing.T) {
			if ado != (adoCheckoutAuth{}) {
				t.Fatalf("adoCheckoutAuthFor = %+v, want the zero value", ado)
			}
			env, err := checkoutGitAuthEnv(context.Background(), t.TempDir(), creds, ado)
			if err != nil {
				t.Fatalf("checkoutGitAuthEnv: %v", err)
			}
			if _, ok := gitEnvMap(env)["GIT_ASKPASS"]; !ok {
				t.Fatal("non-ADO checkout lost its askpass helper")
			}
		})
	}
}

// fakeCredentialPlane answers the pod's credential resolve calls with value
// for every requested capability and the given scheme.
func fakeCredentialPlane(t *testing.T, value, scheme string) {
	t.Helper()
	plane := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != apicontract.CredentialResolvePath {
			http.NotFound(w, r)
			return
		}
		var request struct {
			Capabilities []string `json:"capabilities"`
		}
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &request)
		minted := make([]map[string]string, 0, len(request.Capabilities))
		for _, c := range request.Capabilities {
			minted = append(minted, map[string]string{"capability": c, "value": value})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"credentials": minted, "repoAuthScheme": scheme})
	}))
	t.Cleanup(plane.Close)
	t.Setenv(dispatcher.EnvDaemonAPI, plane.URL)
	t.Setenv(dispatcher.EnvPodToken, "pod-token")
	t.Setenv(dispatcher.EnvRunID, "run-pod-ado")
	t.Setenv(dispatcher.EnvStage, "stage")
}

// The checkout-only credential (#3770) keeps the scheme the plane stated for
// it, so a stage that declared no repository capability still checks out an
// Entra-backed repository with the Bearer header.
func TestPodCheckoutCredentialsCarryTheCheckoutResolutionsScheme(t *testing.T) {
	fakeCredentialPlane(t, "entra-access-token", "bearer")
	t.Setenv(dispatcher.EnvCheckoutCapability, "repo:push")

	creds, scheme, err := podCheckoutCredentials(context.Background(), nil, "")
	if err != nil {
		t.Fatalf("podCheckoutCredentials: %v", err)
	}
	if scheme != "bearer" || len(creds) != 1 || creds[0].Value != "entra-access-token" {
		t.Fatalf("podCheckoutCredentials = %+v, %q; want the checkout credential with the bearer scheme", creds, scheme)
	}

	stage := []dispatcher.MintedCredential{{Capability: "github:pr:write", Value: "stage-token"}}
	creds, scheme, err = podCheckoutCredentials(context.Background(), stage, "basic")
	if err != nil {
		t.Fatalf("podCheckoutCredentials: %v", err)
	}
	if scheme != "basic" || len(creds) != 2 || creds[0].Value != "stage-token" {
		t.Fatalf("podCheckoutCredentials = %+v, %q; want the stage's credential first and its scheme", creds, scheme)
	}
}

// Every form an Azure DevOps PAT is sent in is registered with the pod's
// scrubber, not only the raw value: a bare base64 Basic value has no
// "Basic " prefix for the pattern net to key on.
func TestPodStageScrubberRedactsADOPATForms(t *testing.T) {
	const pat = "example-pat-value-0123"
	encoded := base64.StdEncoding.EncodeToString([]byte("goobers:" + pat))
	creds := []dispatcher.MintedCredential{{Capability: "repo:push", Value: pat}}

	scrubbed := string(podStageScrubber(creds, "basic").Scrub([]byte("raw=" + pat + " bare=" + encoded + "\n")))
	if strings.Contains(scrubbed, pat) || strings.Contains(scrubbed, encoded) {
		t.Fatalf("scrubbed = %q, want the raw PAT and its bare base64 Basic form redacted", scrubbed)
	}
	scrubbed = string(podStageScrubber(creds, "").Scrub([]byte("raw=" + pat + "\n")))
	if strings.Contains(scrubbed, pat) {
		t.Fatalf("scrubbed = %q, want the raw value redacted without a scheme", scrubbed)
	}
}

// A failing pod stage's envelope carries its stderr in Error.Message and
// Summary. Those travel into Temporal history, so they carry the SCRUBBED
// stderr, as the local executor's failure message does.
func TestPodFailureEnvelopeMessageIsScrubbed(t *testing.T) {
	const pat = "example-pat-value-0123"
	encoded := base64.StdEncoding.EncodeToString([]byte("goobers:" + pat))
	fakeCredentialPlane(t, pat, "basic")
	t.Setenv(dispatcher.EnvStageCapabilities, `["repo:push"]`)
	t.Setenv(dispatcher.EnvCheckoutCapability, "")
	t.Setenv(dispatcher.EnvStageWorkspace, "")
	t.Setenv(executor.InstanceRootEnvVar, "")
	t.Setenv(dispatcher.EnvStageCommand, `["sh","-c","echo \"token $GOOBERS_CRED_REPO_PUSH\" >&2; echo \"bare `+encoded+`\" >&2; exit 3"]`)
	t.Setenv(dispatcher.EnvStageScript, "")
	t.Setenv(dispatcher.EnvStageTimeout, "10s")

	result := runDeclaredStage(context.Background(), io.Discard, io.Discard)
	if result.Status != apiv1.ResultFailure || result.Error == nil || result.Error.Code != "stage_failed" {
		t.Fatalf("result = %+v, want a stage_failed failure", result)
	}
	if !strings.HasPrefix(result.Error.Message, "exit code 3: ") {
		t.Fatalf("Error.Message = %q, want the exit code and the stage's stderr", result.Error.Message)
	}
	surfaces := map[string]string{
		"Error.Message":   result.Error.Message,
		"Summary":         result.Summary,
		"Outputs[stderr]": result.Outputs["stderr"].(string),
	}
	for name, got := range surfaces {
		for _, secret := range []string{pat, encoded} {
			if strings.Contains(got, secret) {
				t.Fatalf("%s = %q carries a delivered credential form", name, got)
			}
		}
	}
	if !strings.Contains(result.Error.Message, "token ") {
		t.Fatalf("Error.Message = %q lost the stage's stderr", result.Error.Message)
	}
}

// podADOGitOrigin is an Azure DevOps stand-in that records the credential
// headers of every git smart-HTTP discovery request and refuses it, so a
// checkout's authentication is observable on the wire even though the clone
// itself fails.
func podADOGitOrigin(t *testing.T) (cloneURL string, sent func() []string) {
	t.Helper()
	var mu sync.Mutex
	var seen []string
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/info/refs") {
			mu.Lock()
			seen = append(seen, r.Header.Get("Authorization")+"|"+r.Header.Get("X-VSS-ForceMsaPassThrough"))
			mu.Unlock()
		}
		http.Error(w, "forbidden", http.StatusForbidden)
	}))
	t.Cleanup(origin.Close)
	return origin.URL + "/example-org/example-project/_git/example-repo", func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), seen...)
	}
}

// The pod's workspace checkout is wired end to end: the scheme the credential
// plane states reaches runDeclaredStage, then checkoutRepoWorkspace, then the
// git request itself — whether the credential came from the stage's own
// capability or from the checkout-only capability (#3770).
func TestPodStageCheckoutSendsTheStatedADOSchemeOnTheWire(t *testing.T) {
	for name, caps := range map[string]struct{ stage, checkout string }{
		"stage capability":         {stage: `["repo:push"]`},
		"checkout-only capability": {checkout: "repo:push"},
	} {
		t.Run(name, func(t *testing.T) {
			cloneURL, sent := podADOGitOrigin(t)
			prev := checkoutCloneURL
			checkoutCloneURL = func(apiv1.RepoRef) (string, error) { return cloneURL, nil }
			t.Cleanup(func() { checkoutCloneURL = prev })

			fakeCredentialPlane(t, "entra-access-token", "bearer")
			t.Setenv(dispatcher.EnvStageCapabilities, caps.stage)
			t.Setenv(dispatcher.EnvCheckoutCapability, caps.checkout)
			t.Setenv(dispatcher.EnvStageWorkspace, string(apiv1.WorkspaceRepo))
			t.Setenv(dispatcher.EnvWorkspaceBranch, "")
			t.Setenv(dispatcher.EnvWorkflow, "probe")
			t.Setenv(executor.RepoProviderEnvVar, string(apiv1.ProviderADO))
			t.Setenv(executor.RepoOwnerEnvVar, "example-org")
			t.Setenv(executor.RepoProjectEnvVar, "example-project")
			t.Setenv(executor.RepoNameEnvVar, "example-repo")
			t.Setenv(executor.BranchNamespaceEnvVar, "goobers/")
			t.Setenv(executor.BaseBranchEnvVar, "main")
			t.Setenv(executor.InstanceRootEnvVar, "")
			t.Setenv(dispatcher.EnvStageCommand, `["true"]`)
			t.Setenv(dispatcher.EnvStageScript, "")
			t.Setenv(dispatcher.EnvStageTimeout, "30s")
			t.Chdir(t.TempDir())

			result := runDeclaredStage(context.Background(), io.Discard, io.Discard)
			if result.Status == apiv1.ResultSuccess {
				t.Fatalf("result = %+v, want the refused checkout to fail the stage", result)
			}
			got := sent()
			if len(got) == 0 {
				t.Fatalf("checkout never reached the origin; result = %+v", result)
			}
			for _, headers := range got {
				if headers != "Bearer entra-access-token|true" {
					t.Fatalf("checkout sent %q, want the Bearer header plus the MSA passthrough", headers)
				}
			}
		})
	}
}

// The recovery-custody base fetch sends the checkout credential the way the
// checkout does: Bearer plus the MSA passthrough on an Entra-backed Azure
// DevOps repository, and the Basic extraheader everywhere else.
func TestRecoveryBaseFetchUsesTheStatedADOScheme(t *testing.T) {
	fakeCredentialPlane(t, "entra-access-token", "bearer")
	t.Setenv(dispatcher.EnvCheckoutCapability, "repo:push")

	t.Setenv(executor.RepoProviderEnvVar, string(apiv1.ProviderADO))
	env, err := recoveryFetchAuthEnv(context.Background(), podADOCloneURL)
	if err != nil {
		t.Fatalf("recoveryFetchAuthEnv: %v", err)
	}
	eff := gitEnvMap(env)
	scoped := "http." + podADOCloneURL + "/.extraheader"
	if eff["GIT_CONFIG_KEY_1"] != scoped || eff["GIT_CONFIG_VALUE_1"] != "AUTHORIZATION: Bearer entra-access-token" {
		t.Fatalf("authorization slot = %q/%q, want the scoped Bearer header", eff["GIT_CONFIG_KEY_1"], eff["GIT_CONFIG_VALUE_1"])
	}
	if eff["GIT_CONFIG_KEY_2"] != scoped || eff["GIT_CONFIG_VALUE_2"] != "X-VSS-ForceMsaPassThrough: true" {
		t.Fatalf("passthrough slot = %q/%q, want the MSA passthrough header", eff["GIT_CONFIG_KEY_2"], eff["GIT_CONFIG_VALUE_2"])
	}

	t.Setenv(executor.RepoProviderEnvVar, string(apiv1.ProviderGitHub))
	env, err = recoveryFetchAuthEnv(context.Background(), "https://github.com/example-org/example-repo")
	if err != nil {
		t.Fatalf("recoveryFetchAuthEnv: %v", err)
	}
	want := "AUTHORIZATION: basic " + base64.StdEncoding.EncodeToString([]byte("x-access-token:entra-access-token"))
	if got := gitEnvMap(env)["GIT_CONFIG_VALUE_0"]; got != want {
		t.Fatalf("non-ADO recovery fetch header = %q, want the Basic extraheader", got)
	}
}
