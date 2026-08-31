package worktree

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/goobers/goobers/internal/gooberassets"
	"github.com/goobers/goobers/internal/platform/proc"
)

// Manager owns managed working copies under Root — one mirror clone per
// distinct repo URL — and hands out per-run worktrees branched off them. The
// zero value is not usable; construct with NewManager.
type Manager struct {
	// Root is the workcopies directory (ARCHITECTURE.md §6:
	// <instance-root>/workcopies), always absolute (NewManager resolves it) —
	// see NewManager's doc comment for why.
	Root string

	// runBranchNamespaces are the refs/heads/ prefixes WorkingCopy's mirror
	// prune must exclude so a run's local-only branch is never force-reset
	// mid-run (see WorkingCopy's doc). One instance may host several
	// gaggles with distinct branch namespaces (GaggleSpec.BranchNamespace),
	// so this is a set rather than a single value; every entry ends with "/".
	// Never empty — NewManager seeds the DefaultBranchNamespace when no option
	// configures it, so the default-prefix case is unchanged.
	runBranchNamespaces []string

	mu        sync.Mutex // guards repoLocks
	repoLocks map[string]*sync.Mutex
	pruneMu   sync.Mutex

	// symlinkFallback is true on platforms where git checks a repo's symlinks
	// out as plain text files holding the link target rather than as real
	// symlinks — the Windows default (core.symlinks=false), where symlink
	// creation needs Developer Mode or elevation. When set, Create scans a
	// freshly provisioned worktree for symlinks that were flattened this way
	// and records a per-run warning (see checkSymlinkSupport) so the condition
	// surfaces rather than corrupting a run silently. Defaults to
	// runtime.GOOS == "windows"; darwin/linux materialize symlinks natively, so
	// the scan never runs there and behavior is unchanged. Overridable in tests.
	symlinkFallback bool
	// lstat abstracts os.Lstat so the symlink-flattening scan is testable off
	// Windows. Defaults to os.Lstat.
	lstat func(string) (os.FileInfo, error)

	gaggle         string
	writerIdentity string
	usageObserver  UsageObserver
	diskUsage      func(string) (int64, error)
	gitEnv         func(context.Context, string) ([]string, error)
	remoteGitGate  func(context.Context, string) error

	// partialClone provisions NEW mirrors as blobless partial clones and
	// narrows their refresh refspec — see WithPartialClone. Never set for an
	// unconfigured Manager, so the default path issues byte-identical git
	// invocations to previous releases.
	partialClone bool
	// objectCache opts newly created mirrors into referencing a shared,
	// node-level object cache — see WithObjectCache. Never set for an
	// unconfigured Manager, so the default path creates no `_objects`
	// directory and issues byte-identical git invocations to previous
	// releases.
	objectCache bool
	// pinnedRoot is the node-wide root for persistent pinned workspaces. It may
	// differ from Root, which remains gaggle-scoped for disposable worktrees.
	pinnedRoot          string
	pinnedProcessKiller func(string) error

	pathLengthMu           sync.RWMutex
	pathLengthLimits       map[string]PathLengthLimit
	defaultPathLengthLimit *PathLengthLimit
}

// defaultRunBranchNamespace mirrors providers.DefaultBranchNamespace. It is
// restated as a local literal rather than imported so this low-level package
// keeps no worktree -> providers dependency (the same reasoning the former
// package-level namespace const carried); the wiring that constructs a Manager
// passes the authoritative providers value via WithRunBranchNamespaces, so a
// gaggle that retunes its namespace is honored without this fallback ever
// diverging in the configured path.
const defaultRunBranchNamespace = "goobers/"

// DefaultMaxPathLength is the Windows MAX_PATH ceiling used when no
// repository-specific maximum is configured.
const DefaultMaxPathLength = 260

// PathLengthLimit configures preflight for one repository URL.
type PathLengthLimit struct {
	MaxPathLength        int
	BuildOutputAllowance int
}

// ManagerOption configures a Manager at construction.
type ManagerOption func(*Manager)

// WithRunBranchNamespaces sets the refs/heads/ prefixes WorkingCopy excludes
// from its mirror prune (see Manager.runBranchNamespaces). Each namespace is
// normalized to a single trailing "/"; empty entries are dropped. Passing no
// non-empty namespace leaves the default in place. Supplying the set derived
// from the instance's gaggles (their configured BranchNamespace values) is
// what ties the mirror-fetch exclusion to the same value BranchName produces
// and pr-select filters on, closing #965's silent-revert gap.
func WithRunBranchNamespaces(namespaces ...string) ManagerOption {
	return func(m *Manager) {
		seen := make(map[string]bool, len(namespaces))
		var out []string
		for _, ns := range namespaces {
			if ns == "" {
				continue
			}
			if !strings.HasSuffix(ns, "/") {
				ns += "/"
			}
			if seen[ns] {
				continue
			}
			seen[ns] = true
			out = append(out, ns)
		}
		if len(out) > 0 {
			m.runBranchNamespaces = out
		}
	}
}

// WithGitEnvironment configures credentials for remote clone/fetch commands.
// The callback receives the repository URL and returns the complete child
// environment; a nil environment (with nil error) runs the command with the
// process's own environment — the unauthenticated default, for remotes the
// callback holds no credential for. Local worktree operations never receive
// this environment, with one deliberate exception: on a partial-clone mirror
// (WithPartialClone), any operation that materializes blobs spawns a fetch
// from the promisor remote — `git worktree add`'s checkout, Create's SyncBase
// merge of an advanced base, and Worktree.Diff against a base no checkout
// materialized — so those are remote operations and carry this environment
// too.
func WithGitEnvironment(resolve func(context.Context, string) ([]string, error)) ManagerOption {
	return func(m *Manager) {
		m.gitEnv = resolve
	}
}

// WithRemoteGitGate admits remote git operations before credentials are
// resolved or a git subprocess is started.
func WithRemoteGitGate(acquire func(context.Context, string) error) ManagerOption {
	return func(m *Manager) {
		m.remoteGitGate = acquire
	}
}

// WithPartialClone opts newly created mirrors into blobless partial clones
// (#646, design §3 B1): WorkingCopy clones with --filter=blob:none, storing
// every commit and tree but fetching blobs on demand from the promisor remote
// when a worktree checkout first materializes them. The refresh fetch for such
// a mirror narrows its refspec from every ref to heads + tags (run-branch
// namespaces stay excluded from the prune exactly as before, #133/#965) —
// branch, tag, and reachable-sha pinned bases all still resolve.
//
// Two consequences callers own:
//   - Blob-materializing worktree operations become network-dependent (and,
//     for private repos, credential-dependent — WithGitEnvironment covers the
//     checkout's blob fetch, the SyncBase merge, and Worktree.Diff): a
//     blob-fetch failure fails the operation closed, classified by
//     IsTransientProvisionError for the runner's bounded infrastructure retry.
//   - Only NEW mirrors are affected. An existing full mirror keeps full-mirror
//     fetches; a blobless mirror keeps its promisor config even if the option
//     is later dropped. There is no in-place migration in either direction.
func WithPartialClone() ManagerOption {
	return func(m *Manager) {
		m.partialClone = true
	}
}

// WithObjectCache opts newly created mirrors into borrowing objects from a
// shared, node-level object cache (design §3 B3, issue #654):
// `workcopies/_objects/<repo-key>` under PinnedRoot (already the node-wide
// root shared across every gaggle's Manager targeting the same repository —
// see WithPinnedRoot) holds one bare mirror clone per repository URL. A new
// mirror is created with `git clone --mirror --reference <cache>`, so its
// `objects/info/alternates` borrows the cache's objects instead of each
// gaggle paying for a full clone of its own.
//
// Consequences callers own:
//   - Only NEW mirrors are affected; an existing mirror predating the option
//     keeps its own full object store and is never migrated onto the cache.
//   - A dependent mirror's alternates reference must never outlive the cache
//     it points at — see GCObjectCache's fail-closed dependents check. There
//     is no automatic/background GC; a cache entry accumulates until an
//     operator runs it explicitly.
//   - The cache itself is always a full mirror clone regardless of
//     WithPartialClone: a blobless cache could not satisfy an alternates
//     lookup for a blob it never fetched.
func WithObjectCache() ManagerOption {
	return func(m *Manager) {
		m.objectCache = true
	}
}

// WithPinnedRoot sets the node-wide root shared by pinned workspaces across
// gaggles targeting the same repository.
func WithPinnedRoot(root string) ManagerOption {
	return func(m *Manager) {
		if root != "" {
			m.pinnedRoot = root
		}
	}
}

// WithPinnedProcessKiller overrides pinned-workspace lock-holder termination.
func WithPinnedProcessKiller(kill func(string) error) ManagerOption {
	return func(m *Manager) {
		if kill != nil {
			m.pinnedProcessKiller = kill
		}
	}
}

// WithWriterIdentity records a host-meaningful writer on each worktree marker.
func WithWriterIdentity(identity string) ManagerOption {
	return func(m *Manager) {
		m.writerIdentity = identity
	}
}

// WithPathLengthLimit enables checkout path-length preflight for repoURL. A
// zero maximum uses DefaultMaxPathLength.
func WithPathLengthLimit(repoURL string, limit PathLengthLimit) ManagerOption {
	return func(m *Manager) {
		if m.pathLengthLimits == nil {
			m.pathLengthLimits = make(map[string]PathLengthLimit)
		}
		if limit.MaxPathLength == 0 {
			limit.MaxPathLength = DefaultMaxPathLength
		}
		m.pathLengthLimits[repoURL] = limit
	}
}

// WithDefaultPathLengthLimit enables checkout path-length preflight for
// repositories without an explicit limit. A zero maximum uses
// DefaultMaxPathLength.
func WithDefaultPathLengthLimit(limit PathLengthLimit) ManagerOption {
	return func(m *Manager) {
		if limit.MaxPathLength == 0 {
			limit.MaxPathLength = DefaultMaxPathLength
		}
		m.defaultPathLengthLimit = &limit
	}
}

// SetPathLengthLimits atomically replaces the repository path-length policy.
// Replacing rather than merging ensures repositories disabled or removed by a
// configuration reload no longer retain their prior limits.
func (m *Manager) SetPathLengthLimits(limits map[string]PathLengthLimit) {
	replacement := make(map[string]PathLengthLimit, len(limits))
	for repoURL, limit := range limits {
		if limit.MaxPathLength == 0 {
			limit.MaxPathLength = DefaultMaxPathLength
		}
		replacement[repoURL] = limit
	}
	m.pathLengthMu.Lock()
	m.pathLengthLimits = replacement
	m.pathLengthMu.Unlock()
}

func (m *Manager) pathLengthLimit(repoURL string) (PathLengthLimit, bool) {
	m.pathLengthMu.RLock()
	defer m.pathLengthMu.RUnlock()
	limit, ok := m.pathLengthLimits[repoURL]
	if ok {
		return limit, true
	}
	if m.defaultPathLengthLimit == nil {
		return PathLengthLimit{}, false
	}
	return *m.defaultPathLengthLimit, true
}

// NewManager returns a Manager rooted at root, creating the directory if it
// does not already exist. root is resolved to an absolute path immediately
// (#282): every path this package derives from Root (repoDirForKey,
// runsDirForKey, a worktree's own destination path) is used both as a plain
// `git worktree add`/`git config` argument AND, later, as a subprocess's own
// cmd.Dir — two different processes potentially resolving it against two
// different cwds. A relative Root (the common case: an instance rooted at
// ".") let git resolve a worktree's relative destination against runGit's
// cmd.Dir (the managed mirror), not the daemon/CLI's own cwd it was actually
// built against — silently nesting every worktree inside the mirror instead
// of at its intended flat path. Resolving once, here, makes every path
// derived from Root unambiguous regardless of which subprocess's cwd it is
// later used against.
//
// Options configure the run-branch namespaces the mirror prune preserves; with
// none, the DefaultBranchNamespace ("goobers/") is used, so an unconfigured
// Manager behaves exactly as before.
func NewManager(root string, opts ...ManagerOption) (*Manager, error) {
	if root == "" {
		return nil, fmt.Errorf("worktree: root must not be empty")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("worktree: resolve absolute root for %s: %w", root, err)
	}
	if err := os.MkdirAll(abs, 0o755); err != nil {
		return nil, fmt.Errorf("worktree: create root %s: %w", abs, err)
	}
	m := &Manager{
		Root:                abs,
		runBranchNamespaces: []string{defaultRunBranchNamespace},
		repoLocks:           make(map[string]*sync.Mutex),
		symlinkFallback:     runtime.GOOS == "windows",
		lstat:               os.Lstat,
		diskUsage:           apparentDiskUsage,
		pinnedProcessKiller: proc.KillWorkspaceProcesses,
	}
	for _, opt := range opts {
		opt(m)
	}
	if m.pinnedRoot == "" {
		m.pinnedRoot = abs
	} else {
		pinnedAbs, err := filepath.Abs(m.pinnedRoot)
		if err != nil {
			return nil, fmt.Errorf("worktree: resolve absolute pinned root for %s: %w", m.pinnedRoot, err)
		}
		if err := os.MkdirAll(pinnedAbs, 0o755); err != nil {
			return nil, fmt.Errorf("worktree: create pinned root %s: %w", pinnedAbs, err)
		}
		m.pinnedRoot = pinnedAbs
	}
	return m, nil
}

// PinnedRoot returns the node-wide root pinned workspaces are materialized
// under (WithPinnedRoot, defaulting to Root when unset).
func (m *Manager) PinnedRoot() string {
	return m.pinnedRoot
}

// repoKey derives a stable, filesystem-safe directory name for a repo URL so
// two managers (or two runs) referring to the same repo always land on the
// same managed working copy.
func repoKey(repoURL string) string {
	sum := sha256.Sum256([]byte(repoURL))
	return hex.EncodeToString(sum[:])[:16]
}

const worktreeDirectoryHashBytes = 12

// worktreeDirectoryName bounds the run-specific checkout path segment at 27
// characters ("wt-" plus 96 hash bits), down from roughly 50 characters for a
// typical trace ID plus stage name.
func worktreeDirectoryName(runID string) string {
	sum := sha256.Sum256([]byte(runID))
	return "wt-" + hex.EncodeToString(sum[:worktreeDirectoryHashBytes])
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

func (m *Manager) ownershipPath(key, directory string) string {
	return filepath.Join(m.Root, key, "owners", directory+".json")
}

func (m *Manager) markerPath(key, runID string) string {
	return filepath.Join(m.markersDirForKey(key), runID+".json")
}

func (m *Manager) branchAcquisitionRunDir(key, ownerRunID string) string {
	sum := sha256.Sum256([]byte(ownerRunID))
	return filepath.Join(m.Root, key, "acquisitions", hex.EncodeToString(sum[:]))
}

func (m *Manager) branchAcquisitionPath(key, ownerRunID, branch string) string {
	sum := sha256.Sum256([]byte(branch))
	return filepath.Join(m.branchAcquisitionRunDir(key, ownerRunID), hex.EncodeToString(sum[:])+".json")
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

// A run's own branch lives under a run-branch namespace — providers.BranchName
// produces "<namespace><workflow>/<run-id>", DefaultBranchNamespace being
// "goobers/". These branches exist only in the managed clone (a run commits to
// them locally; they are never on origin), so WorkingCopy's mirror prune must
// exclude the namespace or it would delete a run's branch between the run's
// stages and silently break run-branch continuity (#133). The set of
// namespaces to preserve is Manager.runBranchNamespaces, seeded from the
// instance's gaggles (WithRunBranchNamespaces) so the exclusion tracks the
// same value BranchName produces and pr-select filters on rather than
// restating a lone "goobers/" literal that a retuned namespace would leave
// behind (#965).

// WorkingCopy ensures a managed mirror clone of repoURL exists and is up to
// date under Root, cloning on first use and fetching thereafter. A mirror
// clone has no working tree of its own — worktrees created via Create are the
// only mutable views onto it — and its fetch refspec covers every ref, so a
// pinned base ref (branch, tag, or sha) reachable on the remote is always
// available to branch a worktree from after WorkingCopy returns. The one
// exception is the run-branch namespaces (Manager.runBranchNamespaces), which
// the fetch deliberately excludes from its prune so a run's local-only branch
// survives across the run's stages (#133).
//
// Under WithPartialClone a NEW mirror is a blobless promisor clone and its
// refresh fetch narrows to heads + tags (#646). Commits and trees stay
// complete, so the pinned-base guarantee holds for branches, tags, and any
// sha reachable from them; blobs materialize on demand at worktree checkout.
// A mirror that predates the option keeps full-mirror fetches — the option
// never migrates existing mirrors.
//
// Concurrent calls for the same repo URL serialize on the clone/fetch step;
// calls for different repos proceed independently.
func (m *Manager) WorkingCopy(ctx context.Context, repoURL string) (string, error) {
	return m.workingCopy(ctx, repoURL, false)
}

func (m *Manager) workingCopy(ctx context.Context, repoURL string, narrow bool) (string, error) {
	key := repoKey(repoURL)
	lock := m.lockFor(key)
	lock.Lock()
	defer lock.Unlock()

	// Ensure/refresh the node-level object cache on the same cadence as the
	// mirror itself, before touching the mirror either way: a brand-new
	// mirror needs the cache directory to exist before it can reference it,
	// and an existing mirror's cache should stay fresh too (design §3 B3).
	// The narrow/legacy provisioning path (init --bare + remote add) is left
	// untouched, matching WithPartialClone's own scope.
	var objectCacheDir string
	if m.objectCache && !narrow {
		cacheDir, err := m.ensureObjectCache(ctx, repoURL, key)
		if err != nil {
			return "", fmt.Errorf("worktree: object cache for %s: %w", repoURL, err)
		}
		objectCacheDir = cacheDir
	}

	dir := m.repoDirForKey(key)
	switch _, err := os.Stat(dir); {
	case os.IsNotExist(err):
		if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
			return "", fmt.Errorf("worktree: create workcopy parent for %s: %w", repoURL, err)
		}
		var provisionErr error
		if narrow {
			if err := runGit(ctx, "", "init", "--bare", dir); err == nil {
				if err = runGit(ctx, dir, "remote", "add", "origin", repoURL); err == nil {
					provisionErr = m.fetchMirror(ctx, repoURL, dir, true)
				} else {
					provisionErr = err
				}
			} else {
				provisionErr = err
			}
		} else {
			cloneArgs := []string{"clone", "--mirror"}
			if m.partialClone {
				cloneArgs = append(cloneArgs, "--filter=blob:none")
			}
			if objectCacheDir != "" {
				cloneArgs = append(cloneArgs, "--reference", objectCacheDir)
			}
			cloneArgs = append(cloneArgs, repoURL, dir)
			provisionErr = m.runRemoteGit(ctx, repoURL, "", cloneArgs...)
		}
		if provisionErr != nil {
			_ = os.RemoveAll(dir) // don't leave a partial clone masquerading as a valid one
			return "", fmt.Errorf("worktree: clone %s: %w", repoURL, provisionErr)
		}
		if err := ensureManagedGitConfig(ctx, dir); err != nil {
			return "", err
		}
		if err := ensureScratchExcluded(ctx, dir); err != nil {
			return "", err
		}
		return dir, nil
	case err != nil:
		return "", fmt.Errorf("worktree: stat workcopy for %s: %w", repoURL, err)
	}

	// Refresh origin and prune refs it deleted, but exclude every run-branch
	// namespace: those branches live only here, never on origin, so a plain
	// mirror prune (+refs/*:refs/*) would delete a run's branch mid-run and
	// silently revert its stages to a pristine base (#133/#965). The explicit
	// refspec restates the mirror's default and appends one negative refspec
	// per configured namespace.
	//
	// A partial-clone mirror narrows the refspec to heads + tags (#646):
	// every base-ref kind Create accepts still resolves (tags cover pinned
	// tag bases; a pinned sha must be reachable from a fetched ref, which
	// full mirrors already required in practice for a base anyone could name)
	// while the long tail of provider-synthesized refs (refs/pull/* and
	// friends) stops being re-fetched. The probe is mirror-scoped, not
	// flag-scoped, so a full mirror that predates the option keeps its
	// full-mirror fetch untouched.
	if err := m.fetchMirror(ctx, repoURL, dir, narrow || m.partialClone && mirrorIsPartial(ctx, dir)); err != nil {
		return "", fmt.Errorf("worktree: fetch %s: %w", repoURL, err)
	}
	// A pre-existing mirror (cloned before #240) also needs the scratch exclude;
	// it is idempotent, so refreshing it on every WorkingCopy is safe.
	if err := ensureManagedGitConfig(ctx, dir); err != nil {
		return "", err
	}
	if err := ensureScratchExcluded(ctx, dir); err != nil {
		return "", err
	}
	return dir, nil
}

func (m *Manager) fetchMirror(ctx context.Context, repoURL, dir string, narrow bool) error {
	refspecs := []string{"+refs/*:refs/*"}
	if narrow {
		refspecs = []string{"+refs/heads/*:refs/heads/*", "+refs/tags/*:refs/tags/*"}
	}
	fetchArgs := append([]string{"fetch", "--prune", "origin"}, refspecs...)
	for _, ns := range m.runBranchNamespaces {
		fetchArgs = append(fetchArgs, "^refs/heads/"+ns+"*")
	}
	return m.runRemoteGit(ctx, repoURL, dir, fetchArgs...)
}

func (m *Manager) runRemoteGit(ctx context.Context, repoURL, dir string, args ...string) error {
	if m.remoteGitGate != nil {
		if err := m.remoteGitGate(ctx, repoURL); err != nil {
			return fmt.Errorf("admit remote git operation: %w", err)
		}
	}
	if m.gitEnv == nil {
		return runGit(ctx, dir, args...)
	}
	env, err := m.gitEnv(ctx, repoURL)
	if err != nil {
		return fmt.Errorf("resolve git environment: %w", err)
	}
	return runGitWithEnv(ctx, dir, env, args...)
}

// remoteGitOutput is runRemoteGit's output-capturing counterpart for
// operations that both return bytes and may reach the remote (Worktree.Diff on
// a partial-clone mirror): the credential environment is applied the same way,
// stdout comes back raw, and a failure is a typed *gitCommandError so a
// promisor fetch spawned mid-command classifies through
// IsTransientProvisionError.
func (m *Manager) remoteGitOutput(ctx context.Context, repoURL, dir string, args ...string) ([]byte, error) {
	if m.remoteGitGate != nil {
		if err := m.remoteGitGate(ctx, repoURL); err != nil {
			return nil, fmt.Errorf("admit remote git operation: %w", err)
		}
	}
	if m.gitEnv == nil {
		return rawGitOutput(ctx, dir, nil, args...)
	}
	env, err := m.gitEnv(ctx, repoURL)
	if err != nil {
		return nil, fmt.Errorf("resolve git environment: %w", err)
	}
	return rawGitOutput(ctx, dir, env, args...)
}

// managedGitConfig is the explicit per-mirror git config the worktree layer sets
// so a run's checkout behaves deterministically regardless of the host's ambient
// or installer-provided git configuration (#643). A linked worktree inherits the
// mirror's shared config, so setting these on the bare mirror covers every
// worktree branched from it — including the tree materialization that
// `git worktree add` performs.
//
// Both values are chosen to be behavior-identical on darwin/linux (where git's
// own defaults already match) and to matter only on Windows:
//   - core.autocrlf=false: never rewrite line endings on checkin/checkout. The
//     unix default is already false; the Git-for-Windows installer commonly sets
//     it to true globally, which would give a managed working copy phantom
//     whole-file CRLF diffs. Pinning it false makes the checkout deterministic
//     and defers all line-ending policy to the target repo's own .gitattributes.
//   - core.longpaths=true: let git operate on paths longer than the Win32
//     MAX_PATH (260) limit, which the nested workcopies/<key>/runs/<runId>-<stage>
//     scheme plus a target repo's own deep paths can exceed. A no-op off Windows.
var managedGitConfig = []struct{ key, value string }{
	{"core.autocrlf", "false"},
	{"core.longpaths", "true"},
}

// ensureManagedGitConfig sets managedGitConfig on the mirror at dir. `git config`
// with a plain key/value is idempotent and cheap, so applying it on every
// WorkingCopy (both the first clone and every later fetch) is safe and lets a
// mirror created before this policy existed self-heal on its next use — the same
// rationale ensureScratchExcluded relies on.
func ensureManagedGitConfig(ctx context.Context, dir string) error {
	for _, c := range managedGitConfig {
		if err := runGit(ctx, dir, "config", c.key, c.value); err != nil {
			return fmt.Errorf("worktree: set %s in %s: %w", c.key, dir, err)
		}
	}
	return nil
}

// scratchExcludePattern is the harness scratch dir (internal/harness writes
// <workspace>/.goobers/{prompt.md,result.json,verdict.json,context/}) that must
// never be committed into a run's PR (#240).
const scratchExcludePattern = ".goobers/"
const assetExcludePattern = "/" + gooberassets.WorkspaceDir + "/"

// ensureScratchExcluded makes harness-owned workspace paths invisible to git in
// every worktree branched from this managed mirror, so the common `git add -A`
// agent commit pattern never captures scratch files or goober assets. It appends
// patterns to the mirror's shared info/exclude, keeping the exclusion local.
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
	present := map[string]bool{}
	for _, line := range strings.Split(string(existing), "\n") {
		switch strings.TrimSpace(line) {
		case scratchExcludePattern, ".goobers":
			present[scratchExcludePattern] = true
		case assetExcludePattern:
			present[assetExcludePattern] = true
		}
	}
	patterns := []string{scratchExcludePattern, assetExcludePattern}
	missing := make([]string, 0, len(patterns))
	for _, pattern := range patterns {
		if !present[pattern] {
			missing = append(missing, pattern)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(excludePath), 0o755); err != nil {
		return fmt.Errorf("worktree: create info dir: %w", err)
	}
	buf := existing
	if len(buf) > 0 && buf[len(buf)-1] != '\n' {
		buf = append(buf, '\n')
	}
	for _, pattern := range missing {
		buf = append(buf, []byte(pattern+"\n")...)
	}
	if err := os.WriteFile(excludePath, buf, 0o644); err != nil {
		return fmt.Errorf("worktree: write info/exclude: %w", err)
	}
	return nil
}

// symlinkGitMode is git's index mode for a symbolic link (as opposed to
// 100644/100755 for regular files and 160000 for a gitlink/submodule).
const symlinkGitMode = "120000"

// checkSymlinkSupport reports per-run warnings for symlinks in the worktree at
// path that this platform could not materialize as real symlinks. On platforms
// that check symlinks out natively (darwin/linux — symlinkFallback false) it
// returns nil without touching git, so it is free and behavior-neutral there.
//
// On a symlink-fallback platform (Windows default: core.symlinks=false) git
// writes each symlink out as an ordinary text file containing the link target,
// which looks like real content to an agent and to `git status` — a corruption
// that would otherwise pass silently. Surfacing it as a warning is the decided
// policy (#643): Goobers does not fail the run (a repo's symlinks are often
// incidental to the change at hand), but it must not hide the degradation.
func (m *Manager) checkSymlinkSupport(ctx context.Context, path string) ([]string, error) {
	if !m.symlinkFallback {
		return nil, nil
	}
	entries, err := symlinkIndexEntries(ctx, path)
	if err != nil {
		return nil, err
	}
	flattened := flattenedSymlinks(path, entries, m.lstat)
	if len(flattened) == 0 {
		return nil, nil
	}
	return []string{fmt.Sprintf(
		"%d symlink(s) in this repo were checked out as plain files because this "+
			"platform lacks symlink support (git core.symlinks=false, the Windows "+
			"default without Developer Mode): %s. Their working-tree contents are the "+
			"link target text, not the linked file — edits and diffs involving them "+
			"may be wrong.",
		len(flattened), strings.Join(flattened, ", "),
	)}, nil
}

// symlinkIndexEntries returns the repo-relative paths of every entry checked
// into the worktree at path as a symlink (git index mode 120000). `git ls-files
// -s` prints "<mode> <sha> <stage>\t<path>" per entry.
func symlinkIndexEntries(ctx context.Context, path string) ([]string, error) {
	out, err := gitOutput(ctx, path, "ls-files", "-s")
	if err != nil {
		return nil, fmt.Errorf("worktree: list index entries in %s: %w", path, err)
	}
	var links []string
	for _, line := range strings.Split(out, "\n") {
		if !strings.HasPrefix(line, symlinkGitMode+" ") {
			continue
		}
		tab := strings.IndexByte(line, '\t')
		if tab < 0 {
			continue
		}
		links = append(links, line[tab+1:])
	}
	return links, nil
}

// flattenedSymlinks returns the subset of symlinkPaths (repo-relative, from the
// index) that did NOT materialize on disk as real symlinks — i.e. git wrote them
// as plain files because the platform lacks symlink support. lstat abstracts
// os.Lstat so the classification is testable off Windows. A path that cannot be
// lstat'd is skipped rather than reported: absence is a different condition (an
// excluded or not-yet-written path), not a flattened symlink.
func flattenedSymlinks(root string, symlinkPaths []string, lstat func(string) (os.FileInfo, error)) []string {
	var flattened []string
	for _, rel := range symlinkPaths {
		fi, err := lstat(filepath.Join(root, filepath.FromSlash(rel)))
		if err != nil {
			continue
		}
		if fi.Mode()&os.ModeSymlink == 0 {
			flattened = append(flattened, rel)
		}
	}
	return flattened
}

// hardenedGitArgs prepends the config overrides every git invocation this
// package issues must carry:
//
//   - `safe.bareRepository=all` (#247): under a hardened
//     `safe.bareRepository=explicit` git config, git refuses cwd-based
//     discovery of a bare repo, which is exactly how every call here reaches
//     our managed mirrors (cmd.Dir set to the mirror, no --git-dir/GIT_DIR).
//     Opting back into implicit discovery is safe for these specific
//     invocations because the mirrors are ones this package created and owns;
//     it does not relax the setting for anything else on the machine.
//   - `core.hooksPath=<null device>` and `core.fsmonitor=false`: never run
//     repo-state-configured programs. These commands execute unconfined as
//     the daemon against state an agentic stage can influence — the checked
//     out workspace tree always, and (before the harness narrowed its sandbox
//     grants, or on any not-yet-narrowed deployment) the shared mirror's own
//     hooks/ and config — so a planted post-checkout hook or fsmonitor
//     command must stay inert during provisioning (`git worktree add` runs
//     post-checkout), fetch, diff, and teardown (S3/#166). Command-line -c
//     carries the highest config precedence, so no tampered repo or worktree
//     config can re-enable either. Behavior-neutral for every legitimate
//     flow: hooks are never cloned into a mirror and Goobers never configures
//     fsmonitor.
//   - `maintenance.autoDetach=false` and `gc.autoDetach=false` (#3990/#4000):
//     housekeeping stays in the FOREGROUND, so it is over before the git
//     process this package waited on exits. Every fetch into a managed mirror
//     — including the bundle fetch ApplyBundle performs — ends with
//     `git maintenance run --auto`, which git DETACHES by default. The orphan
//     outlives its parent and keeps creating and deleting files under the
//     mirror's git dir (`gc.log`, `maintenance.lock`, `objects/pack/tmp_*`)
//     while this package's next step believes the mirror is quiescent, so a
//     teardown that walks the same tree — Reap/FinalizeRun in production,
//     t.TempDir's RemoveAll in tests — races a live writer and fails with
//     "directory not empty". Detaching buys nothing here: the daemon already
//     serializes mirror work behind the per-repo lock, and a fetch that
//     returns while housekeeping is still running only moves that work onto
//     an unsupervised process whose failures nobody reads. Maintenance still
//     runs on exactly the same schedule (`gc.auto`/`maintenance.auto` are
//     untouched), so mirror hygiene is unchanged. `gc.autoDetach` is git's
//     older name for the same switch and is the fallback the newer key defers
//     to; setting both covers every git version we support.
func hardenedGitArgs(args []string) []string {
	return append([]string{
		"-c", "safe.bareRepository=all",
		"-c", "core.hooksPath=" + os.DevNull,
		"-c", "core.fsmonitor=false",
		"-c", "maintenance.autoDetach=false",
		"-c", "gc.autoDetach=false",
	}, args...)
}

// gitOutput runs git in dir and returns its trimmed stdout.
func gitOutput(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", hardenedGitArgs(args)...)
	if dir != "" {
		cmd.Dir = dir
	}
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git %v: %w", args, err)
	}
	return strings.TrimSpace(string(out)), nil
}

// rawGitOutput runs git in dir (with env as the child environment when
// non-nil) and returns its raw, untrimmed stdout — for callers whose bytes are
// digested verbatim (Worktree.Diff). Failure is a typed *gitCommandError
// carrying the exit code and captured stderr, so IsTransientProvisionError can
// classify it — unlike gitOutput's plain wrap.
func rawGitOutput(ctx context.Context, dir string, env []string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "git", hardenedGitArgs(args)...)
	if dir != "" {
		cmd.Dir = dir
	}
	if env != nil {
		cmd.Env = env
	}
	out, err := cmd.Output()
	if err != nil {
		exitCode := -1
		var stderr []byte
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			exitCode = exitErr.ExitCode()
			stderr = exitErr.Stderr
		}
		return nil, &gitCommandError{args: args, cause: err, output: stderr, exitCode: exitCode}
	}
	return out, nil
}

// gitCommandError is runGit's typed failure: the raw exit code and combined
// output alongside the underlying exec error, so a caller (IsTransientProvisionError)
// can classify the failure without re-parsing runGit's formatted message string.
type gitCommandError struct {
	args     []string
	cause    error
	output   []byte
	exitCode int
}

func (e *gitCommandError) Error() string {
	return fmt.Sprintf("git %v: %v: %s", e.args, e.cause, e.output)
}

func (e *gitCommandError) Unwrap() error {
	return e.cause
}

// remote5xxPattern matches git's own "HTTP 5xx"/"returned error: 5xx"
// phrasing for a failed smart-HTTP request (curl's -f behavior surfaces the
// remote status this way, not as a distinct git exit code).
var remote5xxPattern = regexp.MustCompile(`\b(?:http(?:/[0-9.]+)?[\s:=-]+|error:\s*)5[0-9]{2}\b`)

// IsTransientProvisionError reports whether err is a git exit-128 caused by
// a temporary network or remote-server failure during worktree provisioning
// (issue #572) — the shape internal/runner's dispatchTask classifies to
// invoke.InfrastructureFailure so it retries through the runner's bounded
// infrastructure budget instead of failing the run before an attempt even
// exists. Git exits 128 for BOTH transient network failures and permanent
// ones (auth, missing ref, missing repo) — the exit code alone cannot
// distinguish them, so this matches on the combined output's own message
// text instead. Authentication/authorization failures, bad refs, and other
// deterministic git errors deliberately do NOT match — retrying those can
// only reproduce the identical failure.
//
// Checkout-time blob backfill from a partial-clone mirror (#646) is its own
// failure class: `git worktree add` on a blobless mirror spawns a promisor
// fetch whose failure surfaces as "could not fetch <oid> from promisor
// remote" / "failed to fetch some objects", wrapping whatever transport error
// caused it. Those wrappers match here even though the wrapped cause is not
// always network text: unlike clone/fetch, the promisor path only runs after
// this same remote was successfully cloned or fetched moments earlier with
// the same credentials, so a mid-provision blip is the overwhelmingly common
// cause and the rare deterministic one is contained by the runner's bounded
// infrastructure-retry budget.
func IsTransientProvisionError(err error) bool {
	var gitErr *gitCommandError
	if !errors.As(err, &gitErr) || gitErr.exitCode != 128 {
		return false
	}
	output := string(gitErr.output)
	if isNetworkGitOutput(output) {
		return true
	}
	message := strings.ToLower(output)
	for _, fragment := range []string{
		// Promisor blob-backfill failures at worktree checkout (#646) — see
		// the function doc for why these wrappers classify as transient
		// without being provably network-owned (ClassifyProvisionError
		// therefore leaves them on the git tier).
		"from promisor remote",
		"failed to fetch some objects",
	} {
		if strings.Contains(message, fragment) {
			return true
		}
	}
	return false
}

// runGit runs git with args, using dir as the working directory (the process
// default if dir is empty), and returns a typed *gitCommandError (carrying
// exit code + combined output for IsTransientProvisionError's classification)
// on failure.
func runGit(ctx context.Context, dir string, args ...string) error {
	return runGitWithEnv(ctx, dir, nil, args...)
}

func runGitWithEnv(ctx context.Context, dir string, env []string, args ...string) error {
	cmd := exec.CommandContext(ctx, "git", hardenedGitArgs(args)...)
	if dir != "" {
		cmd.Dir = dir
	}
	if env != nil {
		cmd.Env = env
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		exitCode := -1
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			exitCode = exitErr.ExitCode()
		}
		return &gitCommandError{args: args, cause: err, output: out, exitCode: exitCode}
	}
	return nil
}

// fileLockRetryAttempts and fileLockRetryBackoff bound the teardown retry loop
// that absorbs transient OS-level file locks during worktree removal. On
// Windows a background process — the Search Indexer, Defender real-time scan,
// or a lingering build server — routinely holds a momentary handle on a
// just-written worktree file, so a single `git worktree remove` / os.RemoveAll
// hits a sharing violation and, without a retry, aborts the whole run even
// though the work is sound (observed: run failed at "clear stale worktree ...
// used by another process"). Kept small: such locks clear in well under a
// second in practice, and teardown must not stall the run.
const (
	fileLockRetryAttempts = 6
	fileLockRetryBackoff  = 250 * time.Millisecond
)

// isTransientFileLockError reports whether err looks like a momentary OS-level
// file lock during worktree teardown rather than a permanent fault. The same
// underlying Windows errors (ERROR_SHARING_VIOLATION / ERROR_ACCESS_DENIED)
// surface both from git's own stderr ("Permission denied", "unable to unlink")
// and from os.RemoveAll's *PathError ("The process cannot access the file
// because it is being used by another process"), so it classifies on the
// error text, which covers both sources. A genuinely permanent permission
// fault still matches, but is contained by retryOnFileLock's bounded budget —
// at worst it delays an inevitable failure by ~1s.
func isTransientFileLockError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	for _, fragment := range []string{
		"used by another process",
		"the process cannot access the file",
		"permission denied",
		"access is denied",
		"sharing violation",
	} {
		if strings.Contains(msg, fragment) {
			return true
		}
	}
	return false
}

// retryOnFileLock runs op, retrying on a transient file-lock error (see
// isTransientFileLockError) with a short backoff up to fileLockRetryAttempts
// times. It returns nil on the first success and the last error otherwise,
// and aborts early if ctx is cancelled. Non-lock errors are returned
// immediately without retry, so deterministic failures are not masked.
func retryOnFileLock(ctx context.Context, op func() error) error {
	var err error
	for attempt := 0; attempt < fileLockRetryAttempts; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return errors.Join(err, ctx.Err())
			case <-time.After(fileLockRetryBackoff):
			}
		}
		if err = op(); err == nil || !isTransientFileLockError(err) {
			return err
		}
	}
	return err
}

// mirrorIsPartial reports whether the mirror at dir was created as a
// partial-clone promisor mirror (clone --filter sets remote.origin.promisor).
// Like branchExists this is a boolean probe: an unset key exits non-zero,
// which is an ordinary false. Consulted only when the Manager has
// WithPartialClone, so the default path's git invocations stay untouched, and
// per mirror rather than per Manager so a pre-existing full mirror is left
// as-is (#646: new mirrors only, no in-place migration).
func mirrorIsPartial(ctx context.Context, dir string) bool {
	cmd := exec.CommandContext(ctx, "git", hardenedGitArgs([]string{"config", "--get", "remote.origin.promisor"})...)
	cmd.Dir = dir
	out, err := cmd.Output()
	return err == nil && strings.TrimSpace(string(out)) == "true"
}

// branchExists reports whether a local branch of the given name exists in the
// repo at repoDir. `show-ref --verify --quiet` exits 0 iff the ref exists and
// prints nothing, so non-existence is an ordinary false, not an error — this
// is a boolean probe, distinct from runGit's must-succeed contract. Used by
// Create to decide whether to create the run branch or check out the existing
// one (#133).
func branchExists(ctx context.Context, repoDir, branch string) bool {
	cmd := exec.CommandContext(ctx, "git", hardenedGitArgs([]string{"show-ref", "--verify", "--quiet", "refs/heads/" + branch})...)
	cmd.Dir = repoDir
	return cmd.Run() == nil
}
