package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"path/filepath"
	"strings"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/runner"
	"github.com/goobers/goobers/internal/secretstore"
	"github.com/goobers/goobers/internal/worktree"
)

const workspaceHelp = "Usage: goobers workspace reset <repo> [path]\n\n" +
	"Explicitly recover a configured pinned workspace. Reset refuses a live\n" +
	"lease, terminates lingering build processes, deletes the workspace, and\n" +
	"performs a full checkout at the same stable path. It never runs\n" +
	"automatically. Default instance path \".\".\n"

const workspaceResetHelp = "Usage: goobers workspace reset <repo> [path]\n\n" +
	"Tear down and re-materialize the pinned workspace for <repo>. The repo may\n" +
	"be selected by name, owner/name, or (for ADO) owner/project/name. A live\n" +
	"run lease makes the command fail without changing the workspace.\n" +
	"Exit codes: 0 = reset complete, 1 = reset refused/failed, 2 = usage error.\n"

func runWorkspace(args []string, stdout, stderr io.Writer) int {
	if len(args) > 0 && (args[0] == "-h" || args[0] == "--help" || args[0] == "help") {
		pf(stdout, "%s", workspaceHelp)
		return 0
	}
	if len(args) > 0 {
		pf(stderr, "error: unknown workspace command %q\n", args[0])
	}
	pf(stderr, "%s", workspaceHelp)
	return 2
}

func runWorkspaceReset(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("workspace reset", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = helpUsage(stderr, "workspace reset")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() < 1 || fs.NArg() > 2 {
		fs.Usage()
		return 2
	}
	selector := fs.Arg(0)
	root := "."
	if fs.NArg() == 2 {
		root = fs.Arg(1)
	}
	layout := instance.NewLayout(root)
	cfg, err := instance.LoadConfig(layout.ConfigFile())
	if err != nil {
		pf(stderr, "error: load instance config: %v\n", err)
		return 1
	}
	set, report, err := instance.LoadConfigDir(layout.ConfigDir())
	if err != nil {
		pf(stderr, "error: load definitions: %v (%v)\n", err, report)
		return 1
	}
	project, err := resolvePinnedWorkspaceProject(set, selector)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	cloneURL := repoCloneURL
	if cloneURL == nil {
		cloneURL = runner.DefaultRepoCloneURL
	}
	repoURL, err := cloneURL(project)
	if err != nil {
		pf(stderr, "error: resolve repository URL: %v\n", err)
		return 1
	}
	stores, err := secretstore.NewRegistry(cfg.SecretStores)
	if err != nil {
		pf(stderr, "error: secretStores: %v\n", err)
		return 1
	}
	registry := journal.NewRegistryScrubber()
	owner := project.Owner
	if project.Provider == apiv1.ProviderADO && project.Project != "" {
		owner += "/" + project.Project
	}
	resolver, grants, err := buildCredentials(cfg, stores, owner, project.Name, nil, registry)
	if err != nil {
		pf(stderr, "error: configure repository credentials: %v\n", err)
		return 1
	}
	gitEnv, err := buildWorktreeGitEnv(cfg, layout.WorkcopiesDir(), project, nil, resolver, grants, cloneURL, registry, stores)
	if err != nil {
		pf(stderr, "error: configure repository checkout: %v\n", err)
		return 1
	}
	options := []worktree.ManagerOption{worktree.WithPinnedRoot(layout.WorkcopiesDir())}
	if gitEnv != nil {
		options = append(options, worktree.WithGitEnvironment(gitEnv))
	}
	manager, err := worktree.NewManager(layout.WorkcopiesDir(), options...)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	baseRef := project.Branch
	if baseRef == "" {
		baseRef = "main"
	}
	resetPath, err := manager.ResetPinned(context.Background(), worktree.PinnedResetOptions{
		RepoURL: repoURL,
		BaseRef: baseRef,
	})
	if err != nil {
		pf(stderr, "error: reset workspace %s: %v\n", selector, err)
		return 1
	}
	pf(stdout, "reset pinned workspace %s at %s\n", selector, resetPath)
	return 0
}

func resolvePinnedWorkspaceProject(set *instance.ConfigSet, selector string) (apiv1.RepoRef, error) {
	var matches []apiv1.RepoRef
	seen := make(map[string]bool)
	for _, gaggle := range set.Gaggles {
		project := gaggle.Spec.Project
		if !workspaceRepoMatches(project, selector) {
			continue
		}
		if project.Checkout == nil || project.Checkout.Mode != apiv1.CheckoutModePinned {
			return apiv1.RepoRef{}, fmt.Errorf("repository %q is not configured for pinned checkout", selector)
		}
		key := strings.Join([]string{string(project.Provider), project.BaseURL, project.Owner, project.Project, project.Name}, "\x00")
		if !seen[key] {
			seen[key] = true
			matches = append(matches, project)
		}
	}
	switch len(matches) {
	case 0:
		return apiv1.RepoRef{}, fmt.Errorf("no configured pinned repository matches %q", selector)
	case 1:
		return matches[0], nil
	default:
		return apiv1.RepoRef{}, fmt.Errorf("repository selector %q is ambiguous; use owner/name or owner/project/name", selector)
	}
}

func workspaceRepoMatches(repo apiv1.RepoRef, selector string) bool {
	candidates := []string{repo.Name, filepath.ToSlash(filepath.Join(repo.Owner, repo.Name))}
	if repo.Project != "" {
		candidates = append(candidates, filepath.ToSlash(filepath.Join(repo.Owner, repo.Project, repo.Name)))
	}
	for _, candidate := range candidates {
		if strings.EqualFold(candidate, selector) {
			return true
		}
	}
	return false
}
