package worktree

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
)

// Manager owns managed working copies under Root — one mirror clone per
// distinct repo URL — and hands out per-run worktrees branched off them. The
// zero value is not usable; construct with NewManager.
type Manager struct {
	// Root is the workcopies directory (ARCHITECTURE.md §6:
	// <instance-root>/workcopies).
	Root string

	mu        sync.Mutex // guards repoLocks
	repoLocks map[string]*sync.Mutex
}

// NewManager returns a Manager rooted at root, creating the directory if it
// does not already exist.
func NewManager(root string) (*Manager, error) {
	if root == "" {
		return nil, fmt.Errorf("worktree: root must not be empty")
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, fmt.Errorf("worktree: create root %s: %w", root, err)
	}
	return &Manager{Root: root, repoLocks: make(map[string]*sync.Mutex)}, nil
}

// repoKey derives a stable, filesystem-safe directory name for a repo URL so
// two managers (or two runs) referring to the same repo always land on the
// same managed working copy.
func repoKey(repoURL string) string {
	sum := sha256.Sum256([]byte(repoURL))
	return hex.EncodeToString(sum[:])[:16]
}

func (m *Manager) repoDirForKey(key string) string {
	return filepath.Join(m.Root, key, "repo.git")
}

func (m *Manager) runsDirForKey(key string) string {
	return filepath.Join(m.Root, key, "runs")
}

func (m *Manager) markersDirForKey(key string) string {
	return filepath.Join(m.Root, key, "markers")
}

func (m *Manager) markerPath(key, runID string) string {
	return filepath.Join(m.markersDirForKey(key), runID+".json")
}

// lockFor returns the per-repo mutex used to serialize clone/fetch and
// worktree-add for a given repo, creating it on first use.
func (m *Manager) lockFor(key string) *sync.Mutex {
	m.mu.Lock()
	defer m.mu.Unlock()
	l, ok := m.repoLocks[key]
	if !ok {
		l = &sync.Mutex{}
		m.repoLocks[key] = l
	}
	return l
}

// runBranchNamespace is the refs/heads/ prefix under which a run's own branch
// lives — providers.BranchName produces "goobers/<workflow>/<run-id>". These
// branches exist only in the managed clone (a run commits to them locally;
// they are never on origin), so WorkingCopy's mirror prune must exclude this
// namespace or it would delete a run's branch between the run's stages and
// silently break run-branch continuity (#133). Kept in sync with
// providers.BranchName's prefix by convention rather than an import, to avoid
// a worktree -> providers dependency for one string.
const runBranchNamespace = "goobers/"

// WorkingCopy ensures a managed mirror clone of repoURL exists and is up to
// date under Root, cloning on first use and fetching thereafter. A mirror
// clone has no working tree of its own — worktrees created via Create are the
// only mutable views onto it — and its fetch refspec covers every ref, so a
// pinned base ref (branch, tag, or sha) reachable on the remote is always
// available to branch a worktree from after WorkingCopy returns. The one
// exception is the run-branch namespace (runBranchNamespace), which the fetch
// deliberately excludes from its prune so a run's local-only branch survives
// across the run's stages (#133).
//
// Concurrent calls for the same repo URL serialize on the clone/fetch step;
// calls for different repos proceed independently.
func (m *Manager) WorkingCopy(ctx context.Context, repoURL string) (string, error) {
	key := repoKey(repoURL)
	lock := m.lockFor(key)
	lock.Lock()
	defer lock.Unlock()

	dir := m.repoDirForKey(key)
	switch _, err := os.Stat(dir); {
	case os.IsNotExist(err):
		if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
			return "", fmt.Errorf("worktree: create workcopy parent for %s: %w", repoURL, err)
		}
		if err := runGit(ctx, "", "clone", "--mirror", repoURL, dir); err != nil {
			_ = os.RemoveAll(dir) // don't leave a partial clone masquerading as a valid one
			return "", fmt.Errorf("worktree: clone %s: %w", repoURL, err)
		}
		if err := ensureScratchExcluded(ctx, dir); err != nil {
			return "", err
		}
		return dir, nil
	case err != nil:
		return "", fmt.Errorf("worktree: stat workcopy for %s: %w", repoURL, err)
	}

	// Refresh origin and prune refs it deleted, but exclude the run-branch
	// namespace: those branches live only here, never on origin, so a plain
	// mirror prune (+refs/*:refs/*) would delete a run's branch mid-run and
	// silently revert its stages to a pristine base (#133). The explicit
	// refspec restates the mirror's default and appends the exclusion.
	if err := runGit(ctx, dir, "fetch", "--prune", "origin",
		"+refs/*:refs/*", "^refs/heads/"+runBranchNamespace+"*"); err != nil {
		return "", fmt.Errorf("worktree: fetch %s: %w", repoURL, err)
	}
	// A pre-existing mirror (cloned before #240) also needs the scratch exclude;
	// it is idempotent, so refreshing it on every WorkingCopy is safe.
	if err := ensureScratchExcluded(ctx, dir); err != nil {
		return "", err
	}
	return dir, nil
}

// scratchExcludePattern is the harness scratch dir (internal/harness writes
// <workspace>/.goobers/{prompt.md,result.json,verdict.json,context/}) that must
// never be committed into a run's PR (#240).
const scratchExcludePattern = ".goobers/"

// ensureScratchExcluded makes the harness scratch dir invisible to git in every
// worktree branched from this managed mirror, so the common `git add -A` agent
// commit pattern never captures harness debris into the run's PR (#240). It
// appends the pattern to the mirror's shared info/exclude — a common-dir file
// (verified: it applies to every linked worktree, not just the mirror) — so the
// exclusion is per-mirror and local, never a repo-visible change, and works for
// any target repo regardless of its own .gitignore. Idempotent.
func ensureScratchExcluded(ctx context.Context, dir string) error {
	// `git rev-parse --git-path info/exclude` resolves the exclude file for both
	// the bare mirror used here and any future non-bare layout; the path is
	// returned relative to dir.
	rel, err := gitOutput(ctx, dir, "rev-parse", "--git-path", "info/exclude")
	if err != nil {
		return fmt.Errorf("worktree: resolve info/exclude in %s: %w", dir, err)
	}
	excludePath := rel
	if !filepath.IsAbs(excludePath) {
		excludePath = filepath.Join(dir, excludePath)
	}
	existing, err := os.ReadFile(excludePath)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("worktree: read info/exclude: %w", err)
	}
	for _, line := range strings.Split(string(existing), "\n") {
		if t := strings.TrimSpace(line); t == scratchExcludePattern || t == ".goobers" {
			return nil // already excluded
		}
	}
	if err := os.MkdirAll(filepath.Dir(excludePath), 0o755); err != nil {
		return fmt.Errorf("worktree: create info dir: %w", err)
	}
	buf := existing
	if len(buf) > 0 && buf[len(buf)-1] != '\n' {
		buf = append(buf, '\n')
	}
	buf = append(buf, []byte(scratchExcludePattern+"\n")...)
	if err := os.WriteFile(excludePath, buf, 0o644); err != nil {
		return fmt.Errorf("worktree: write info/exclude: %w", err)
	}
	return nil
}

// gitOutput runs git in dir and returns its trimmed stdout.
func gitOutput(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git %v: %w", args, err)
	}
	return strings.TrimSpace(string(out)), nil
}

// runGit runs git with args, using dir as the working directory (the process
// default if dir is empty), and returns combined output on failure for
// debuggability.
func runGit(ctx context.Context, dir string, args ...string) error {
	cmd := exec.CommandContext(ctx, "git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("git %v: %w: %s", args, err, out)
	}
	return nil
}

// branchExists reports whether a local branch of the given name exists in the
// repo at repoDir. `show-ref --verify --quiet` exits 0 iff the ref exists and
// prints nothing, so non-existence is an ordinary false, not an error — this
// is a boolean probe, distinct from runGit's must-succeed contract. Used by
// Create to decide whether to create the run branch or check out the existing
// one (#133).
func branchExists(ctx context.Context, repoDir, branch string) bool {
	cmd := exec.CommandContext(ctx, "git", "show-ref", "--verify", "--quiet", "refs/heads/"+branch)
	cmd.Dir = repoDir
	return cmd.Run() == nil
}
