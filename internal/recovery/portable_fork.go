package recovery

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
)

// PreparePortableFork maps an archived, filtered contribution onto the source
// checkout's immutable real ancestry. Synthetic transport roots never become
// host branch history. Excluded tracked paths retain their baseline versions;
// the next worker transport filters them again before leaving host custody.
// Only a private index and a deterministic CAS retention ref are changed.
func PreparePortableFork(ctx context.Context, repository, baseSHA, operation string, carrier PortableCarrier, at time.Time) (Record, error) {
	if !gitObjectID.MatchString(baseSHA) || at.IsZero() {
		return Record{}, fmt.Errorf("portable fork requires exact source ancestry and time")
	}
	if _, err := RefForRun(operation); err != nil {
		return Record{}, err
	}
	if err := ImportPortableCarrier(ctx, repository, carrier); err != nil {
		return Record{}, err
	}
	if _, err := snapshotObject(ctx, repository, baseSHA+"^{commit}"); err != nil {
		return Record{}, err
	}
	directory, err := os.MkdirTemp("", "goobers-parent-fork-index-*")
	if err != nil {
		return Record{}, err
	}
	defer func() { _ = os.RemoveAll(directory) }()
	env, err := snapshotEnvironment(ctx, repository, directory, baseSHA)
	if err != nil {
		return Record{}, err
	}
	if err = recoveryGitWithEnv(ctx, repository, io.Discard, env, "read-tree", carrier.Snapshot.TreeSHA); err != nil {
		return Record{}, err
	}
	parent := ChildSnapshot{Record: Record{BaseSHA: baseSHA, SnapshotSHA: baseSHA, RepositoryKey: carrier.Snapshot.Record.RepositoryKey, RetainUntil: carrier.Snapshot.Record.RetainUntil}, Policy: carrier.Snapshot.Policy}
	if err = restorePublicationOmissions(ctx, repository, env, parent); err != nil {
		return Record{}, err
	}
	var tree boundedRefOutput
	if err = recoveryGitWithEnv(ctx, repository, &tree, env, "write-tree"); err != nil {
		return Record{}, err
	}
	treeID := strings.TrimSpace(tree.String())
	if !gitObjectID.MatchString(treeID) {
		return Record{}, fmt.Errorf("invalid portable fork tree")
	}
	return pinDispositionTree(ctx, repository, parent, carrier.Snapshot.Record.SnapshotSHA, treeID, ChildReplace, operation, at)
}
