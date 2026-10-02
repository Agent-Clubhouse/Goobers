package main

import (
	"context"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/capability"
	"github.com/goobers/goobers/internal/dispatcher"
	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/providers"
)

func declareExternalAppToken(t *testing.T, root, slug string) providers.RepositoryRef {
	t.Helper()
	path := instance.NewLayout(root).ConfigFile()
	cfg, err := instance.LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	repo := githubRepoRefFromConfig(t, cfg)
	for i := range cfg.Repos {
		if cfg.Repos[i].Provider == "github" {
			cfg.Repos[i].Token = instance.TokenRef{Env: "EXTERNAL_INSTALLATION_TOKEN"}
			cfg.Repos[i].Auth = &instance.RepoAuthConfig{Kind: instance.GitHubAuthAppToken, Slug: slug}
		}
	}
	if err := instance.WriteConfig(path, cfg); err != nil {
		t.Fatal(err)
	}
	return repo
}

func TestExternalAppTokenLocalAndPodIdentity(t *testing.T) {
	root := initDemo(t)
	repo := declareExternalAppToken(t, root, "qualification")
	t.Setenv(executor.CredentialEnvVar(string(capability.ProviderPRWrite)), "installation-token")
	unsetForTest(t, dispatcher.ProviderBotLoginEnv)
	forge := newRecordingForge(t, "")
	login, err := resolveStageLogin(t, root, repo, forge)
	if err != nil || login != "qualification[bot]" {
		t.Fatalf("local identity = %q, %v", login, err)
	}
	other := repo
	other.Name = "other"
	if got := stageProviderConfiguredLogin(root, other); got.login != "" {
		t.Fatalf("identity leaked to another repo: %+v", got)
	}
	podStageEnv(t, root, repo)
	login, err = resolveStageLogin(t, podStageRoot(t), repo, forge)
	if err != nil || login != "qualification[bot]" {
		t.Fatalf("pod identity = %q, %v", login, err)
	}
	if got := stageProviderConfiguredLogin(podStageRoot(t), other); got.refusal == "" {
		t.Fatal("cross-repo pod identity was not refused")
	}
	if paths := forge.requestedPaths(); len(paths) != 0 {
		t.Fatalf("installation-token identity made HTTP requests: %v", paths)
	}
}

type externalAppClient struct{ userCalls atomic.Int32 }

func (c *externalAppClient) Do(req *http.Request) (*http.Response, error) {
	if req.URL.Path == "/user" {
		c.userCalls.Add(1)
		return &http.Response{StatusCode: http.StatusForbidden, Status: "403 Forbidden",
			Body: http.NoBody, Header: make(http.Header), Request: req}, nil
	}
	return http.DefaultClient.Do(req)
}

// The actual backlog CLI must claim AND release with a token whose /user
// endpoint refuses access. A wrong declared identity must not trust another
// App's marker. No App key or minting source is configured in either case.
func TestExternalAppTokenBacklogClaimAndRelease(t *testing.T) {
	for _, slug := range []string{"qualification", "wrong-app"} {
		t.Run(slug, func(t *testing.T) {
			root := initDemo(t)
			declareExternalAppToken(t, root, slug)
			unsetForTest(t, dispatcher.ProviderBotLoginEnv)
			server := newFakeGitHubServer(t, "your-org", "your-repo")
			server.authenticatedLogin = "qualification[bot]"
			server.addIssue(7, "Fix the bug", "goobers:approved", "goobers:ready")
			providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_ISSUES_WRITE", "external-app-run")
			client := &externalAppClient{}
			newGitHubProvider = func(token string, opts ...func(*providers.GitHubProvider)) *providers.GitHubProvider {
				return server.newGitHubProvider(token, append(opts, providers.WithHTTPClient(client))...)
			}
			t.Setenv("GOOBERS_INPUT_TRUSTLABEL", "goobers:approved")
			t.Setenv("GOOBERS_INPUT_REQUIRELABELS", "goobers:ready")
			t.Chdir(t.TempDir())
			code, stdout, stderr := runArgs(t, "backlog-query", "--claim", root)
			if slug == "wrong-app" {
				if code == 0 {
					t.Fatalf("wrong App identity trusted a claim: %s", stdout)
				}
			} else {
				if code != 0 || !strings.Contains(stdout, "claimed 7") {
					t.Fatalf("claim: code=%d stdout=%s stderr=%s", code, stdout, stderr)
				}
				server.mu.Lock()
				claimedBeforeRelease := hasAnyLabel(server.issues[7].labels, []string{providers.LabelClaimed})
				server.mu.Unlock()
				if !claimedBeforeRelease {
					t.Fatal("claim never acquired the provider marker")
				}
				code, stdout, stderr = runArgs(t, "backlog-query", "--release", root)
				if code != 0 || !strings.Contains(stdout, "released 7") {
					t.Fatalf("release: code=%d stdout=%s stderr=%s", code, stdout, stderr)
				}
				server.mu.Lock()
				claimed := hasAnyLabel(server.issues[7].labels, []string{providers.LabelClaimed})
				server.mu.Unlock()
				if claimed {
					t.Fatal("claim label remained after release")
				}
			}
			if client.userCalls.Load() != 0 {
				t.Fatal("backlog claim/release attempted GET /user")
			}
		})
	}
}

func TestExternalAppTokenTerminalReleaseUsesStaticTokenAndIdentity(t *testing.T) {
	root := initDemo(t)
	repo := declareExternalAppToken(t, root, "qualification")
	cfg, err := instance.LoadConfig(instance.NewLayout(root).ConfigFile())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("EXTERNAL_INSTALLATION_TOKEN", "externally-minted-token")
	// The ordinary resolver path must work without a private key or a mint.
	if token, err := resolveRepoToken(cfg.Repos[0], "repo", nil); err != nil || token != "externally-minted-token" {
		t.Fatalf("resolveRepoToken = %q, %v", token, err)
	}
	prev := newTerminalClaimMarkerProvider
	t.Cleanup(func() { newTerminalClaimMarkerProvider = prev })
	called := false
	newTerminalClaimMarkerProvider = func(source providers.TokenSource, opts ...func(*providers.GitHubProvider)) workItemClaimReleaser {
		called = true
		if token, err := source.Token(context.Background()); err != nil || token != "externally-minted-token" {
			t.Fatalf("terminal credential = %q, %v", token, err)
		}
		forge := newRecordingForge(t, "")
		p := providers.NewGitHubProvider("", opts...)
		p.BaseURL = forge.server.URL
		if login, err := p.AuthenticatedLogin(context.Background()); err != nil || login != "qualification[bot]" {
			t.Fatalf("terminal identity = %q, %v", login, err)
		}
		return claimReleaserFunc(func(context.Context, providers.ClaimWorkItemRequest) (providers.WorkItem, error) {
			return providers.WorkItem{}, nil
		})
	}
	registrar, _ := journal.DefaultScrubber()
	release, _, err := buildTerminalClaimMarkerRelease(instance.Layout{}, cfg, apiv1.RepoRef{}, registrar, nil)
	if err != nil || release == nil {
		t.Fatalf("terminal release construction: %v", err)
	}
	if _, err := release(context.Background(), providers.ClaimWorkItemRequest{Repository: repo, ID: "7", RunID: "run-1"}); err != nil || !called {
		t.Fatalf("terminal release: called=%v err=%v", called, err)
	}
}
