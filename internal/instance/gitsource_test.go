package instance

import (
	"context"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/goobers/goobers/internal/testgit"
)

func TestGitSourceLocalTracksCommittedMain(t *testing.T) {
	repo := newGitSourceTestRepo(t, "main-v1\n")
	runGitSourceTest(t, repo, "switch", "-c", "feature")
	writeGitSourceTestFile(t, repo, "config.txt", "feature\n")
	writeGitSourceTestFile(t, repo, "feature-only.txt", "feature\n")
	runGitSourceTest(t, repo, "add", ".")
	runGitSourceTest(t, repo, "commit", "-m", "feature")
	writeGitSourceTestFile(t, repo, "config.txt", "uncommitted feature edit\n")

	source, err := NewGitSource(GitSourceOptions{
		InstanceRoot: t.TempDir(),
		Repository:   repo,
	})
	if err != nil {
		t.Fatalf("NewGitSource: %v", err)
	}
	if !source.local || source.mirror != "" {
		t.Fatalf("local source = %v, mirror = %q; want direct local ref access", source.local, source.mirror)
	}

	first, err := source.Resolve(context.Background())
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	assertGitSourceTestFile(t, first, "config.txt", "main-v1\n")
	if _, err := os.Stat(filepath.Join(first, "feature-only.txt")); !os.IsNotExist(err) {
		t.Fatalf("feature-only file leaked from checked-out branch: %v", err)
	}
	wantFirstRevision := strings.TrimSpace(runGitSourceTest(t, repo, "rev-parse", "main"))
	if filepath.Base(first) != wantFirstRevision {
		t.Fatalf("snapshot revision = %q, want %q", filepath.Base(first), wantFirstRevision)
	}
	reused, err := source.Resolve(context.Background())
	if err != nil {
		t.Fatalf("Resolve unchanged revision: %v", err)
	}
	if reused != first {
		t.Fatalf("unchanged revision resolved to %q, want %q", reused, first)
	}

	runGitSourceTest(t, repo, "restore", "config.txt")
	runGitSourceTest(t, repo, "switch", "main")
	writeGitSourceTestFile(t, repo, "config.txt", "main-v2\n")
	runGitSourceTest(t, repo, "add", "config.txt")
	runGitSourceTest(t, repo, "commit", "-m", "main v2")
	wantSecondRevision := strings.TrimSpace(runGitSourceTest(t, repo, "rev-parse", "main"))
	runGitSourceTest(t, repo, "switch", "feature")

	second, err := source.Resolve(context.Background())
	if err != nil {
		t.Fatalf("Resolve after main commit: %v", err)
	}
	assertGitSourceTestFile(t, second, "config.txt", "main-v2\n")
	if filepath.Base(second) != wantSecondRevision {
		t.Fatalf("snapshot revision = %q, want %q", filepath.Base(second), wantSecondRevision)
	}
	if second == first {
		t.Fatal("snapshot path did not advance after main commit")
	}
	assertGitSourceTestFile(t, first, "config.txt", "main-v1\n")
}

func TestGitSourceRemoteClonesManagedMirrorAndFetchesMain(t *testing.T) {
	repo := newGitSourceTestRepo(t, "remote-v1\n")
	instanceRoot := t.TempDir()
	repositoryURL, servedRepo, auth := newAuthenticatedGitSourceTestServer(t, repo, "workflow-source-token")
	t.Setenv("WORKFLOW_SOURCE_TOKEN", "workflow-source-token")
	registrar := &gitSourceTestRegistrar{}

	source, err := NewWorkflowGitSource(instanceRoot, WorkflowSource{
		Kind:  WorkflowSourceKindGit,
		URL:   repositoryURL,
		Ref:   "main",
		Token: &TokenRef{Env: "WORKFLOW_SOURCE_TOKEN"},
	}, nil, registrar, nil)
	if err != nil {
		t.Fatalf("NewWorkflowGitSource: %v", err)
	}
	if source.local || source.mirror == "" {
		t.Fatalf("remote source = local %v, mirror %q", source.local, source.mirror)
	}

	first, err := source.Resolve(context.Background())
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	assertGitSourceTestFile(t, first, "config.txt", "remote-v1\n")
	if len(registrar.values) != 1 || registrar.values[0] != "workflow-source-token" {
		t.Fatalf("registered secrets after clone = %q, want workflow-source token", registrar.values)
	}

	if got := strings.TrimSpace(runGitSourceTest(t, "", "--git-dir="+source.mirror, "rev-parse", "--is-bare-repository")); got != "true" {
		t.Fatalf("managed repository is bare = %q, want true", got)
	}
	if rel, err := filepath.Rel(instanceRoot, source.mirror); err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		t.Fatalf("managed mirror %q is not beneath instance root %q", source.mirror, instanceRoot)
	}
	if _, err := os.Stat(filepath.Join(source.mirror, "config.txt")); !os.IsNotExist(err) {
		t.Fatalf("managed mirror unexpectedly has a working tree: %v", err)
	}

	writeGitSourceTestFile(t, repo, "config.txt", "remote-v2\n")
	runGitSourceTest(t, repo, "add", "config.txt")
	runGitSourceTest(t, repo, "commit", "-m", "remote v2")
	runGitSourceTest(t, "", "--git-dir="+servedRepo, "fetch", repo, "+refs/heads/*:refs/heads/*")
	runGitSourceTest(t, "", "--git-dir="+servedRepo, "update-server-info")

	second, err := source.Resolve(context.Background())
	if err != nil {
		t.Fatalf("Resolve after remote update: %v", err)
	}
	assertGitSourceTestFile(t, second, "config.txt", "remote-v2\n")
	if second == first {
		t.Fatalf("snapshot did not advance after remote main update: %s", second)
	}
	assertGitSourceTestFile(t, first, "config.txt", "remote-v1\n")
	if len(registrar.values) != 2 || registrar.values[1] != "workflow-source-token" {
		t.Fatalf("registered secrets after fetch = %q, want token resolved for each remote operation", registrar.values)
	}
	if auth.accepted.Load() == 0 {
		t.Fatal("authenticated Git server did not receive the workflow-source token")
	}
}

// TestGitSourceRemoteAuthenticatesWithRelativeInstanceRoot pins the
// gitsource half of the askpass-relative-residual finding: NewGitSource
// resolves InstanceRoot to an absolute path before deriving managedRoot,
// repositoryDir, and the askpass script path from it, so a relative
// InstanceRoot (e.g. "." — the default `goobers up`/`goobers apply`
// instance root) must still authenticate even though this source's git
// subprocesses set no cmd.Dir override at all (gitOutputWithEnv), relying
// entirely on the paths baked into GIT_ASKPASS and its --git-dir/-C
// arguments already being absolute at construction time.
func TestGitSourceRemoteAuthenticatesWithRelativeInstanceRoot(t *testing.T) {
	repo := newGitSourceTestRepo(t, "relative-root-v1\n")
	repositoryURL, _, auth := newAuthenticatedGitSourceTestServer(t, repo, "workflow-source-token")
	t.Setenv("WORKFLOW_SOURCE_TOKEN", "workflow-source-token")
	registrar := &gitSourceTestRegistrar{}

	instanceRoot := t.TempDir()
	t.Chdir(instanceRoot)

	source, err := NewWorkflowGitSource(".", WorkflowSource{
		Kind:  WorkflowSourceKindGit,
		URL:   repositoryURL,
		Ref:   "main",
		Token: &TokenRef{Env: "WORKFLOW_SOURCE_TOKEN"},
	}, nil, registrar, nil)
	if err != nil {
		t.Fatalf("NewWorkflowGitSource: %v", err)
	}
	if !filepath.IsAbs(source.askpass) {
		t.Fatalf("askpass path %q is not absolute for relative instance root %q", source.askpass, ".")
	}

	if _, err := source.Resolve(context.Background()); err != nil {
		t.Fatalf("Resolve with relative instance root: %v", err)
	}
	if auth.accepted.Load() == 0 {
		t.Fatal("authenticated Git server did not receive the workflow-source token")
	}
}

func TestWorkflowGitSourceFailsWhenDedicatedTokenIsWrong(t *testing.T) {
	repo := newGitSourceTestRepo(t, "remote\n")
	repositoryURL, _, auth := newAuthenticatedGitSourceTestServer(t, repo, "correct-workflow-token")

	ambientHome := t.TempDir()
	credentialURL := strings.Replace(repositoryURL, "https://", "https://x-access-token:correct-workflow-token@", 1)
	globalConfig := "[url \"" + strings.TrimSuffix(credentialURL, "repo.git") + "\"]\n" +
		"\tinsteadOf = " + strings.TrimSuffix(repositoryURL, "repo.git") + "\n"
	if err := os.WriteFile(filepath.Join(ambientHome, ".gitconfig"), []byte(globalConfig), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", ambientHome)
	t.Setenv("WORKFLOW_SOURCE_TOKEN", "wrong-workflow-token")

	source, err := NewWorkflowGitSource(t.TempDir(), WorkflowSource{
		Kind:  WorkflowSourceKindGit,
		URL:   repositoryURL,
		Token: &TokenRef{Env: "WORKFLOW_SOURCE_TOKEN"},
	}, nil, &gitSourceTestRegistrar{}, nil)
	if err != nil {
		t.Fatalf("NewWorkflowGitSource: %v", err)
	}
	if _, err := source.Resolve(context.Background()); err == nil {
		t.Fatal("Resolve succeeded with the wrong dedicated workflow-source token")
	}
	if _, err := os.Stat(source.mirror); !os.IsNotExist(err) {
		t.Fatalf("managed mirror exists after authentication failure: %v", err)
	}
	if auth.rejected.Load() == 0 {
		t.Fatal("authenticated Git server did not receive the wrong workflow-source token")
	}
}

func TestWorkflowGitSourceFailsClosedWhenTokenDoesNotResolve(t *testing.T) {
	t.Setenv("EMPTY_WORKFLOW_SOURCE_TOKEN", "")

	source, err := NewWorkflowGitSource(t.TempDir(), WorkflowSource{
		Kind:  WorkflowSourceKindGit,
		URL:   "https://example.invalid/workflows.git",
		Token: &TokenRef{Env: "EMPTY_WORKFLOW_SOURCE_TOKEN"},
	}, nil, &gitSourceTestRegistrar{}, nil)
	if err != nil {
		t.Fatalf("NewWorkflowGitSource: %v", err)
	}
	if _, err := source.Resolve(context.Background()); err == nil ||
		!strings.Contains(err.Error(), "configrepo:read") ||
		!strings.Contains(err.Error(), "empty value") {
		t.Fatalf("Resolve error = %v, want fail-closed configrepo:read credential error", err)
	}
	if _, err := os.Stat(source.mirror); !os.IsNotExist(err) {
		t.Fatalf("managed mirror exists after credential failure: %v", err)
	}
}

// TestWorkflowGitSourceStoreBackedTokenFailsClosedWithoutStores pins the #683
// guard at this seam: a store-backed workflowSource token without a store
// resolver is a construction error, never an unauthenticated source.
func TestWorkflowGitSourceStoreBackedTokenFailsClosedWithoutStores(t *testing.T) {
	_, err := NewWorkflowGitSource(t.TempDir(), WorkflowSource{
		Kind:  WorkflowSourceKindGit,
		URL:   "https://example.invalid/workflows.git",
		Token: &TokenRef{Store: "prod-kv/workflow-source-token"},
	}, nil, &gitSourceTestRegistrar{}, nil)
	if err == nil || !strings.Contains(err.Error(), "no secret store resolver is configured") {
		t.Fatalf("NewWorkflowGitSource error = %v, want fail-closed store-resolver error", err)
	}
}

// gitSourceTestAppTokens fakes the composition root's installation-token
// minting seam (#3274): NewWorkflowGitSource receives a GitTokenSource because
// this package cannot import internal/githubapp, so a test double standing in
// for the real minter exercises exactly the seam production uses.
type gitSourceTestAppTokens struct {
	token string
	mints atomic.Int32
}

func (s *gitSourceTestAppTokens) Token(context.Context) (string, error) {
	s.mints.Add(1)
	return s.token, nil
}

// TestWorkflowGitSourceGitHubAppAuthUsesMintedInstallationToken pins #3274's
// credential flow: with auth kind github-app the remote fetch authenticates
// with the minted installation token — consulted through the minting seam per
// remote operation, so near-expiry refresh in the real minter is picked up —
// and every used token still reaches the journal scrubber via the injector,
// exactly like the static-token path.
func TestWorkflowGitSourceGitHubAppAuthUsesMintedInstallationToken(t *testing.T) {
	repo := newGitSourceTestRepo(t, "app-auth-v1\n")
	repositoryURL, _, auth := newAuthenticatedGitSourceTestServer(t, repo, "minted-installation-token")
	registrar := &gitSourceTestRegistrar{}
	appTokens := &gitSourceTestAppTokens{token: "minted-installation-token"}

	source, err := NewWorkflowGitSource(t.TempDir(), WorkflowSource{
		Kind: WorkflowSourceKindGit,
		URL:  repositoryURL,
		Ref:  "main",
		Auth: &RepoAuthConfig{
			Kind:           GitHubAuthApp,
			AppID:          "123456",
			InstallationID: "654321",
			// Never read here: minting happens behind the fake seam. Only the
			// ref shape must validate.
			PrivateKey: &TokenRef{File: filepath.Join(t.TempDir(), "app-key.pem")},
		},
	}, appTokens, registrar, nil)
	if err != nil {
		t.Fatalf("NewWorkflowGitSource: %v", err)
	}

	snapshot, err := source.Resolve(context.Background())
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	assertGitSourceTestFile(t, snapshot, "config.txt", "app-auth-v1\n")
	if auth.accepted.Load() == 0 {
		t.Fatal("authenticated Git server did not receive the minted installation token")
	}
	if appTokens.mints.Load() == 0 {
		t.Fatal("minting seam was never consulted for the remote fetch")
	}
	if len(registrar.values) == 0 || registrar.values[0] != "minted-installation-token" {
		t.Fatalf("registered secrets = %q, want the minted installation token registered with the journal scrubber", registrar.values)
	}
}

// TestWorkflowGitSourceGitHubAppAuthRequiresMintingSource pins the fail-closed
// half of #3274's composition-root wiring: an auth kind github-app source with
// no installation-token source is a construction error, never a source that
// silently fetches unauthenticated or falls back to a static ref.
func TestWorkflowGitSourceGitHubAppAuthRequiresMintingSource(t *testing.T) {
	_, err := NewWorkflowGitSource(t.TempDir(), WorkflowSource{
		Kind: WorkflowSourceKindGit,
		URL:  "https://example.invalid/workflows.git",
		Auth: &RepoAuthConfig{
			Kind:           GitHubAuthApp,
			AppID:          "123456",
			InstallationID: "654321",
			PrivateKey:     &TokenRef{File: "/run/secrets/app-key.pem"},
		},
	}, nil, &gitSourceTestRegistrar{}, nil)
	if err == nil || !strings.Contains(err.Error(), "requires an installation-token source") {
		t.Fatalf("NewWorkflowGitSource error = %v, want fail-closed missing-minter error", err)
	}
}

// TestWorkflowGitSourceRejectsMintingSourceWithoutGitHubAppAuth pins the other
// direction of the same wiring guard: handing a minting source to a
// static-token (or local) source is a composition-root bug that must fail
// loudly rather than leave it ambiguous which credential reads the config repo.
func TestWorkflowGitSourceRejectsMintingSourceWithoutGitHubAppAuth(t *testing.T) {
	t.Setenv("WORKFLOW_SOURCE_TOKEN", "static-token")
	_, err := NewWorkflowGitSource(t.TempDir(), WorkflowSource{
		Kind:  WorkflowSourceKindGit,
		URL:   "https://example.invalid/workflows.git",
		Token: &TokenRef{Env: "WORKFLOW_SOURCE_TOKEN"},
	}, &gitSourceTestAppTokens{token: "minted"}, &gitSourceTestRegistrar{}, nil)
	if err == nil || !strings.Contains(err.Error(), "only valid for workflowSource auth kind") {
		t.Fatalf("NewWorkflowGitSource error = %v, want minting-source-without-app-auth rejection", err)
	}
}

// TestWorkflowGitSourceRejectsGitHubAppAuthWithStaticToken pins #3274's
// mutual-exclusion rule at the seam that actually fetches: auth and token
// declared together are refused at construction with the same diagnostic the
// config loader gives, mirroring how repos[] treats the pair.
func TestWorkflowGitSourceRejectsGitHubAppAuthWithStaticToken(t *testing.T) {
	t.Setenv("WORKFLOW_SOURCE_TOKEN", "static-token")
	_, err := NewWorkflowGitSource(t.TempDir(), WorkflowSource{
		Kind:  WorkflowSourceKindGit,
		URL:   "https://example.invalid/workflows.git",
		Token: &TokenRef{Env: "WORKFLOW_SOURCE_TOKEN"},
		Auth: &RepoAuthConfig{
			Kind:           GitHubAuthApp,
			AppID:          "123456",
			InstallationID: "654321",
			PrivateKey:     &TokenRef{File: "/run/secrets/app-key.pem"},
		},
	}, &gitSourceTestAppTokens{token: "minted"}, &gitSourceTestRegistrar{}, nil)
	if err == nil || !strings.Contains(err.Error(), "must not configure token") {
		t.Fatalf("NewWorkflowGitSource error = %v, want token/auth mutual-exclusion rejection", err)
	}
}

func TestNewGitSourceRejectsRemoteWithoutCredentials(t *testing.T) {
	_, err := NewGitSource(GitSourceOptions{
		InstanceRoot: t.TempDir(),
		Repository:   "https://example.com/workflow-config.git",
	})
	if err == nil || !strings.Contains(err.Error(), "requires configrepo:read credentials") {
		t.Fatalf("NewGitSource error = %v, want remote credential requirement", err)
	}
}

func TestNewGitSourceRejectsUnsupportedRemoteTransport(t *testing.T) {
	_, err := NewGitSource(GitSourceOptions{
		InstanceRoot: t.TempDir(),
		Repository:   "git@example.com:workflow-config.git",
	})
	if err == nil || !strings.Contains(err.Error(), "must use https") {
		t.Fatalf("NewGitSource error = %v, want HTTPS-only transport rejection", err)
	}
}

func TestGitSourceEnvExcludesAmbientCredentialRefs(t *testing.T) {
	t.Setenv("CODE_REPO_TOKEN", "code-token")
	t.Setenv("WORKFLOW_SOURCE_TOKEN", "workflow-token")
	for _, entry := range gitSourceEnv() {
		if strings.HasPrefix(entry, "CODE_REPO_TOKEN=") || strings.HasPrefix(entry, "WORKFLOW_SOURCE_TOKEN=") {
			t.Fatalf("git source inherited credential ref: %s", entry)
		}
	}
}

func TestGitSourcePreservesCommittedBlobsWithArchiveAttributes(t *testing.T) {
	repo := newGitSourceTestRepo(t, "main\n")
	writeGitSourceTestFile(t, repo, ".gitattributes", "ignored.txt export-ignore\nsubstituted.txt export-subst\n")
	writeGitSourceTestFile(t, repo, "ignored.txt", "committed but export-ignored\n")
	writeGitSourceTestFile(t, repo, "substituted.txt", "$Format:%H$\n")
	runGitSourceTest(t, repo, "add", ".")
	runGitSourceTest(t, repo, "commit", "-m", "archive attributes")

	source, err := NewGitSource(GitSourceOptions{
		InstanceRoot: t.TempDir(),
		Repository:   repo,
	})
	if err != nil {
		t.Fatalf("NewGitSource: %v", err)
	}
	dir, err := source.Resolve(context.Background())
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	assertGitSourceTestFile(t, dir, "ignored.txt", "committed but export-ignored\n")
	assertGitSourceTestFile(t, dir, "substituted.txt", "$Format:%H$\n")
}

func TestGitSourceIgnoresReplacementObjects(t *testing.T) {
	repo := newGitSourceTestRepo(t, "committed\n")
	original := strings.TrimSpace(runGitSourceTest(t, repo, "rev-parse", "main:config.txt"))
	writeGitSourceTestFile(t, repo, "replacement.txt", "replacement\n")
	replacement := strings.TrimSpace(runGitSourceTest(t, repo, "hash-object", "-w", "replacement.txt"))
	runGitSourceTest(t, repo, "replace", original, replacement)

	source, err := NewGitSource(GitSourceOptions{
		InstanceRoot: t.TempDir(),
		Repository:   repo,
	})
	if err != nil {
		t.Fatalf("NewGitSource: %v", err)
	}
	dir, err := source.Resolve(context.Background())
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	assertGitSourceTestFile(t, dir, "config.txt", "committed\n")
}

func TestNewGitSourceRejectsNonBranchRef(t *testing.T) {
	_, err := NewGitSource(GitSourceOptions{
		InstanceRoot: t.TempDir(),
		Repository:   newGitSourceTestRepo(t, "main\n"),
		Ref:          "refs/tags/release",
	})
	if err == nil || !strings.Contains(err.Error(), "not a branch") {
		t.Fatalf("NewGitSource error = %v, want non-branch rejection", err)
	}
}

func newGitSourceTestRepo(t *testing.T, content string) string {
	t.Helper()
	repo := t.TempDir()
	runGitSourceTest(t, repo, "init", "-b", "main")
	runGitSourceTest(t, repo, "config", "user.email", "test@example.com")
	runGitSourceTest(t, repo, "config", "user.name", "Test")
	writeGitSourceTestFile(t, repo, "config.txt", content)
	runGitSourceTest(t, repo, "add", "config.txt")
	runGitSourceTest(t, repo, "commit", "-m", "initial")
	return repo
}

func writeGitSourceTestFile(t *testing.T, root, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(root, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func assertGitSourceTestFile(t *testing.T, root, name, want string) {
	t.Helper()
	got, err := os.ReadFile(filepath.Join(root, name))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != want {
		t.Fatalf("%s = %q, want %q", name, got, want)
	}
}

func runGitSourceTest(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := testgit.Command(args...)
	if dir != "" {
		cmd.Dir = dir
	}
	cmd.Env = append(cmd.Env,
		"GIT_CONFIG_COUNT=3",
		"GIT_CONFIG_KEY_0=core.fsync",
		"GIT_CONFIG_VALUE_0=none",
		"GIT_CONFIG_KEY_1=core.autocrlf",
		"GIT_CONFIG_VALUE_1=false",
		"GIT_CONFIG_KEY_2=core.safecrlf",
		"GIT_CONFIG_VALUE_2=false",
	)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, output)
	}
	return string(output)
}

func newAuthenticatedGitSourceTestServer(t *testing.T, repo, token string) (string, string, *gitSourceAuthRecorder) {
	t.Helper()

	root := t.TempDir()
	servedRepo := filepath.Join(root, "repo.git")
	runGitSourceTest(t, "", "clone", "--bare", repo, servedRepo)
	runGitSourceTest(t, "", "--git-dir="+servedRepo, "update-server-info")

	files := http.FileServer(http.Dir(root))
	auth := &gitSourceAuthRecorder{}
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		username, password, ok := r.BasicAuth()
		if !ok || username != "x-access-token" || password != token {
			if ok {
				auth.rejected.Add(1)
			}
			w.Header().Set("WWW-Authenticate", `Basic realm="git"`)
			http.Error(w, "authentication required", http.StatusUnauthorized)
			return
		}
		auth.accepted.Add(1)
		files.ServeHTTP(w, r)
	}))
	t.Cleanup(server.Close)

	certificate := pem.EncodeToMemory(&pem.Block{
		Type:  "CERTIFICATE",
		Bytes: server.Certificate().Raw,
	})
	certificatePath := filepath.Join(t.TempDir(), "git-source-test-ca.pem")
	if err := os.WriteFile(certificatePath, certificate, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SSL_CERT_FILE", certificatePath)
	return server.URL + "/repo.git", servedRepo, auth
}

type gitSourceAuthRecorder struct {
	accepted atomic.Int32
	rejected atomic.Int32
}

type gitSourceTestRegistrar struct {
	values []string
}

func (r *gitSourceTestRegistrar) Register(secret []byte) {
	r.values = append(r.values, string(secret))
}
