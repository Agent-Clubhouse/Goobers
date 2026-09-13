package recovery

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
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
	entries, err := ReadInventory(ctx, request.InventoryRoot, request.MaxSnapshots)
	if err != nil {
		repaired, repairErr := repairAbandonedReservation(ctx, request, prepared)
		if repairErr != nil {
			return Record{}, "", errors.Join(err, fmt.Errorf("repair matching recovery reservation: %w", repairErr))
		}
		if !repaired {
			return Record{}, "", err
		}
		// Preserve ReadInventory's all-or-nothing contract. Repairing this
		// exact reservation must not hide a second partial or corrupt entry.
		entries, err = ReadInventory(ctx, request.InventoryRoot, request.MaxSnapshots)
		if err != nil {
			return Record{}, "", err
		}
	}
	for _, entry := range entries {
		prior := entry.Record
		if prior.RunID != prepared.RunID || prior.RepositoryKey != prepared.RepositoryKey || prior.SnapshotSHA != prepared.SnapshotSHA {
			continue
		}
		if prior.Ref != prepared.Ref || prior.BaseSHA != prepared.BaseSHA || prior.PatchDigest != prepared.PatchDigest {
			return Record{}, "", ErrRecordConflict
		}
		// The same prepared commit can cross the stage-to-terminal boundary.
		// Keep its immutable capture identity and bytes; renew only the sidecar.
		prepared = prior
		break
	}
	_, path, err := PublishToInventoryWithEviction(ctx, request.Repository, request.InventoryRoot, request.CleanupRoots, prepared, request.MaxSnapshots, request.MaxArchiveBytes, request.EvictFull)
	if err != nil {
		return Record{}, "", err
	}
	renewed, err := RenewRetention(ctx, path, request.RetainUntil, request.MaxArchiveBytes)
	return renewed, path, err
}

// repairAbandonedReservation completes only the deterministic reservation for
// the prepared snapshot. A crash may leave its verified bundle durable before
// record.json is renamed into place. The live, exclusively owned preparation
// supplies that missing identity, and PublishToInventory verifies any existing
// archive before publishing metadata. Foreign and corrupt reservations remain
// untouched and keep the subsequent whole-inventory read fail-closed.
func repairAbandonedReservation(ctx context.Context, request RetentionRequest, prepared Record) (bool, error) {
	directory := filepath.Join(request.InventoryRoot, inventoryDirectoryName(prepared))
	info, err := os.Lstat(directory)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil || !info.IsDir() {
		return false, nil
	}
	if _, err := ReadRecord(filepath.Join(directory, RecordFileName)); err == nil || !errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	_, _, err = PublishToInventoryWithEviction(
		ctx,
		request.Repository,
		request.InventoryRoot,
		request.CleanupRoots,
		prepared,
		request.MaxSnapshots,
		request.MaxArchiveBytes,
		request.EvictFull,
	)
	return err == nil, err
}
