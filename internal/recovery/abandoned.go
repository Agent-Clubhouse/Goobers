package recovery

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
)

// RetainAbandonedPreparation hands an unadopted preparation to the bounded
// durable inventory before deleting its exact temporary branch. The caller
// must exclusively own the receiving run's workspace and be entering cleanup.
// It leaves HEAD/index/files unchanged, including dirty work that the caller
// must capture separately. A failed handoff leaves the preparation recoverable.
// With SkipEmpty, a preparation carrying no change against its parent is
// deleted without consuming an inventory slot.
func RetainAbandonedPreparation(ctx context.Context, request RetentionRequest, log PublicationJournal) error {
	if log == nil {
		return fmt.Errorf("prepared recovery cleanup requires a durable journal")
	}
	branch, err := PreparedRestoreBranch(request.RunID)
	if err != nil {
		return err
	}
	ref := "refs/heads/" + branch
	if err := recoveryGit(ctx, request.Repository, io.Discard, "show-ref", "--verify", "--quiet", ref); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
			return nil
		}
		return fmt.Errorf("inspect abandoned preparation: %w", err)
	}
	var metadata snapshotPathOutput
	if err := recoveryGit(ctx, request.Repository, &metadata, "show", "--no-patch", "--format=%H%n%P", ref); err != nil {
		return err
	}
	fields := strings.Fields(metadata.String())
	if len(fields) != 2 || !gitObjectID.MatchString(fields[0]) || !gitObjectID.MatchString(fields[1]) {
		return fmt.Errorf("abandoned preparation requires one parent commit")
	}
	commit, parent := fields[0], fields[1]
	digest, err := WriteSnapshotPatch(ctx, request.Repository, parent, commit, io.Discard)
	if err != nil {
		return err
	}
	// A preparation whose tree matches its parent protects no work: nothing
	// was authored before it was abandoned. Publishing it anyway consumes a
	// retention-floored inventory slot that neither expiry nor eviction can
	// reclaim, so one burst of failed worktree preparations wedges the
	// inventory for the whole floor and then fails every later run's durable
	// handoff, unrelated to that burst (#4994). Delete the exact preparation
	// directly instead, matching Retain's SkipEmpty contract.
	if request.SkipEmpty && digest == emptyPatchDigest {
		return deleteExactRecoveryRef(ctx, request.Repository, ref, commit)
	}
	snapshotRef, err := RefForSnapshot(request.RunID, commit)
	if err != nil {
		return err
	}
	record := Record{Version: 1, RunID: request.RunID, RepositoryKey: request.RepositoryKey,
		Ref: snapshotRef, BaseSHA: parent, SnapshotSHA: commit, PatchDigest: digest,
		CreatedAt: request.IdentityTime, RetainUntil: request.RetainUntil}
	retained, recordPath, err := publishAbandonedPreparation(ctx, request, record)
	if err != nil {
		return err
	}
	event, err := RetainedEvent(retained)
	if err != nil {
		return err
	}
	event.Runner["recoveryCapture"] = true
	if err := log.Append(event); err != nil {
		return fmt.Errorf("acknowledge abandoned preparation: %w", err)
	}
	if err := request.acknowledgeArchive(ctx, retained, recordPath); err != nil {
		return err
	}
	return deleteExactRecoveryRef(ctx, request.Repository, ref, commit)
}

func publishAbandonedPreparation(ctx context.Context, request RetentionRequest, prepared Record) (Record, string, error) {
	prepared, err := matchAbandonedReservation(ctx, request, prepared)
	if err != nil {
		return Record{}, "", err
	}
	_, path, err := PublishToInventoryWithEviction(ctx, request.Repository, request.InventoryRoot, request.CleanupRoots, prepared, request.MaxSnapshots, request.MaxArchiveBytes, request.EvictFull)
	if err != nil {
		return Record{}, "", err
	}
	renewed, err := RenewRetention(ctx, path, request.RetainUntil, request.MaxArchiveBytes)
	return renewed, path, err
}

// matchAbandonedReservation returns the immutable capture identity this
// preparation was already published under, or prepared unchanged when it has
// none. The same prepared commit can cross the stage-to-terminal boundary:
// its identity and bytes are kept and only the retention sidecar is renewed.
//
// It reads TOLERANTLY (#5177 AC3, #5354). This caller is not asking whether
// recovery state exists — it is asking about ONE identity it exclusively
// owns, and it is about to publish that identity whatever the answer. A
// second reservation it has nothing to do with, left incomplete by some other
// run's crashed publish, used to make the strict read fail closed here and so
// failed EVERY later worktree cleanup on the instance, forever, until an
// operator deleted files by hand. Reservations the scan cannot interpret are
// never a match, so skipping them cannot change this decision; they are
// reported and reclaimed by ReconcileIncompleteReservations instead. Strict
// ReadInventory remains right for callers deciding whether it is safe to
// discard work, which is why it is still what recovery-abandon, the expiry
// sweep and the publication API use.
func matchAbandonedReservation(ctx context.Context, request RetentionRequest, prepared Record) (Record, error) {
	entries, err := readAbandonedInventory(ctx, request)
	if err != nil {
		return Record{}, err
	}
	for _, entry := range entries {
		prior := entry.Record
		if prior.RunID != prepared.RunID || prior.RepositoryKey != prepared.RepositoryKey || prior.SnapshotSHA != prepared.SnapshotSHA {
			continue
		}
		if prior.Ref != prepared.Ref || prior.BaseSHA != prepared.BaseSHA || prior.PatchDigest != prepared.PatchDigest {
			return Record{}, ErrRecordConflict
		}
		return prior, nil
	}
	return prepared, nil
}

// readAbandonedInventory tolerates broken entries and, when the scan itself
// refuses because the directory already holds more entries than the cap
// (the "130 of 128" wedge), reconciles stale incomplete reservations once and
// retries. That is the only read this cleanup needs, and an inventory wedged
// above its cap by debris is exactly the state that must heal itself.
func readAbandonedInventory(ctx context.Context, request RetentionRequest) ([]InventoryEntry, error) {
	entries, _, err := ReadInventoryTolerant(ctx, request.InventoryRoot, request.MaxSnapshots)
	if err == nil {
		return entries, nil
	}
	if _, reconcileErr := ReconcileIncompleteReservations(ctx, request.InventoryRoot, request.MaxSnapshots, IncompleteReservationGrace, true); reconcileErr != nil {
		return nil, errors.Join(err, fmt.Errorf("reconcile incomplete recovery reservations: %w", reconcileErr))
	}
	entries, _, retryErr := ReadInventoryTolerant(ctx, request.InventoryRoot, request.MaxSnapshots)
	if retryErr != nil {
		return nil, errors.Join(err, retryErr)
	}
	return entries, nil
}
