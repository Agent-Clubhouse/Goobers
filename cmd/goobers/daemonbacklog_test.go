package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/credentials"
	"github.com/goobers/goobers/internal/gate"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/runner"
	"github.com/goobers/goobers/providers"
)

// These tests cover the DAEMON half of personal-gaggle-routing §5.1/§5.2: the
// blocked, failure, circuit-breaker, escalation and comment handlers run in the
// daemon process, not a routed stage, so they resolve spec.backlog from the
// gaggle-scoped layout instead of the injected stage env. Before this, only the
// ADO branch rewrote the addressed repository at all, and nothing anywhere
// honored spec.backlog.connectionRef — so a GitHub or Gitea gaggle whose
// backlog lives in another repository parked, commented on, and counted
// failure streaks against its CODE repository, with the project's token.

const (
	daemonBacklogOwner   = "gim-home"
	daemonBacklogName    = "brandiv.goobers"
	daemonProjectOwner   = "your-org"
	daemonProjectName    = "your-repo"
	daemonConnectionName = "private-backlog"
)

func daemonProjectRepoRef() providers.RepositoryRef {
	return providers.RepositoryRef{
		Provider: providers.ProviderGitHub, Owner: daemonProjectOwner, Name: daemonProjectName,
	}
}

// pointDemoGaggleAtCrossRepositoryBacklog rewrites the scaffolded gaggle's
// spec.backlog so its work items live in another repository behind their own
// connection — the topology the daemon handlers must follow — and declares that
// connection in the Manifest, which validation (REF004) requires.
func pointDemoGaggleAtCrossRepositoryBacklog(t *testing.T, root, gaggle string) {
	t.Helper()
	path := filepath.Join(root, "config", "gaggles", gaggle, "gaggle.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read gaggle config: %v", err)
	}
	updated := strings.Replace(string(data),
		"  backlog:\n    provider: github\n    project: "+daemonProjectOwner+"/"+daemonProjectName+"\n",
		"  backlog:\n    provider: github\n    project: "+daemonBacklogOwner+"/"+daemonBacklogName+"\n", 1)
	if updated == string(data) {
		t.Fatalf("gaggle config did not contain the expected backlog block:\n%s", data)
	}
	// The backlog's own connection: distinct from the project's repo-token.
	withConnection := strings.Replace(updated,
		"    labels:\n      - goobers\n    connectionRef: repo-token\n",
		"    labels:\n      - goobers\n    connectionRef: "+daemonConnectionName+"\n", 1)
	if withConnection == updated {
		t.Fatalf("gaggle config did not contain the expected backlog connectionRef:\n%s", updated)
	}
	if err := os.WriteFile(path, []byte(withConnection), 0o644); err != nil {
		t.Fatalf("write gaggle config: %v", err)
	}
	declareDemoConnection(t, root, daemonConnectionName)
}

// declareDemoConnection adds a Connection to the scaffolded Manifest so a
// gaggle may reference it (api/validate REF004).
func declareDemoConnection(t *testing.T, root, name string) {
	t.Helper()
	path := filepath.Join(root, "config", "manifest.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	const anchor = "  connections:\n"
	if !strings.Contains(string(data), anchor) {
		t.Fatalf("manifest has no connections block:\n%s", data)
	}
	entry := anchor +
		"    - name: " + name + "\n" +
		"      type: backlog\n" +
		"      provider: github\n" +
		"      secretRef:\n" +
		"        name: " + name + "\n"
	updated := strings.Replace(string(data), anchor, entry, 1)
	if err := os.WriteFile(path, []byte(updated), 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
}

// daemonBacklogLayout returns the gaggle-scoped layout the daemon handlers run
// with, for a gaggle whose backlog is a different repository.
func daemonBacklogLayout(t *testing.T) instance.Layout {
	t.Helper()
	root := initDemo(t)
	pointDemoGaggleAtCrossRepositoryBacklog(t, root, "example")
	layout := instance.NewLayout(root).ForGaggle("example")
	if err := os.MkdirAll(layout.SchedulerDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	return layout
}

// daemonBacklogResolver mirrors what buildCredentials registers: the project
// repository binding under "owner/name" and the connection credential under
// credentialRefName(ConnectionCredentialKey(...)).
func daemonBacklogResolver(t *testing.T, withConnection bool) credentials.Resolver {
	t.Helper()
	t.Setenv("DAEMON_PROJECT_TOK", "project-token-value")
	refs := []credentials.TokenRef{
		{Name: daemonProjectOwner + "/" + daemonProjectName, Env: "DAEMON_PROJECT_TOK"},
	}
	if withConnection {
		t.Setenv("DAEMON_BACKLOG_TOK", "backlog-token-value")
		refs = append(refs, credentials.TokenRef{
			Name: credentialRefName(credentials.ConnectionCredentialKey(daemonConnectionName)),
			Env:  "DAEMON_BACKLOG_TOK",
		})
	}
	resolver, err := credentials.NewResolver(refs)
	if err != nil {
		t.Fatalf("NewResolver: %v", err)
	}
	return resolver
}

// daemonRecordingCommenter records the repository every method was handed, so a
// test can prove which container the daemon addressed.
type daemonRecordingCommenter struct {
	updateRepos  []providers.RepositoryRef
	listRepos    []providers.RepositoryRef
	commentRepos []providers.RepositoryRef
	comments     []providers.Comment
	nextID       int
}

func (f *daemonRecordingCommenter) ListComments(_ context.Context, repo providers.RepositoryRef, _ string) ([]providers.Comment, error) {
	f.listRepos = append(f.listRepos, repo)
	return append([]providers.Comment(nil), f.comments...), nil
}

func (f *daemonRecordingCommenter) UpdateWorkItem(_ context.Context, req providers.UpdateWorkItemRequest) (providers.WorkItem, error) {
	f.updateRepos = append(f.updateRepos, req.Repository)
	if req.Comment != "" {
		f.nextID++
		f.comments = append(f.comments, providers.Comment{ID: itoa(f.nextID), Body: req.Comment})
	}
	return providers.WorkItem{}, nil
}

func (f *daemonRecordingCommenter) UpdateComment(_ context.Context, repo providers.RepositoryRef, commentID, body string) error {
	f.commentRepos = append(f.commentRepos, repo)
	for i, c := range f.comments {
		if c.ID == commentID {
			f.comments[i].Body = body
			return nil
		}
	}
	return nil
}

func assertDaemonBacklogRepos(t *testing.T, what string, repos []providers.RepositoryRef) {
	t.Helper()
	if len(repos) == 0 {
		t.Fatalf("%s: the daemon made no provider call", what)
	}
	for _, repo := range repos {
		if repo.Owner != daemonBacklogOwner || repo.Name != daemonBacklogName {
			t.Fatalf("%s addressed %s/%s, want the backlog %s/%s",
				what, repo.Owner, repo.Name, daemonBacklogOwner, daemonBacklogName)
		}
	}
}

func installEscalationPoster(t *testing.T, fake gate.Commenter) *string {
	t.Helper()
	var token string
	previous := newEscalationPoster
	newEscalationPoster = func(t string) gate.Commenter { token = t; return fake }
	t.Cleanup(func() { newEscalationPoster = previous })
	return &token
}

// TestEscalationCommenterAddressesBacklogWithBacklogCredential is the core of
// the daemon-side finding: a GitHub gaggle whose backlog is another repository
// must mutate THAT repository, authenticated as spec.backlog.connectionRef.
// Previously only ADO rewrote the repository, and no provider used the
// connection — so the park/needs-human mutation landed on the code repo with
// the project's token.
func TestEscalationCommenterAddressesBacklogWithBacklogCredential(t *testing.T) {
	layout := daemonBacklogLayout(t)
	fake := &daemonRecordingCommenter{}
	token := installEscalationPoster(t, fake)
	reg := &escTestRegistrar{}

	c := &escalationCommenter{
		resolver: daemonBacklogResolver(t, true),
		reg:      reg,
		layout:   layout,
	}
	if _, err := c.UpdateWorkItem(context.Background(), providers.UpdateWorkItemRequest{
		Repository: daemonProjectRepoRef(),
		ID:         "281",
		AddLabels:  []string{providers.LabelNeedsHuman},
	}); err != nil {
		t.Fatalf("UpdateWorkItem: %v", err)
	}
	assertDaemonBacklogRepos(t, "UpdateWorkItem", fake.updateRepos)
	if *token != "backlog-token-value" {
		t.Fatalf("token = %q, want the backlog connection credential; the backlog was reached with the project's token", *token)
	}

	if _, err := c.ListComments(context.Background(), daemonProjectRepoRef(), "281"); err != nil {
		t.Fatalf("ListComments: %v", err)
	}
	assertDaemonBacklogRepos(t, "ListComments", fake.listRepos)

	fake.comments = []providers.Comment{{ID: "9", Body: "streak"}}
	if err := c.UpdateComment(context.Background(), daemonProjectRepoRef(), "9", "updated"); err != nil {
		t.Fatalf("UpdateComment: %v", err)
	}
	assertDaemonBacklogRepos(t, "UpdateComment", fake.commentRepos)

	var registered bool
	for _, secret := range reg.registered {
		if string(secret) == "backlog-token-value" {
			registered = true
		}
	}
	if !registered {
		t.Fatalf("backlog credential was not registered for scrubbing; registered = %v", reg.registered)
	}
}

// TestEscalationCommenterResolvesBacklogCredentialPerCall keeps #312's rotation
// contract intact across the new connection path: the credential is re-read on
// every call rather than captured once at daemon startup.
func TestEscalationCommenterResolvesBacklogCredentialPerCall(t *testing.T) {
	layout := daemonBacklogLayout(t)
	fake := &daemonRecordingCommenter{}
	token := installEscalationPoster(t, fake)

	c := &escalationCommenter{
		resolver: daemonBacklogResolver(t, true),
		reg:      &escTestRegistrar{},
		layout:   layout,
	}
	req := providers.UpdateWorkItemRequest{Repository: daemonProjectRepoRef(), ID: "281", Comment: "hi"}
	if _, err := c.UpdateWorkItem(context.Background(), req); err != nil {
		t.Fatalf("first UpdateWorkItem: %v", err)
	}
	if *token != "backlog-token-value" {
		t.Fatalf("first token = %q, want backlog-token-value", *token)
	}
	t.Setenv("DAEMON_BACKLOG_TOK", "rotated-backlog-token")
	if _, err := c.UpdateWorkItem(context.Background(), req); err != nil {
		t.Fatalf("second UpdateWorkItem: %v", err)
	}
	if *token != "rotated-backlog-token" {
		t.Fatalf("token after rotation = %q, want the re-read credential", *token)
	}
}

// TestEscalationCommenterFallsBackWithoutDeclaredConnection preserves the
// same-repository majority and the "connection named for documentation only"
// case: with no connection credential configured the handler keeps exactly its
// previous repository binding and token.
func TestEscalationCommenterFallsBackWithoutDeclaredConnection(t *testing.T) {
	for name, crossRepository := range map[string]bool{
		"no distinct backlog":               false,
		"backlog connection with no secret": true,
	} {
		t.Run(name, func(t *testing.T) {
			root := initDemo(t)
			if crossRepository {
				pointDemoGaggleAtCrossRepositoryBacklog(t, root, "example")
			}
			layout := instance.NewLayout(root).ForGaggle("example")
			fake := &daemonRecordingCommenter{}
			token := installEscalationPoster(t, fake)

			c := &escalationCommenter{
				// withConnection=false: the instance declares no credential for
				// the connection, so resolution must fall through.
				resolver: daemonBacklogResolver(t, false),
				reg:      &escTestRegistrar{},
				layout:   layout,
			}
			if _, err := c.UpdateWorkItem(context.Background(), providers.UpdateWorkItemRequest{
				Repository: daemonProjectRepoRef(), ID: "281", AddLabels: []string{providers.LabelNeedsHuman},
			}); err != nil {
				t.Fatalf("UpdateWorkItem: %v", err)
			}
			if *token != "project-token-value" {
				t.Fatalf("token = %q, want the repository binding to remain the fallback", *token)
			}
			wantOwner, wantName := daemonProjectOwner, daemonProjectName
			if crossRepository {
				wantOwner, wantName = daemonBacklogOwner, daemonBacklogName
			}
			got := fake.updateRepos[0]
			if got.Owner != wantOwner || got.Name != wantName {
				t.Fatalf("addressed %s/%s, want %s/%s", got.Owner, got.Name, wantOwner, wantName)
			}
		})
	}
}

// TestApplyCircuitBreakerCountsAndParksOnTheBacklog is the circuit-breaker half
// of the finding. The rewrite has to happen here as well as in the commenter:
// gate.CountFailureStreak reads the comment thread of whatever repository it is
// handed, so counting against the code repo while the comment lands on the
// backlog restarts the streak at 1 every time and the breaker never trips.
func TestApplyCircuitBreakerCountsAndParksOnTheBacklog(t *testing.T) {
	layout := daemonBacklogLayout(t)
	const runID = "run-circuit-backlog"
	ledger, err := localscheduler.OpenClaimLedger(filepath.Join(layout.SchedulerDir(), claimLedgerFileName))
	if err != nil {
		t.Fatal(err)
	}
	if ok, _, err := ledger.Claim("281", runID, "implementation", time.Hour); err != nil || !ok {
		t.Fatalf("seed claim: ok=%v err=%v", ok, err)
	}

	fake := &daemonRecordingCommenter{}
	for i := 0; i < failureStreakThreshold; i++ {
		if err := applyCircuitBreaker(context.Background(), fake, layout,
			daemonProjectRepoRef(), runID, "implement", ""); err != nil {
			t.Fatalf("applyCircuitBreaker call %d: %v", i+1, err)
		}
	}
	assertDaemonBacklogRepos(t, "circuit-breaker streak read", fake.listRepos)
	assertDaemonBacklogRepos(t, "circuit-breaker mutation", fake.updateRepos)

	// The streak must actually accumulate on one thread and trip the breaker.
	var parked bool
	for _, repo := range fake.updateRepos {
		_ = repo
	}
	for _, comment := range fake.comments {
		if strings.Contains(comment.Body, "3") {
			parked = true
		}
	}
	if !parked {
		t.Fatalf("failure streak did not accumulate on the backlog thread: %+v", fake.comments)
	}
}

// TestBlockedHandlerParksOnBacklogWithBacklogCredential covers the blocked path
// end to end through the composition root: the park mutation must reach the
// backlog repository with the backlog credential.
func TestBlockedHandlerParksOnBacklogWithBacklogCredential(t *testing.T) {
	layout := daemonBacklogLayout(t)
	const runID = "run-blocked-backlog"
	ledger, err := localscheduler.OpenClaimLedger(filepath.Join(layout.SchedulerDir(), claimLedgerFileName))
	if err != nil {
		t.Fatal(err)
	}
	if ok, _, err := ledger.Claim("281", runID, "implementation", time.Hour); err != nil || !ok {
		t.Fatalf("seed claim: ok=%v err=%v", ok, err)
	}

	fake := &daemonRecordingCommenter{}
	token := installEscalationPoster(t, fake)
	cfg := &instance.Config{Repos: []instance.RepoRef{{
		Provider: "github", Owner: daemonProjectOwner, Name: daemonProjectName,
		Token: instance.TokenRef{Env: "DAEMON_PROJECT_TOK"},
	}}}
	handler := buildBlockedHandler(layout, cfg, daemonBacklogResolver(t, true), &escTestRegistrar{})
	if handler == nil {
		t.Fatal("blocked handler was not wired")
	}
	if err := handler(context.Background(), runner.BlockedOutcome{
		RunID:   runID,
		RepoRef: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: daemonProjectOwner, Name: daemonProjectName},
		Stage:   "implement",
		Reason:  "waiting on a sibling",
	}); err != nil {
		t.Fatalf("blocked handler: %v", err)
	}
	assertDaemonBacklogRepos(t, "blocked park", fake.updateRepos)
	if *token != "backlog-token-value" {
		t.Fatalf("token = %q, want the backlog connection credential", *token)
	}
}

// TestADOBacklogConnectionRepoSubstitutesOnlyPATAuth is the ADO half of the
// connection-aware daemon construction, mirroring newADOProviderForConnection's
// stage-side rule: only PAT auth has a token ref to substitute, and only when
// the instance actually configures a credential for the declared connection.
// Everything else falls through to the unchanged newADOProviderForStage seam.
func TestADOBacklogConnectionRepoSubstitutesOnlyPATAuth(t *testing.T) {
	root := initDemo(t)
	layout := instance.NewLayout(root)
	adoRepo := providers.RepositoryRef{
		Provider: providers.ProviderADO, Owner: "contoso", Project: "Example Backlog", Name: "service",
	}
	target := daemonBacklogTarget{
		routed: providers.RepositoryRef{
			Provider: providers.ProviderADO, Owner: "contoso", Project: "Example Service", Name: "service",
		},
		repo:          adoRepo,
		connectionRef: daemonConnectionName,
	}

	writeConfig := func(t *testing.T, mutate func(*instance.Config)) {
		t.Helper()
		// Rebuilt from a pristine load every time: subtests run in map order,
		// and one leaving a config the loader rejects would make the next
		// "fall through" for the wrong reason.
		cfg, err := instance.LoadConfig(layout.ConfigFile())
		if err != nil {
			t.Fatal(err)
		}
		cfg.Repos = []instance.RepoRef{{
			Provider: "ado", Owner: "contoso", Project: "Example Service", Name: "service",
			Token: instance.TokenRef{Env: "ADO_PAT"},
		}}
		cfg.Credentials = nil
		mutate(cfg)
		if err := instance.WriteConfig(layout.ConfigFile(), cfg); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			pristine, err := instance.LoadConfig(layout.ConfigFile())
			if err != nil {
				return
			}
			pristine.Repos = nil
			pristine.Credentials = nil
			_ = instance.WriteConfig(layout.ConfigFile(), pristine)
		})
	}
	connectionGrant := []instance.CredentialGrant{{
		Connection: daemonConnectionName, Token: instance.TokenRef{Env: "ADO_BACKLOG_PAT"},
	}}

	t.Run("pat auth with a configured connection credential", func(t *testing.T) {
		writeConfig(t, func(cfg *instance.Config) { cfg.Credentials = connectionGrant })
		repo, ok := adoBacklogConnectionRepo(root, target)
		if !ok {
			t.Fatal("a PAT-auth ADO backlog with a configured connection must use the connection credential")
		}
		if repo.Token.Env != "ADO_BACKLOG_PAT" {
			t.Fatalf("token ref = %+v, want the connection's own credential", repo.Token)
		}
	})

	t.Run("no configured connection credential falls through", func(t *testing.T) {
		writeConfig(t, func(*instance.Config) {})
		if _, ok := adoBacklogConnectionRepo(root, target); ok {
			t.Fatal("a connection with no configured credential must keep the repo's own auth")
		}
	})

	t.Run("entra auth is never redirected", func(t *testing.T) {
		writeConfig(t, func(cfg *instance.Config) {
			// Entra auth authenticates as an ambient identity, so it declares
			// no token ref at all.
			cfg.Repos[0].Token = instance.TokenRef{}
			cfg.Repos[0].Auth = &instance.RepoAuthConfig{Kind: instance.ADOAuthAzureCLI}
			cfg.Credentials = connectionGrant
		})
		if _, err := instance.LoadConfig(layout.ConfigFile()); err != nil {
			t.Fatalf("fixture must remain loadable or the fall-through is vacuous: %v", err)
		}
		if _, ok := adoBacklogConnectionRepo(root, target); ok {
			t.Fatal("azure-cli auth has no token ref to substitute and must be left as configured")
		}
	})

	t.Run("no declared connection falls through", func(t *testing.T) {
		writeConfig(t, func(cfg *instance.Config) { cfg.Credentials = connectionGrant })
		bare := target
		bare.connectionRef = ""
		if _, ok := adoBacklogConnectionRepo(root, bare); ok {
			t.Fatal("a gaggle declaring no backlog connection must be untouched")
		}
	})
}

// TestDaemonBacklogTargetCredentialOrder pins the fallback chain the daemon
// resolves credentials through, so a future change cannot silently reorder it
// and hand the backlog the project's token.
func TestDaemonBacklogTargetCredentialOrder(t *testing.T) {
	backlog := providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: daemonBacklogOwner, Name: daemonBacklogName}
	project := daemonProjectRepoRef()

	withConnection := daemonBacklogTarget{routed: project, repo: backlog, connectionRef: daemonConnectionName}
	wantConnection := credentialRefName(credentials.ConnectionCredentialKey(daemonConnectionName))
	if got := withConnection.credentialRefs(); len(got) != 3 ||
		got[0] != wantConnection ||
		got[1] != daemonBacklogOwner+"/"+daemonBacklogName ||
		got[2] != daemonProjectOwner+"/"+daemonProjectName {
		t.Fatalf("credential refs = %v, want connection, backlog repo, then routed repo", got)
	}

	sameRepository := daemonBacklogTarget{routed: project, repo: project}
	if got := sameRepository.credentialRefs(); len(got) != 1 || got[0] != daemonProjectOwner+"/"+daemonProjectName {
		t.Fatalf("credential refs = %v, want exactly the unchanged repository binding", got)
	}
}
