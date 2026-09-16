package worktree

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/goobers/goobers/internal/workspacerevision"
)

var exactRevisionSHA = regexp.MustCompile(`^(?:[0-9a-f]{40}|[0-9a-f]{64})$`)

func revisionFailure(code, message string, cause error) error {
	var refusal *workspacerevision.Error
	if errors.As(cause, &refusal) {
		return cause
	}
	return &workspacerevision.Error{Code: code, Message: message, Cause: cause}
}

func validateRevisionOptions(opts CreateOptions) error {
	if !exactRevisionSHA.MatchString(opts.ExpectedSHA) || opts.BaseRef != opts.ExpectedSHA ||
		opts.RepoURL == "" || opts.Branch != "" || opts.SyncBase || opts.RequireExistingBranch || opts.AcquireRemoteBranch {
		return revisionFailure(workspacerevision.CodeInvalid, "exact revision requires a full SHA equal to BaseRef, a source URL, and no branch or synchronization options", nil)
	}
	return nil
}

// exactRevisionWorkingCopy deliberately does not call WorkingCopy: refreshing
// moving refs cannot establish that this source still serves the selected object.
// --refetch also prevents an existing object/alternate from satisfying acquisition.
func (m *Manager) exactRevisionWorkingCopy(ctx context.Context, sourceURL, sha string) (string, error) {
	key := repoKey(sourceURL)
	lock := m.lockFor(key)
	lock.Lock()
	defer lock.Unlock()
	dir := m.repoDirForKey(key)
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
			return "", revisionFailure(workspacerevision.CodeAcquisition, "create exact revision mirror parent", err)
		}
		format := "sha1"
		if len(sha) == 64 {
			format = "sha256"
		}
		if err := runGit(ctx, "", "init", "--bare", "--template=", "--object-format="+format, dir); err != nil {
			return "", revisionFailure(workspacerevision.CodeAcquisition, "initialize exact revision mirror", err)
		}
		if err := runGit(ctx, dir, "remote", "add", "origin", sourceURL); err != nil {
			return "", revisionFailure(workspacerevision.CodeAcquisition, "configure exact revision source", err)
		}
		if m.partialClone {
			for _, config := range []string{"remote.origin.promisor=true", "remote.origin.partialclonefilter=blob:none"} {
				key, value, _ := strings.Cut(config, "=")
				if err := runGit(ctx, dir, "config", key, value); err != nil {
					return "", revisionFailure(workspacerevision.CodeAcquisition, "configure partial revision mirror", err)
				}
			}
		}
	} else if err != nil {
		return "", revisionFailure(workspacerevision.CodeAcquisition, "inspect exact revision mirror", err)
	}
	if err := ensureManagedGitConfig(ctx, dir); err != nil {
		return "", revisionFailure(workspacerevision.CodeAcquisition, "configure revision mirror", err)
	}
	if err := ensureScratchExcluded(ctx, dir); err != nil {
		return "", revisionFailure(workspacerevision.CodeAcquisition, "exclude harness files", err)
	}
	if err := m.fetchRevision(ctx, sourceURL, dir, sha, mirrorIsPartial(ctx, dir)); err != nil {
		return "", err
	}
	return dir, nil
}

func (m *Manager) fetchRevision(ctx context.Context, sourceURL, dir, sha string, partial bool) error {
	args := []string{"fetch", "--refetch", "--no-tags", "--no-recurse-submodules"}
	if partial {
		args = append(args, "--filter=blob:none")
	}
	args = append(args, "--", sourceURL, sha)
	if err := m.runRevisionGit(ctx, sourceURL, dir, args...); err != nil {
		return err
	}
	fetched, err := gitOutput(ctx, dir, "--no-replace-objects", "--no-lazy-fetch", "rev-parse", "--verify", "FETCH_HEAD")
	if err != nil || fetched != sha {
		return revisionFailure(workspacerevision.CodeSHAMismatch, "source did not supply the exact selected object", err)
	}
	return verifyRevisionObject(ctx, dir, sha)
}

func verifyRevisionObject(ctx context.Context, dir, sha string) error {
	kind, err := gitOutput(ctx, dir, "--no-replace-objects", "--no-lazy-fetch", "cat-file", "-t", sha)
	if err != nil {
		return revisionFailure(workspacerevision.CodeAcquisition, "selected object is unavailable", err)
	}
	if kind != "commit" {
		return revisionFailure(workspacerevision.CodeObjectType, "selected object is not a commit", nil)
	}
	resolved, err := gitOutput(ctx, dir, "--no-replace-objects", "--no-lazy-fetch", "rev-parse", "--verify", sha+"^{commit}")
	if err != nil {
		return revisionFailure(workspacerevision.CodeObjectType, "selected object does not resolve to a commit", err)
	}
	if resolved != sha {
		return revisionFailure(workspacerevision.CodeSHAMismatch, "selected commit differs from expected SHA", nil)
	}
	return nil
}

func verifyRevisionHEAD(ctx context.Context, dir, sha string) error {
	head, err := gitOutput(ctx, dir, "--no-replace-objects", "--no-lazy-fetch", "rev-parse", "--verify", "HEAD")
	if err != nil || head != sha {
		return revisionFailure(workspacerevision.CodeSHAMismatch, "workspace HEAD differs from selected SHA", err)
	}
	_, err = rawGitOutput(ctx, dir, nil, "symbolic-ref", "-q", "HEAD")
	if err == nil {
		return revisionFailure(workspacerevision.CodeSHAMismatch, "selected revision workspace is not detached", nil)
	}
	var gitErr *gitCommandError
	if !errors.As(err, &gitErr) || gitErr.exitCode != 1 {
		return revisionFailure(workspacerevision.CodeSHAMismatch, "cannot verify detached revision HEAD", err)
	}
	return nil
}

// revisionGitArgs disables every configured content filter, not just LFS.
// Command-line overrides keep even worktree-local configuration inert during
// reset/clean. Hooks and fsmonitor are already disabled by hardenedGitArgs.
func revisionGitArgs(ctx context.Context, dir string, args []string) ([]string, error) {
	keys, err := rawGitOutput(ctx, dir, nil, "config", "--null", "--name-only", "--get-regexp", `^filter\.`)
	if err != nil {
		var gitErr *gitCommandError
		if !errors.As(err, &gitErr) || gitErr.exitCode != 1 {
			return nil, err
		}
	}
	prefix := []string{"--no-replace-objects", "-c", "submodule.recurse=false", "-c", "fetch.recurseSubmodules=false"}
	filters := map[string]bool{"filter.lfs": true}
	for _, key := range strings.Split(string(keys), "\x00") {
		if dot := strings.LastIndexByte(key, '.'); dot > len("filter.") {
			filters[key[:dot]] = true
		}
	}
	for filter := range filters {
		for _, suffix := range []string{".clean=", ".smudge=", ".process=", ".required=false"} {
			prefix = append(prefix, "-c", filter+suffix)
		}
	}
	return append(prefix, args...), nil
}

func (m *Manager) runRevisionGit(ctx context.Context, sourceURL, dir string, args ...string) error {
	safeArgs, err := revisionGitArgs(ctx, dir, args)
	if err == nil {
		// A pinned checkout can have objects/promisors from the configured
		// base repository. Lazy source acquisition must not consult those
		// remotes, or a stage-mutated origin, using the source credential.
		var keys []byte
		keys, err = rawGitOutput(ctx, dir, nil, "config", "--null", "--name-only", "--get-regexp", `^remote\..*\.promisor$`)
		var gitErr *gitCommandError
		if errors.As(err, &gitErr) && gitErr.exitCode == 1 {
			err = nil
		}
		if err != nil {
			return revisionFailure(workspacerevision.CodeAcquisition, "inspect revision promisors", err)
		}
		var remoteArgs []string
		for _, key := range strings.Split(string(keys), "\x00") {
			if key != "" {
				remoteArgs = append(remoteArgs, "-c", key+"=false")
			}
		}
		remoteArgs = append(remoteArgs,
			"-c", "remote.goobers-selected-revision.url="+sourceURL,
			"-c", "remote.goobers-selected-revision.promisor=true",
			"-c", "remote.goobers-selected-revision.partialclonefilter=blob:none")
		safeArgs = append(remoteArgs, safeArgs...)
		err = m.runRemoteGit(ctx, sourceURL, dir, safeArgs...)
	}
	if err != nil {
		return revisionFailure(workspacerevision.CodeAcquisition, "acquire or materialize exact revision", err)
	}
	return nil
}

// PreparePinnedRevision discards repository changes and selects an exact source
// commit without changing origin. The caller must hold the whole-run lease and
// serialize this operation with every stage using the pinned checkout.
func (wt *Worktree) PreparePinnedRevision(ctx context.Context, sourceURL, expectedSHA string, sparse []string) error {
	if !wt.pinned || wt.manager == nil || sourceURL == "" || !exactRevisionSHA.MatchString(expectedSHA) {
		return revisionFailure(workspacerevision.CodeInvalid, "pinned revision requires a pinned workspace, authorized source URL and full SHA", nil)
	}
	if wt.manager.partialClone {
		// Keep the push remote unchanged; only this read-only promisor serves
		// missing source blobs. It has no fetch refspec or publication branch.
		for _, setting := range []struct{ key, value string }{
			{"remote.goobers-selected-revision.url", sourceURL},
			{"remote.goobers-selected-revision.promisor", "true"},
			{"remote.goobers-selected-revision.partialclonefilter", "blob:none"},
		} {
			if err := runGit(ctx, wt.Path, "config", setting.key, setting.value); err != nil {
				return revisionFailure(workspacerevision.CodeAcquisition, "configure pinned revision promisor", err)
			}
		}
	}
	if err := wt.manager.fetchRevision(ctx, sourceURL, wt.Path, expectedSHA, wt.manager.partialClone); err != nil {
		return err
	}
	// Record discard-only custody before exposing source content. A crash must
	// not make the normal pinned handoff preserve it against the base repository.
	root := filepath.Join(wt.manager.pinnedRoot, wt.key)
	owner, err := readPinnedCustody(root)
	if err != nil {
		return revisionFailure(workspacerevision.CodeAcquisition, "read pinned revision custody", err)
	}
	owner.SelectedRevisionSHA = expectedSHA
	owner.RevisionSparse = append([]string(nil), sparse...)
	if err := writeMarker(filepath.Join(root, pinnedCustodyFile), owner); err != nil {
		return revisionFailure(workspacerevision.CodeAcquisition, "record discard-only pinned custody", err)
	}
	run := func(args ...string) error { return wt.manager.runRevisionGit(ctx, sourceURL, wt.Path, args...) }
	if err := resetRevision(ctx, wt.Path, expectedSHA, sparse, run); err != nil {
		return err
	}
	wt.Branch, wt.startRef = "", expectedSHA
	wt.revisionSparse = append([]string(nil), sparse...)
	return nil
}

// ResetPinnedRevision discards local commits, tracked edits, untracked and
// ignored files. It never fetches or hands changes to the preservation pipeline.
func (wt *Worktree) ResetPinnedRevision(ctx context.Context, expectedSHA string) error {
	if !wt.pinned || !exactRevisionSHA.MatchString(expectedSHA) {
		return revisionFailure(workspacerevision.CodeInvalid, "reset revision requires a pinned workspace and full SHA", nil)
	}
	run := func(args ...string) error {
		safeArgs, err := revisionGitArgs(ctx, wt.Path, append([]string{"--no-lazy-fetch"}, args...))
		if err == nil {
			err = runGit(ctx, wt.Path, safeArgs...)
		}
		if err != nil {
			return revisionFailure(workspacerevision.CodeAcquisition, "reset pinned revision", err)
		}
		return nil
	}
	if err := resetRevision(ctx, wt.Path, expectedSHA, wt.revisionSparse, run); err != nil {
		return err
	}
	if _, err := gitOutput(ctx, wt.Path, "config", "--get", "remote.goobers-selected-revision.url"); err == nil {
		if err := run("config", "--remove-section", "remote.goobers-selected-revision"); err != nil {
			return err
		}
	}
	wt.Branch, wt.startRef = "", expectedSHA
	return nil
}

func (wt *Worktree) clearPinnedRevisionCustody(ctx context.Context, baseRef string) error {
	root := filepath.Join(wt.manager.pinnedRoot, wt.key)
	owner, err := readPinnedCustody(root)
	if err != nil {
		return err
	}
	if owner.SelectedRevisionSHA == "" {
		return nil
	}
	owner.SelectedRevisionSHA, owner.RevisionSparse = "", nil
	owner.BaseRef = pinnedBaseRef(ctx, wt.Path, baseRef)
	return writeMarker(filepath.Join(root, pinnedCustodyFile), owner)
}

func resetRevision(ctx context.Context, dir, sha string, sparse []string, run func(...string) error) error {
	if err := verifyRevisionObject(ctx, dir, sha); err != nil {
		return err
	}
	// Clean BEFORE switching: an untracked directory can obstruct checkout of
	// a tracked path. Reset HEAD first without moving a possibly attached branch.
	for _, args := range [][]string{{"reset", "--hard", "HEAD"}, {"clean", "-ffdx"}} {
		if err := run(args...); err != nil {
			return err
		}
	}
	if len(sparse) > 0 {
		if err := run(append([]string{"sparse-checkout", "set", "--cone", "--"}, sparse...)...); err != nil {
			return err
		}
	} else if err := run("sparse-checkout", "disable"); err != nil {
		return err
	}
	for _, args := range [][]string{{"checkout", "--detach", "--force", sha}, {"reset", "--hard", sha}, {"clean", "-ffdx"}} {
		if err := run(args...); err != nil {
			return err
		}
	}
	return verifyRevisionHEAD(ctx, dir, sha)
}
