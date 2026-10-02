package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/goobers/goobers/internal/capability"
)

const configCheckoutHelp = "Usage: goobers config-checkout [dir]\n\n" +
	"Clone the instance CONFIG repository (the workflowSource repository) at\n" +
	"its tracked ref into [dir] (default: the configRepoDir input, else\n" +
	"\"config-repo\", relative to the stage workspace) and check out the run's\n" +
	"branch there, so a stage can edit the config tree and push-branch/open-pr\n" +
	"(--config-repo) can publish it. Requires the stage to declare\n" +
	"configrepo:write. The credential authenticates the clone through the git\n" +
	"environment only; it is never written to .git/config.\n\n" +
	"Inputs: head (branch to create/reuse; default the run's stable branch),\n" +
	"configRepoDir, and for stage pods without instance config, configRepo\n" +
	"(owner/name) and configRepoBase.\n" +
	"A pre-existing branch on the remote (a repass) is checked out and\n" +
	"continued rather than recreated. A non-empty [dir] is refused.\n" +
	"Exit codes: 0 = checked out, 1 = business error, 2 = usage/IO error.\n"

const configCheckoutTimeout = 5 * time.Minute

func runConfigCheckout(args []string, stdout, stderr io.Writer) int {
	fs := newCLIFlagSet("config-checkout", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = helpUsage(stderr, "config-checkout")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 1 {
		fs.Usage()
		return 2
	}
	dir := configRepoDir()
	if fs.NArg() == 1 {
		dir = fs.Arg(0)
	}
	root := providerStageRoot("")
	target, err := resolveConfigRepoTarget(root)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	token, err := configRepoWriteToken()
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	runID, workflow, err := providerRunContext()
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	head := providerInput("head", preferredOpenPRHead(root, runID, workflow))

	if entries, readErr := os.ReadDir(dir); readErr == nil && len(entries) > 0 {
		pf(stderr, "error: %s already exists and is not empty\n", dir)
		return 1
	}
	ctx, cancel := context.WithTimeout(context.Background(), configCheckoutTimeout)
	defer cancel()
	if err := checkoutConfigRepo(ctx, dir, target, head, token); err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	pf(stdout, "checked out %s/%s@%s into %s on branch %s\n", target.Repo.Owner, target.Repo.Name, target.Base, dir, head)
	return 0
}

// checkoutConfigRepo clones the config repository's base branch into dir and
// puts head checked out there: the remote head when it already exists (a
// repass), otherwise a new branch off base.
func checkoutConfigRepo(ctx context.Context, dir string, target configRepoTarget, head, token string) error {
	authEnv := append(gitAuthEnvFor(capability.ConfigRepoWrite, token), "GIT_TERMINAL_PROMPT=0")
	clone := exec.CommandContext(ctx, "git", "clone", "--quiet", "--branch", target.Base, "--single-branch", target.CloneURL(), dir)
	clone.Env = authEnv
	if out, err := clone.CombinedOutput(); err != nil {
		return fmt.Errorf("clone config repository %s/%s@%s: %w: %s", target.Repo.Owner, target.Repo.Name, target.Base, err, strings.TrimSpace(string(out)))
	}
	git := func(env []string, args ...string) ([]byte, error) {
		cmd := workspaceGitCommand(dir, args...)
		cmd.Env = composeGitEnv(dir, env)
		return workspaceGitCombinedOutput(cmd)
	}
	// A repass: continue the branch a previous attempt already pushed.
	if _, err := git(authEnv, "fetch", "--quiet", "origin", head); err == nil {
		if out, err := git(nil, "checkout", "--quiet", "-B", head, "FETCH_HEAD"); err != nil {
			return fmt.Errorf("check out existing branch %q: %w: %s", head, err, strings.TrimSpace(string(out)))
		}
		return nil
	}
	if out, err := git(nil, "checkout", "--quiet", "-b", head); err != nil {
		return fmt.Errorf("create branch %q: %w: %s", head, err, strings.TrimSpace(string(out)))
	}
	return nil
}
