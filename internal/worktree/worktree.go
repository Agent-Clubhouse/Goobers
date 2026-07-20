package worktree

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/goobers/goobers/internal/gooberassets"
)

// botGitUserName/botGitUserEmail are the commit identity Create sets local
// to every worktree it provisions (#237) — an agentic implementer stage
// commits inside the worktree, and that commit must not depend on the
// daemon host's own ambient git config (which V0's isolation story
// otherwise never relies on: worktrees, credential injection, and env
// allowlisting all exist precisely so a stage's behavior doesn't depend on
// host dotfiles).
const (
	botGitUserName  = "goobers-bot"
	botGitUserEmail = "goobers-bot@users.noreply.github.com"
)

// CreateOptions configures a single per-run worktree.
type CreateOptions struct {
	// RepoURL identifies the target repo; fed to Manager.WorkingCopy.
	RepoURL string
	// RunID uniquely identifies this run. It keys the worktree's path and
	// marker, so it must be unique per Manager for the lifetime of the run.
	RunID string
	// OwnerRunID identifies the workflow run that owns this stage worktree.
	// Empty defaults to RunID for direct package users.
	OwnerRunID string
	// BaseRef is the pinned ref (branch, tag, or commit sha) to branch or
	// check out from. Required.
	BaseRef string
	// Branch, if set, is the run branch this worktree checks out (e.g.
	// "goobers/<workflow>/<run-id>", providers.BranchName). It is created off
	// BaseRef the first time it is requested and checked out as-is (carrying
	// the prior stages' commits, ignoring BaseRef) every time after — this is
	// what gives a run's sequential stages continuity while keeping each stage
	// isolated in a fresh worktree (#133). If empty, the worktree is a detached
	// checkout of BaseRef.
	Branch string
	// RequireExistingBranch refuses to CREATE Branch, failing instead if it is
	// not already in the managed working copy (issue #392).
	//
	// The create-if-absent default is correct-by-construction for a run's own
	// branch: the first stage is supposed to cut it from BaseRef. It is
	// actively dangerous for a branch the caller believes already exists —
	// a rebound workspace branch naming an existing PR. There, silently
	// creating an empty branch off BaseRef hands the stage a pristine base
	// checkout that merely carries the PR's branch NAME, which downstream
	// looks exactly like "the PR legitimately contains nothing": tests pass on
	// it, and a force-push then replaces the PR's real content with base.
	//
	// The failure is realistic rather than theoretical — WorkingCopy's fetch
	// deliberately excludes the run-branch namespace from its refspec, so the
	// only reason a PR's branch is in the mirror at all is that an earlier
	// stage in this same run fetched it. Anything that clears the mirror
	// between stages reaches this path.
	RequireExistingBranch bool
}

// Worktree is a disposable, isolated working copy for one run, branched off
// a Manager's managed working copy. Obtain one via Manager.Create and release
// it via Remove.
type Worktree struct {
	// RunID is the run this worktree was created for.
	RunID string
	// Path is the worktree's filesystem location — hand this to the stage.
	Path string
	// Branch is the branch checked out in the worktree, or empty if detached.
	Branch string

	manager  *Manager
	key      string
	startRef string
}

// validRunID reports whether id is safe to join onto a directory as a
// single path segment: non-empty, not "." or "..", and not itself a
// multi-segment or absolute path (filepath.Base(id) == id is false for any
// of those) — mirrors api/v1alpha1.ValidRunID; duplicated rather than
// shared since this package has no other reason to depend on the stage
// contract package (see doc.go), the same tradeoff already accepted for
// marker.go's fsyncDir (which mirrors internal/journal's own copy).
func validRunID(id string) bool {
	return id != "" && id != "." && id != ".." && filepath.Base(id) == id
}

// Create prepares repoURL's managed working copy (cloning or fetching as
// needed) and adds a new worktree off it for opts.BaseRef, keyed by
// opts.RunID. Two calls with different RunIDs against the same repo may run
// concurrently and never observe each other's worktree contents.
func (m *Manager) Create(ctx context.Context, opts CreateOptions) (*Worktree, error) {
	if opts.RunID == "" {
		return nil, fmt.Errorf("worktree: RunID is required")
	}
	// opts.RunID is joined into this worktree's path and marker key below —
	// it must never itself be able to escape those directories (#244).
	if !validRunID(opts.RunID) {
		return nil, fmt.Errorf("worktree: RunID %q must be a single path segment (no \"..\", no \"/\")", opts.RunID)
	}
	if opts.OwnerRunID == "" {
		opts.OwnerRunID = opts.RunID
	}
	if !validRunID(opts.OwnerRunID) {
		return nil, fmt.Errorf("worktree: OwnerRunID %q must be a single path segment (no \"..\", no \"/\")", opts.OwnerRunID)
	}
	if opts.BaseRef == "" {
		return nil, fmt.Errorf("worktree: BaseRef is required")
	}

	repoDir, err := m.WorkingCopy(ctx, opts.RepoURL)
	if err != nil {
		return nil, err
	}
	key := repoKey(opts.RepoURL)

	// Worktree add mutates the repo's administrative worktree list; serialize
	// it per repo alongside clone/fetch so concurrent Creates for the same
	// repo don't race git's internal locking.
	lock := m.lockFor(key)
	lock.Lock()
	defer lock.Unlock()

	path := filepath.Join(m.runsDirForKey(key), opts.RunID)
	if _, err := os.Stat(path); err == nil {
		// Adopt-and-reset (issue #136), not a hard error: a leftover
		// worktree at this exact key can only be a previous attempt of the
		// SAME (run, stage) that never got torn down — a crash mid-attempt
		// (this key survives until the daemon resumes the same stage), or a
		// same-process retry whose own Remove call failed (RemoveOptions
		// errors were being silently discarded). Both cases are always
		// sequential with whatever is calling Create now — a genuinely
		// concurrent second attempt of the same (run, stage) never happens
		// — so it is always safe to clear it and start fresh rather than
		// refusing forever until an operator does disk surgery.
		if err := m.forceClear(ctx, key, path); err != nil {
			return nil, fmt.Errorf("worktree: clear stale worktree for run %s: %w", opts.RunID, err)
		}
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("worktree: stat %s: %w", path, err)
	}
	if err := os.MkdirAll(m.runsDirForKey(key), 0o755); err != nil {
		return nil, fmt.Errorf("worktree: create runs dir: %w", err)
	}

	// A run's stages share one branch, not one tree: the first stage creates
	// the run branch off BaseRef; every later stage checks out that same
	// branch — now carrying the prior stages' commits — in its own fresh
	// worktree. That is what makes local-ci and the reviewer gate evaluate the
	// run's actual diff rather than a pristine BaseRef (#133). A detached
	// checkout (Branch == "") keeps the pre-#133 behavior.
	args := []string{"worktree", "add"}
	switch {
	case opts.Branch == "":
		args = append(args, "--detach", path, opts.BaseRef)
	case branchExists(ctx, repoDir, opts.Branch):
		// Existing run branch: check it out as-is. BaseRef is not the
		// continuity point — the branch's own tip is. git forbids the same
		// branch in two live worktrees, which holds here because stages run
		// sequentially and each stage's worktree is removed before the next.
		args = append(args, path, opts.Branch)
	case opts.RequireExistingBranch:
		// Never silently substitute a fresh branch off BaseRef for a branch
		// the caller asserted already exists — see RequireExistingBranch.
		return nil, fmt.Errorf("worktree: branch %q does not exist in the working copy for run %s (refusing to create it)", opts.Branch, opts.RunID)
	default:
		// First stage of the run: create the run branch off BaseRef.
		args = append(args, "-b", opts.Branch, path, opts.BaseRef)
	}
	if err := runGit(ctx, repoDir, args...); err != nil {
		return nil, fmt.Errorf("worktree: create for run %s: %w", opts.RunID, err)
	}
	startRef, err := gitOutput(ctx, path, "rev-parse", "HEAD")
	if err != nil {
		cleanupErr := runGit(ctx, repoDir, "worktree", "remove", "--force", path)
		return nil, fmt.Errorf("worktree: resolve starting ref for run %s: %w", opts.RunID, errors.Join(err, cleanupErr))
	}

	// A bot identity local to THIS worktree's own .git/config (`git config`
	// with no --global, so it never touches the managed working copy or the
	// host's ambient git config) — an agentic stage's commit must not depend
	// on the daemon host happening to have user.name/user.email set (#237).
	if err := runGit(ctx, path, "config", "user.name", botGitUserName); err != nil {
		return nil, fmt.Errorf("worktree: set bot identity for run %s: %w", opts.RunID, err)
	}
	if err := runGit(ctx, path, "config", "user.email", botGitUserEmail); err != nil {
		return nil, fmt.Errorf("worktree: set bot identity for run %s: %w", opts.RunID, err)
	}

	mk := marker{
		RunID:      opts.RunID,
		OwnerRunID: opts.OwnerRunID,
		Branch:     opts.Branch,
		StartRef:   startRef,
		PID:        os.Getpid(),
		CreatedAt:  time.Now(),
		Status:     statusActive,
	}
	if err := writeMarker(m.markerPath(key, opts.RunID), mk); err != nil {
		// Without a marker, Reap can never distinguish this worktree from an
		// orphan, so don't leave it behind half-registered.
		_ = runGit(ctx, repoDir, "worktree", "remove", "--force", path)
		return nil, fmt.Errorf("worktree: register run %s: %w", opts.RunID, err)
	}

	return &Worktree{
		RunID: opts.RunID, Path: path, Branch: opts.Branch,
		manager: m, key: key, startRef: startRef,
	}, nil
}

// ActivateAssetPathGuard persists that this invocation reserves the asset
// workspace, allowing crash recovery to distinguish it from a stage for which
// the same path is ordinary repository content.
func (wt *Worktree) ActivateAssetPathGuard() error {
	markerPath := wt.manager.markerPath(wt.key, wt.RunID)
	mk, err := readMarker(markerPath)
	if err != nil {
		return fmt.Errorf("worktree: read marker for run %s: %w", wt.RunID, err)
	}
	mk.AssetPathGuard = true
	if err := writeMarker(markerPath, mk); err != nil {
		return fmt.Errorf("worktree: activate asset path guard for run %s: %w", wt.RunID, err)
	}
	return nil
}

// ValidateReservedPaths rejects a stage that forced the materialized asset
// directory into the index or any commit it added, rewinding those commits so
// the reserved content cannot cross the shared run-branch boundary.
func (wt *Worktree) ValidateReservedPaths(ctx context.Context) error {
	collision := fmt.Errorf("%w: %s must not be tracked on the run branch", gooberassets.ErrWorkspaceCollision, gooberassets.WorkspaceDir)
	branchRef, branchCommitted, err := wt.inspectReservedBranch(ctx)
	if err != nil {
		return err
	}
	if branchCommitted {
		if branchRef != wt.startRef {
			if rollbackErr := wt.rollbackBranch(ctx, branchRef); rollbackErr != nil {
				return fmt.Errorf("worktree: remove reserved asset path from run %s: %w", wt.RunID, errors.Join(collision, rollbackErr))
			}
		}
		return collision
	}

	indexed, err := gitOutput(ctx, wt.Path, "ls-files", "--cached", "--", gooberassets.WorkspaceDir)
	if err != nil {
		return fmt.Errorf("worktree: inspect indexed asset path for run %s: %w", wt.RunID, err)
	}
	headCommitted, err := wt.reservedPathCommits(ctx, wt.Path, "HEAD")
	if err != nil {
		return err
	}
	if indexed == "" && !headCommitted {
		return nil
	}
	return collision
}

func (wt *Worktree) reservedPathCommits(ctx context.Context, dir, endRef string) (bool, error) {
	committed, err := gitOutput(ctx, dir, "log", "--full-history", "--format=%H", wt.startRef+".."+endRef, "--", gooberassets.WorkspaceDir)
	if err != nil {
		return false, fmt.Errorf("worktree: inspect committed asset path for run %s: %w", wt.RunID, err)
	}
	return committed != "", nil
}

func (wt *Worktree) inspectReservedBranch(ctx context.Context) (string, bool, error) {
	if wt.Branch == "" {
		return "", false, nil
	}
	repoDir := wt.manager.repoDirForKey(wt.key)
	refName := "refs/heads/" + wt.Branch
	currentRef, err := gitOutput(ctx, repoDir, "rev-parse", "--verify", refName)
	if err != nil {
		return "", false, fmt.Errorf("worktree: resolve run branch %q for run %s: %w", wt.Branch, wt.RunID, err)
	}
	committed, err := wt.reservedPathCommits(ctx, repoDir, refName)
	if err != nil {
		return "", false, err
	}
	return currentRef, committed, nil
}

func (wt *Worktree) rollbackBranch(ctx context.Context, currentRef string) error {
	return runGit(
		ctx,
		wt.manager.repoDirForKey(wt.key),
		"update-ref",
		"refs/heads/"+wt.Branch,
		wt.startRef,
		currentRef,
	)
}

func (wt *Worktree) restoreReservedBranch(ctx context.Context) error {
	currentRef, committed, err := wt.inspectReservedBranch(ctx)
	if err != nil {
		return err
	}
	if !committed || currentRef == wt.startRef {
		return nil
	}
	if err := wt.rollbackBranch(ctx, currentRef); err != nil {
		return fmt.Errorf("worktree: restore run branch after reserved asset commit for run %s: %w", wt.RunID, err)
	}
	return nil
}

func (m *Manager) restoreReservedBranchFromMarker(ctx context.Context, key, path string, mk marker) error {
	if !mk.AssetPathGuard || mk.Branch == "" || mk.StartRef == "" {
		return nil
	}
	wt := &Worktree{
		RunID:    mk.RunID,
		Path:     path,
		Branch:   mk.Branch,
		manager:  m,
		key:      key,
		startRef: mk.StartRef,
	}
	return wt.restoreReservedBranch(ctx)
}

// Diff returns the unified diff of this worktree's branch against baseRef
// (`git diff baseRef...HEAD`) — the cumulative change the run's stages have
// committed on top of the base, computed from the actual commits rather than
// self-reported by any stage. Used to produce a deterministic, digested
// evidence artifact for the reviewer gate (#301). Raw bytes (not trimmed) so
// the artifact digest is a faithful hash of the diff. An empty result (no
// committed changes vs. base) returns an empty slice, no error.
func (wt *Worktree) Diff(ctx context.Context, baseRef string) ([]byte, error) {
	if baseRef == "" {
		return nil, fmt.Errorf("worktree: Diff requires a baseRef")
	}
	cmd := exec.CommandContext(ctx, "git", bareRepoSafeArgs([]string{"diff", baseRef + "...HEAD"})...)
	cmd.Dir = wt.Path
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("worktree: git diff %s...HEAD for run %s: %w", baseRef, wt.RunID, err)
	}
	return out, nil
}

// forceClear tears down whatever is left at path from a previous, never-torn-
// down attempt at this same worktree key (issue #136's adopt-and-reset),
// so Create can proceed as if the key were fresh. Tries git's own worktree
// removal first (the common case — git still has it registered); if git
// doesn't know about it (e.g. the crash happened between `worktree add` and
// this process ever registering it, or a prior force-remove already pruned
// git's record but left the directory), falls back to removing the
// directory directly and pruning git's administrative state. The marker is
// cleared too — Create writes a fresh one immediately after.
func (m *Manager) forceClear(ctx context.Context, key, path string) error {
	repoDir := m.repoDirForKey(key)
	runID := filepath.Base(path)
	markerPath := m.markerPath(key, runID)
	mk, markerErr := readMarker(markerPath)
	switch {
	case markerErr == nil:
		if err := m.restoreReservedBranchFromMarker(ctx, key, path, mk); err != nil {
			return fmt.Errorf("restore guarded branch for stale worktree: %w", err)
		}
	case !os.IsNotExist(markerErr):
		return fmt.Errorf("read stale marker: %w", markerErr)
	}
	if err := runGit(ctx, repoDir, "worktree", "remove", "--force", path); err != nil {
		if err := os.RemoveAll(path); err != nil {
			return fmt.Errorf("remove stale worktree directory: %w", err)
		}
		if err := runGit(ctx, repoDir, "worktree", "prune"); err != nil {
			return fmt.Errorf("prune stale worktree registration: %w", err)
		}
	}
	if err := os.Remove(markerPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove stale marker: %w", err)
	}
	return nil
}

// RemoveOptions configures worktree teardown.
type RemoveOptions struct {
	// Keep leaves the worktree on disk for debugging instead of removing it
	// (the run's declared keep-on-failure policy). A kept worktree is only
	// swept up later by Reap, once it ages past ReapOptions.StaleAfter.
	Keep bool
}

// Remove tears down the worktree: by default it removes the worktree from
// disk and unregisters it; with RemoveOptions.Keep it leaves the worktree in
// place and marks it kept, so Reap does not treat it as a crash orphan.
func (wt *Worktree) Remove(ctx context.Context, opts RemoveOptions) error {
	repoDir := wt.manager.repoDirForKey(wt.key)
	markerPath := wt.manager.markerPath(wt.key, wt.RunID)

	lock := wt.manager.lockFor(wt.key)
	lock.Lock()
	defer lock.Unlock()

	mk, markerErr := readMarker(markerPath)
	switch {
	case markerErr == nil:
		if err := wt.manager.restoreReservedBranchFromMarker(ctx, wt.key, wt.Path, mk); err != nil {
			return fmt.Errorf("worktree: restore guarded branch for run %s: %w", wt.RunID, err)
		}
	case !os.IsNotExist(markerErr):
		return fmt.Errorf("worktree: read marker for run %s: %w", wt.RunID, markerErr)
	}

	if opts.Keep {
		if markerErr != nil {
			return fmt.Errorf("worktree: read marker for run %s: %w", wt.RunID, markerErr)
		}
		mk.Status = statusKept
		if err := writeMarker(markerPath, mk); err != nil {
			return fmt.Errorf("worktree: mark run %s kept: %w", wt.RunID, err)
		}
		return nil
	}

	if err := runGit(ctx, repoDir, "worktree", "remove", "--force", wt.Path); err != nil {
		return fmt.Errorf("worktree: remove for run %s: %w", wt.RunID, err)
	}
	if err := os.Remove(markerPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("worktree: unregister run %s: %w", wt.RunID, err)
	}
	return nil
}
