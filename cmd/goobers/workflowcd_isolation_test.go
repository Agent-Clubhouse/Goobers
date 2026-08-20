package main

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/goobers/goobers/internal/capability"
	"github.com/goobers/goobers/internal/credentials"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/testgit"
)

type workflowCDIsolationFixture struct {
	Goober         string                         `json:"goober"`
	CodeCapability string                         `json:"codeCapability"`
	WorkflowSource workflowCDIsolationRepoFixture `json:"workflowSource"`
	CodeRepository workflowCDIsolationRepoFixture `json:"codeRepository"`
}

type workflowCDIsolationRepoFixture struct {
	GitName  string `json:"gitName"`
	Owner    string `json:"owner"`
	Name     string `json:"name"`
	TokenEnv string `json:"tokenEnv"`
	Token    string `json:"token"`
	Marker   string `json:"marker"`
	Content  string `json:"content"`
}

func TestWorkflowCDAdversarialIsolation(t *testing.T) {
	fixture := loadWorkflowCDIsolationFixture(t)
	t.Setenv(fixture.WorkflowSource.TokenEnv, fixture.WorkflowSource.Token)
	t.Setenv(fixture.CodeRepository.TokenEnv, fixture.CodeRepository.Token)

	server := newWorkflowCDIsolationGitServer(t, fixture.WorkflowSource, fixture.CodeRepository)
	workflowURL := server.url(fixture.WorkflowSource)
	codeURL := server.url(fixture.CodeRepository)
	config := workflowCDIsolationConfig(fixture, workflowURL)

	t.Run("dedicated grants stay within their repository", func(t *testing.T) {
		sourceRegistrar := &workflowCDIsolationRegistrar{}
		source, err := instance.NewWorkflowGitSource(
			t.TempDir(),
			*config.WorkflowSource,
			nil,
			sourceRegistrar,
			nil,
		)
		if err != nil {
			t.Fatalf("NewWorkflowGitSource: %v", err)
		}
		before := server.acceptedCount(fixture.WorkflowSource.GitName, fixture.WorkflowSource.Token)
		snapshot, err := source.Resolve(context.Background())
		if err != nil {
			t.Fatalf("resolve workflow source with CD_PAT: %v", err)
		}
		if server.acceptedCount(fixture.WorkflowSource.GitName, fixture.WorkflowSource.Token) <= before {
			t.Fatal("workflow source did not authenticate to the config repository with CD_PAT")
		}
		assertWorkflowCDIsolationMarker(t, snapshot, fixture.WorkflowSource)
		if !sourceRegistrar.saw(fixture.WorkflowSource.Token) || sourceRegistrar.saw(fixture.CodeRepository.Token) {
			t.Fatalf("workflow source registered credentials outside its CD_PAT boundary: %q", sourceRegistrar.values)
		}

		codeWithCDToken := instance.WorkflowSource{
			Kind:  instance.WorkflowSourceKindGit,
			URL:   codeURL,
			Ref:   "main",
			Token: &instance.TokenRef{Env: fixture.WorkflowSource.TokenEnv},
		}
		crossedSource, err := instance.NewWorkflowGitSource(t.TempDir(), codeWithCDToken, nil, &workflowCDIsolationRegistrar{}, nil)
		if err != nil {
			t.Fatalf("NewWorkflowGitSource for adversarial code-repo probe: %v", err)
		}
		before = server.rejectedCount(fixture.CodeRepository.GitName, fixture.WorkflowSource.Token)
		if _, err := crossedSource.Resolve(context.Background()); err == nil {
			t.Fatal("CD reconciler read the code repository with CD_PAT")
		}
		if server.rejectedCount(fixture.CodeRepository.GitName, fixture.WorkflowSource.Token) <= before {
			t.Fatal("CD reconciler failure did not reach the code repository authorization boundary")
		}

		resolver, runnerGrants, err := buildCredentials(config, nil, "", "", nil, nil)
		if err != nil {
			t.Fatalf("buildCredentials: %v", err)
		}
		stageGrants := buildGooberCredentialGrants(
			fixture.Goober,
			[]string{fixture.CodeCapability},
			runnerGrants,
		)
		stageRegistrar := &workflowCDIsolationRegistrar{}
		stageInjector, err := credentials.NewGooberInjector(resolver, fixture.Goober, stageGrants, stageRegistrar)
		if err != nil {
			t.Fatalf("NewGooberInjector: %v", err)
		}
		stageSet, err := stageInjector.Materialize(context.Background(), []string{fixture.CodeCapability})
		if err != nil {
			t.Fatalf("materialize code-stage credentials: %v", err)
		}
		codeToken, err := stageSet.Token(context.Background(), fixture.CodeCapability)
		if err != nil {
			t.Fatalf("resolve code-stage token: %v", err)
		}
		if codeToken != fixture.CodeRepository.Token {
			t.Fatalf("code-stage token = %q, want fixture CODE_PAT", codeToken)
		}
		if !stageRegistrar.saw(fixture.CodeRepository.Token) || stageRegistrar.saw(fixture.WorkflowSource.Token) {
			t.Fatalf("code stage registered credentials outside its CODE_PAT boundary: %q", stageRegistrar.values)
		}
		assertWorkflowCDGitAllowed(t, server, fixture.CodeRepository, codeURL, codeToken)
		assertWorkflowCDGitDenied(t, server, fixture.WorkflowSource, workflowURL, codeToken)

		forgedGrants := buildGooberCredentialGrants(
			fixture.Goober,
			[]string{string(capability.ConfigRepoRead)},
			runnerGrants,
		)
		if len(forgedGrants) != 0 {
			t.Fatalf("stage obtained runner-only configrepo:read grant: %+v", forgedGrants)
		}
		forgedInjector, err := credentials.NewGooberInjector(
			resolver,
			fixture.Goober,
			forgedGrants,
			&workflowCDIsolationRegistrar{},
		)
		if err != nil {
			t.Fatalf("NewGooberInjector for forged declaration: %v", err)
		}
		forgedSet, err := forgedInjector.Materialize(
			context.Background(),
			[]string{string(capability.ConfigRepoRead)},
		)
		if err != nil {
			t.Fatalf("materialize forged configrepo:read declaration: %v", err)
		}
		if _, err := forgedSet.Token(context.Background(), string(capability.ConfigRepoRead)); !errors.Is(err, credentials.ErrNoCredentialForCapability) {
			t.Fatalf("forged configrepo:read error = %v, want ErrNoCredentialForCapability", err)
		}
	})

	t.Run("crossed grants fail closed", func(t *testing.T) {
		crossedWorkflowSource := *config.WorkflowSource
		crossedWorkflowSource.Token = &instance.TokenRef{Env: fixture.CodeRepository.TokenEnv}
		source, err := instance.NewWorkflowGitSource(
			t.TempDir(),
			crossedWorkflowSource,
			nil,
			&workflowCDIsolationRegistrar{},
			nil,
		)
		if err != nil {
			t.Fatalf("NewWorkflowGitSource with crossed token: %v", err)
		}
		before := server.rejectedCount(fixture.WorkflowSource.GitName, fixture.CodeRepository.Token)
		if _, err := source.Resolve(context.Background()); err == nil {
			t.Fatal("workflow source succeeded after crossing CODE_PAT into the CD grant")
		}
		if server.rejectedCount(fixture.WorkflowSource.GitName, fixture.CodeRepository.Token) <= before {
			t.Fatal("crossed workflow-source failure did not reach the config repository authorization boundary")
		}

		crossedConfig := *config
		crossedConfig.Repos = append([]instance.RepoRef(nil), config.Repos...)
		crossedConfig.Repos[0].Token = instance.TokenRef{Env: fixture.WorkflowSource.TokenEnv}
		resolver, runnerGrants, err := buildCredentials(&crossedConfig, nil, "", "", nil, nil)
		if err != nil {
			t.Fatalf("buildCredentials with crossed token: %v", err)
		}
		stageGrants := buildGooberCredentialGrants(
			fixture.Goober,
			[]string{fixture.CodeCapability},
			runnerGrants,
		)
		injector, err := credentials.NewGooberInjector(
			resolver,
			fixture.Goober,
			stageGrants,
			&workflowCDIsolationRegistrar{},
		)
		if err != nil {
			t.Fatalf("NewGooberInjector with crossed token: %v", err)
		}
		set, err := injector.Materialize(context.Background(), []string{fixture.CodeCapability})
		if err != nil {
			t.Fatalf("materialize crossed code-stage credentials: %v", err)
		}
		token, err := set.Token(context.Background(), fixture.CodeCapability)
		if err != nil {
			t.Fatalf("resolve crossed code-stage token: %v", err)
		}
		if token != fixture.WorkflowSource.Token {
			t.Fatalf("crossed code-stage token = %q, want fixture CD_PAT", token)
		}
		assertWorkflowCDGitDenied(t, server, fixture.CodeRepository, codeURL, token)
	})
}

func loadWorkflowCDIsolationFixture(t *testing.T) workflowCDIsolationFixture {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "workflowcd-isolation.json"))
	if err != nil {
		t.Fatal(err)
	}
	var fixture workflowCDIsolationFixture
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatalf("decode Workflow CD isolation fixture: %v", err)
	}
	required := map[string]string{
		"goober":                  fixture.Goober,
		"codeCapability":          fixture.CodeCapability,
		"workflowSource.gitName":  fixture.WorkflowSource.GitName,
		"workflowSource.tokenEnv": fixture.WorkflowSource.TokenEnv,
		"workflowSource.token":    fixture.WorkflowSource.Token,
		"workflowSource.marker":   fixture.WorkflowSource.Marker,
		"codeRepository.gitName":  fixture.CodeRepository.GitName,
		"codeRepository.owner":    fixture.CodeRepository.Owner,
		"codeRepository.name":     fixture.CodeRepository.Name,
		"codeRepository.tokenEnv": fixture.CodeRepository.TokenEnv,
		"codeRepository.token":    fixture.CodeRepository.Token,
		"codeRepository.marker":   fixture.CodeRepository.Marker,
	}
	for field, value := range required {
		if strings.TrimSpace(value) == "" {
			t.Fatalf("Workflow CD isolation fixture field %s is required", field)
		}
	}
	if fixture.WorkflowSource.Token == fixture.CodeRepository.Token {
		t.Fatal("Workflow CD isolation fixture must use distinct CD_PAT and CODE_PAT values")
	}
	return fixture
}

func workflowCDIsolationConfig(fixture workflowCDIsolationFixture, workflowURL string) *instance.Config {
	return &instance.Config{
		Repos: []instance.RepoRef{{
			Provider: "github",
			Owner:    fixture.CodeRepository.Owner,
			Name:     fixture.CodeRepository.Name,
			Token:    instance.TokenRef{Env: fixture.CodeRepository.TokenEnv},
		}},
		WorkflowSource: &instance.WorkflowSource{
			Kind:  instance.WorkflowSourceKindGit,
			URL:   workflowURL,
			Ref:   "main",
			Token: &instance.TokenRef{Env: fixture.WorkflowSource.TokenEnv},
		},
	}
}

type workflowCDIsolationGitServer struct {
	baseURL  string
	recorder *workflowCDIsolationAuthRecorder
}

func newWorkflowCDIsolationGitServer(t *testing.T, repos ...workflowCDIsolationRepoFixture) *workflowCDIsolationGitServer {
	t.Helper()
	root := t.TempDir()
	expected := make(map[string]string, len(repos))
	for _, repo := range repos {
		createWorkflowCDIsolationBareRepo(t, root, repo)
		expected[repo.GitName] = repo.Token
	}

	recorder := &workflowCDIsolationAuthRecorder{
		accepted: make(map[workflowCDIsolationAuthKey]int),
		rejected: make(map[workflowCDIsolationAuthKey]int),
	}
	files := http.FileServer(http.Dir(root))
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		repository := strings.SplitN(strings.TrimPrefix(r.URL.Path, "/"), "/", 2)[0]
		wantToken, exists := expected[repository]
		if !exists {
			http.NotFound(w, r)
			return
		}
		username, token, ok := r.BasicAuth()
		if !ok || username != "x-access-token" || token != wantToken {
			if ok {
				recorder.recordRejected(repository, token)
			}
			w.Header().Set("WWW-Authenticate", `Basic realm="git"`)
			http.Error(w, "authentication required", http.StatusUnauthorized)
			return
		}
		recorder.recordAccepted(repository, token)
		files.ServeHTTP(w, r)
	}))
	t.Cleanup(server.Close)

	certificate := pem.EncodeToMemory(&pem.Block{
		Type:  "CERTIFICATE",
		Bytes: server.Certificate().Raw,
	})
	certificatePath := filepath.Join(t.TempDir(), "workflowcd-test-ca.pem")
	if err := os.WriteFile(certificatePath, certificate, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SSL_CERT_FILE", certificatePath)
	t.Setenv("GIT_SSL_CAINFO", certificatePath)

	return &workflowCDIsolationGitServer{
		baseURL:  server.URL,
		recorder: recorder,
	}
}

func (s *workflowCDIsolationGitServer) url(repo workflowCDIsolationRepoFixture) string {
	return s.baseURL + "/" + repo.GitName
}

func (s *workflowCDIsolationGitServer) acceptedCount(repository, token string) int {
	return s.recorder.count(s.recorder.accepted, workflowCDIsolationAuthKey{repository: repository, token: token})
}

func (s *workflowCDIsolationGitServer) rejectedCount(repository, token string) int {
	return s.recorder.count(s.recorder.rejected, workflowCDIsolationAuthKey{repository: repository, token: token})
}

type workflowCDIsolationAuthKey struct {
	repository string
	token      string
}

type workflowCDIsolationAuthRecorder struct {
	mu       sync.Mutex
	accepted map[workflowCDIsolationAuthKey]int
	rejected map[workflowCDIsolationAuthKey]int
}

func (r *workflowCDIsolationAuthRecorder) recordAccepted(repository, token string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.accepted[workflowCDIsolationAuthKey{repository: repository, token: token}]++
}

func (r *workflowCDIsolationAuthRecorder) recordRejected(repository, token string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.rejected[workflowCDIsolationAuthKey{repository: repository, token: token}]++
}

func (r *workflowCDIsolationAuthRecorder) count(counts map[workflowCDIsolationAuthKey]int, key workflowCDIsolationAuthKey) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return counts[key]
}

func createWorkflowCDIsolationBareRepo(t *testing.T, root string, repo workflowCDIsolationRepoFixture) {
	t.Helper()
	worktree := t.TempDir()
	runWorkflowCDIsolationGit(t, worktree, "init", "-b", "main")
	runWorkflowCDIsolationGit(t, worktree, "config", "user.email", "test@example.com")
	runWorkflowCDIsolationGit(t, worktree, "config", "user.name", "Workflow CD test")
	if err := os.WriteFile(filepath.Join(worktree, repo.Marker), []byte(repo.Content), 0o644); err != nil {
		t.Fatal(err)
	}
	runWorkflowCDIsolationGit(t, worktree, "add", repo.Marker)
	runWorkflowCDIsolationGit(t, worktree, "commit", "-m", "fixture")

	bare := filepath.Join(root, repo.GitName)
	runWorkflowCDIsolationGit(t, "", "clone", "--bare", worktree, bare)
	runWorkflowCDIsolationGit(t, "", "--git-dir="+bare, "update-server-info")
}

func runWorkflowCDIsolationGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	command := testgit.Command(args...)
	if dir != "" {
		command.Dir = dir
	}
	command.Env = append(command.Env,
		"GIT_CONFIG_COUNT=3",
		"GIT_CONFIG_KEY_0=core.fsync",
		"GIT_CONFIG_VALUE_0=none",
		"GIT_CONFIG_KEY_1=core.autocrlf",
		"GIT_CONFIG_VALUE_1=false",
		"GIT_CONFIG_KEY_2=core.safecrlf",
		"GIT_CONFIG_VALUE_2=false",
	)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, output)
	}
}

func assertWorkflowCDGitAllowed(
	t *testing.T,
	server *workflowCDIsolationGitServer,
	repo workflowCDIsolationRepoFixture,
	repositoryURL string,
	token string,
) {
	t.Helper()
	before := server.acceptedCount(repo.GitName, token)
	if err := probeWorkflowCDIsolationGit(t, repositoryURL, token); err != nil {
		t.Fatalf("git access to %s was denied with its own scoped token: %v", repo.GitName, err)
	}
	if server.acceptedCount(repo.GitName, token) <= before {
		t.Fatalf("git access to %s did not reach its authorization boundary", repo.GitName)
	}
}

func assertWorkflowCDGitDenied(
	t *testing.T,
	server *workflowCDIsolationGitServer,
	repo workflowCDIsolationRepoFixture,
	repositoryURL string,
	token string,
) {
	t.Helper()
	before := server.rejectedCount(repo.GitName, token)
	if err := probeWorkflowCDIsolationGit(t, repositoryURL, token); err == nil {
		t.Fatalf("git access to %s succeeded with a token scoped to the other repository", repo.GitName)
	}
	if server.rejectedCount(repo.GitName, token) <= before {
		t.Fatalf("git failure for %s did not reach its authorization boundary", repo.GitName)
	}
}

func probeWorkflowCDIsolationGit(t *testing.T, repositoryURL, token string) error {
	t.Helper()
	askpass, err := credentials.WriteAskpassScript(t.TempDir())
	if err != nil {
		return err
	}
	command := testgit.Command("ls-remote", "--exit-code", repositoryURL, "refs/heads/main")
	command.Env = testgit.IsolateEnvironment(credentials.GitAuthEnvironment(askpass, token))
	output, err := command.CombinedOutput()
	if err != nil {
		return fmt.Errorf("git ls-remote: %w: %s", err, strings.TrimSpace(string(output)))
	}
	return nil
}

func assertWorkflowCDIsolationMarker(t *testing.T, snapshot string, repo workflowCDIsolationRepoFixture) {
	t.Helper()
	content, err := os.ReadFile(filepath.Join(snapshot, repo.Marker))
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != repo.Content {
		t.Fatalf("%s = %q, want %q", repo.Marker, content, repo.Content)
	}
}

type workflowCDIsolationRegistrar struct {
	values []string
}

func (r *workflowCDIsolationRegistrar) Register(secret []byte) {
	r.values = append(r.values, string(secret))
}

func (r *workflowCDIsolationRegistrar) saw(token string) bool {
	for _, value := range r.values {
		if value == token {
			return true
		}
	}
	return false
}
