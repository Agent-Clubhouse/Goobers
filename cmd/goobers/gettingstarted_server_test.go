package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/providers"
)

func newTestGuidedServer(t *testing.T, workdir string) *guidedServer {
	t.Helper()
	return &guidedServer{
		workdir:      workdir,
		instancePath: filepath.Join(workdir, "tutorial-instance"),
		executable:   "goobers-under-test",
		errorLog:     log.New(io.Discard, "", 0),
		completed:    make(chan struct{}),
	}
}

// stubGuidedExec replaces the subprocess seam, recording each action's argv and
// running the given shell script in its place.
func stubGuidedExec(t *testing.T, script string) *[][]string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("guided exec stubs use /bin/sh")
	}
	previous := guidedExecCommand
	calls := &[][]string{}
	guidedExecCommand = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		if name != "goobers-under-test" {
			t.Errorf("guided exec name = %q, want the server's executable", name)
		}
		*calls = append(*calls, append([]string{}, args...))
		return exec.CommandContext(ctx, "/bin/sh", "-c", script)
	}
	t.Cleanup(func() { guidedExecCommand = previous })
	return calls
}

func guidedGet(handler http.Handler, path string) *httptest.ResponseRecorder {
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))
	return recorder
}

func guidedPost(handler http.Handler, path, body string) *httptest.ResponseRecorder {
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	handler.ServeHTTP(recorder, request)
	return recorder
}

func decodeGuidedResponse[T any](t *testing.T, recorder *httptest.ResponseRecorder) T {
	t.Helper()
	var out T
	if err := json.Unmarshal(recorder.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode guided response %q: %v", recorder.Body.String(), err)
	}
	return out
}

func TestGettingStartedStateEndpoint(t *testing.T) {
	workdir := t.TempDir()
	server := newTestGuidedServer(t, workdir)
	t.Setenv("GOOBERS_GITHUB_TOKEN", "")
	t.Setenv("GOOBERS_GITHUB_ISSUES_TOKEN", "")

	before := guidedGet(http.HandlerFunc(server.serveGuided), "/guided/state")
	if before.Code != http.StatusOK {
		t.Fatalf("state status = %d body = %q", before.Code, before.Body.String())
	}
	if contentType := before.Header().Get("Content-Type"); contentType != "application/json" {
		t.Fatalf("state Content-Type = %q", contentType)
	}
	state := decodeGuidedResponse[guidedStateBody](t, before)
	if state.Version != guidedStateVersion ||
		state.Platform != runtime.GOOS ||
		state.Workdir != workdir ||
		state.InstancePath != filepath.Join(workdir, "tutorial-instance") {
		t.Fatalf("state paths = %+v", state)
	}
	if state.InstanceExists || state.APIReady || state.Job != nil {
		t.Fatalf("fresh workdir state = %+v", state)
	}
	if state.Env.GoobersGithubToken || state.Env.GoobersGithubIssuesToken {
		t.Fatalf("empty env reported set: %+v", state.Env)
	}
	// The raw body must say "job":null explicitly, not omit it.
	if !strings.Contains(before.Body.String(), `"job":null`) {
		t.Fatalf("state body missing explicit null job: %q", before.Body.String())
	}

	if err := os.MkdirAll(server.instancePath, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(server.instancePath, "instance.yaml"), []byte("x: y\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GOOBERS_GITHUB_TOKEN", "token-value")
	t.Setenv("GOOBERS_GITHUB_ISSUES_TOKEN", "issues-token")

	after := decodeGuidedResponse[guidedStateBody](t, guidedGet(http.HandlerFunc(server.serveGuided), "/guided/state"))
	if !after.InstanceExists {
		t.Fatalf("state after creation = %+v", after)
	}
	if !after.Env.GoobersGithubToken || !after.Env.GoobersGithubIssuesToken {
		t.Fatalf("set env reported unset: %+v", after.Env)
	}
}

func TestGettingStartedStateNeverLeaksTokenValues(t *testing.T) {
	server := newTestGuidedServer(t, t.TempDir())
	t.Setenv("GOOBERS_GITHUB_TOKEN", "super-secret-value")
	recorder := guidedGet(http.HandlerFunc(server.serveGuided), "/guided/state")
	if strings.Contains(recorder.Body.String(), "super-secret-value") {
		t.Fatalf("state leaked a token value: %q", recorder.Body.String())
	}
}

func TestGettingStartedCompleteSignalsServerShutdown(t *testing.T) {
	server := newTestGuidedServer(t, t.TempDir())
	recorder := guidedPost(
		http.HandlerFunc(server.serveGuided),
		"/guided/actions/complete",
		`{}`,
	)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d body = %q", recorder.Code, recorder.Body.String())
	}
	select {
	case <-server.completed:
	default:
		t.Fatal("complete action did not signal server shutdown")
	}
}

func TestGettingStartedInspectsLocalGitHubRepository(t *testing.T) {
	repository := filepath.Join(t.TempDir(), "widgets")
	if err := os.MkdirAll(repository, 0o755); err != nil {
		t.Fatal(err)
	}

	for _, args := range [][]string{
		{"init", "-b", "trunk"},
		{"remote", "add", "origin", "https://github.com/acme/widgets.git"},
	} {
		runAgentKitTestGit(t, repository, args...)
	}
	if err := os.WriteFile(filepath.Join(repository, "Makefile"), []byte("ci:\n\t@true\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	request, err := json.Marshal(map[string]string{"location": repository})
	if err != nil {
		t.Fatal(err)
	}
	server := newTestGuidedServer(t, t.TempDir())
	recorder := guidedPost(
		http.HandlerFunc(server.serveGuided),
		"/guided/actions/inspect-repository",
		string(request),
	)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d body = %q", recorder.Code, recorder.Body.String())
	}
	inspection := decodeGuidedResponse[guidedRepositoryInspection](t, recorder)
	if inspection.Provider != "github" ||
		inspection.Owner != "acme" ||
		inspection.Name != "widgets" ||
		inspection.DefaultBranch != "trunk" ||
		inspection.Stack != "Makefile" ||
		!reflect.DeepEqual(inspection.CICommand, []string{"make", "ci"}) ||
		inspection.NeedsClone {
		t.Fatalf("inspection = %+v", inspection)
	}
	if inspection.LocalPath == "" || inspection.PeerInstancePath == "" {
		t.Fatalf("inspection paths = %+v", inspection)
	}
}

func TestGettingStartedInspectsRepositoryWithoutLocalCI(t *testing.T) {
	repository := filepath.Join(t.TempDir(), "widgets")
	if err := os.MkdirAll(repository, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"init", "-b", "main"},
		{"remote", "add", "origin", "https://github.com/acme/widgets.git"},
	} {
		runAgentKitTestGit(t, repository, args...)
	}
	previousAuthDiscovery := guidedAuthDiscovery
	guidedAuthDiscovery = func(context.Context, guidedRepositoryIdentity) guidedAuthState {
		return guidedAuthState{Kind: "github-cli", Ready: true, Identity: "octocat"}
	}
	t.Cleanup(func() { guidedAuthDiscovery = previousAuthDiscovery })

	request, err := json.Marshal(map[string]string{"location": repository})
	if err != nil {
		t.Fatal(err)
	}
	server := newTestGuidedServer(t, t.TempDir())
	recorder := guidedPost(
		http.HandlerFunc(server.serveGuided),
		"/guided/actions/inspect-repository",
		string(request),
	)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d body = %q", recorder.Code, recorder.Body.String())
	}
	inspection := decodeGuidedResponse[guidedRepositoryInspection](t, recorder)
	if !inspection.PullRequestCI || inspection.Discovery != "unresolved" {
		t.Fatalf("inspection without local CI = %+v", inspection)
	}
	if len(inspection.CICommand) != 0 || len(inspection.RequiredCapabilities) != 0 {
		t.Fatalf("inspection inferred unsupported local CI = %+v", inspection)
	}
}

func stubGitHubDiscovery(t *testing.T, repositoryAccessible bool) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("GitHub discovery stubs use /bin/sh")
	}
	previous := guidedDiscoveryCommand
	guidedDiscoveryCommand = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		if name != "gh" {
			t.Fatalf("discovery command = %q, want gh", name)
		}
		if len(args) > 0 && args[0] == "api" {
			return exec.CommandContext(ctx, "/bin/sh", "-c", "printf 'octocat\\n'")
		}
		if repositoryAccessible {
			return exec.CommandContext(ctx, "/bin/sh", "-c", "printf 'widgets\\n'")
		}
		return exec.CommandContext(ctx, "/bin/sh", "-c", "exit 1")
	}
	t.Cleanup(func() { guidedDiscoveryCommand = previous })
}

func stubGitHubAuthorization(t *testing.T, script string) *[][]string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("GitHub authorization stubs use /bin/sh")
	}
	previous := guidedGitHubAuthorizationCommand
	calls := &[][]string{}
	guidedGitHubAuthorizationCommand = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		if name != "gh" {
			t.Errorf("authorization command = %q, want gh", name)
		}
		*calls = append(*calls, append([]string{}, args...))
		return exec.CommandContext(ctx, "/bin/sh", "-c", script)
	}
	t.Cleanup(func() { guidedGitHubAuthorizationCommand = previous })
	return calls
}

func stubADOAuthDiscovery(t *testing.T, accountReady, tokenReady bool) *[][]string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("Azure authentication stubs use /bin/sh")
	}
	previous := guidedDiscoveryCommand
	calls := &[][]string{}
	guidedDiscoveryCommand = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		if name != "az" {
			t.Fatalf("discovery command = %q, want az", name)
		}
		*calls = append(*calls, append([]string{}, args...))
		switch {
		case len(args) >= 2 && args[0] == "account" && args[1] == "show":
			if !accountReady {
				return exec.CommandContext(ctx, "/bin/sh", "-c", "exit 1")
			}
			return exec.CommandContext(ctx, "/bin/sh", "-c", "printf 'azure-user@example.com\\n'")
		case len(args) >= 2 && args[0] == "account" && args[1] == "get-access-token":
			if !tokenReady {
				return exec.CommandContext(ctx, "/bin/sh", "-c", "exit 1")
			}
			return exec.CommandContext(ctx, "/bin/sh", "-c", "printf '2026-09-01T00:00:00Z\\n'")
		default:
			t.Fatalf("unexpected Azure discovery args = %q", args)
			return exec.CommandContext(ctx, "/bin/sh", "-c", "exit 1")
		}
	}
	t.Cleanup(func() { guidedDiscoveryCommand = previous })
	return calls
}

func testADORepositoryIdentity() guidedRepositoryIdentity {
	return guidedRepositoryIdentity{
		provider: "ado",
		owner:    "acme",
		project:  "platform",
		name:     "widgets",
	}
}

func TestDiscoverGuidedAuthADORequiresLoginWhenAccountShowFails(t *testing.T) {
	calls := stubADOAuthDiscovery(t, false, false)

	auth := discoverGuidedAuth(context.Background(), testADORepositoryIdentity())
	if auth.Ready || !auth.NeedsLogin {
		t.Fatalf("unauthenticated Azure auth state = %+v", auth)
	}
	if auth.Identity != "" || auth.RemediationCommand != "az login" {
		t.Fatalf("unauthenticated Azure remediation = %+v", auth)
	}
	if auth.Message != "Azure CLI authentication is required before setup can continue." {
		t.Fatalf("unauthenticated Azure message = %q", auth.Message)
	}
	want := [][]string{{"account", "show", "--query", "user.name", "--output", "tsv"}}
	if !reflect.DeepEqual(*calls, want) {
		t.Fatalf("unauthenticated Azure discovery argv = %v, want %v", *calls, want)
	}
}

func TestDiscoverGuidedAuthADOReportsAccessFailureAfterLogin(t *testing.T) {
	calls := stubADOAuthDiscovery(t, true, false)

	auth := discoverGuidedAuth(context.Background(), testADORepositoryIdentity())
	if auth.Ready || auth.NeedsLogin {
		t.Fatalf("authenticated Azure access state = %+v", auth)
	}
	if auth.Identity != "azure-user@example.com" {
		t.Fatalf("authenticated Azure identity = %q", auth.Identity)
	}
	if !strings.Contains(auth.Message, "authenticated as azure-user@example.com") ||
		!strings.Contains(auth.Message, "acme/platform/widgets") {
		t.Fatalf("authenticated Azure access message = %q", auth.Message)
	}
	wantRemediation := `az repos show --organization https://dev.azure.com/acme --project "platform" --repository "widgets"`
	if auth.RemediationCommand != wantRemediation {
		t.Fatalf("authenticated Azure remediation = %q, want %q", auth.RemediationCommand, wantRemediation)
	}
	want := [][]string{
		{"account", "show", "--query", "user.name", "--output", "tsv"},
		{"account", "get-access-token", "--resource", "499b84ac-1321-427f-aa17-267ca6975798", "--query", "expiresOn", "--output", "tsv"},
	}
	if !reflect.DeepEqual(*calls, want) {
		t.Fatalf("authenticated Azure discovery argv = %v, want %v", *calls, want)
	}
}

func TestGettingStartedGitHubAuthorizationStartsDeviceFlowWhenNeeded(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("GitHub authorization stubs use /bin/sh")
	}
	previousDiscovery := guidedDiscoveryCommand
	authenticated := false
	guidedDiscoveryCommand = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		if name != "gh" {
			t.Fatalf("discovery command = %q, want gh", name)
		}
		if len(args) > 0 && args[0] == "api" && !authenticated {
			return exec.CommandContext(ctx, "/bin/sh", "-c", "exit 1")
		}
		return exec.CommandContext(ctx, "/bin/sh", "-c", "printf 'octocat\\n'")
	}
	t.Cleanup(func() { guidedDiscoveryCommand = previousDiscovery })
	calls := stubGitHubAuthorization(t, "exit 0")
	previousAuthorization := guidedGitHubAuthorizationCommand
	guidedGitHubAuthorizationCommand = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		authenticated = true
		return previousAuthorization(ctx, name, args...)
	}
	t.Cleanup(func() { guidedGitHubAuthorizationCommand = previousAuthorization })
	server := newTestGuidedServer(t, t.TempDir())

	recorder := guidedPost(
		http.HandlerFunc(server.serveGuided),
		"/guided/actions/authorize-github",
		`{"repository":"acme/widgets"}`,
	)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d body = %q", recorder.Code, recorder.Body.String())
	}
	response := decodeGuidedResponse[guidedAuthorizeGitHubResponse](t, recorder)
	if !response.Auth.Ready || response.Auth.Identity != "octocat" {
		t.Fatalf("authorization response = %+v", response)
	}
	if response.Message != "GitHub device/web authorization completed as octocat." {
		t.Fatalf("authorization message = %q", response.Message)
	}
	want := []string{"auth", "login", "--hostname", "github.com", "--git-protocol", "https", "--web"}
	if len(*calls) != 1 || !reflect.DeepEqual((*calls)[0], want) {
		t.Fatalf("authorization argv = %v, want [%v]", *calls, want)
	}
}

func TestGettingStartedGitHubAuthorizationSkipsAlreadyReadyAccount(t *testing.T) {
	stubGitHubDiscovery(t, true)
	calls := stubGitHubAuthorization(t, "exit 1")
	server := newTestGuidedServer(t, t.TempDir())

	recorder := guidedPost(
		http.HandlerFunc(server.serveGuided),
		"/guided/actions/authorize-github",
		`{"repository":"acme/widgets"}`,
	)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d body = %q", recorder.Code, recorder.Body.String())
	}
	response := decodeGuidedResponse[guidedAuthorizeGitHubResponse](t, recorder)
	if !response.Auth.Ready || !strings.Contains(response.Message, "already ready") {
		t.Fatalf("already-ready response = %+v", response)
	}
	if len(*calls) != 0 {
		t.Fatalf("already-authenticated account started login: %v", *calls)
	}
}

func TestGettingStartedGitHubAuthorizationDoesNotReloginForRepositoryAccess(t *testing.T) {
	stubGitHubDiscovery(t, false)
	calls := stubGitHubAuthorization(t, "exit 1")
	server := newTestGuidedServer(t, t.TempDir())

	recorder := guidedPost(
		http.HandlerFunc(server.serveGuided),
		"/guided/actions/authorize-github",
		`{"repository":"acme/widgets"}`,
	)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d body = %q", recorder.Code, recorder.Body.String())
	}
	response := decodeGuidedResponse[guidedErrorBody](t, recorder)
	if response.Code != "github_repository_access_unavailable" ||
		!strings.Contains(response.Message, "authenticated as octocat") {
		t.Fatalf("repository access response = %+v", response)
	}
	if len(*calls) != 0 {
		t.Fatalf("repository access failure started login: %v", *calls)
	}
}

func TestGettingStartedGitHubAuthorizationReportsMissingCLI(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("GitHub authorization stubs use /bin/sh")
	}
	previousDiscovery := guidedDiscoveryCommand
	guidedDiscoveryCommand = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		return exec.CommandContext(ctx, "/bin/sh", "-c", "exit 1")
	}
	previousRunner := guidedGitHubAuthorizationRunner
	guidedGitHubAuthorizationRunner = func(context.Context) error {
		return exec.ErrNotFound
	}
	t.Cleanup(func() {
		guidedDiscoveryCommand = previousDiscovery
		guidedGitHubAuthorizationRunner = previousRunner
	})
	server := newTestGuidedServer(t, t.TempDir())

	recorder := guidedPost(
		http.HandlerFunc(server.serveGuided),
		"/guided/actions/authorize-github",
		`{"repository":"acme/widgets"}`,
	)
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d body = %q", recorder.Code, recorder.Body.String())
	}
	response := decodeGuidedResponse[guidedErrorBody](t, recorder)
	if response.Code != "github_authorization_unavailable" ||
		!strings.Contains(response.Message, "GitHub CLI") ||
		!strings.Contains(response.Message, guidedGitHubLoginCommand) {
		t.Fatalf("missing CLI response = %+v", response)
	}
}

func TestGettingStartedGitHubAuthorizationReportsVerificationFailure(t *testing.T) {
	previousAuthDiscovery := guidedAuthDiscovery
	authDiscoveryCalls := 0
	guidedAuthDiscovery = func(context.Context, guidedRepositoryIdentity) guidedAuthState {
		authDiscoveryCalls++
		if authDiscoveryCalls == 1 {
			return guidedAuthState{
				Kind:               "github-cli",
				RemediationCommand: guidedGitHubLoginCommand,
				NeedsLogin:         true,
			}
		}
		return guidedAuthState{
			Kind:               "github-cli",
			RemediationCommand: "gh repo view acme/widgets",
		}
	}
	previousRunner := guidedGitHubAuthorizationRunner
	authorizationCalled := false
	guidedGitHubAuthorizationRunner = func(context.Context) error {
		authorizationCalled = true
		return nil
	}
	t.Cleanup(func() {
		guidedAuthDiscovery = previousAuthDiscovery
		guidedGitHubAuthorizationRunner = previousRunner
	})
	server := newTestGuidedServer(t, t.TempDir())

	recorder := guidedPost(
		http.HandlerFunc(server.serveGuided),
		"/guided/actions/authorize-github",
		`{"repository":"acme/widgets"}`,
	)
	if recorder.Code != http.StatusBadGateway {
		t.Fatalf("status = %d body = %q", recorder.Code, recorder.Body.String())
	}
	response := decodeGuidedResponse[guidedErrorBody](t, recorder)
	wantMessage := "GitHub authorization completed, but access to acme/widgets could not be verified. Next: gh repo view acme/widgets"
	if response.Code != "github_authorization_verification_failed" || response.Message != wantMessage {
		t.Fatalf("verification failure response = %+v, want message %q", response, wantMessage)
	}
	if !authorizationCalled || authDiscoveryCalls != 2 {
		t.Fatalf("verification flow calls = authorization %v, auth discovery %d; want true, 2", authorizationCalled, authDiscoveryCalls)
	}
}

func TestGettingStartedGitHubAuthorizationRejectsInvalidRepositories(t *testing.T) {
	for _, test := range []struct {
		name       string
		repository string
	}{
		{name: "malformed repository", repository: "not-a-repository"},
		{name: "Azure DevOps repository", repository: "https://dev.azure.com/acme/platform/_git/widgets"},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := newTestGuidedServer(t, t.TempDir())
			recorder := guidedPost(
				http.HandlerFunc(server.serveGuided),
				"/guided/actions/authorize-github",
				fmt.Sprintf(`{"repository":%q}`, test.repository),
			)
			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("status = %d body = %q", recorder.Code, recorder.Body.String())
			}
			response := decodeGuidedResponse[guidedErrorBody](t, recorder)
			if response.Code != "invalid_github_repository" ||
				response.Message != "a GitHub repository in owner/name form is required" {
				t.Fatalf("invalid repository response = %+v", response)
			}
		})
	}
}

func TestGettingStartedRefusesUnsafeGuidedInstanceTarget(t *testing.T) {
	repository := seedGitInitTargetRepository(t)
	linked := filepath.Join(t.TempDir(), "session-worktree")
	runInitTargetGit(t, repository, "worktree", "add", "-b", "session", linked, "main")

	server := newTestGuidedServer(t, t.TempDir())
	server.instancePath = filepath.Join(linked, "instance")
	recorder := guidedPost(
		http.HandlerFunc(server.serveGuided),
		"/guided/actions/init-instance",
		`{"template":"quickstart"}`,
	)
	if recorder.Code != http.StatusConflict {
		t.Fatalf("status = %d body = %q", recorder.Code, recorder.Body.String())
	}
	response := decodeGuidedResponse[guidedErrorBody](t, recorder)
	if response.Code != "unsafe_init_target" ||
		!strings.Contains(response.Message, "--allow-ephemeral") ||
		!strings.Contains(response.Message, "goobers init --guided --instance-path") {
		t.Fatalf("response = %+v, want actionable unsafe-target refusal", response)
	}
	if strings.Contains(response.Message, `goobers init --allow-ephemeral "`+server.instancePath+`"`) {
		t.Fatalf("response = %+v, must not offer a non-guided remediation", response)
	}
	if _, err := os.Stat(server.instancePath); !os.IsNotExist(err) {
		t.Fatalf("unsafe guided init created instance target: %v", err)
	}
}

func TestGettingStartedChooseRepositoryFolder(t *testing.T) {
	previous := guidedChooseRepositoryFolder
	guidedChooseRepositoryFolder = func(context.Context) (string, bool, error) {
		return `C:\src\widgets`, false, nil
	}
	t.Cleanup(func() { guidedChooseRepositoryFolder = previous })

	server := newTestGuidedServer(t, t.TempDir())
	recorder := guidedPost(
		http.HandlerFunc(server.serveGuided),
		"/guided/actions/choose-repository-folder",
		`{}`,
	)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d body = %q", recorder.Code, recorder.Body.String())
	}
	response := decodeGuidedResponse[guidedChooseFolderResponse](t, recorder)
	if response.Path != `C:\src\widgets` || response.Canceled {
		t.Fatalf("response = %+v", response)
	}
}

func TestGettingStartedActionArgv(t *testing.T) {
	workdir := t.TempDir()
	tutorial := filepath.Join(workdir, "tutorial-instance")
	cases := []struct {
		name   string
		invoke func(handler http.Handler) *httptest.ResponseRecorder
		want   []string
	}{
		{
			name: "init-instance",
			invoke: func(h http.Handler) *httptest.ResponseRecorder {
				return guidedPost(h, "/guided/actions/init-instance", `{}`)
			},
			want: []string{"init", "--template=quickstart", tutorial},
		},
		{
			name: "init-instance explicit quickstart",
			invoke: func(h http.Handler) *httptest.ResponseRecorder {
				return guidedPost(h, "/guided/actions/init-instance", `{"template":"quickstart"}`)
			},
			want: []string{"init", "--template=quickstart", tutorial},
		},
		{
			name: "init-instance starter",
			invoke: func(h http.Handler) *httptest.ResponseRecorder {
				return guidedPost(h, "/guided/actions/init-instance", `{"template":"starter"}`)
			},
			want: []string{"init", tutorial},
		},
		{
			name: "connect minimal",
			invoke: func(h http.Handler) *httptest.ResponseRecorder {
				return guidedPost(h, "/guided/actions/connect", `{"repo":"acme/web"}`)
			},
			want: []string{"connect", "acme/web", "--json", tutorial},
		},
		{
			name: "connect all options",
			invoke: func(h http.Handler) *httptest.ResponseRecorder {
				return guidedPost(h, "/guided/actions/connect",
					`{"repo":"acme/web","tokenEnv":"MY_TOKEN","seed":true,"replace":true}`)
			},
			want: []string{
				"connect", "acme/web", "--json",
				"--token-env", "MY_TOKEN", "--seed", "--replace", tutorial,
			},
		},
		{
			name: "validate default",
			invoke: func(h http.Handler) *httptest.ResponseRecorder {
				return guidedPost(h, "/guided/actions/validate", `{"checkHarness":false,"checkRepos":false}`)
			},
			want: []string{"validate", "--json", tutorial},
		},
		{
			name: "validate with checks",
			invoke: func(h http.Handler) *httptest.ResponseRecorder {
				return guidedPost(h, "/guided/actions/validate", `{"checkHarness":true,"checkRepos":true}`)
			},
			want: []string{"validate", "--json", "--check-harness", "--check-repos", tutorial},
		},
		{
			name: "status",
			invoke: func(h http.Handler) *httptest.ResponseRecorder {
				return guidedGet(h, "/guided/status")
			},
			want: []string{"status", "--json", tutorial},
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			server := newTestGuidedServer(t, workdir)
			calls := stubGuidedExec(t, `printf '{"action":"stub","version":1}'`)
			recorder := testCase.invoke(http.HandlerFunc(server.serveGuided))
			if recorder.Code != http.StatusOK {
				t.Fatalf("status = %d body = %q", recorder.Code, recorder.Body.String())
			}
			if len(*calls) != 1 || !reflect.DeepEqual((*calls)[0], testCase.want) {
				t.Fatalf("argv = %v, want [%v]", *calls, testCase.want)
			}
			body := decodeGuidedResponse[map[string]any](t, recorder)
			if _, hasExit := body["exitCode"]; !hasExit {
				t.Fatalf("response missing exitCode: %v", body)
			}
		})
	}
}

func TestGettingStartedAllowlistRejections(t *testing.T) {
	server := newTestGuidedServer(t, t.TempDir())
	handler := http.HandlerFunc(server.serveGuided)
	calls := stubGuidedExec(t, `printf '{}'`)

	cases := []struct {
		name     string
		path     string
		body     string
		wantCode string
	}{
		{"unknown init template", "/guided/actions/init-instance", `{"template":"demo"}`, "invalid_template"},
		{"connect without repo", "/guided/actions/connect", `{}`, "invalid_repo"},
		{"unknown run workflow", "/guided/actions/run", `{"workflow":"merge-review"}`, "invalid_workflow"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			recorder := guidedPost(handler, testCase.path, testCase.body)
			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("status = %d body = %q", recorder.Code, recorder.Body.String())
			}
			if body := decodeGuidedResponse[guidedErrorBody](t, recorder); body.Code != testCase.wantCode {
				t.Fatalf("error code = %q, want %q", body.Code, testCase.wantCode)
			}
		})
	}
	if len(*calls) != 0 {
		t.Fatalf("rejected requests must never exec: %v", *calls)
	}
}

func TestGettingStartedGuidedInitMaterializesSelectedModules(t *testing.T) {
	workdir := t.TempDir()
	server := newTestGuidedServer(t, workdir)
	defaultInstancePath := server.instancePath
	selectedInstancePath := filepath.Join(t.TempDir(), "widgets-goobers")
	body := `{
		"template":"guided",
		"guided":{
			"provider":"github",
			"owner":"acme",
			"name":"widgets",
			"localPath":"C:\\src\\widgets",
			"instancePath":"` + filepath.ToSlash(selectedInstancePath) + `",
			"branch":"main",
			"workflows":["backlog-curation","work-nomination"],
			"harness":"copilot",
			"githubCLIUser":"octocat"
		}
	}`

	recorder := guidedPost(http.HandlerFunc(server.serveGuided), "/guided/actions/init-instance", body)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d body = %q", recorder.Code, recorder.Body.String())
	}
	response := decodeGuidedResponse[guidedInitBody](t, recorder)
	wantStdout := fmt.Sprintf(
		"Created 2 workflow module(s) in the Goobers Instance at %s.",
		selectedInstancePath,
	)
	if response.ExitCode != 0 || response.Stdout != wantStdout {
		t.Fatalf("guided init response = %+v", response)
	}
	if server.instancePath != selectedInstancePath {
		t.Fatalf("server instance path = %q, want selected neighboring path %q", server.instancePath, selectedInstancePath)
	}
	if _, err := os.Stat(defaultInstancePath); !os.IsNotExist(err) {
		t.Fatalf("guided init created the default instance path: %v", err)
	}
	for _, path := range []string{
		filepath.Join(selectedInstancePath, instance.ConfigFileName),
		filepath.Join(selectedInstancePath, instance.ConfigDirName, "gaggles", "widgets", "workflows", "backlog-curation.yaml"),
		filepath.Join(selectedInstancePath, instance.ConfigDirName, "gaggles", "widgets", "workflows", "work-nomination.yaml"),
	} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("guided init did not create %s: %v", path, err)
		}
	}
	if _, err := os.Stat(filepath.Join(
		selectedInstancePath,
		instance.ConfigDirName,
		"gaggles",
		"widgets",
		"workflows",
		"implementation.yaml",
	)); !os.IsNotExist(err) {
		t.Fatalf("guided init created unselected implementation workflow: %v", err)
	}
}

func TestMissingGuidedLabelsReturnsEmptyArrayWhenAllLabelsExist(t *testing.T) {
	required := []providers.WorkItemLabel{
		{Name: "goobers:ready"},
		{Name: "goobers:approved"},
	}

	missing := missingGuidedLabels(required, []string{"goobers:ready", "goobers:approved"})
	if missing == nil || len(missing) != 0 {
		t.Fatalf("missing labels = %#v, want non-nil empty slice", missing)
	}
}

func TestGettingStartedRunDoesNotRequireEnvTokenForCLIAuth(t *testing.T) {
	cases := []struct {
		name string
		opts instance.GuidedOptions
	}{
		{
			name: "GitHub CLI",
			opts: instance.GuidedOptions{
				GaggleName:    "widgets",
				DisplayName:   "acme/widgets",
				RepoProvider:  "github",
				RepoOwner:     "acme",
				RepoName:      "widgets",
				RepoBranch:    "main",
				GitHubCLIUser: "octocat",
				Workflows:     []string{instance.GuidedWorkflowWorkNomination},
			},
		},
		{
			name: "Azure CLI",
			opts: instance.GuidedOptions{
				GaggleName:   "widgets",
				DisplayName:  "acme/platform/widgets",
				RepoProvider: "ado",
				RepoOwner:    "acme",
				RepoProject:  "platform",
				RepoName:     "widgets",
				RepoBranch:   "main",
				RepoAuthKind: instance.ADOAuthAzureCLI,
				Workflows:    []string{instance.GuidedWorkflowWorkNomination},
			},
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			server := newTestGuidedServer(t, t.TempDir())
			source := filepath.Join(t.TempDir(), "config")
			if _, err := instance.SeedGuidedConfigSource(source, testCase.opts); err != nil {
				t.Fatal(err)
			}
			cfg, err := instance.LoadGuidedSourceConfig(source)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := instance.InitGuidedFromSource(server.instancePath, source, cfg); err != nil {
				t.Fatal(err)
			}
			if env, required := server.requiredRunTokenEnv(); required || env != "" {
				t.Fatalf("requiredRunTokenEnv() = (%q, %v), want no environment token", env, required)
			}
		})
	}
}

// stubGuidedExecCapturingCmd is stubGuidedExec's sibling: it hands back the
// live *exec.Cmd the handler goes on to configure, so a test can inspect
// .Env after the handler sets it (unlike argv, which stubGuidedExec's
// closure parameters already capture, .Env is set by the CALLER on the
// returned *exec.Cmd, so it isn't visible until after the handler runs).
func stubGuidedExecCapturingCmd(t *testing.T, script string) (argv *[]string, cmd **exec.Cmd) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("guided exec stubs use /bin/sh")
	}
	previous := guidedExecCommand
	var gotArgv []string
	var gotCmd *exec.Cmd
	guidedExecCommand = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		gotArgv = append([]string{}, args...)
		gotCmd = exec.CommandContext(ctx, "/bin/sh", "-c", script)
		return gotCmd
	}
	t.Cleanup(func() { guidedExecCommand = previous })
	return &gotArgv, &gotCmd
}

func TestGettingStartedProbeBacklogNoTokenYetSkipsExec(t *testing.T) {
	server := newTestGuidedServer(t, t.TempDir())
	t.Setenv("GOOBERS_GITHUB_TOKEN", "")
	t.Setenv(defaultWorkTrackingTokenEnv, "")
	calls := stubGuidedExec(t, `printf 'no eligible items\n'`)

	recorder := guidedGet(http.HandlerFunc(server.serveGuided), "/guided/actions/probe-backlog")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d body = %q", recorder.Code, recorder.Body.String())
	}
	body := decodeGuidedResponse[guidedProbeBody](t, recorder)
	if body.EligibleCount != nil {
		t.Fatalf("eligibleCount = %v, want nil (no token exported yet)", *body.EligibleCount)
	}
	if len(*calls) != 0 {
		t.Fatalf("must not exec backlog-query without a token: %v", *calls)
	}
}

func TestGettingStartedProbeBacklogParsesEligibleCount(t *testing.T) {
	cases := []struct {
		name      string
		stdout    string
		stderr    string
		want      int
		wantTrunc bool
	}{
		{"none eligible", "no eligible items\n", "", 0, false},
		{"one eligible", "123\tFix the flaky test\n", "", 1, false},
		{"multiple eligible", "1\tFirst\n2\tSecond\n3\tThird\n", "", 3, false},
		{"truncated empty", "no eligible items\n", "warning: backlog scan found no eligible item after examining 260 candidate(s), and stopped on its 250-candidate scan budget rather than the end of the backlog: unexamined items remain\n", 0, true},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			server := newTestGuidedServer(t, t.TempDir())
			t.Setenv(defaultWorkTrackingTokenEnv, "issues-token")
			stubGuidedExec(t, fmt.Sprintf("printf %q; printf %q 1>&2", testCase.stdout, testCase.stderr))

			recorder := guidedGet(http.HandlerFunc(server.serveGuided), "/guided/actions/probe-backlog")
			if recorder.Code != http.StatusOK {
				t.Fatalf("status = %d body = %q", recorder.Code, recorder.Body.String())
			}
			body := decodeGuidedResponse[guidedProbeBody](t, recorder)
			if body.EligibleCount == nil || *body.EligibleCount != testCase.want {
				t.Fatalf("eligibleCount = %v, want %d", body.EligibleCount, testCase.want)
			}
			if body.Truncated != testCase.wantTrunc {
				t.Fatalf("truncated = %v, want %v; stderr=%q", body.Truncated, testCase.wantTrunc, body.Stderr)
			}
		})
	}
}

func TestGettingStartedProbeBacklogArgvAndCredentialEnv(t *testing.T) {
	workdir := t.TempDir()
	tutorial := filepath.Join(workdir, "tutorial-instance")
	server := newTestGuidedServer(t, workdir)
	t.Setenv("GOOBERS_GITHUB_TOKEN", "")
	t.Setenv(defaultWorkTrackingTokenEnv, "super-secret-issues-token")
	argv, cmd := stubGuidedExecCapturingCmd(t, `printf 'no eligible items\n'`)

	recorder := guidedGet(http.HandlerFunc(server.serveGuided), "/guided/actions/probe-backlog")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d body = %q", recorder.Code, recorder.Body.String())
	}

	wantArgv := []string{"backlog-query", "--read-only", tutorial}
	if !reflect.DeepEqual(*argv, wantArgv) {
		t.Fatalf("argv = %v, want %v", *argv, wantArgv)
	}
	if *cmd == nil {
		t.Fatal("guidedExecCommand was not invoked")
	}
	wantEnvContains := []string{
		"GOOBERS_CRED_GITHUB_ISSUES_READ=super-secret-issues-token",
		"GOOBERS_INPUT_TRUSTLABEL=goobers:approved",
		"GOOBERS_INPUT_REQUIRELABELS=goobers:ready",
	}
	for _, want := range wantEnvContains {
		if !slices.Contains((*cmd).Env, want) {
			t.Fatalf("command.Env missing %q; env = %v", want, (*cmd).Env)
		}
	}
}

func TestGettingStartedProbeBacklogFallsBackToMainToken(t *testing.T) {
	server := newTestGuidedServer(t, t.TempDir())
	t.Setenv(defaultWorkTrackingTokenEnv, "")
	t.Setenv("GOOBERS_GITHUB_TOKEN", "main-token-value")
	_, cmd := stubGuidedExecCapturingCmd(t, `printf 'no eligible items\n'`)

	recorder := guidedGet(http.HandlerFunc(server.serveGuided), "/guided/actions/probe-backlog")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d body = %q", recorder.Code, recorder.Body.String())
	}
	want := "GOOBERS_CRED_GITHUB_ISSUES_READ=main-token-value"
	if !slices.Contains((*cmd).Env, want) {
		t.Fatalf("command.Env missing %q (main-token fallback); env = %v", want, (*cmd).Env)
	}
}

func TestGettingStartedProbeBacklogRejectsPost(t *testing.T) {
	server := newTestGuidedServer(t, t.TempDir())
	recorder := guidedPost(http.HandlerFunc(server.serveGuided), "/guided/actions/probe-backlog", `{}`)
	if recorder.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", recorder.Code)
	}
}

func TestGettingStartedRunWorkflowChooser(t *testing.T) {
	workdir := t.TempDir()
	tutorial := filepath.Join(workdir, "tutorial-instance")
	t.Setenv(connectDefaultTokenEnv, "token-value")
	for _, testCase := range []struct {
		body string
		want []string
	}{
		{`{}`, []string{"run", "quickstart", tutorial}},
		{`{"workflow":"quickstart"}`, []string{"run", "quickstart", tutorial}},
		{`{"workflow":"default-implement"}`, []string{"run", "default-implement", tutorial}},
		{`{"workflow":"implementation"}`, []string{"run", "implementation", tutorial}},
	} {
		server := newTestGuidedServer(t, workdir)
		handler := http.HandlerFunc(server.serveGuided)
		calls := stubGuidedExec(t, `exit 0`)
		accepted := guidedPost(handler, "/guided/actions/run", testCase.body)
		if accepted.Code != http.StatusAccepted {
			t.Fatalf("run %s status = %d body = %q", testCase.body, accepted.Code, accepted.Body.String())
		}
		deadline := time.Now().Add(5 * time.Second)
		for len(*calls) == 0 && time.Now().Before(deadline) {
			time.Sleep(5 * time.Millisecond)
		}
		if len(*calls) != 1 || !reflect.DeepEqual((*calls)[0], testCase.want) {
			t.Fatalf("run %s argv = %v, want [%v]", testCase.body, *calls, testCase.want)
		}
		if err := server.close(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestGettingStartedStateReportsConnectedRepo(t *testing.T) {
	workdir := t.TempDir()
	t.Setenv(connectDefaultTokenEnv, "")
	server := newTestGuidedServer(t, workdir)
	handler := http.HandlerFunc(server.serveGuided)

	// No instance yet: connected.repo is explicit null.
	before := guidedGet(handler, "/guided/state")
	state := decodeGuidedResponse[guidedStateBody](t, before)
	if state.Connected.Repo != nil {
		t.Fatalf("connected before init = %+v", state.Connected)
	}
	if !strings.Contains(before.Body.String(), `"connected":{"repo":null}`) {
		t.Fatalf("state body missing explicit null connected repo: %q", before.Body.String())
	}

	// A placeholder instance is not connected.
	if _, err := instance.InitQuickstart(server.instancePath); err != nil {
		t.Fatal(err)
	}
	state = decodeGuidedResponse[guidedStateBody](t, guidedGet(handler, "/guided/state"))
	if state.Connected.Repo != nil {
		t.Fatalf("connected with placeholder = %+v", state.Connected)
	}

	// After a real connect, the state names the repository.
	if code := executeConnect(connectOptions{
		owner:    "acme",
		name:     "web",
		root:     server.instancePath,
		tokenEnv: connectDefaultTokenEnv,
	}, io.Discard, io.Discard); code != 0 {
		t.Fatalf("connect exit = %d", code)
	}
	state = decodeGuidedResponse[guidedStateBody](t, guidedGet(handler, "/guided/state"))
	if state.Connected.Repo == nil || *state.Connected.Repo != "acme/web" {
		t.Fatalf("connected after connect = %+v", state.Connected)
	}
}

// TestGettingStartedRunRefusesWhenRecordedTokenEnvUnset is the #2639
// regression: a token export reaching some OTHER variable than the one the
// server actually recorded (default GOOBERS_GITHUB_TOKEN, or whatever
// `connect --token-env` persisted) must never mark credentials ready or
// unblock dispatch — there is no mechanism, live or otherwise, for a client
// to make the server's own process environment agree with a shell the
// server doesn't share. The only thing that changes the outcome is the
// RECORDED name's own variable, in THIS process, before the request.
func TestGettingStartedRunRefusesWhenRecordedTokenEnvUnset(t *testing.T) {
	workdir := t.TempDir()
	t.Setenv(connectDefaultTokenEnv, "")
	server := newTestGuidedServer(t, workdir)
	handler := http.HandlerFunc(server.serveGuided)
	calls := stubGuidedExec(t, `exit 0`)

	// Unconnected instance, default recorded name, nothing exported: refused.
	refused := guidedPost(handler, "/guided/actions/run", `{}`)
	if refused.Code != http.StatusBadRequest {
		t.Fatalf("unset-token run status = %d body = %q", refused.Code, refused.Body.String())
	}
	body := decodeGuidedResponse[guidedErrorBody](t, refused)
	if body.Code != "token_env_unset" || !strings.Contains(body.Message, connectDefaultTokenEnv) {
		t.Fatalf("unset-token run body = %+v", body)
	}
	if len(*calls) != 0 {
		t.Fatalf("refused run still exec'd: %v", *calls)
	}
	state := decodeGuidedResponse[guidedStateBody](t, guidedGet(handler, "/guided/state"))
	if state.Env.GoobersGithubToken {
		t.Fatalf("state reported ready with nothing exported: %+v", state.Env)
	}

	// Connect with a NON-default recorded name.
	if _, err := instance.InitQuickstart(server.instancePath); err != nil {
		t.Fatal(err)
	}
	if code := executeConnect(connectOptions{
		owner:    "acme",
		name:     "web",
		root:     server.instancePath,
		tokenEnv: "MY_CUSTOM_TOKEN",
	}, io.Discard, io.Discard); code != 0 {
		t.Fatalf("connect exit = %d", code)
	}

	// A post-launch export landing on the DEFAULT name (what an operator who
	// missed the "--token-env" detail, or exported in the wrong shell, would
	// actually do) must not satisfy the preflight — the recorded name is
	// MY_CUSTOM_TOKEN now, not the default, and that is the only name that
	// counts.
	t.Setenv(connectDefaultTokenEnv, "token-value")
	wrongName := guidedPost(handler, "/guided/actions/run", `{}`)
	if wrongName.Code != http.StatusBadRequest {
		t.Fatalf("wrong-name run status = %d body = %q", wrongName.Code, wrongName.Body.String())
	}
	wrongNameBody := decodeGuidedResponse[guidedErrorBody](t, wrongName)
	if wrongNameBody.Code != "token_env_unset" || !strings.Contains(wrongNameBody.Message, "MY_CUSTOM_TOKEN") {
		t.Fatalf("wrong-name run body = %+v", wrongNameBody)
	}
	if len(*calls) != 0 {
		t.Fatalf("run with wrong exported name still exec'd: %v", *calls)
	}
	stateAfterConnect := decodeGuidedResponse[guidedStateBody](t, guidedGet(handler, "/guided/state"))
	if stateAfterConnect.Env.TokenEnv != "MY_CUSTOM_TOKEN" {
		t.Fatalf("state.env.tokenEnv = %q, want MY_CUSTOM_TOKEN", stateAfterConnect.Env.TokenEnv)
	}
	if stateAfterConnect.Env.GoobersGithubToken {
		t.Fatalf("state reported ready for the recorded name despite only the default being exported: %+v", stateAfterConnect.Env)
	}

	// Exporting the actually-recorded name, in THIS process (standing in for
	// "before the server launched"), is the only thing that unblocks it.
	t.Setenv("MY_CUSTOM_TOKEN", "token-value")
	ready := decodeGuidedResponse[guidedStateBody](t, guidedGet(handler, "/guided/state"))
	if !ready.Env.GoobersGithubToken {
		t.Fatalf("state still not ready with the recorded name exported: %+v", ready.Env)
	}
	accepted := guidedPost(handler, "/guided/actions/run", `{}`)
	if accepted.Code != http.StatusAccepted {
		t.Fatalf("run with the recorded name exported status = %d body = %q", accepted.Code, accepted.Body.String())
	}
}

func TestGettingStartedEnvelopeParsingAndExitCode(t *testing.T) {
	server := newTestGuidedServer(t, t.TempDir())
	handler := http.HandlerFunc(server.serveGuided)

	stubGuidedExec(t, `printf '{"action":"validate.report","version":1}'; printf 'warned\n' >&2; exit 1`)
	envelope := decodeGuidedResponse[guidedEnvelopeBody](t, guidedPost(handler, "/guided/actions/validate", `{}`))
	if envelope.ExitCode != 1 {
		t.Fatalf("exitCode = %d", envelope.ExitCode)
	}
	if string(envelope.Envelope) != `{"action":"validate.report","version":1}` {
		t.Fatalf("envelope = %s", envelope.Envelope)
	}
	if envelope.Stderr != "warned\n" {
		t.Fatalf("stderr = %q", envelope.Stderr)
	}

	stubGuidedExec(t, `printf 'not json at all\n'; exit 2`)
	unparsed := decodeGuidedResponse[guidedEnvelopeBody](t, guidedPost(handler, "/guided/actions/validate", `{}`))
	if unparsed.ExitCode != 2 || string(unparsed.Envelope) != "null" {
		t.Fatalf("unparseable stdout: exit = %d envelope = %s", unparsed.ExitCode, unparsed.Envelope)
	}
}

func TestGettingStartedGuidedRefusesCrossOrigin(t *testing.T) {
	server := newTestGuidedServer(t, t.TempDir())
	handler := http.HandlerFunc(server.serveGuided)

	crossOrigin := httptest.NewRequest(http.MethodGet, "/guided/state", nil)
	crossOrigin.Host = "127.0.0.1:8081"
	crossOrigin.Header.Set("Origin", "http://attacker.example")
	refused := httptest.NewRecorder()
	handler.ServeHTTP(refused, crossOrigin)
	if refused.Code != http.StatusForbidden {
		t.Fatalf("cross-origin status = %d body = %q", refused.Code, refused.Body.String())
	}
	if body := decodeGuidedResponse[guidedErrorBody](t, refused); body.Code != "origin_forbidden" {
		t.Fatalf("cross-origin code = %q", body.Code)
	}

	// Non-loopback Origin is refused even when it matches the request host.
	nonLoopback := httptest.NewRequest(http.MethodGet, "/guided/state", nil)
	nonLoopback.Host = "portal.example:8081"
	nonLoopback.Header.Set("Origin", "http://portal.example:8081")
	refusedNonLoopback := httptest.NewRecorder()
	handler.ServeHTTP(refusedNonLoopback, nonLoopback)
	if refusedNonLoopback.Code != http.StatusForbidden {
		t.Fatalf("non-loopback status = %d", refusedNonLoopback.Code)
	}

	matching := httptest.NewRequest(http.MethodGet, "/guided/state", nil)
	matching.Host = "127.0.0.1:8081"
	matching.Header.Set("Origin", "http://127.0.0.1:8081")
	allowed := httptest.NewRecorder()
	handler.ServeHTTP(allowed, matching)
	if allowed.Code != http.StatusOK {
		t.Fatalf("loopback same-origin status = %d body = %q", allowed.Code, allowed.Body.String())
	}
}

func TestGettingStartedPostRequiresJSONContentType(t *testing.T) {
	server := newTestGuidedServer(t, t.TempDir())
	handler := http.HandlerFunc(server.serveGuided)
	calls := stubGuidedExec(t, `printf '{}'`)

	request := httptest.NewRequest(http.MethodPost, "/guided/actions/validate", strings.NewReader(`{}`))
	request.Header.Set("Content-Type", "text/plain")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("content-type refusal status = %d", recorder.Code)
	}
	if len(*calls) != 0 {
		t.Fatalf("refused request still exec'd: %v", *calls)
	}
}

func TestGettingStartedPostRejectsUnknownFields(t *testing.T) {
	server := newTestGuidedServer(t, t.TempDir())
	handler := http.HandlerFunc(server.serveGuided)
	calls := stubGuidedExec(t, `printf '{}'`)

	recorder := guidedPost(handler, "/guided/actions/validate", `{"checkHarness":true,"surprise":1}`)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("unknown-field status = %d body = %q", recorder.Code, recorder.Body.String())
	}
	if len(*calls) != 0 {
		t.Fatalf("refused request still exec'd: %v", *calls)
	}
}

func TestGettingStartedJobLifecycle(t *testing.T) {
	workdir := t.TempDir()
	t.Setenv(connectDefaultTokenEnv, "token-value")
	server := newTestGuidedServer(t, workdir)
	handler := http.HandlerFunc(server.serveGuided)
	calls := stubGuidedExec(t,
		`printf 'created run abc123 (workflow=quickstart gaggle=example)\n'; printf 'stage research started\n' >&2; exit 3`)

	accepted := guidedPost(handler, "/guided/actions/run", `{}`)
	if accepted.Code != http.StatusAccepted {
		t.Fatalf("run status = %d body = %q", accepted.Code, accepted.Body.String())
	}
	jobID := decodeGuidedResponse[map[string]string](t, accepted)["jobId"]
	if jobID == "" {
		t.Fatal("run returned no jobId")
	}
	if want := []string{"run", "quickstart", filepath.Join(workdir, "tutorial-instance")}; !reflect.DeepEqual((*calls)[0], want) {
		t.Fatalf("run argv = %v, want %v", (*calls)[0], want)
	}

	deadline := time.Now().Add(5 * time.Second)
	var detail guidedJobDetail
	for {
		detail = decodeGuidedResponse[guidedJobDetail](t, guidedGet(handler, "/guided/jobs/"+jobID))
		if detail.Done || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !detail.Done {
		t.Fatalf("job never finished: %+v", detail)
	}
	if detail.ID != jobID || detail.Kind != "run" {
		t.Fatalf("job identity = %+v", detail)
	}
	if detail.ExitCode == nil || *detail.ExitCode != 3 {
		t.Fatalf("job exitCode = %v", detail.ExitCode)
	}
	if detail.RunID == nil || *detail.RunID != "abc123" {
		t.Fatalf("job runId = %v", detail.RunID)
	}
	output := strings.Join(detail.Output, "\n")
	if !strings.Contains(output, "created run abc123") || !strings.Contains(output, "stage research started") {
		t.Fatalf("job output missing interleaved lines: %q", output)
	}

	// The state endpoint reflects the finished job.
	state := decodeGuidedResponse[guidedStateBody](t, guidedGet(handler, "/guided/state"))
	if state.Job == nil || state.Job.ID != jobID || !state.Job.Done {
		t.Fatalf("state job = %+v", state.Job)
	}

	if missing := guidedGet(handler, "/guided/jobs/nope"); missing.Code != http.StatusNotFound {
		t.Fatalf("unknown job status = %d", missing.Code)
	}
}

func TestGettingStartedSecondRunWhileRunningConflicts(t *testing.T) {
	t.Setenv(connectDefaultTokenEnv, "token-value")
	server := newTestGuidedServer(t, t.TempDir())
	handler := http.HandlerFunc(server.serveGuided)
	stubGuidedExec(t, `sleep 5`)

	first := guidedPost(handler, "/guided/actions/run", `{}`)
	if first.Code != http.StatusAccepted {
		t.Fatalf("first run status = %d", first.Code)
	}
	second := guidedPost(handler, "/guided/actions/run", `{}`)
	if second.Code != http.StatusConflict {
		t.Fatalf("second run status = %d body = %q", second.Code, second.Body.String())
	}
	if body := decodeGuidedResponse[guidedErrorBody](t, second); body.Code != "job_running" {
		t.Fatalf("conflict code = %q", body.Code)
	}
	if err := server.close(); err != nil {
		t.Fatal(err)
	}
}

func TestGettingStartedAPIUnavailableThenReady(t *testing.T) {
	workdir := t.TempDir()
	server := newTestGuidedServer(t, workdir)
	t.Cleanup(func() { _ = server.close() })
	assets := fstest.MapFS{"index.html": &fstest.MapFile{Data: []byte(dashboardTestIndex)}}
	handler, err := newGettingStartedHandler(assets, server)
	if err != nil {
		t.Fatal(err)
	}

	before := guidedGet(handler, "/api/v1/health")
	if before.Code != http.StatusServiceUnavailable {
		t.Fatalf("pre-instance API status = %d body = %q", before.Code, before.Body.String())
	}
	if body := decodeGuidedResponse[guidedErrorBody](t, before); body.Code != "guided_no_instance" ||
		body.Message != "initialize the tutorial instance first" {
		t.Fatalf("pre-instance API body = %+v", body)
	}

	if code, _, stderr := runArgs(t, "init", server.instancePath); code != 0 {
		t.Fatalf("init tutorial instance: code = %d stderr = %q", code, stderr)
	}
	// The starter scaffold ships its own gaggle-scoped implement/run-tests
	// skill packages (SKILL002 fix); no shared-level stand-ins needed here.

	after := guidedGet(handler, "/api/v1/health")
	if after.Code != http.StatusOK {
		t.Fatalf("post-instance API status = %d body = %q", after.Code, after.Body.String())
	}
	state := decodeGuidedResponse[guidedStateBody](t, guidedGet(handler, "/guided/state"))
	if !state.APIReady || !state.InstanceExists {
		t.Fatalf("state after API ready = %+v", state)
	}
}

func TestGettingStartedHandlerServesRewrittenIndexAndAssets(t *testing.T) {
	server := newTestGuidedServer(t, t.TempDir())
	assets := fstest.MapFS{
		"index.html": &fstest.MapFile{Data: []byte(dashboardTestIndex)},
		"app.js":     &fstest.MapFile{Data: []byte("app")},
	}
	handler, err := newGettingStartedHandler(assets, server)
	if err != nil {
		t.Fatal(err)
	}

	index := guidedGet(handler, "/")
	if index.Code != http.StatusOK || !strings.Contains(index.Body.String(), `content="getting-started"`) {
		t.Fatalf("index response = %d %q", index.Code, index.Body.String())
	}
	if cache := index.Header().Get("Cache-Control"); cache != "no-store" {
		t.Fatalf("index Cache-Control = %q", cache)
	}

	static := guidedGet(handler, "/app.js")
	if static.Code != http.StatusOK || static.Body.String() != "app" {
		t.Fatalf("static response = %d %q", static.Code, static.Body.String())
	}

	spa := guidedGet(handler, "/getting-started")
	if spa.Code != http.StatusNotFound {
		t.Fatalf("unknown path status = %d", spa.Code)
	}
}
