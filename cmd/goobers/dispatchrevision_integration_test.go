//go:build integration

package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/dispatcher"
	"github.com/goobers/goobers/internal/testgit"
	"github.com/goobers/goobers/internal/workspacerevision"
	"github.com/goobers/goobers/test/testsupport/testdep"
)

func podRevisionGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := testgit.Command(args...)
	cmd.Dir = dir
	cmd.Env = append(cmd.Env, "GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@example.invalid",
		"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@example.invalid")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func podRevisionSource(t *testing.T) (string, string) {
	t.Helper()
	dir := t.TempDir()
	podRevisionGit(t, dir, "init", "--quiet", "--template=", "-b", "same-name")
	files := map[string]string{
		"included/data.txt": "selected\n",
		"excluded/data.txt": "excluded\n",
		".gitattributes":    "*.txt filter=custom\n*.lfs filter=lfs\n",
		"pointer.lfs":       "version https://git-lfs.github.com/spec/v1\noid sha256:" + strings.Repeat("a", 64) + "\nsize 1\n",
		".gitmodules":       "[submodule \"nested\"]\n\tpath = nested\n\turl = https://must-not-contact.invalid/submodule\n",
	}
	for path, content := range files {
		target := filepath.Join(dir, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(target, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	podRevisionGit(t, dir, "add", ".")
	podRevisionGit(t, dir, "commit", "--quiet", "-m", "source")
	first := podRevisionGit(t, dir, "rev-parse", "HEAD")
	podRevisionGit(t, dir, "update-index", "--add", "--cacheinfo", "160000,"+first+",nested")
	podRevisionGit(t, dir, "commit", "--quiet", "-m", "submodule pointer")
	podRevisionGit(t, dir, "config", "uploadpack.allowFilter", "true")
	podRevisionGit(t, dir, "config", "uploadpack.allowAnySHA1InWant", "true")
	return dir, podRevisionGit(t, dir, "rev-parse", "HEAD")
}

func podRevisionFileURL(path string) string {
	path = filepath.ToSlash(path)
	if runtime.GOOS == "windows" {
		path = "/" + path
	}
	return (&url.URL{Scheme: "file", Path: path}).String()
}

func TestIntegrationPodWorkspaceRevisionMaterialization(t *testing.T) {
	testdep.Require(t, "git")
	source, sha := podRevisionSource(t)
	original := checkoutCloneURL
	t.Cleanup(func() { checkoutCloneURL = original })
	checkoutCloneURL = func(repo apiv1.RepoRef) (string, error) {
		if repo.Owner != "fork" {
			t.Fatalf("base repository substituted: %+v", repo)
		}
		return podRevisionFileURL(source), nil
	}
	// A moving, same-named source ref is provenance, never resolution input.
	podRevisionGit(t, source, "commit", "--quiet", "--allow-empty", "-m", "later branch tip")
	for _, partial := range []bool{false, true} {
		for _, sparse := range []bool{false, true} {
			name := map[bool]string{false: "full", true: "partial"}[partial]
			if sparse {
				name += "-sparse"
			}
			t.Run(name, func(t *testing.T) {
				revision, checkout := podRevisionFixture()
				revision.CommitSHA, checkout.PartialClone = sha, partial
				if sparse {
					checkout.Repository.Checkout = &apiv1.CheckoutSpec{Sparse: []string{"included"}}
				}
				stampPodRevision(t, revision, checkout)
				// None of the inherited filter, hook or fsmonitor commands may execute.
				ambient := t.TempDir()
				config := filepath.Join(ambient, "gitconfig")
				marker := filepath.Join(ambient, "executed")
				hook := filepath.Join(ambient, "post-checkout")
				script := "#!/bin/sh\nprintf unsafe > '" + filepath.ToSlash(marker) + "'\nexit 77\n"
				if err := os.WriteFile(hook, []byte(script), 0o755); err != nil {
					t.Fatal(err)
				}
				settings := "[filter \"custom\"]\n smudge = exit 77\n clean = exit 77\n process = exit 77\n required = true\n[core]\n fsmonitor = " +
					filepath.ToSlash(hook) + "\n hooksPath = " + filepath.ToSlash(ambient) + "\n[submodule]\n recurse = true\n"
				if err := os.WriteFile(config, []byte(settings), 0o644); err != nil {
					t.Fatal(err)
				}
				t.Setenv("GIT_CONFIG_GLOBAL", config)
				dir := t.TempDir()
				creds := []dispatcher.MintedCredential{{Capability: dispatcher.WorkspaceRevisionCheckoutCapability, Value: "source-only"}}
				if err := checkoutRepoWorkspace(context.Background(), dir, io.Discard, creds); err != nil {
					t.Fatal(err)
				}
				if head := podRevisionGit(t, dir, "rev-parse", "HEAD"); head != sha {
					t.Fatalf("HEAD=%s, want %s", head, sha)
				}
				if _, err := os.Stat(filepath.Join(dir, "included", "data.txt")); err != nil {
					t.Fatal(err)
				}
				_, statErr := os.Stat(filepath.Join(dir, "excluded", "data.txt"))
				if sparse != os.IsNotExist(statErr) {
					t.Fatalf("sparse=%v excluded=%v", sparse, statErr)
				}
				pointer, err := os.ReadFile(filepath.Join(dir, "pointer.lfs"))
				if err != nil || !strings.HasPrefix(string(pointer), "version https://git-lfs.github.com/spec/v1") {
					t.Fatalf("LFS expanded: %s %v", pointer, err)
				}
				if _, err := os.Stat(filepath.Join(dir, "nested", ".git")); !os.IsNotExist(err) {
					t.Fatalf("submodule expanded: %v", err)
				}
				if _, err := os.Stat(marker); !os.IsNotExist(err) {
					t.Fatalf("ambient hook or fsmonitor executed: %v", err)
				}
				if partial && sparse {
					blob := podRevisionGit(t, source, "rev-parse", sha+":excluded/data.txt")
					cmd := testgit.Command("--no-lazy-fetch", "cat-file", "-e", blob)
					cmd.Dir = dir
					if err := cmd.Run(); err == nil {
						t.Fatal("sparse partial checkout acquired excluded blob")
					}
				}
				git := selectedRevisionGit{dir: dir, env: sterileRevisionEnvironment(os.Environ())}
				if err := git.verifyHEAD(context.Background(), sha); err != nil {
					t.Fatal(err)
				}
				if code := podWorkspaceFailureCode("", git.verifyHEAD(context.Background(), strings.Repeat("b", 40))); code != workspacerevision.CodeSHAMismatch {
					t.Fatalf("mismatched HEAD classified %q", code)
				}
			})
		}
	}
}

func TestIntegrationPodWorkspaceRevisionAnonymousGrant(t *testing.T) {
	testdep.Require(t, "git")
	source, sha := podRevisionSource(t)
	original := checkoutCloneURL
	t.Cleanup(func() { checkoutCloneURL = original })
	checkoutCloneURL = func(apiv1.RepoRef) (string, error) { return podRevisionFileURL(source), nil }
	revision, checkout := podRevisionFixture()
	revision.CommitSHA = sha
	stampPodRevision(t, revision, checkout)
	credentialCalls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		credentialCalls++
		var request struct {
			RunID             string                   `json:"runId"`
			Stage             string                   `json:"stage"`
			WorkspaceRevision *apiv1.WorkspaceRevision `json:"workspaceRevision"`
			Capabilities      []string                 `json:"capabilities"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
		}
		if request.RunID != "public-run" || request.Stage != "inspect" ||
			request.WorkspaceRevision == nil || request.WorkspaceRevision.CommitSHA != sha || len(request.Capabilities) != 0 {
			t.Errorf("unexpected public checkout request: %+v", request)
		}
		_, _ = w.Write([]byte(`{"credentials":[]}`))
	}))
	defer server.Close()
	t.Setenv(dispatcher.EnvDaemonAPI, server.URL)
	t.Setenv(dispatcher.EnvRunID, "public-run")
	t.Setenv(dispatcher.EnvStage, "inspect")
	t.Setenv("GIT_ASKPASS", "must-not-use-ambient-auth")
	t.Setenv("GOOBERS_GIT_TOKEN", "must-not-use-base-token")
	t.Setenv("SSH_ASKPASS", "must-not-use-ambient-auth")
	for _, entry := range anonymousRevisionEnvironment() {
		if strings.HasPrefix(entry, "GIT_ASKPASS=") || strings.HasPrefix(entry, "SSH_ASKPASS=") || strings.HasPrefix(entry, "GOOBERS_GIT_TOKEN=") {
			t.Fatal("anonymous grant inherited authentication")
		}
	}
	dir := t.TempDir()
	creds, err := resolveCheckoutCredential(context.Background())
	if err != nil || credentialCalls != 1 {
		t.Fatalf("public credential resolve: calls=%d, %v", credentialCalls, err)
	}
	err = checkoutRepoWorkspace(context.Background(), dir, io.Discard, creds)
	if err != nil {
		t.Fatal(err)
	}
	if got := podRevisionGit(t, dir, "rev-parse", "HEAD"); got != sha {
		t.Fatalf("anonymous selected HEAD=%s, want %s", got, sha)
	}
}

func TestIntegrationPodWorkspaceRevisionRefusals(t *testing.T) {
	testdep.Require(t, "git")
	source, sha := podRevisionSource(t)
	other, _ := podRevisionSource(t)
	podRevisionGit(t, other, "commit", "--quiet", "--allow-empty", "-m", "different repository")
	blob := podRevisionGit(t, source, "rev-parse", sha+":included/data.txt")
	original := checkoutCloneURL
	t.Cleanup(func() { checkoutCloneURL = original })
	for _, tc := range []struct{ name, remote, sha, code string }{
		{"missing fork", filepath.Join(source, "missing.git"), sha, workspacerevision.CodeAcquisition},
		{"missing object", source, strings.Repeat("b", 40), workspacerevision.CodeAcquisition},
		{"noncommit", source, blob, workspacerevision.CodeObjectType},
		// A source-specific object that no same-named base branch can supply.
		{"same branch other repository", other, podRevisionGit(t, source, "rev-parse", "HEAD"), ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.name == "same branch other repository" {
				podRevisionGit(t, source, "commit", "--quiet", "--allow-empty", "-m", "fork-only")
				tc.sha, tc.code = podRevisionGit(t, source, "rev-parse", "HEAD"), workspacerevision.CodeAcquisition
			}
			checkoutCloneURL = func(apiv1.RepoRef) (string, error) { return podRevisionFileURL(tc.remote), nil }
			revision, checkout := podRevisionFixture()
			revision.CommitSHA = tc.sha
			stampPodRevision(t, revision, checkout)
			err := checkoutRepoWorkspace(context.Background(), t.TempDir(), io.Discard,
				[]dispatcher.MintedCredential{{Capability: dispatcher.WorkspaceRevisionCheckoutCapability, Value: "source-only"}})
			if code := podWorkspaceFailureCode("", err); code != tc.code {
				t.Fatalf("error=%v, want %s", err, tc.code)
			}
		})
	}
}
