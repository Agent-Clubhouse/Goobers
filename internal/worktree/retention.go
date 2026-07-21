package worktree

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// RetentionRule identifies the limit that made a resource eligible.
type RetentionRule string

// Retention rules distinguish age, storage-cap, and merged-branch candidates.
const (
	RetentionRuleWindow       RetentionRule = "retention-window"
	RetentionRuleStorageCap   RetentionRule = "storage-cap"
	RetentionRuleMergedBranch RetentionRule = "merged-local-branch"
)

// RetentionKind identifies the resource considered by a retention pass.
type RetentionKind string

// Retention kinds distinguish worktree and branch candidates.
const (
	RetentionKindWorktree RetentionKind = "worktree"
	RetentionKindBranch   RetentionKind = "branch"
)

// RetentionOptions configures an instance-wide retention pass. Delete must be
// explicitly true for the pass to mutate anything; false is candidate-reporting
// mode.
type RetentionOptions struct {
	Delete           bool
	MaxRetainedBytes int64
	MaxAge           time.Duration
	Now              time.Time

	// IsTerminalFailure authorizes a retained worktree for pruning. The
	// manager root selects the owning gaggle journal; ownerRunID is empty only
	// for legacy markers.
	IsTerminalFailure func(managerRoot, worktreeID, ownerRunID string) (bool, error)
	// IsRunTerminal authorizes a merged local branch for pruning.
	IsRunTerminal func(managerRoot, runID string) (bool, error)
	// IsBranchProtected reports whether a branch is still referenced by a
	// nonterminal run, even when the run encoded in its name is terminal.
	IsBranchProtected func(managerRoot, branch string) (bool, error)
}

// RetentionResult reports one candidate. Deleted is true only after successful
// removal; BytesReclaimed is therefore never optimistic.
type RetentionResult struct {
	Kind           RetentionKind
	Rule           RetentionRule
	ManagerRoot    string
	RepositoryPath string
	RunID          string
	WorktreeID     string
	Path           string
	Branch         string
	Bytes          int64
	BytesReclaimed int64
	Deleted        bool
	DryRun         bool
	Err            error
}

// RetentionWarning reports a resource that could not be evaluated safely.
type RetentionWarning struct {
	Path string
	Err  error
}

type retainedWorktree struct {
	manager    *Manager
	key        string
	markerPath string
	marker     marker
	path       string
	bytes      int64
	retainedAt time.Time
	eligible   bool
}

type localBranch struct {
	name string
	tip  string
}

// PruneRetained applies one storage cap across all managers and prunes merged
// local run branches in the same serialized pass. Managers and candidates are
// normalized before evaluation so map iteration and caller order cannot affect
// the outcome.
func PruneRetained(ctx context.Context, managers []*Manager, opts RetentionOptions) ([]RetentionResult, []RetentionWarning, error) {
	if opts.MaxRetainedBytes < 0 {
		return nil, nil, fmt.Errorf("worktree: retained byte cap must not be negative")
	}
	if opts.MaxAge < 0 {
		return nil, nil, fmt.Errorf("worktree: retention window must not be negative")
	}
	if opts.Now.IsZero() {
		opts.Now = time.Now()
	}

	ordered := uniqueManagers(managers)
	for _, manager := range ordered {
		manager.pruneMu.Lock()
	}
	defer func() {
		for i := len(ordered) - 1; i >= 0; i-- {
			ordered[i].pruneMu.Unlock()
		}
	}()

	retained, totalBytes, warnings, err := inventoryRetainedWorktrees(ordered, opts)
	if err != nil {
		return nil, warnings, err
	}
	sort.Slice(retained, func(i, j int) bool {
		if !retained[i].retainedAt.Equal(retained[j].retainedAt) {
			return retained[i].retainedAt.Before(retained[j].retainedAt)
		}
		if retained[i].manager.Root != retained[j].manager.Root {
			return retained[i].manager.Root < retained[j].manager.Root
		}
		if retained[i].key != retained[j].key {
			return retained[i].key < retained[j].key
		}
		return retained[i].marker.RunID < retained[j].marker.RunID
	})

	var results []RetentionResult
	attempted := make(map[string]bool, len(retained))
	for i := range retained {
		candidate := &retained[i]
		if !candidate.eligible || opts.MaxAge == 0 || opts.Now.Sub(candidate.retainedAt) < opts.MaxAge {
			continue
		}
		attempted[candidate.markerPath] = true
		result := pruneRetainedWorktree(ctx, candidate, RetentionRuleWindow, opts.Delete)
		results = append(results, result)
		if result.Deleted || result.DryRun {
			totalBytes -= candidate.bytes
		}
	}

	if opts.MaxRetainedBytes > 0 {
		for i := range retained {
			if totalBytes <= opts.MaxRetainedBytes {
				break
			}
			candidate := &retained[i]
			if !candidate.eligible || attempted[candidate.markerPath] {
				continue
			}
			attempted[candidate.markerPath] = true
			result := pruneRetainedWorktree(ctx, candidate, RetentionRuleStorageCap, opts.Delete)
			results = append(results, result)
			if result.Deleted || result.DryRun {
				totalBytes -= candidate.bytes
			}
		}
	}

	branchResults, branchWarnings, err := pruneMergedBranches(ctx, ordered, opts)
	results = append(results, branchResults...)
	warnings = append(warnings, branchWarnings...)
	if err != nil {
		return results, warnings, err
	}
	return results, warnings, nil
}

func uniqueManagers(managers []*Manager) []*Manager {
	byRoot := make(map[string]*Manager, len(managers))
	for _, manager := range managers {
		if manager != nil {
			byRoot[manager.Root] = manager
		}
	}
	roots := make([]string, 0, len(byRoot))
	for root := range byRoot {
		roots = append(roots, root)
	}
	sort.Strings(roots)
	ordered := make([]*Manager, 0, len(roots))
	for _, root := range roots {
		ordered = append(ordered, byRoot[root])
	}
	return ordered
}

func inventoryRetainedWorktrees(managers []*Manager, opts RetentionOptions) ([]retainedWorktree, int64, []RetentionWarning, error) {
	var retained []retainedWorktree
	var totalBytes int64
	var warnings []RetentionWarning
	for _, manager := range managers {
		repositories, err := os.ReadDir(manager.Root)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return retained, totalBytes, warnings, fmt.Errorf("worktree: list root %s: %w", manager.Root, err)
		}
		for _, repository := range repositories {
			if !repository.IsDir() {
				continue
			}
			key := repository.Name()
			markers, err := os.ReadDir(manager.markersDirForKey(key))
			if err != nil {
				if os.IsNotExist(err) {
					continue
				}
				return retained, totalBytes, warnings, fmt.Errorf("worktree: list retained markers for %s: %w", key, err)
			}
			for _, entry := range markers {
				if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
					continue
				}
				markerPath := filepath.Join(manager.markersDirForKey(key), entry.Name())
				mk, err := readMarker(markerPath)
				if err != nil {
					warnings = append(warnings, RetentionWarning{Path: markerPath, Err: err})
					continue
				}
				if mk.Status != statusKept {
					continue
				}
				worktreeID := strings.TrimSuffix(entry.Name(), ".json")
				if !validRunID(worktreeID) || mk.RunID != worktreeID {
					warnings = append(warnings, RetentionWarning{
						Path: markerPath,
						Err:  fmt.Errorf("marker run ID %q does not match safe filename %q", mk.RunID, worktreeID),
					})
					continue
				}
				path := filepath.Join(manager.runsDirForKey(key), worktreeID)
				bytes, err := directorySize(path)
				if err != nil {
					warnings = append(warnings, RetentionWarning{Path: path, Err: err})
					continue
				}
				totalBytes += bytes
				eligible := false
				if opts.IsTerminalFailure != nil {
					eligible, err = opts.IsTerminalFailure(manager.Root, worktreeID, mk.OwnerRunID)
					if err != nil {
						warnings = append(warnings, RetentionWarning{Path: path, Err: err})
						eligible = false
					}
				}
				retained = append(retained, retainedWorktree{
					manager: manager, key: key, markerPath: markerPath, marker: mk,
					path: path, bytes: bytes, retainedAt: mk.retainedAt(), eligible: eligible,
				})
			}
		}
	}
	return retained, totalBytes, warnings, nil
}

func directorySize(root string) (int64, error) {
	var total int64
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		total += info.Size()
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("measure retained worktree %s: %w", root, err)
	}
	return total, nil
}

func pruneRetainedWorktree(ctx context.Context, candidate *retainedWorktree, rule RetentionRule, delete bool) RetentionResult {
	result := RetentionResult{
		Kind: RetentionKindWorktree, Rule: rule, ManagerRoot: candidate.manager.Root,
		RepositoryPath: candidate.manager.repoDirForKey(candidate.key),
		RunID:          candidate.marker.OwnerRunID, WorktreeID: candidate.marker.RunID,
		Path: candidate.path, Bytes: candidate.bytes, DryRun: !delete,
	}
	if !delete {
		return result
	}
	if err := candidate.manager.reapOne(ctx, candidate.key, candidate.path, candidate.markerPath, &candidate.marker); err != nil {
		result.Err = err
		return result
	}
	result.Deleted = true
	result.BytesReclaimed = candidate.bytes
	return result
}

func pruneMergedBranches(ctx context.Context, managers []*Manager, opts RetentionOptions) ([]RetentionResult, []RetentionWarning, error) {
	var results []RetentionResult
	var warnings []RetentionWarning
	for _, manager := range managers {
		repositories, err := os.ReadDir(manager.Root)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return results, warnings, fmt.Errorf("worktree: list root %s: %w", manager.Root, err)
		}
		for _, repository := range repositories {
			if !repository.IsDir() {
				continue
			}
			key := repository.Name()
			repoDir := manager.repoDirForKey(key)
			if _, err := os.Stat(repoDir); err != nil {
				if os.IsNotExist(err) {
					continue
				}
				return results, warnings, fmt.Errorf("worktree: stat managed repository %s: %w", repoDir, err)
			}
			lock := manager.lockFor(key)
			lock.Lock()
			repoResults, repoWarnings, err := pruneRepoMergedBranches(ctx, manager, repoDir, opts)
			lock.Unlock()
			results = append(results, repoResults...)
			warnings = append(warnings, repoWarnings...)
			if err != nil {
				return results, warnings, err
			}
		}
	}
	return results, warnings, nil
}

func pruneRepoMergedBranches(ctx context.Context, manager *Manager, repoDir string, opts RetentionOptions) ([]RetentionResult, []RetentionWarning, error) {
	branches, err := localBranches(ctx, repoDir)
	if err != nil {
		return nil, nil, fmt.Errorf("worktree: list local branches in %s: %w", repoDir, err)
	}
	var bases []localBranch
	type runBranch struct {
		localBranch
		runID string
	}
	var runs []runBranch
	for _, branch := range branches {
		runID, ok := manager.runIDForBranch(branch.name)
		if !ok {
			bases = append(bases, branch)
			continue
		}
		runs = append(runs, runBranch{localBranch: branch, runID: runID})
	}

	var results []RetentionResult
	var warnings []RetentionWarning
	for _, branch := range runs {
		terminal := false
		if opts.IsRunTerminal != nil {
			terminal, err = opts.IsRunTerminal(manager.Root, branch.runID)
			if err != nil {
				warnings = append(warnings, RetentionWarning{Path: "refs/heads/" + branch.name, Err: err})
				continue
			}
		}
		if !terminal {
			continue
		}
		protected := false
		if opts.IsBranchProtected != nil {
			protected, err = opts.IsBranchProtected(manager.Root, branch.name)
			if err != nil {
				warnings = append(warnings, RetentionWarning{Path: "refs/heads/" + branch.name, Err: err})
				continue
			}
		}
		if protected {
			continue
		}
		merged := false
		for _, base := range bases {
			merged, err = commitIsAncestor(ctx, repoDir, branch.tip, base.tip)
			if err != nil {
				return results, warnings, fmt.Errorf("worktree: inspect whether branch %s is merged into %s: %w", branch.name, base.name, err)
			}
			if merged {
				break
			}
		}
		if !merged {
			continue
		}
		result := RetentionResult{
			Kind: RetentionKindBranch, Rule: RetentionRuleMergedBranch,
			ManagerRoot: manager.Root, RepositoryPath: repoDir,
			RunID: branch.runID, Branch: branch.name, DryRun: !opts.Delete,
		}
		if opts.Delete {
			if err := runGit(ctx, repoDir, "branch", "-D", "--", branch.name); err != nil {
				result.Err = err
			} else {
				result.Deleted = true
			}
		}
		results = append(results, result)
	}
	return results, warnings, nil
}

func localBranches(ctx context.Context, repoDir string) ([]localBranch, error) {
	out, err := gitOutput(ctx, repoDir, "for-each-ref", "--format=%(refname:short)%00%(objectname)", "refs/heads")
	if err != nil || out == "" {
		return nil, err
	}
	lines := strings.Split(out, "\n")
	branches := make([]localBranch, 0, len(lines))
	for _, line := range lines {
		parts := strings.SplitN(line, "\x00", 2)
		if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
			return nil, fmt.Errorf("unexpected for-each-ref record %q", line)
		}
		branches = append(branches, localBranch{name: parts[0], tip: parts[1]})
	}
	sort.Slice(branches, func(i, j int) bool { return branches[i].name < branches[j].name })
	return branches, nil
}

func (m *Manager) runIDForBranch(branch string) (string, bool) {
	namespaces := append([]string(nil), m.runBranchNamespaces...)
	sort.Slice(namespaces, func(i, j int) bool {
		if len(namespaces[i]) != len(namespaces[j]) {
			return len(namespaces[i]) > len(namespaces[j])
		}
		return namespaces[i] < namespaces[j]
	})
	for _, namespace := range namespaces {
		if !strings.HasPrefix(branch, namespace) {
			continue
		}
		remainder := strings.TrimPrefix(branch, namespace)
		slash := strings.LastIndex(remainder, "/")
		if slash <= 0 || slash == len(remainder)-1 {
			return "", false
		}
		runID := remainder[slash+1:]
		return runID, validRunID(runID)
	}
	return "", false
}

func commitIsAncestor(ctx context.Context, repoDir, ancestor, descendant string) (bool, error) {
	cmd := exec.CommandContext(ctx, "git", bareRepoSafeArgs([]string{"merge-base", "--is-ancestor", ancestor, descendant})...)
	cmd.Dir = repoDir
	err := cmd.Run()
	if err == nil {
		return true, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
		return false, nil
	}
	return false, err
}
