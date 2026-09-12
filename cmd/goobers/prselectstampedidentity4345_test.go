package main

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/capability"
	"github.com/goobers/goobers/internal/dispatcher"
	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/providers"
)

// prselectstampedidentity4345_test.go is Goobers#4345's regression evidence.
//
// MEASURED failure: daemonIdentityAuthorLogin — the ONE seam both pr-select
// and gather-sibling-context decide "is this PR ours" through — opened
// instance.yaml itself and returned "" the instant LoadConfig failed. In a
// pod there is no instance.yaml, so both stages dropped to branch-prefix
// ownership while the identical run on the local substrate used identity
// ownership. The same workflow classified the same PR two different ways
// depending only on where the stage happened to run, and #3914's stamped
// identity — already sitting in the provider handed to this very function —
// was never consulted.
//
// These tests build providers through the REAL stage constructor and render
// the pod environment with the REAL dispatcher, for the reason #3914's file
// gives: a stubbed seam would have passed against the bug.

// ownershipIdentity resolves the ownership login exactly as both stages do:
// through a provider built by the real remediation stage constructor, talking
// to a forge that records every request.
func ownershipIdentity(t *testing.T, root string, repo providers.RepositoryRef, forge *recordingForge) (string, error) {
	t.Helper()
	provider, err := remediationStageProvider(root, repo, "stage-token", false)
	if err != nil {
		t.Fatalf("remediationStageProvider: %v", err)
	}
	githubProvider, ok := provider.(*providers.GitHubProvider)
	if !ok {
		t.Fatalf("provider type = %T, want *providers.GitHubProvider", provider)
	}
	githubProvider.BaseURL = forge.server.URL
	return daemonIdentityAuthorLogin(context.Background(), root, githubProvider)
}

// setAppDaemonIdentity configures a github-app DaemonIdentity with slug — the
// local substrate's own answer, which must keep winning where it is readable.
func setAppDaemonIdentity(t *testing.T, root, slug string) {
	t.Helper()
	configPath := layoutFor(root).ConfigFile()
	cfg, err := instance.LoadConfig(configPath)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	cfg.DaemonIdentity = &instance.DaemonIdentityConfig{
		Kind:           instance.GitHubAuthApp,
		AppID:          "123456",
		InstallationID: "42",
		PrivateKey:     &instance.TokenRef{File: "/run/secrets/goobers-app.pem"},
		Slug:           slug,
	}
	if err := instance.WriteConfig(configPath, cfg); err != nil {
		t.Fatalf("WriteConfig: %v", err)
	}
}

// podStageWorkspace is podStageRoot plus the config-as-code directory a stage
// pod's workspace checkout carries, copied from the instance at from. Still
// NO instance.yaml — the local lookup
// remains unavailable, which is the whole point — but the state machinery a
// command-level run touches has somewhere to look.
func podStageWorkspace(t *testing.T, from string) string {
	t.Helper()
	root := podStageRoot(t)
	if err := os.CopyFS(instance.NewLayout(root).ConfigDir(), os.DirFS(instance.NewLayout(from).ConfigDir())); err != nil {
		t.Fatalf("populate pod workspace config dir: %v", err)
	}
	if _, err := instance.LoadConfig(instance.NewLayout(root).ConfigFile()); err == nil {
		t.Fatal("pod workspace loaded an instance config; it must not have one")
	}
	return root
}

// THE #4345 TEST. Under GitHub App auth, PR ownership resolves to the SAME
// login in a pod as it does locally, and costs zero API requests doing it.
//
// Both halves run against one instance config in one test so they cannot
// drift: the pod half's identity is produced by dispatcher.RenderPod from
// that config, never asserted as a literal.
func TestOwnershipIdentityIsTheSameOnSelfAndInAPod(t *testing.T) {
	root := initDemo(t)
	repo := declareGitHubAppAuth(t, root, "goobersbot")
	setAppDaemonIdentity(t, root, "goobersbot")
	t.Setenv(executor.CredentialEnvVar(string(capability.ProviderPRWrite)), "ghs-installation-token")

	unsetForTest(t, dispatcher.ProviderBotLoginEnv)
	localForge := newRecordingForge(t, "")
	localLogin, err := ownershipIdentity(t, root, repo, localForge)
	if err != nil {
		t.Fatalf("local ownership identity: %v", err)
	}
	if localLogin != "goobersbot[bot]" {
		t.Fatalf("local ownership identity = %q, want %q", localLogin, "goobersbot[bot]")
	}

	podStageEnv(t, root, repo)
	podForge := newRecordingForge(t, "")
	podLogin, err := ownershipIdentity(t, podStageRoot(t), repo, podForge)
	if err != nil {
		t.Fatalf("pod ownership identity: %v", err)
	}
	if podLogin != localLogin {
		t.Fatalf("pod ownership identity %q != local %q; the same PR would be classified two different ways depending on where the stage ran, which is Goobers#4345", podLogin, localLogin)
	}
	if paths := podForge.requestedPaths(); len(paths) != 0 {
		t.Fatalf("pod ownership identity cost %d API request(s) %v under App auth, want none", len(paths), paths)
	}
}

// ABLATION, and the acceptance criterion "missing pod identity produces an
// actionable failure, not a branch-prefix fallback". Remove the stamp and the
// same pod must FAIL, naming what is missing — not quietly return "", which
// is precisely the silent reclassification #4345 reports.
func TestOwnershipIdentityInAPodWithoutTheStampFailsClosed(t *testing.T) {
	root := initDemo(t)
	repo := declareGitHubAppAuth(t, root, "goobersbot")
	setAppDaemonIdentity(t, root, "goobersbot")
	t.Setenv(executor.CredentialEnvVar(string(capability.ProviderPRWrite)), "ghs-installation-token")

	podStageEnv(t, root, repo)
	unsetForTest(t, dispatcher.ProviderBotLoginEnv) // the ablation

	forge := newRecordingForge(t, "")
	login, err := ownershipIdentity(t, podStageRoot(t), repo, forge)
	if err == nil {
		t.Fatalf("ownership identity = %q with no error; an unresolved pod identity must not fall back to branch-prefix ownership (Goobers#4345)", login)
	}
	var refused *providers.LoginSelfReportRefusedError
	if !errors.As(err, &refused) {
		t.Fatalf("err = %v, want it to wrap *providers.LoginSelfReportRefusedError", err)
	}
	if !strings.Contains(err.Error(), dispatcher.ProviderBotLoginEnv) {
		t.Fatalf("error %q does not name %s; an operator has to be able to act on it", err, dispatcher.ProviderBotLoginEnv)
	}
	if paths := forge.requestedPaths(); len(paths) != 0 {
		t.Fatalf("failing closed cost %d API request(s) %v, want none", len(paths), paths)
	}
}

// PAT COMPATIBILITY in a pod. The dispatcher stamps the variable PRESENT and
// EMPTY for a repo that declares no bot login: "resolved, nothing declared",
// which is the PAT posture. GET /user is the right answer for it, and it is
// the same answer the local substrate gives for the same repository.
func TestOwnershipIdentityInAPodWithAPATStillSelfReports(t *testing.T) {
	root := initDemo(t) // the demo config declares a PAT repo: no auth.slug
	cfg, err := instance.LoadConfig(layoutFor(root).ConfigFile())
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	repo := githubRepoRefFromConfig(t, cfg)
	t.Setenv(executor.CredentialEnvVar(string(capability.ProviderPRWrite)), "pat-token")

	podStageEnv(t, root, repo)
	forge := newRecordingForge(t, "goobers-daemon")
	login, err := ownershipIdentity(t, podStageRoot(t), repo, forge)
	if err != nil {
		t.Fatalf("pod PAT ownership identity: %v", err)
	}
	if login != "goobers-daemon" {
		t.Fatalf("pod PAT ownership identity = %q, want %q", login, "goobers-daemon")
	}
	if !forge.requested("/user") {
		t.Fatalf("PAT posture made no GET /user; requests = %v", forge.requestedPaths())
	}
}

// THE REGRESSION GUARD the previous attempt at #4345 tripped over. A GitHub
// App daemon identity with NO slug is a VALID, documented configuration (see
// DaemonIdentityConfig.Slug: forward-compatible until #1779): it cannot be
// distinguished from the branch-prefix heuristic, so it returns "" and falls
// back — it must never become an error, and it must never reach
// AuthenticatedLogin, which an installation token cannot satisfy.
func TestOwnershipIdentityForLocalAppWithoutSlugStillFallsBackToBranchPrefix(t *testing.T) {
	root := initDemo(t)
	repo := declareGitHubAppAuth(t, root, "") // no slug anywhere
	setAppDaemonIdentity(t, root, "")
	t.Setenv(executor.CredentialEnvVar(string(capability.ProviderPRWrite)), "ghs-installation-token")
	unsetForTest(t, dispatcher.ProviderBotLoginEnv)

	forge := newRecordingForge(t, "")
	login, err := ownershipIdentity(t, root, repo, forge)
	if err != nil {
		t.Fatalf("App-without-slug ownership identity: %v — this configuration is valid and must keep falling back, not fail", err)
	}
	if login != "" {
		t.Fatalf("App-without-slug ownership identity = %q, want \"\" (branch-prefix fallback)", login)
	}
	if paths := forge.requestedPaths(); len(paths) != 0 {
		t.Fatalf("App-without-slug made %d API request(s) %v; an installation token cannot self-report, so this path must not reach the forge at all", len(paths), paths)
	}
}

// LOCAL PAT, unchanged: the daemon identity's own credential self-reports and
// that login is what ownership compares against (#1780's behaviour).
func TestOwnershipIdentityForLocalPATSelfReports(t *testing.T) {
	root := initDemo(t)
	cfg, err := instance.LoadConfig(layoutFor(root).ConfigFile())
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	repo := githubRepoRefFromConfig(t, cfg)
	setDaemonIdentity(t, root)
	t.Setenv(executor.CredentialEnvVar(string(capability.ProviderPRWrite)), "pat-token")
	unsetForTest(t, dispatcher.ProviderBotLoginEnv)

	forge := newRecordingForge(t, "goobers-daemon")
	login, err := ownershipIdentity(t, root, repo, forge)
	if err != nil {
		t.Fatalf("local PAT ownership identity: %v", err)
	}
	if login != "goobers-daemon" {
		t.Fatalf("local PAT ownership identity = %q, want %q", login, "goobers-daemon")
	}
}

// A TRANSIENT lookup failure is NOT a refusal and must keep failing OPEN: a
// momentary identity hiccup must never block a merge-review cycle outright.
// The forge here 403s /user, which is what a live network or permission
// wobble looks like from this seam.
func TestOwnershipIdentityFailsOpenOnATransientLookupFailure(t *testing.T) {
	root := initDemo(t)
	cfg, err := instance.LoadConfig(layoutFor(root).ConfigFile())
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	repo := githubRepoRefFromConfig(t, cfg)
	setDaemonIdentity(t, root)
	t.Setenv(executor.CredentialEnvVar(string(capability.ProviderPRWrite)), "pat-token")
	unsetForTest(t, dispatcher.ProviderBotLoginEnv)

	forge := newRecordingForge(t, "") // every path, /user included, 403s
	login, err := ownershipIdentity(t, root, repo, forge)
	if err != nil {
		t.Fatalf("transient lookup failure returned err = %v, want the branch-prefix fallback", err)
	}
	if login != "" {
		t.Fatalf("ownership identity = %q, want \"\"", login)
	}
}

// COMMAND LEVEL, pr-select. The seam tests above prove the login resolves in
// a pod; this proves pr-select ACTS on it. The PR's head deliberately carries
// no managed prefix, so branch-prefix ownership — what the stage silently
// fell back to before #4345 — excludes it. Selecting it can only happen by
// comparing its author against the dispatcher-stamped identity.
func TestPRSelectInAPodSelectsByStampedIdentity(t *testing.T) {
	root := initDemo(t)
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	repo := declareGitHubAppAuth(t, root, "goobersbot")
	server.authenticatedLogin = "" // an installation token cannot self-report

	const ownNumber = 30
	const otherNumber = 31
	server.addIssue(ownNumber, "Our PR")
	server.addIssue(otherNumber, "Someone else's PR")
	server.addOpenPR(ownNumber, "feature/unrelated-naming", "main", "sha30head", "shamainbase", false, nil, nil)
	server.addOpenPR(otherNumber, "feature/also-unrelated", "main", "sha31head", "shamainbase", false, nil, nil)
	server.setPRIdentities(ownNumber, "goobersbot[bot]", nil, nil)
	server.setPRIdentities(otherNumber, "someone-else", nil, nil)

	podRoot := podStageWorkspace(t, root)
	podStageEnv(t, root, repo)
	providerCmdEnv(t, server, executor.CredentialEnvVar(string(capability.GitHubPRWrite)), "run-4345")
	t.Chdir(t.TempDir())

	code, stdout, stderr := runArgs(t, "pr-select", podRoot)
	if code != 0 {
		t.Fatalf("pr-select: code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "selected PR #30") {
		t.Fatalf("stdout = %q, want PR #30 selected by the pod's stamped identity", stdout)
	}
}

// COMMAND LEVEL, pr-select, ablation. Same pod, stamp removed: the stage must
// FAIL with an actionable diagnostic rather than quietly reverting to
// branch-prefix ownership and reaching a different verdict than the identical
// run on self.
func TestPRSelectInAPodWithoutTheStampFailsWithADiagnostic(t *testing.T) {
	root := initDemo(t)
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	repo := declareGitHubAppAuth(t, root, "goobersbot")

	server.addIssue(30, "Our PR")
	server.addOpenPR(30, "goobers/implementation/run-4345", "main", "sha30head", "shamainbase", false, nil, nil)
	server.setPRIdentities(30, "goobersbot[bot]", nil, nil)

	podRoot := podStageWorkspace(t, root)
	podStageEnv(t, root, repo)
	providerCmdEnv(t, server, executor.CredentialEnvVar(string(capability.GitHubPRWrite)), "run-4345")
	unsetForTest(t, dispatcher.ProviderBotLoginEnv) // the ablation
	t.Chdir(t.TempDir())

	code, stdout, stderr := runArgs(t, "pr-select", podRoot)
	if code == 0 {
		t.Fatalf("pr-select succeeded in a pod with no resolved identity: stdout = %q", stdout)
	}
	if !strings.Contains(stderr, dispatcher.ProviderBotLoginEnv) {
		t.Fatalf("stderr = %q, want it to name %s so an operator can fix the wiring", stderr, dispatcher.ProviderBotLoginEnv)
	}
}

// COMMAND LEVEL, gather-sibling-context. Advisory mode is the classification
// #4345 names for this stage: under authorScope=any a PR is handled
// automatically only if it is OURS, and with no resolved identity that
// question is answered by the head prefix alone. The selected PR here carries
// an unmanaged head and the bot's author login, so the two answers disagree —
// a pod that ignores the stamp calls it advisory and rejects the pipeline's
// advisoryMode=false, which is the pre-#4345 behaviour.
func TestGatherSiblingContextInAPodClassifiesByStampedIdentity(t *testing.T) {
	root := initDemo(t)
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	repo := declareGitHubAppAuth(t, root, "goobersbot")
	server.authenticatedLogin = ""

	const selected = 40
	server.addIssue(selected, "Our PR on an unmanaged head")
	server.addOpenPR(selected, "feature/unmanaged-head", "main", "sha40head", "shamainbase", false, nil, []fakePRFile{{
		path: "internal/shared.go", status: "modified", additions: 2,
	}})
	server.setPRIdentities(selected, "goobersbot[bot]", nil, nil)

	podRoot := podStageWorkspace(t, root)
	podStageEnv(t, root, repo)
	providerCmdEnv(t, server, executor.CredentialEnvVar(string(capability.GitHubPRWrite)), "run-4345")
	t.Setenv(executor.InputEnvVar("authorScope"), authorScopeAny)
	t.Setenv(executor.InputEnvVar("selectedNumber"), "40")
	t.Setenv(executor.InputEnvVar("advisoryMode"), "false")
	t.Chdir(t.TempDir())

	code, stdout, stderr := runArgs(t, "gather-sibling-context", podRoot)
	if code != 0 {
		t.Fatalf("gather-sibling-context: code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
}

// COMMAND LEVEL, gather-sibling-context, ablation.
func TestGatherSiblingContextInAPodWithoutTheStampFailsWithADiagnostic(t *testing.T) {
	root := initDemo(t)
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	repo := declareGitHubAppAuth(t, root, "goobersbot")

	server.addIssue(40, "Our PR on an unmanaged head")
	server.addOpenPR(40, "feature/unmanaged-head", "main", "sha40head", "shamainbase", false, nil, nil)
	server.setPRIdentities(40, "goobersbot[bot]", nil, nil)

	podRoot := podStageWorkspace(t, root)
	podStageEnv(t, root, repo)
	providerCmdEnv(t, server, executor.CredentialEnvVar(string(capability.GitHubPRWrite)), "run-4345")
	unsetForTest(t, dispatcher.ProviderBotLoginEnv) // the ablation
	t.Setenv(executor.InputEnvVar("authorScope"), authorScopeAny)
	t.Setenv(executor.InputEnvVar("selectedNumber"), "40")
	t.Setenv(executor.InputEnvVar("advisoryMode"), "false")
	t.Chdir(t.TempDir())

	code, stdout, stderr := runArgs(t, "gather-sibling-context", podRoot)
	if code == 0 {
		t.Fatalf("gather-sibling-context succeeded in a pod with no resolved identity: stdout = %q", stdout)
	}
	if !strings.Contains(stderr, dispatcher.ProviderBotLoginEnv) {
		t.Fatalf("stderr = %q, want it to name %s", stderr, dispatcher.ProviderBotLoginEnv)
	}
}
