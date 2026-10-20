package prstatus

import (
	"context"
	"fmt"

	"github.com/goobers/goobers/providers"
)

// BaseDiffReader supplies immutable commit comparisons and the live PR diff.
type BaseDiffReader interface {
	PullRequestFiles(context.Context, providers.RepositoryRef, string) ([]providers.ChangedFile, error)
	CompareCommits(context.Context, providers.RepositoryRef, string, string) (providers.CompareResult, error)
}

// BaseMovementIntersectsPR is the delta-aware review invalidation rule shared
// by landing and native status withdrawal. Both sides of renames participate.
func BaseMovementIntersectsPR(ctx context.Context, reader BaseDiffReader, repo providers.RepositoryRef, pull, oldBase, newBase string) (bool, error) {
	if oldBase == "" || newBase == "" {
		return false, fmt.Errorf("base comparison needs both commit pins")
	}
	if oldBase == newBase {
		return false, nil
	}
	files, err := reader.PullRequestFiles(ctx, repo, pull)
	if err != nil {
		return false, fmt.Errorf("list PR's own files: %w", err)
	}
	moved, err := reader.CompareCommits(ctx, repo, oldBase, newBase)
	if err != nil {
		return false, fmt.Errorf("compare base %s...%s: %w", oldBase, newBase, err)
	}
	// GitHub returns at most 300 files for an entire comparison; commit
	// pagination does not fetch the omitted files. At the boundary, absence
	// of overlap is not evidence of disjointness. See the REST compare docs.
	if repo.Provider == providers.ProviderGitHub && len(moved.Files) >= 300 {
		return false, fmt.Errorf("base comparison reached GitHub's 300-file completeness limit")
	}
	// A rewind/divergence is not an ordinary additive base advance.
	if moved.MergeBaseSHA != "" && moved.MergeBaseSHA != oldBase {
		return true, nil
	}
	paths := make(map[string]bool, 2*len(files))
	for _, f := range files {
		paths[f.Path] = true
		if f.PreviousPath != "" {
			paths[f.PreviousPath] = true
		}
	}
	for _, f := range moved.Files {
		if paths[f.Path] || (f.PreviousPath != "" && paths[f.PreviousPath]) {
			return true, nil
		}
	}
	return false, nil
}
