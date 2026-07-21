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

	"github.com/goobers/goobers/internal/gooberassets"
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
}

// defaultRunBranchNamespace mirrors providers.DefaultBranchNamespace. It is
// restated as a local literal rather than imported so this low-level package
// keeps no worktree -> providers dependency (the same reasoning the former
// package-level namespace const carried); the wiring that constructs a Manager
// passes the authoritative providers value via WithRunBranchNamespaces, so a
// gaggle that retunes its namespace is honored without this fallback ever
// diverging in the configured path.
const defaultRunBranchNamespace = "goobers/"

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
	}
	for _, opt := range opts {
		opt(m)
	}
	return m, nil
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
	fetchArgs := []string{"fetch", "--prune", "origin", "+refs/*:refs/*"}
	for _, ns := range m.runBranchNamespaces {
		fetchArgs = append(fetchArgs, "^refs/heads/"+ns+"*")
	}
	if err := runGit(ctx, dir, fetchArgs...); err != nil {
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

// bareRepoSafeArgs prepends `-c safe.bareRepository=all` to args (#247): under
// a hardened `safe.bareRepository=explicit` git config, git refuses cwd-based
// discovery of a bare repo, which is exactly how every call here reaches our
// managed mirrors (cmd.Dir set to the mirror, no --git-dir/GIT_DIR). Opting
// back into implicit discovery is safe for these specific invocations because
// the mirrors are ones this package created and owns; it does not relax the
// setting for anything else on the machine.
func bareRepoSafeArgs(args []string) []string {
	return append([]string{"-c", "safe.bareRepository=all"}, args...)
}

// gitOutput runs git in dir and returns its trimmed stdout.
func gitOutput(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", bareRepoSafeArgs(args)...)
	if dir != "" {
		cmd.Dir = dir
	}
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git %v: %w", args, err)
	}
	return strings.TrimSpace(string(out)), nil
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
func IsTransientProvisionError(err error) bool {
	var gitErr *gitCommandError
	if !errors.As(err, &gitErr) || gitErr.exitCode != 128 {
		return false
	}
	message := strings.ToLower(string(gitErr.output))
	for _, fragment := range []string{
		"could not resolve host",
		"couldn't resolve host",
		"failed to connect to",
		"could not connect to",
		"connection refused",
		"connection reset",
		"connection timed out",
		"ssl connection timeout",
		"empty reply from server",
		"network is unreachable",
		"operation timed out",
		"timeout was reached",
		"timed out after",
		"the remote end hung up unexpectedly",
		"unexpected disconnect",
		"early eof",
	} {
		if strings.Contains(message, fragment) {
			return true
		}
	}
	return remote5xxPattern.MatchString(message)
}

// runGit runs git with args, using dir as the working directory (the process
// default if dir is empty), and returns a typed *gitCommandError (carrying
// exit code + combined output for IsTransientProvisionError's classification)
// on failure.
func runGit(ctx context.Context, dir string, args ...string) error {
	cmd := exec.CommandContext(ctx, "git", bareRepoSafeArgs(args)...)
	if dir != "" {
		cmd.Dir = dir
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

// branchExists reports whether a local branch of the given name exists in the
// repo at repoDir. `show-ref --verify --quiet` exits 0 iff the ref exists and
// prints nothing, so non-existence is an ordinary false, not an error — this
// is a boolean probe, distinct from runGit's must-succeed contract. Used by
// Create to decide whether to create the run branch or check out the existing
// one (#133).
func branchExists(ctx context.Context, repoDir, branch string) bool {
	cmd := exec.CommandContext(ctx, "git", bareRepoSafeArgs([]string{"show-ref", "--verify", "--quiet", "refs/heads/" + branch})...)
	cmd.Dir = repoDir
	return cmd.Run() == nil
}
