package main

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/instance"
)

func TestResolvePinnedWorkspaceProject(t *testing.T) {
	cfg := &instance.Config{Repos: []instance.RepoRef{
		{
			Provider:  "github",
			Owner:     "acme",
			Name:      "web",
			Workspace: &instance.RepoWorkspaceConfig{Pinned: true},
		},
	}}
	for _, selector := range []string{"web", "acme/web", "ACME/WEB"} {
		got, err := resolvePinnedWorkspaceProject(cfg, selector)
		if err != nil {
			t.Fatalf("selector %q: %v", selector, err)
		}
		if got.Name != "web" {
			t.Fatalf("selector %q resolved %q", selector, got.Name)
		}
	}
}

func TestResolvePinnedWorkspaceProjectRejectsDisposableRepo(t *testing.T) {
	cfg := &instance.Config{Repos: []instance.RepoRef{
		{
			Provider: "github",
			Owner:    "acme",
			Name:     "web",
		},
	}}
	if _, err := resolvePinnedWorkspaceProject(cfg, "web"); err == nil ||
		!strings.Contains(err.Error(), "not configured for pinned workspace") {
		t.Fatalf("error = %v", err)
	}
}

func TestResolvePinnedWorkspaceProjectRejectsAmbiguousShortName(t *testing.T) {
	cfg := &instance.Config{Repos: []instance.RepoRef{
		{
			Provider:  "github",
			Owner:     "acme",
			Name:      "web",
			Workspace: &instance.RepoWorkspaceConfig{Pinned: true},
		},
		{
			Provider:  "github",
			Owner:     "contoso",
			Name:      "web",
			Workspace: &instance.RepoWorkspaceConfig{Pinned: true},
		},
	}}

	if _, err := resolvePinnedWorkspaceProject(cfg, "web"); err == nil ||
		!strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("error = %v", err)
	}

	got, err := resolvePinnedWorkspaceProject(cfg, "contoso/web")
	if err != nil {
		t.Fatalf("qualified selector: %v", err)
	}
	if got.Owner != "contoso" {
		t.Fatalf("qualified selector resolved owner %q", got.Owner)
	}
}

func TestResolvePinnedWorkspaceProjectNoMatch(t *testing.T) {
	cfg := &instance.Config{Repos: []instance.RepoRef{
		{
			Provider:  "github",
			Owner:     "acme",
			Name:      "web",
			Workspace: &instance.RepoWorkspaceConfig{Pinned: true},
		},
	}}
	if _, err := resolvePinnedWorkspaceProject(cfg, "does-not-exist"); err == nil ||
		!strings.Contains(err.Error(), "no configured pinned repository matches") {
		t.Fatalf("error = %v", err)
	}
}

func TestPinnedWorkspaceLayoutUsesGaggleOverride(t *testing.T) {
	root := t.TempDir()
	project := apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web"}
	override := filepath.Join(t.TempDir(), "short")
	cfg := &instance.Config{
		Workcopies: &instance.WorkcopiesConfig{Root: filepath.Join(t.TempDir(), "instance")},
		Repos: []instance.RepoRef{{
			Provider: "github",
			Owner:    "acme",
			Name:     "web",
			Workspace: &instance.RepoWorkspaceConfig{
				Pinned: true,
			},
		}},
	}
	set := &instance.ConfigSet{Gaggles: []apiv1.Gaggle{{
		ObjectMeta: metav1.ObjectMeta{Name: "widgets"},
		Spec: apiv1.GaggleSpec{
			Project:    project,
			Workcopies: &apiv1.GaggleWorkcopies{Root: override},
		},
	}}}

	layout, err := pinnedWorkspaceLayout(instance.NewLayout(root), cfg, set, project)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := layout.WorkcopiesBaseDir(), filepath.Join(override, "widgets"); got != want {
		t.Fatalf("WorkcopiesBaseDir = %q, want %q", got, want)
	}
}

func TestPinnedWorkspaceLayoutRejectsDifferentGaggleRoots(t *testing.T) {
	project := apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web"}
	cfg := &instance.Config{Repos: []instance.RepoRef{{
		Provider:  "github",
		Owner:     "acme",
		Name:      "web",
		Workspace: &instance.RepoWorkspaceConfig{Pinned: true},
	}}}
	set := &instance.ConfigSet{Gaggles: []apiv1.Gaggle{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "alpha"},
			Spec:       apiv1.GaggleSpec{Project: project, Workcopies: &apiv1.GaggleWorkcopies{Root: filepath.Join(t.TempDir(), "alpha")}},
		},
		{
			ObjectMeta: metav1.ObjectMeta{Name: "beta"},
			Spec:       apiv1.GaggleSpec{Project: project, Workcopies: &apiv1.GaggleWorkcopies{Root: filepath.Join(t.TempDir(), "beta")}},
		},
	}}

	_, err := pinnedWorkspaceLayout(instance.NewLayout(t.TempDir()), cfg, set, project)
	if err == nil || !strings.Contains(err.Error(), "different workcopies roots") {
		t.Fatalf("error = %v", err)
	}
}

// TestRunWorkspaceResetGitAskpassUsesAbsoluteWorkcopiesRoot pins the
// `goobers workspace reset` half of the askpass-relative-residual fix:
// a3b2e636 absolutized the daemon path's workcopies root
// (cmd/goobers/runnerwiring.go) but left this command building its
// GIT_ASKPASS from layout.WorkcopiesBaseDir() un-Abs'd. With the command's
// own default instance path "." (workspaceHelp), that produced a relative
// askpass script path — and worktree.Manager's git subprocesses run with
// cmd.Dir set to a mirror directory nested under the workcopies root, not
// the process's own cwd (internal/worktree/manager.go's NewManager doc),
// so git resolved the relative path against the wrong directory and never
// got as far as presenting a token. This drives a real reset against a
// local HTTPS remote and asserts git got far enough to authenticate.
func TestRunWorkspaceResetGitAskpassUsesAbsoluteWorkcopiesRoot(t *testing.T) {
	t.Setenv("GOOBERS_GITHUB_TOKEN", "test-token")
	root := initDemo(t)

	instanceYAMLPath := filepath.Join(root, "instance.yaml")
	raw, err := os.ReadFile(instanceYAMLPath)
	if err != nil {
		t.Fatal(err)
	}
	updated := strings.Replace(
		string(raw),
		"  provider: github\n",
		"  provider: github\n  workspace:\n    pinned: true\n",
		1,
	)
	if updated == string(raw) {
		t.Fatalf("starter instance.yaml did not contain the expected repo provider line:\n%s", raw)
	}
	if err := os.WriteFile(instanceYAMLPath, []byte(updated), 0o644); err != nil {
		t.Fatal(err)
	}

	authenticated := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "" {
			w.Header().Set("WWW-Authenticate", `Basic realm="test"`)
			http.Error(w, "authentication required", http.StatusUnauthorized)
			return
		}
		select {
		case authenticated <- struct{}{}:
		default:
		}
		http.Error(w, "stop after authentication", http.StatusInternalServerError)
	}))
	t.Cleanup(server.Close)

	gitConfig := filepath.Join(root, "gitconfig")
	rewrite := fmt.Sprintf("[url %q]\n\tinsteadOf = https://github.com/\n", server.URL+"/")
	if err := os.WriteFile(gitConfig, []byte(rewrite), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", gitConfig)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")

	t.Chdir(root)

	var stdout, stderr bytes.Buffer
	// No explicit path argument, so the instance root defaults to "." —
	// exactly the relative root workspaceHelp documents and the one that
	// broke the daemon path pre-a3b2e636.
	runWorkspaceReset([]string{"your-repo"}, &stdout, &stderr)

	select {
	case <-authenticated:
	default:
		t.Fatalf("git never presented a token to the remote — askpass path likely failed to resolve; stderr: %s", stderr.String())
	}
}
