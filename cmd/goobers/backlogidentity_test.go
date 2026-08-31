package main

import (
	"io"
	"os"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/capability"
	"github.com/goobers/goobers/internal/credentials"
	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/providers"
)

// These tests cover personal-gaggle-routing §5.1/§5.2: a gaggle's backlog is
// resolved — for addressing, for claim scope, and for CREDENTIALS — from
// spec.backlog rather than from the target code repository.

func githubProjectRepo() providers.RepositoryRef {
	return providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "gim-home", Name: "dev-brandiv"}
}

func setBacklogStageEnv(t *testing.T, ref apiv1.BacklogRef) {
	t.Helper()
	t.Setenv(executor.BacklogProviderEnvVar, string(ref.Provider))
	t.Setenv(executor.BacklogProjectEnvVar, ref.Project)
	t.Setenv(executor.BacklogBaseURLEnvVar, ref.BaseURL)
	t.Setenv(executor.BacklogConnectionRefEnvVar, ref.ConnectionRef)
}

// TestStageResolvesBacklogInAnotherRepository is the topology this feature
// exists for: the run targets repo A, the work item lives in private repo B.
func TestStageResolvesBacklogInAnotherRepository(t *testing.T) {
	setBacklogStageEnv(t, apiv1.BacklogRef{
		Provider:      apiv1.ProviderGitHub,
		Project:       "gim-home/brandiv.goobers",
		ConnectionRef: "private-backlog",
	})
	project := githubProjectRepo()

	backlogRepo, err := backlogRepositoryRefForStage("", project)
	if err != nil {
		t.Fatalf("resolve backlog repository: %v", err)
	}
	if backlogRepo.Owner != "gim-home" || backlogRepo.Name != "brandiv.goobers" {
		t.Fatalf("backlog repo = %s/%s, want gim-home/brandiv.goobers", backlogRepo.Owner, backlogRepo.Name)
	}
	if backlogRepo.Name == project.Name {
		t.Fatal("backlog resolution fell back to the project repository")
	}

	identity, err := backlogIdentityForStage("", project)
	if err != nil {
		t.Fatalf("resolve backlog identity: %v", err)
	}
	if identity.Owner != "gim-home" || identity.Name != "brandiv.goobers" {
		t.Fatalf("identity = %+v, want the private backlog", identity)
	}
	if got := backlogConnectionRefForStage("", project); got != "private-backlog" {
		t.Fatalf("connectionRef = %q, want %q", got, "private-backlog")
	}
}

// TestStageBacklogFallsBackToProjectRepository keeps the same-project /
// same-backlog majority behaving exactly as before.
func TestStageBacklogFallsBackToProjectRepository(t *testing.T) {
	t.Setenv(executor.BacklogProviderEnvVar, "")
	t.Setenv("GOOBERS_GAGGLE", "")
	project := githubProjectRepo()

	backlogRepo, err := backlogRepositoryRefForStage("", project)
	if err != nil {
		t.Fatalf("resolve backlog repository: %v", err)
	}
	if backlogRepo.Owner != project.Owner || backlogRepo.Name != project.Name {
		t.Fatalf("backlog repo = %s/%s, want the project repo", backlogRepo.Owner, backlogRepo.Name)
	}
	identity, err := backlogIdentityForStage("", project)
	if err != nil {
		t.Fatalf("resolve backlog identity: %v", err)
	}
	if identity.Owner != project.Owner || identity.Name != project.Name {
		t.Fatalf("identity = %+v, want the project repo", identity)
	}
}

// TestADOBacklogKeepsOrganizationAndOverridesProject preserves the pre-existing
// ADO behavior: only the project tier moves, since the ADO provider is
// organization-scoped.
func TestADOBacklogKeepsOrganizationAndOverridesProject(t *testing.T) {
	setBacklogStageEnv(t, apiv1.BacklogRef{Provider: apiv1.ProviderADO, Project: "Example Backlog"})
	routed := providers.RepositoryRef{
		Provider: providers.ProviderADO, Owner: "contoso", Project: "Example Service", Name: "service",
	}

	backlogRepo, err := backlogRepositoryRefForStage("", routed)
	if err != nil {
		t.Fatalf("resolve backlog repository: %v", err)
	}
	if backlogRepo.Owner != "contoso" || backlogRepo.Name != "service" {
		t.Fatalf("backlog repo = %+v, want organization and repo name preserved", backlogRepo)
	}
	if backlogRepo.Project != "Example Backlog" {
		t.Fatalf("backlog project = %q, want %q", backlogRepo.Project, "Example Backlog")
	}
	identity, err := backlogIdentityForStage("", routed)
	if err != nil {
		t.Fatalf("resolve backlog identity: %v", err)
	}
	if identity.Owner != "contoso" || identity.Project != "Example Backlog" {
		t.Fatalf("identity = %+v, want the ADO organization and backlog project", identity)
	}
}

// TestBacklogIdentityRejectsIncompleteStageContext fails loudly rather than
// silently addressing the code repository when the injected context is broken.
func TestBacklogIdentityRejectsIncompleteStageContext(t *testing.T) {
	t.Setenv(executor.BacklogProviderEnvVar, string(apiv1.ProviderGitHub))
	t.Setenv(executor.BacklogProjectEnvVar, "")
	if _, err := backlogIdentityForStage("", githubProjectRepo()); err == nil {
		t.Fatal("an incomplete backlog stage context must be reported, not silently ignored")
	}

	t.Setenv(executor.BacklogProjectEnvVar, "no-slash")
	if _, err := backlogIdentityForStage("", githubProjectRepo()); err == nil {
		t.Fatal("a malformed GitHub backlog project must be reported")
	}
}

// --- Credential resolution (spec.backlog.connectionRef) ---

// TestBacklogConnectionSuppliesDistinctCredential is the credential half of the
// topology: the project repository keeps its capability-scoped token while the
// backlog authenticates as its own connection, so a private backlog in another
// account never sees the project's PAT and vice versa.
func TestBacklogConnectionSuppliesDistinctCredential(t *testing.T) {
	const (
		projectToken = "project-token"
		backlogToken = "backlog-token"
	)
	t.Setenv(executor.CredentialEnvVar(string(capability.GitHubIssuesWrite)), projectToken)
	t.Setenv(executor.CredentialEnvVar(credentials.ConnectionCredentialKey("private-backlog")), backlogToken)

	got, err := stageProviderToken(stageProviderConfig{capability: capability.GitHubIssuesWrite})
	if err != nil {
		t.Fatalf("project token: %v", err)
	}
	if got != projectToken {
		t.Fatalf("project token = %q, want %q", got, projectToken)
	}

	got, err = stageProviderToken(stageProviderConfig{
		capability:    capability.GitHubIssuesWrite,
		connectionRef: "private-backlog",
	})
	if err != nil {
		t.Fatalf("backlog token: %v", err)
	}
	if got != backlogToken {
		t.Fatalf("backlog token = %q, want the connection credential %q", got, backlogToken)
	}
}

// TestBacklogConnectionFallsBackToCapabilityCredential keeps a gaggle that
// names a connection purely for documentation working when project and backlog
// genuinely share one credential.
func TestBacklogConnectionFallsBackToCapabilityCredential(t *testing.T) {
	t.Setenv(executor.CredentialEnvVar(string(capability.GitHubIssuesWrite)), "shared-token")
	t.Setenv(executor.CredentialEnvVar(credentials.ConnectionCredentialKey("private-backlog")), "")

	got, err := stageProviderToken(stageProviderConfig{
		capability:    capability.GitHubIssuesWrite,
		connectionRef: "private-backlog",
	})
	if err != nil {
		t.Fatalf("fallback token: %v", err)
	}
	if got != "shared-token" {
		t.Fatalf("token = %q, want the capability credential", got)
	}
}

func TestBacklogConnectionFailsClosedWithNoCredential(t *testing.T) {
	t.Setenv(executor.CredentialEnvVar(string(capability.GitHubIssuesWrite)), "")
	t.Setenv(executor.CredentialEnvVar(credentials.ConnectionCredentialKey("private-backlog")), "")
	if _, err := stageProviderToken(stageProviderConfig{
		capability:    capability.GitHubIssuesWrite,
		connectionRef: "private-backlog",
	}); err == nil {
		t.Fatal("a connection with no credential and no fallback must fail closed")
	}
}

// TestCredentialGrantKeyNamespacesConnections proves a connection grant is
// reachable only through its own key space, so declaring a capability can never
// obtain a connection's token (and vice versa).
func TestCredentialGrantKeyNamespacesConnections(t *testing.T) {
	key, err := credentialGrantKey(instance.CredentialGrant{Connection: "private-backlog"})
	if err != nil {
		t.Fatalf("connection grant: %v", err)
	}
	if key != "connection:private-backlog" {
		t.Fatalf("key = %q, want %q", key, "connection:private-backlog")
	}
	if capability.StageDeclarable(key) {
		t.Fatal("a connection key must not be stage-declarable")
	}

	for name, grant := range map[string]instance.CredentialGrant{
		"none":                {},
		"capability and conn": {Capability: string(capability.GitHubIssuesWrite), Connection: "x"},
		"mcp and conn":        {MCP: "foo", Connection: "x"},
		"invalid name":        {Connection: "Not Valid"},
	} {
		if _, err := credentialGrantKey(grant); err == nil {
			t.Errorf("grant %q must be rejected", name)
		}
	}
}

// --- Cross-repository backlog credential wiring ---

// recordStageProviderConfigs replaces the GitHub stage-provider factory with a
// recorder, so a test can assert exactly which repository a provider was built
// FOR and which connection it was built to authenticate AS — the two facts that
// must never disagree once a backlog can live in another account.
func recordStageProviderConfigs(t *testing.T) *[]stageProviderConfig {
	t.Helper()
	var recorded []stageProviderConfig
	original := stageProviderFactories[providers.ProviderGitHub]
	stageProviderFactories[providers.ProviderGitHub] = func(cfg stageProviderConfig) (providers.Provider, error) {
		recorded = append(recorded, cfg)
		token, err := stageProviderToken(cfg)
		if err != nil {
			return nil, err
		}
		return newGitHubProvider(token), nil
	}
	t.Cleanup(func() { stageProviderFactories[providers.ProviderGitHub] = original })
	return &recorded
}

// crossRepositoryBacklogEnv is the topology under test: the run targets repo A
// with the project credential, while the backlog lives in repo B behind its own
// connection credential.
func crossRepositoryBacklogEnv(t *testing.T) (project, backlog providers.RepositoryRef) {
	t.Helper()
	setBacklogStageEnv(t, apiv1.BacklogRef{
		Provider:      apiv1.ProviderGitHub,
		Project:       "gim-home/brandiv.goobers",
		ConnectionRef: "private-backlog",
	})
	t.Setenv(executor.CredentialEnvVar(string(capability.GitHubIssuesWrite)), "project-token")
	t.Setenv(executor.CredentialEnvVar(string(capability.GitHubIssuesRead)), "project-token")
	t.Setenv(executor.CredentialEnvVar(credentials.ConnectionCredentialKey("private-backlog")), "backlog-token")

	project = githubProjectRepo()
	backlog, err := backlogRepositoryRefForStage("", project)
	if err != nil {
		t.Fatalf("resolve backlog repository: %v", err)
	}
	return project, backlog
}

func assertBacklogCredential(t *testing.T, cfg stageProviderConfig, backlog providers.RepositoryRef) {
	t.Helper()
	if cfg.repo.Owner != backlog.Owner || cfg.repo.Name != backlog.Name {
		t.Fatalf("provider built for %s/%s, want the backlog %s/%s",
			cfg.repo.Owner, cfg.repo.Name, backlog.Owner, backlog.Name)
	}
	if cfg.connectionRef != "private-backlog" {
		t.Fatalf("connectionRef = %q, want the backlog connection", cfg.connectionRef)
	}
	token, err := stageProviderToken(cfg)
	if err != nil {
		t.Fatalf("resolve provider token: %v", err)
	}
	if token != "backlog-token" {
		t.Fatalf("token = %q, want the backlog credential; the backlog was reached with the project's token", token)
	}
}

// TestBacklogQueryClaimUsesBacklogRepositoryAndCredential is the claim/query
// half of the finding: --claim's provider must be addressed at the backlog
// repository AND authenticated as the backlog connection. Before this, the
// query path built its provider from the routed code repo, so a cross-account
// backlog was reached with a token that cannot see it.
func TestBacklogQueryClaimUsesBacklogRepositoryAndCredential(t *testing.T) {
	project, backlog := crossRepositoryBacklogEnv(t)
	recorded := recordStageProviderConfigs(t)

	env := backlogQueryEnv{root: "", repo: project, backlogRepo: backlog, stderr: io.Discard}
	if code := env.openProvider(false); code != 0 {
		t.Fatalf("openProvider: exit %d", code)
	}
	if len(*recorded) != 1 {
		t.Fatalf("recorded %d provider builds, want 1", len(*recorded))
	}
	assertBacklogCredential(t, (*recorded)[0], backlog)
}

// TestBacklogQueryReadOnlyUsesBacklogCredential covers the read side: a plain
// or --read-only scan of a cross-account backlog needs the backlog credential
// just as much as a claim does.
func TestBacklogQueryReadOnlyUsesBacklogCredential(t *testing.T) {
	project, backlog := crossRepositoryBacklogEnv(t)
	recorded := recordStageProviderConfigs(t)

	env := backlogQueryEnv{root: "", repo: project, backlogRepo: backlog, stderr: io.Discard}
	if code := env.openProvider(true); code != 0 {
		t.Fatalf("openProvider(readOnly): exit %d", code)
	}
	if len(*recorded) != 1 {
		t.Fatalf("recorded %d provider builds, want 1", len(*recorded))
	}
	assertBacklogCredential(t, (*recorded)[0], backlog)
}

// TestBacklogStageProvidersShareOneCredentialPath walks the centralized
// constructors every backlog-touching stage now goes through — release/route,
// health, assignment, dedupe, close-out, staleness, post-merge — and asserts
// each lands on the backlog repository with the backlog credential. Testing the
// constructors rather than each command is the point of centralizing them: a
// new backlog stage inherits the guarantee instead of re-deriving it.
func TestBacklogStageProvidersShareOneCredentialPath(t *testing.T) {
	project, backlog := crossRepositoryBacklogEnv(t)

	for name, build := range map[string]func() error{
		"issue provider (release/route/close-out)": func() error {
			_, err := newBacklogIssueProviderForStage("", project, backlog)
			return err
		},
		"read-only provider (dedupe)": func() error {
			_, err := newBacklogProviderForStage("", project, backlog, true, withStageProviderCache())
			return err
		},
		"mutating provider (health/assignment)": func() error {
			_, err := newBacklogProviderForStage("", project, backlog, false,
				withStageProviderCache(), withStageProviderMutations("issue"))
			return err
		},
		"typed provider (post-merge/reconcile)": func() error {
			_, err := newBacklogProviderForStageAs[*providers.GitHubProvider]("", project, backlog, false,
				withStageProviderCapability(capability.GitHubIssuesWrite))
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			recorded := recordStageProviderConfigs(t)
			if err := build(); err != nil {
				t.Fatalf("build backlog provider: %v", err)
			}
			if len(*recorded) != 1 {
				t.Fatalf("recorded %d provider builds, want 1", len(*recorded))
			}
			assertBacklogCredential(t, (*recorded)[0], backlog)
		})
	}
}

// TestProjectProviderKeepsProjectCredentialAlongsideBacklog is the separation
// this feature exists for, asserted in one place: the SAME stage holds two
// distinct credentials at once. A provider built for the code repository must
// keep the capability-scoped project token even while the backlog connection
// credential is present in the environment.
func TestProjectProviderKeepsProjectCredentialAlongsideBacklog(t *testing.T) {
	project, _ := crossRepositoryBacklogEnv(t)
	recorded := recordStageProviderConfigs(t)

	if _, err := newProviderForStage("", project, false,
		withStageProviderCapability(capability.GitHubIssuesWrite)); err != nil {
		t.Fatalf("build project provider: %v", err)
	}
	cfg := (*recorded)[0]
	if cfg.connectionRef != "" {
		t.Fatalf("connectionRef = %q, want the project provider to carry none", cfg.connectionRef)
	}
	if cfg.repo.Name != project.Name {
		t.Fatalf("project provider built for %s, want %s", cfg.repo.Name, project.Name)
	}
	token, err := stageProviderToken(cfg)
	if err != nil {
		t.Fatalf("resolve project token: %v", err)
	}
	if token != "project-token" {
		t.Fatalf("project token = %q, want the capability credential; the backlog connection leaked into the project provider", token)
	}
}

// TestBacklogProvidersUnchangedWithoutDeclaredConnection preserves behavior for
// the same-project/same-backlog majority: no connection is requested and the
// provider stays on the routed repository and its capability credential.
func TestBacklogProvidersUnchangedWithoutDeclaredConnection(t *testing.T) {
	t.Setenv(executor.BacklogProviderEnvVar, "")
	t.Setenv(executor.BacklogConnectionRefEnvVar, "")
	t.Setenv("GOOBERS_GAGGLE", "")
	t.Setenv(executor.CredentialEnvVar(string(capability.GitHubIssuesWrite)), "project-token")
	project := githubProjectRepo()
	backlog := backlogRepoRefForStage("", project)
	if backlog.Owner != project.Owner || backlog.Name != project.Name {
		t.Fatalf("backlog = %s/%s, want the project repo", backlog.Owner, backlog.Name)
	}
	recorded := recordStageProviderConfigs(t)

	if _, err := newBacklogIssueProviderForStage("", project, backlog); err != nil {
		t.Fatalf("build backlog provider: %v", err)
	}
	cfg := (*recorded)[0]
	if cfg.connectionRef != "" {
		t.Fatalf("connectionRef = %q, want none when no backlog connection is declared", cfg.connectionRef)
	}
	token, err := stageProviderToken(cfg)
	if err != nil {
		t.Fatalf("resolve token: %v", err)
	}
	if token != "project-token" {
		t.Fatalf("token = %q, want the unchanged capability credential", token)
	}
}

// TestGaggleBacklogRefReadsConfiguredBacklog covers the daemon-side path, where
// no stage env exists and the gaggle name comes from the layout.
func TestGaggleBacklogRefReadsConfiguredBacklog(t *testing.T) {
	t.Setenv(executor.BacklogProviderEnvVar, "")
	root := initDemo(t)
	layout := instance.NewLayout(root)

	gaggle := firstConfiguredGaggleName(t, layout)
	ref, _ := gaggleBacklogRef(layout.ForGaggle(gaggle), githubProjectRepo())
	if ref.Provider == "" || ref.Project == "" {
		t.Fatalf("gaggle %q resolved an empty backlog ref: %+v", gaggle, ref)
	}
}

func firstConfiguredGaggleName(t *testing.T, layout instance.Layout) string {
	t.Helper()
	set, report, err := instance.LoadConfigDir(layout.ConfigDir())
	if err != nil || report == nil || set == nil || len(set.Gaggles) == 0 {
		t.Skipf("demo instance exposes no gaggles to resolve (err=%v)", err)
	}
	return set.Gaggles[0].Name
}

// TestBacklogConnectionRefEnvVarIsInjectedForStages guards the wiring between
// the runner and the stage: without this env var the stage cannot know which
// connection to authenticate as.
func TestBacklogConnectionRefEnvVarIsInjectedForStages(t *testing.T) {
	setBacklogStageEnv(t, apiv1.BacklogRef{
		Provider:      apiv1.ProviderGitHub,
		Project:       "gim-home/brandiv.goobers",
		ConnectionRef: "private-backlog",
	})
	if got := os.Getenv(executor.BacklogConnectionRefEnvVar); got != "private-backlog" {
		t.Fatalf("%s = %q, want %q", executor.BacklogConnectionRefEnvVar, got, "private-backlog")
	}
	if got := backlogConnectionRefForStage("", githubProjectRepo()); got != "private-backlog" {
		t.Fatalf("backlogConnectionRefForStage = %q, want %q", got, "private-backlog")
	}
}
