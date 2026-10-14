package recovery

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/worktree"
)

// GoneTargetEventKind marks the instance-journal record that a cleanup target
// was settled because its checkout directory no longer exists.
const GoneTargetEventKind = "recovery-cleanup-target-missing"

// SettleGoneCleanupTarget settles a cleanup whose checkout directory is
// already gone (#5383). A handoff that reads the checkout can never pass, so
// deferring on it would leak the cleanup entry forever. A linked worktree's
// abandoned preparation branch lives in the shared repository rather than the
// checkout, so it is still handed to the inventory from there, and the
// missing checkout is journaled as the reason, before cleanup proceeds.
// gone is false when the checkout exists and ordinary capture applies; an
// error leaves the cleanup deferred for retry.
//
// A directory left empty by a removal that deleted the checkout and its git
// metadata but could not unlink the directory itself (Windows refuses while a
// process still holds it) is settled the same way (#6940): every git command
// there fails "not a git repository", so capture could never pass.
func SettleGoneCleanupTarget(ctx context.Context, manager *worktree.Manager, target worktree.CleanupTarget, request RetentionRequest, log PublicationJournal) (gone bool, err error) {
	checkout, err := goneCheckoutState(target.Path)
	if err != nil || checkout == "" {
		return false, err
	}
	if log == nil {
		return true, fmt.Errorf("gone cleanup target requires a durable journal")
	}
	shared, err := goneTargetSharedRepository(manager, target)
	if err != nil {
		return true, err
	}
	source := "unavailable"
	if shared != "" {
		request.Repository = shared
		if err := RetainAbandonedPreparation(ctx, request, log); err != nil {
			return true, err
		}
		source = "shared"
	}
	event := journal.Event{Type: journal.EventRunnerAnnotation, RunID: target.OwnerRunID,
		Reason: "worktree cleanup target already removed or emptied; its checkout held nothing left to capture",
		Runner: map[string]any{"kind": GoneTargetEventKind, "worktreeId": target.WorktreeID, "preparationRepository": source, "checkout": checkout}}
	if err := log.Append(event); err != nil {
		return true, fmt.Errorf("record missing cleanup target: %w", err)
	}
	return true, nil
}

// goneTargetSharedRepository returns "" when no repository can still hold the
// target's refs: a pinned clone's refs lived in the lost checkout itself.
func goneTargetSharedRepository(manager *worktree.Manager, target worktree.CleanupTarget) (string, error) {
	if manager == nil || target.Pinned {
		return "", nil
	}
	shared, ok := manager.LinkedWorktreeRepository(target.Path)
	if !ok {
		return "", nil
	}
	if _, err := os.Lstat(shared); os.IsNotExist(err) {
		return "", nil
	} else if err != nil {
		return "", fmt.Errorf("inspect shared cleanup repository: %w", err)
	}
	return shared, nil
}

// CleanupTargetGone proves that a checkout is missing or contains no
// recoverable files. It does not publish the required cleanup handoff.
func CleanupTargetGone(path string) (bool, error) {
	state, err := goneCheckoutState(path)
	return state != "", err
}

// goneCheckoutState reports why the checkout at path holds nothing a capture
// could read: "missing" when the directory is gone, "empty" when it remains
// with no entries, or with only a .git file whose admin directory was pruned.
// It returns "" for anything else, including a repository-less directory that
// still holds files: those may be the only copy of uncommitted work, so they
// stay on the ordinary capture path and keep deferring until an operator acts.
func goneCheckoutState(path string) (string, error) {
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return "missing", nil
	} else if err != nil {
		return "", fmt.Errorf("inspect cleanup target: %w", err)
	}
	if !info.IsDir() {
		return "", nil
	}
	entries, err := os.ReadDir(path)
	if err != nil {
		return "", fmt.Errorf("inspect cleanup target contents: %w", err)
	}
	if len(entries) == 0 || len(entries) == 1 && entries[0].Name() == ".git" && danglingGitFile(path) {
		return "empty", nil
	}
	return "", nil
}

// danglingGitFile reports whether path/.git is a linked-worktree gitdir file
// naming an admin directory that no longer exists. Anything it cannot prove
// dangling is left to ordinary capture, which reports the real failure.
func danglingGitFile(path string) bool {
	dotGit := filepath.Join(path, ".git")
	info, err := os.Lstat(dotGit)
	if err != nil || !info.Mode().IsRegular() || info.Size() > 4096 {
		return false
	}
	content, err := os.ReadFile(dotGit)
	if err != nil {
		return false
	}
	gitdir, ok := strings.CutPrefix(strings.TrimSpace(string(content)), "gitdir:")
	gitdir = strings.TrimSpace(gitdir)
	if !ok || gitdir == "" {
		return false
	}
	if !filepath.IsAbs(gitdir) {
		gitdir = filepath.Join(path, gitdir)
	}
	_, err = os.Lstat(gitdir)
	return os.IsNotExist(err)
}
