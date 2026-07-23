package main

import (
	"context"
	"encoding/base64"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"strings"

	"github.com/goobers/goobers/internal/adoauth"
	"github.com/goobers/goobers/internal/capability"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/providers"
)

// runPushBranch implements `goobers push-branch` (#237): the deterministic
// push stage a workflow declares between local-ci and open-pr, closing the
// gap where an implementer's commits never reached origin — open-pr's PR
// creation would then 422 on a branch that was never pushed, with the
// diagnosis invisible from the journal.
//
// Unlike open-pr/backlog-query/issue-close-out (which talk to a provider's
// REST API), push-branch's target is the worktree's own git remote. GitHub uses
// the runner-injected repo:push token. ADO matches that remote against
// instance.yaml and resolves its configured PAT or Entra credential source.
const pushBranchHelp = "Usage: goobers push-branch [path]\n\n" +
	"Push the worktree's checked-out branch to origin, authenticated via the\n" +
	"configured repository credential — never the host's ambient git\n" +
	"credentials, and never persisted to .git/config.\n" +
	"[path] defaults to the current directory (the stage's worktree).\n" +
	"Exit codes: 0 = pushed, 1 = business error, 2 = usage/IO error.\n"

func runPushBranch(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("push-branch", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = helpUsage(stderr, "push-branch")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 1 {
		fs.Usage()
		return 2
	}
	dir := "."
	if fs.NArg() == 1 {
		dir = fs.Arg(0)
	}

	branch, err := currentBranch(dir)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	env, err := pushBranchEnvironment(dir)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	if err := gitPushBranch(dir, branch, env); err != nil {
		pf(stderr, "error: push branch %q: %v\n", branch, err)
		return 1
	}

	pf(stdout, "pushed %s to origin\n", branch)
	return 0
}

// currentBranch returns the branch checked out at dir — the run branch
// worktree.Manager.Create already created or checked out before this stage's
// process started. push-branch pushes exactly that branch rather than
// reconstructing a name from GOOBERS_RUN_ID/GOOBERS_WORKFLOW, so it can never
// drift from what the worktree actually has checked out.
func currentBranch(dir string) (string, error) {
	cmd := exec.Command("git", "symbolic-ref", "--short", "HEAD")
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("determine checked-out branch (detached HEAD?): %w", err)
	}
	branch := strings.TrimSpace(string(out))
	if branch == "" {
		return "", fmt.Errorf("worktree at %s has no checked-out branch (detached HEAD)", dir)
	}
	return branch, nil
}

// gitPushBranch pushes branch to origin, authenticated via gitAuthEnv (#237's
// "token never lands on disk" requirement).
//
// Pushes to origin's resolved URL, not "origin" by name: worktree.Manager's
// managed working copy is a `git clone --mirror`, which sets
// remote.origin.mirror=true — a worktree checked out off it shares that same
// repo config, and git refuses to combine a mirrored remote with an explicit
// refspec ("fatal: --mirror can't be combined with refspecs"). Pushing by
// URL bypasses that remote-name-keyed restriction entirely.
func gitPushBranch(dir, branch string, env []string) error {
	url, err := originURL(dir)
	if err != nil {
		return err
	}
	cmd := exec.Command("git", "push", url, branch+":"+branch)
	cmd.Dir = dir
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func pushBranchEnvironment(dir string) ([]string, error) {
	root := os.Getenv("GOOBERS_INSTANCE_ROOT")
	if root != "" {
		cfg, err := instance.LoadConfig(instance.NewLayout(root).ConfigFile())
		if err != nil {
			return nil, fmt.Errorf("load instance for repository authentication: %w", err)
		}
		remote, err := originURL(dir)
		if err != nil {
			return nil, err
		}
		if repo, ok := adoRepoForOrigin(cfg, remote); ok {
			source, err := adoauth.Source(repo, nil)
			if err != nil {
				return nil, err
			}
			ctx, cancel := context.WithTimeout(context.Background(), repositoryPreflightTimeout)
			defer cancel()
			return providers.ADOGitAuthEnvironment(ctx, source, nil, remote)
		}
		if isADORemote(remote) {
			return nil, fmt.Errorf("ADO origin %q does not match any configured repository", remote)
		}
	}
	token, err := providerToken(capability.RepoPush)
	if err != nil {
		return nil, err
	}
	return gitAuthEnv(token), nil
}

func adoRepoForOrigin(cfg *instance.Config, remote string) (instance.RepoRef, bool) {
	if cfg == nil {
		return instance.RepoRef{}, false
	}
	normalized := strings.TrimSuffix(strings.TrimRight(remote, "/"), ".git")
	for i := range cfg.Repos {
		repo := cfg.Repos[i]
		if repo.Provider != "ado" {
			continue
		}
		expected := fmt.Sprintf("https://dev.azure.com/%s/%s/_git/%s",
			url.PathEscape(repo.Owner), url.PathEscape(repo.Project), url.PathEscape(repo.Name))
		if strings.EqualFold(normalized, expected) {
			return repo, true
		}
	}
	return instance.RepoRef{}, false
}

func isADORemote(remote string) bool {
	parsed, err := url.Parse(remote)
	if err != nil {
		return false
	}
	host := strings.ToLower(parsed.Hostname())
	return host == "dev.azure.com" || strings.HasSuffix(host, ".visualstudio.com")
}

// gitAuthEnv builds the env-var-injected git credential (shared by every git
// subprocess this binary spawns against origin: gitPushBranch,
// checkoutExistingBranch, attemptRebase, forcePushWithLease) — per-invocation
// via GIT_CONFIG_COUNT/GIT_CONFIG_KEY_0/GIT_CONFIG_VALUE_0 (git 2.31+'s
// environment-based config), never a URL-embedded credential or a
// command-line -c flag, so the token never appears in argv (visible via
// `ps`) and is never written to any file. GitHub's HTTPS token convention is
// basic auth with the token as the password and any non-empty username;
// "x-access-token" is GitHub's own documented placeholder for that username.
func gitAuthEnv(token string) []string {
	auth := base64.StdEncoding.EncodeToString([]byte("x-access-token:" + token))
	return append(os.Environ(),
		"GIT_CONFIG_COUNT=1",
		"GIT_CONFIG_KEY_0=http.extraheader",
		"GIT_CONFIG_VALUE_0=AUTHORIZATION: basic "+auth,
	)
}

// originURL resolves the worktree's "origin" remote to its configured URL.
func originURL(dir string) (string, error) {
	cmd := exec.Command("git", "remote", "get-url", "origin")
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("resolve origin URL: %w", err)
	}
	url := strings.TrimSpace(string(out))
	if url == "" {
		return "", fmt.Errorf("worktree at %s has no origin remote configured", dir)
	}
	return url, nil
}
