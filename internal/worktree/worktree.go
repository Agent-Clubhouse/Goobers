package worktree

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/goobers/goobers/internal/gooberassets"
	"github.com/goobers/goobers/internal/mergeresolve"
)

// BotGitUserName/BotGitUserEmail are the commit identity Create sets local
// to every worktree it provisions (#237) — an agentic implementer stage
// commits inside the worktree, and that commit must not depend on the
// daemon host's own ambient git config (which V0's isolation story
// otherwise never relies on: worktrees, credential injection, and env
// allowlisting all exist precisely so a stage's behavior doesn't depend on
// host dotfiles).
//
// EXPORTED because a stage pod's clone is the third substrate that needs it
// (#392): the in-pod syncBase merge writes a merge commit, and a second
// spelling of "who commits for Goobers" is exactly the drift this repository
// keeps paying for.
const (
	BotGitUserName           = "goobers-bot"
	BotGitUserEmail          = "goobers-bot@users.noreply.github.com"
	botIdentityRetryAttempts = 4
	botIdentityRetryBackoff  = 50 * time.Millisecond
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
	// the prior stages' commits) every time after unless SyncBase is set. This
	// gives a run's sequential stages continuity while keeping each stage
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
	// AcquireRemoteBranch fetches Branch explicitly from origin once per
	// OwnerRunID before requiring it. A durable metadata marker makes a retry or
	// process restart reuse the same logical branch without resetting commits
	// made by earlier stages in the run.
	AcquireRemoteBranch bool
	// SyncBase merges the freshly fetched BaseRef into an existing Branch
	// before returning the worktree. New branches already start at BaseRef.
	SyncBase bool
	// Sparse declares repo-relative path cones (project.checkout.sparse,
	// #649): when non-empty, Create materializes a cone-mode sparse checkout
	// containing only these cones plus root-level files, instead of the full
	// tree. Empty (the default) is a full checkout — byte-identical to Create
	// without this field.
	Sparse []string
}

// BaseSyncConflictError identifies a genuine content conflict while merging a
// freshly fetched base into an existing run branch.
type BaseSyncConflictError struct {
	Branch           string
	BaseRef          string
	ConflictingFiles []string
	cause            error
}

func (e *BaseSyncConflictError) Error() string {
	return fmt.Sprintf("worktree: sync branch %q with base %q: merge conflict in %s",
		e.Branch, e.BaseRef, strings.Join(e.ConflictingFiles, ", "))
}

func (e *BaseSyncConflictError) Unwrap() error {
	return e.cause
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
	// Warnings are non-fatal conditions detected while provisioning this
	// worktree that the caller should surface (never a reason to fail the run).
	// Today this is symlink flattening on a platform without symlink support
	// (see Manager.checkSymlinkSupport, #643); empty on darwin/linux. The
	// runner journals any entries as a runner.annotation event.
	Warnings []string
	// PinnedWorkspaceCreated reports whether PreparePinned/AcquirePinned
	// materialized the stable workspace during this call (a fresh clone) as
	// opposed to reusing an existing one. Always false for disposable
	// worktrees. Callers (e.g. the large-repo benchmark harness) use this to
	// distinguish cold-start from warm-reuse timing.
	PinnedWorkspaceCreated bool

	manager  *Manager
	key      string
	startRef string
	repoURL  string
	// partialMirror records that the backing mirror is a blobless promisor
	// clone (#646): any later operation in this worktree that materializes
	// blobs (Diff against a base whose blobs were never checked out) is a
	// remote operation needing the credential environment and transient-
	// failure classification, exactly like Create's own checkout.
	partialMirror bool
	pinned        bool
	repoDir       string
	assetGuard    bool
}

// HeadSHA returns the commit currently checked out in this worktree.
func (wt *Worktree) HeadSHA(ctx context.Context) (string, error) {
	return gitOutput(ctx, wt.Path, "rev-parse", "HEAD")
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
func (m *Manager) Create(ctx context.Context, opts CreateOptions) (_ *Worktree, retErr error) {
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
	if opts.SyncBase && opts.Branch == "" {
		return nil, fmt.Errorf("worktree: SyncBase requires Branch")
	}
	if opts.AcquireRemoteBranch && !opts.RequireExistingBranch {
		return nil, fmt.Errorf("worktree: AcquireRemoteBranch requires RequireExistingBranch")
	}

	repoDir, err := m.WorkingCopy(ctx, opts.RepoURL)
	if err != nil {
		return nil, err
	}
	key := repoKey(opts.RepoURL)
	directory := worktreeDirectoryName(opts.RunID)
	path := filepath.Join(m.runsDirForKey(key), directory)

	// Worktree add mutates the repo's administrative worktree list; serialize
	// it per repo alongside clone/fetch so concurrent Creates for the same
	// repo don't race git's internal locking.
	lock := m.lockFor(key)
	lock.Lock()
	lockHeld := true
	defer func() {
		if lockHeld {
			lock.Unlock()
		}
	}()

	if opts.AcquireRemoteBranch {
		if err := m.acquireRemoteBranchLocked(ctx, key, opts.RepoURL, repoDir, opts.OwnerRunID, opts.Branch); err != nil {
			return nil, err
		}
	}

	existingBranch := opts.Branch != "" && branchExists(ctx, repoDir, opts.Branch)
	if limit, ok := m.pathLengthLimit(opts.RepoURL); ok {
		refs := []string{opts.BaseRef}
		if existingBranch {
			refs[0] = opts.Branch
			if opts.SyncBase {
				refs = append(refs, opts.BaseRef)
			}
		}
		for _, ref := range refs {
			if err := preflightPathLength(ctx, repoDir, ref, path, limit); err != nil {
				return nil, err
			}
		}
	}

	if _, err := os.Stat(path); err == nil {
		// Adopt-and-reset (issue #136), not a hard error: a leftover
		// worktree at this exact key is a previous attempt of the SAME
		// (run, stage) that never got torn down. This is safe only within
		// one manager ownership domain; worker startup enforces a pod-private
		// root before distributed attempts can reach this path.
		if err := m.forceClear(ctx, key, path, opts.RunID); err != nil {
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
	sparse := len(opts.Sparse) > 0
	args := []string{"worktree", "add"}
	if sparse {
		// Skip materializing the full tree here; sparse-checkout is configured
		// below, before the explicit checkout that actually populates the
		// working directory, so only the declared cones are ever written to
		// disk (#649).
		args = append(args, "--no-checkout")
	}
	checkoutTarget := opts.BaseRef
	switch {
	case opts.Branch == "":
		args = append(args, "--detach", path, opts.BaseRef)
	case existingBranch:
		// Existing run branch: check it out as-is. BaseRef is not the
		// continuity point — the branch's own tip is. git forbids the same
		// branch in two live worktrees, which holds here because stages run
		// sequentially and each stage's worktree is removed before the next.
		args = append(args, path, opts.Branch)
		checkoutTarget = opts.Branch
	case opts.RequireExistingBranch:
		// Never silently substitute a fresh branch off BaseRef for a branch
		// the caller asserted already exists — see RequireExistingBranch.
		return nil, fmt.Errorf("worktree: branch %q does not exist in the working copy for run %s (refusing to create it)", opts.Branch, opts.RunID)
	default:
		// First stage of the run: create the run branch off BaseRef.
		// Run continuity comes from the local branch tip, so avoid creating
		// persistent tracking config that branch retention cannot reap.
		args = append(args, "--no-track", "-b", opts.Branch, path, opts.BaseRef)
		checkoutTarget = opts.Branch
	}

	pid := os.Getpid()
	startedAt, _ := processStartTime(pid) // best-effort; zero disables the PID-reuse check for this marker
	mk := marker{
		RunID:        opts.RunID,
		OwnerRunID:   opts.OwnerRunID,
		Directory:    directory,
		Branch:       opts.Branch,
		Writer:       m.writerIdentity,
		PID:          pid,
		PIDStartedAt: startedAt,
		CreatedAt:    time.Now(),
		Status:       statusActive,
	}
	// Persist ownership before git creates the directory so a crash during
	// worktree add never leaves an opaque hash that cleanup cannot resolve.
	ownershipPath := m.ownershipPath(key, directory)
	if err := writeMarker(ownershipPath, mk); err != nil {
		return nil, fmt.Errorf("worktree: persist ownership for run %s: %w", opts.RunID, err)
	}
	if err := writeMarker(m.markerPath(key, opts.RunID), mk); err != nil {
		_ = os.Remove(ownershipPath)
		return nil, fmt.Errorf("worktree: register run %s: %w", opts.RunID, err)
	}
	defer func() {
		if retErr == nil {
			return
		}
		if err := m.forceClear(context.WithoutCancel(ctx), key, path, opts.RunID); err != nil {
			retErr = errors.Join(retErr, fmt.Errorf("worktree: clean up failed create for run %s: %w", opts.RunID, err))
		}
	}()

	partialMirror := m.partialClone && mirrorIsPartial(ctx, repoDir)
	if partialMirror {
		// Materializing a tree from a blobless mirror fetches missing blobs
		// from the promisor remote mid-checkout (#646), so this one nominally
		// local operation is a remote one: it needs the same credential
		// environment as clone/fetch, and its failure classifies through
		// IsTransientProvisionError's promisor fragments.
		if err := m.runRemoteGit(ctx, opts.RepoURL, repoDir, args...); err != nil {
			return nil, fmt.Errorf("worktree: create for run %s: %w", opts.RunID, err)
		}
	} else if err := runGit(ctx, repoDir, args...); err != nil {
		return nil, fmt.Errorf("worktree: create for run %s: %w", opts.RunID, err)
	}
	startRef, err := gitOutput(ctx, path, "rev-parse", "HEAD")
	if err != nil {
		return nil, fmt.Errorf("worktree: resolve starting ref for run %s: %w", opts.RunID, err)
	}

	// A bot identity local to THIS worktree's own .git/config (`git config`
	// with no --global, so it never touches the managed working copy or the
	// host's ambient git config) — an agentic stage's commit must not depend
	// on the daemon host happening to have user.name/user.email set (#237).
	if err := retryBotIdentityConfig(ctx, func() error {
		return runGit(ctx, path, "config", "user.name", BotGitUserName)
	}); err != nil {
		return nil, fmt.Errorf("worktree: set bot identity for run %s: %w", opts.RunID, err)
	}
	if err := retryBotIdentityConfig(ctx, func() error {
		return runGit(ctx, path, "config", "user.email", BotGitUserEmail)
	}); err != nil {
		return nil, fmt.Errorf("worktree: set bot identity for run %s: %w", opts.RunID, err)
	}
	if sparse {
		// Cone mode only, per the design (path-list "legacy" sparse-checkout
		// patterns are out of scope, #649): a plain, fast set of directory
		// prefixes rather than full gitignore-style pattern matching.
		setArgs := append([]string{"sparse-checkout", "set", "--cone"}, opts.Sparse...)
		if err := runGit(ctx, path, setArgs...); err != nil {
			return nil, fmt.Errorf("worktree: configure sparse checkout for run %s: %w", opts.RunID, err)
		}
		// The actual materialization: --no-checkout above left the working
		// directory empty, so this checkout is what populates it — and, with
		// sparse-checkout already configured, populates only the declared
		// cones plus root-level files instead of the full tree.
		checkoutArgs := []string{"checkout", checkoutTarget}
		if partialMirror {
			if err := m.runRemoteGit(ctx, opts.RepoURL, path, checkoutArgs...); err != nil {
				return nil, fmt.Errorf("worktree: materialize sparse checkout for run %s: %w", opts.RunID, err)
			}
		} else if err := runGit(ctx, path, checkoutArgs...); err != nil {
			return nil, fmt.Errorf("worktree: materialize sparse checkout for run %s: %w", opts.RunID, err)
		}
	}
	if opts.SyncBase && existingBranch {
		mergeArgs := []string{"merge", "--ff", "--no-edit", opts.BaseRef}
		var mergeErr error
		if partialMirror {
			// Merging a base that advanced since the branch was cut
			// materializes base-side blobs the narrowed refresh fetch withheld
			// (it brings commits and trees only), so on a blobless mirror the
			// merge is a remote operation too: it must carry the same
			// credential environment as the checkout above or a private
			// repo's promisor fetch fails on auth.
			mergeErr = m.runRemoteGit(ctx, opts.RepoURL, path, mergeArgs...)
		} else {
			mergeErr = runGit(ctx, path, mergeArgs...)
		}
		if mergeErr != nil {
			resolved, resolveErr := resolveBaseSyncConflict(ctx, path)
			if !resolved {
				if resolveErr != nil {
					mergeErr = errors.Join(mergeErr, resolveErr)
				}
				conflictingFiles, inspectErr := mergeConflictFiles(ctx, path)
				return nil, baseSyncFailure(opts, mergeErr, conflictingFiles, inspectErr, nil)
			}
		}
	}

	// Detect symlinks the platform could not materialize (Windows without
	// symlink support checks them out as plain files) so the degradation
	// surfaces as a warning rather than corrupting the run silently (#643). A
	// no-op on darwin/linux, where symlinks check out natively.
	warnings, err := m.checkSymlinkSupport(ctx, path)
	if err != nil {
		return nil, fmt.Errorf("worktree: inspect symlinks for run %s: %w", opts.RunID, err)
	}

	mk.StartRef = startRef
	if err := writeMarker(m.markerPath(key, opts.RunID), mk); err != nil {
		return nil, fmt.Errorf("worktree: register run %s: %w", opts.RunID, err)
	}

	worktreeBytes, worktreeMeasured, measurementErr := m.measureWorktree(path)
	if worktreeMeasured {
		mk.SizeBytes = &worktreeBytes
		if err := writeMarker(m.markerPath(key, opts.RunID), mk); err != nil {
			measurementErr = errors.Join(measurementErr, fmt.Errorf("worktree: persist usage for run %s: %w", opts.RunID, err))
		}
	}
	wt := &Worktree{
		RunID: opts.RunID, Path: path, Branch: opts.Branch, Warnings: warnings,
		manager: m, key: key, startRef: startRef,
		repoURL: opts.RepoURL, partialMirror: partialMirror,
	}
	lock.Unlock()
	lockHeld = false
	m.observeUsage(ctx, UsageOperationCreate, opts.OwnerRunID, opts.RunID, worktreeBytes, worktreeMeasured, measurementErr)
	return wt, nil
}

func retryBotIdentityConfig(ctx context.Context, op func() error) error {
	var err error
	for attempt := 0; attempt < botIdentityRetryAttempts; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return errors.Join(err, ctx.Err())
			case <-time.After(botIdentityRetryBackoff):
			}
		}
		if err = op(); err == nil || !isGitConfigLockContention(err) {
			return err
		}
	}
	return err
}

func isGitConfigLockContention(err error) bool {
	var gitErr *gitCommandError
	if !errors.As(err, &gitErr) {
		return false
	}
	message := strings.ToLower(string(gitErr.output))
	return strings.Contains(message, "could not lock config file") &&
		strings.Contains(message, "file exists")
}

func preflightPathLength(ctx context.Context, repoDir, ref, checkoutPath string, limit PathLengthLimit) error {
	out, err := rawGitOutput(ctx, repoDir, nil, "ls-tree", "-r", "-z", "--name-only", ref)
	if err != nil {
		return fmt.Errorf("worktree: path-length preflight list tracked paths at %q: %w", ref, err)
	}
	var deepest string
	for _, entry := range strings.Split(string(out), "\x00") {
		if len(filepath.FromSlash(entry)) > len(filepath.FromSlash(deepest)) {
			deepest = entry
		}
	}
	if deepest == "" {
		return nil
	}
	available := limit.MaxPathLength - len(checkoutPath) - 1 - limit.BuildOutputAllowance
	required := len(filepath.FromSlash(deepest))
	if required <= available {
		return nil
	}
	return fmt.Errorf(
		"worktree: path-length preflight refused checkout: tracked path %q requires %d characters but only %d are available (maximum %d, checkout prefix %d, build-output allowance %d); shorten the instance root, raise repos[].pathLength.maxPathLength, reduce the allowance, or set repos[].pathLength.disabled: true",
		deepest, required, available, limit.MaxPathLength, len(checkoutPath)+1, limit.BuildOutputAllowance,
	)
}

// resolveBaseSyncConflict attempts the provably safe mechanical resolution of
// a conflicted base merge: two concurrent implementations that each inserted a
// distinct entry into the same line-oriented list (the shared manifest script
// line of #3096) conflict mechanically, not substantively, so failing the
// stage there spends a repass budget on a diff no agent can improve. Anything
// the shared resolver cannot resolve provably safely stays a conflict.
func resolveBaseSyncConflict(ctx context.Context, dir string) (bool, error) {
	git := func(args ...string) ([]byte, error) {
		return rawGitOutput(ctx, dir, nil, args...)
	}
	status, err := mergeresolve.ResolveAdjacentLineConflicts(dir, git)
	if err != nil || status != mergeresolve.StatusResolved {
		return false, err
	}
	if err := runGit(ctx, dir, "commit", "--no-edit"); err != nil {
		return false, fmt.Errorf("commit mechanically resolved base merge: %w", err)
	}
	return true, nil
}

func mergeConflictFiles(ctx context.Context, path string) ([]string, error) {
	out, err := gitOutput(ctx, path, "diff", "--name-only", "--diff-filter=U", "-z")
	if err != nil {
		return nil, err
	}
	var files []string
	for _, file := range strings.Split(out, "\x00") {
		if file != "" {
			files = append(files, file)
		}
	}
	return files, nil
}

func baseSyncFailure(opts CreateOptions, mergeErr error, conflictingFiles []string, inspectErr, cleanupErr error) error {
	if cleanupErr != nil {
		return fmt.Errorf("worktree: sync branch %q with base %q for run %s and clean up conflicted worktree: %w",
			opts.Branch, opts.BaseRef, opts.RunID, errors.Join(mergeErr, inspectErr, cleanupErr))
	}
	if inspectErr == nil && len(conflictingFiles) > 0 {
		return &BaseSyncConflictError{
			Branch:           opts.Branch,
			BaseRef:          opts.BaseRef,
			ConflictingFiles: conflictingFiles,
			cause:            mergeErr,
		}
	}
	return fmt.Errorf("worktree: sync branch %q with base %q for run %s: %w",
		opts.Branch, opts.BaseRef, opts.RunID, errors.Join(mergeErr, inspectErr))
}

// ActivateAssetPathGuard persists that this invocation reserves the asset
// workspace, allowing crash recovery to distinguish it from a stage for which
// the same path is ordinary repository content.
func (wt *Worktree) ActivateAssetPathGuard() error {
	if wt.pinned {
		wt.assetGuard = true
		return nil
	}
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
	if wt.pinned && !wt.assetGuard {
		return nil
	}
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
	repoDir := wt.backingRepoDir()
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
		wt.backingRepoDir(),
		"update-ref",
		"refs/heads/"+wt.Branch,
		wt.startRef,
		currentRef,
	)
}

func (wt *Worktree) backingRepoDir() string {
	if wt.repoDir != "" {
		return wt.repoDir
	}
	return wt.manager.repoDirForKey(wt.key)
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
	if wt.pinned {
		baseRef = pinnedBaseRef(ctx, wt.Path, baseRef)
	}
	args := []string{"diff", baseRef + "...HEAD"}
	var out []byte
	var err error
	if wt.partialMirror {
		// On a blobless mirror the merge-base side of the diff can name blobs
		// no checkout ever materialized (a rebound PR branch's checkout brings
		// only its own tip), so the diff spawns a promisor blob fetch: it
		// needs the credential environment, and its failure must classify
		// through IsTransientProvisionError like every other promisor fetch.
		out, err = wt.manager.remoteGitOutput(ctx, wt.repoURL, wt.Path, args...)
	} else {
		out, err = rawGitOutput(ctx, wt.Path, nil, args...)
	}
	if err != nil {
		return nil, fmt.Errorf("worktree: git diff %s...HEAD for run %s: %w", baseRef, wt.RunID, err)
	}
	return out, nil
}

// HasCommitsAheadOf reports whether HEAD contains commits not reachable from
// baseRef.
func (wt *Worktree) HasCommitsAheadOf(ctx context.Context, baseRef string) (bool, error) {
	if baseRef == "" {
		return false, fmt.Errorf("worktree: HasCommitsAheadOf requires a baseRef")
	}
	if wt.pinned {
		baseRef = pinnedBaseRef(ctx, wt.Path, baseRef)
	}
	commit, err := gitOutput(ctx, wt.Path, "rev-list", "--max-count=1", baseRef+"..HEAD")
	if err != nil {
		return false, fmt.Errorf("worktree: inspect commits ahead of %s for run %s: %w", baseRef, wt.RunID, err)
	}
	return commit != "", nil
}

// HasNewCommits reports whether this stage attempt committed work after the
// HEAD at which its worktree was created.
func (wt *Worktree) HasNewCommits(ctx context.Context) (bool, error) {
	return wt.HasCommitsAheadOf(ctx, wt.startRef)
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
func (m *Manager) forceClear(ctx context.Context, key, path, runID string) error {
	repoDir := m.repoDirForKey(key)
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
	if err := retryOnFileLock(ctx, func() error {
		return runGit(ctx, repoDir, "worktree", "remove", "--force", path)
	}); err != nil {
		if err := retryOnFileLock(ctx, func() error { return os.RemoveAll(path) }); err != nil {
			return fmt.Errorf("remove stale worktree directory: %w", err)
		}
		if err := runGit(ctx, repoDir, "worktree", "prune"); err != nil {
			return fmt.Errorf("prune stale worktree registration: %w", err)
		}
	}
	if err := os.Remove(markerPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove stale marker: %w", err)
	}
	if err := os.Remove(m.ownershipPath(key, filepath.Base(path))); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove stale ownership record: %w", err)
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
	if wt.pinned {
		wt.assetGuard = false
		return nil
	}
	repoDir := wt.manager.repoDirForKey(wt.key)
	markerPath := wt.manager.markerPath(wt.key, wt.RunID)

	lock := wt.manager.lockFor(wt.key)
	lock.Lock()
	worktreeBytes, worktreeMeasured, measurementErr := wt.manager.measureWorktree(wt.Path)
	var ownerRunID string
	defer func() {
		lock.Unlock()
		wt.manager.observeUsage(ctx, UsageOperationTeardown, ownerRunID, wt.RunID, worktreeBytes, worktreeMeasured, measurementErr)
	}()

	mk, markerErr := readMarker(markerPath)
	switch {
	case markerErr == nil:
		ownerRunID = mk.OwnerRunID
		if worktreeMeasured {
			mk.SizeBytes = &worktreeBytes
		}
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
		mk.RetainedAt = time.Now()
		if err := writeMarker(markerPath, mk); err != nil {
			return fmt.Errorf("worktree: mark run %s kept: %w", wt.RunID, err)
		}
		return nil
	}

	if err := retryOnFileLock(ctx, func() error {
		return runGit(ctx, repoDir, "worktree", "remove", "--force", wt.Path)
	}); err != nil {
		if worktreeMeasured && markerErr == nil {
			if usageErr := writeMarker(markerPath, mk); usageErr != nil {
				measurementErr = errors.Join(measurementErr, fmt.Errorf("worktree: persist usage for run %s: %w", wt.RunID, usageErr))
			}
		}
		return fmt.Errorf("worktree: remove for run %s: %w", wt.RunID, err)
	}
	if err := os.Remove(markerPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("worktree: unregister run %s: %w", wt.RunID, err)
	}
	if err := os.Remove(wt.manager.ownershipPath(wt.key, filepath.Base(wt.Path))); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("worktree: remove ownership record for run %s: %w", wt.RunID, err)
	}
	return nil
}
