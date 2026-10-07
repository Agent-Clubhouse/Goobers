package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/goobers/goobers/internal/childworkflow"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/recovery"
	"github.com/goobers/goobers/internal/runner"
)

func (h *daemonChildHandoff) yieldedWorkspace(service *daemonCredentialService, custody runner.ChildWorkspaceCustody, repoURL string) (childworkflow.YieldedWorkspace, error) {
	policy, err := childSnapshotPolicy(custody.Path, h.layout.Root, service.config)
	return childworkflow.YieldedWorkspace{Path: custody.Path, RepoURL: repoURL, RepositoryKey: childRepoKey(custody.RepoRef), Policy: policy}, err
}

// Configured file credentials (including tagged private keys/wrapping stores)
// use the same host-relative path semantics as their resolver. Record both
// lexical and symlink-resolved in-workspace locations. Built-in recovery policy
// also excludes .git, .goobers, injected .goober-assets and launcher sessions.
func childSnapshotPolicy(workspace, instanceRoot string, cfg *instance.Config) (recovery.SnapshotPolicy, error) {
	root, err := filepath.EvalSymlinks(workspace)
	if err != nil {
		return recovery.SnapshotPolicy{}, err
	}
	root, err = filepath.Abs(root)
	if err != nil {
		return recovery.SnapshotPolicy{}, err
	}
	paths := append(instance.GuardedCredentialPaths(cfg), instanceRoot)
	seen := make(map[string]string)
	for _, name := range paths {
		absolute, err := filepath.Abs(name)
		if err != nil {
			return recovery.SnapshotPolicy{}, err
		}
		resolved, err := childGuardedPath(absolute)
		if err != nil {
			return recovery.SnapshotPolicy{}, fmt.Errorf("child snapshot cannot resolve a guarded path: %w", err)
		}
		for _, candidate := range []string{absolute, resolved} {
			if candidate == "" {
				continue
			}
			relative, err := filepath.Rel(root, candidate)
			if err != nil {
				return recovery.SnapshotPolicy{}, err
			}
			if relative == "." {
				return recovery.SnapshotPolicy{}, fmt.Errorf("child workspace is itself guarded instance or credential storage")
			}
			if relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
				continue
			}
			relative = filepath.ToSlash(relative)
			seen[strings.ToLower(relative)] = relative
		}
	}
	policy := recovery.SnapshotPolicy{ExcludedPaths: make([]string, 0, len(seen))}
	for _, name := range seen {
		policy.ExcludedPaths = append(policy.ExcludedPaths, name)
	}
	slices.Sort(policy.ExcludedPaths)
	return policy, policy.Validate()
}

// Resolve existing ancestors even when a configured credential has not yet
// been materialized. This also normalizes host aliases such as /tmp on macOS.
func childGuardedPath(absolute string) (string, error) {
	current := absolute
	var suffix []string
	for {
		resolved, err := filepath.EvalSymlinks(current)
		if err == nil {
			for i := len(suffix) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, suffix[i])
			}
			return resolved, nil
		}
		if !errors.Is(err, os.ErrNotExist) || filepath.Dir(current) == current {
			return "", err
		}
		suffix = append(suffix, filepath.Base(current))
		current = filepath.Dir(current)
	}
}
