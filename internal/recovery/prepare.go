package recovery

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"
)

// PrepareRecord captures the cumulative implementation since its common
// ancestor with the supplied local base ref, including committed and dirty
// work. A stage's checkout/start SHA is NOT a cumulative implementation base.
// The caller verifies repository identity, exclusively owns the workspace,
// and supplies a stable run timestamp plus a bounded retention deadline.
// This prepares objects only; PublishRetainedState must succeed before cleanup.
func PrepareRecord(ctx context.Context, repository, repositoryKey, runID, baseRef string, identityTime, retainUntil time.Time) (Record, error) {
	if !validRepositoryKey(repositoryKey) || baseRef == "" || strings.HasPrefix(baseRef, "-") || strings.ContainsAny(baseRef, "\x00\r\n") {
		return Record{}, fmt.Errorf("invalid recovery preparation identity or base ref")
	}
	if _, err := RefForRun(runID); err != nil {
		return Record{}, err
	}
	if identityTime.IsZero() || !retainUntil.After(identityTime) {
		return Record{}, fmt.Errorf("invalid recovery preparation window")
	}
	var base boundedRefOutput
	if err := recoveryGit(ctx, repository, &base, "merge-base", "--all", baseRef, "HEAD"); err != nil {
		return Record{}, fmt.Errorf("resolve cumulative recovery base against %q: %w", baseRef, err)
	}
	baseSHA := strings.TrimSpace(base.String())
	// Multiple criss-cross merge bases require explicit resolution; picking
	// whichever Git prints first would make recovery content nondeterministic.
	if !gitObjectID.MatchString(baseSHA) {
		return Record{}, fmt.Errorf("recovery requires one unambiguous common ancestor")
	}
	snapshot, err := CaptureSnapshot(ctx, repository, runID, identityTime)
	if err != nil {
		return Record{}, err
	}
	digest, err := WriteSnapshotPatch(ctx, repository, baseSHA, snapshot, io.Discard)
	if err != nil {
		return Record{}, err
	}
	ref, err := RefForSnapshot(runID, snapshot)
	if err != nil {
		return Record{}, err
	}
	return Record{Version: 1, RunID: runID, RepositoryKey: repositoryKey, Ref: ref, BaseRef: remoteRecoveryBaseRef(baseRef), BaseSHA: baseSHA, SnapshotSHA: snapshot, PatchDigest: digest, CreatedAt: identityTime, RetainUntil: retainUntil}, nil
}

// remoteRecoveryBaseRef converts the pinned workspace's local mirror-tracking
// namespace into the source ref a later restore must fetch. Other fully
// qualified refs and object IDs retain their exact identity.
func remoteRecoveryBaseRef(baseRef string) string {
	if branch, ok := strings.CutPrefix(baseRef, "refs/remotes/mirror/"); ok {
		return "refs/heads/" + branch
	}
	if strings.HasPrefix(baseRef, "refs/") || gitObjectID.MatchString(baseRef) {
		return baseRef
	}
	return "refs/heads/" + baseRef
}
