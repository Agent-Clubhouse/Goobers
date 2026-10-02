package recovery

import (
	"context"
	"io"
	"strings"
)

func acceptExistingCleanArchive(ctx context.Context, archivePath string, incoming Record, request RetentionRequest) (Record, string, bool, error) {
	candidates, err := ReadInventory(ctx, request.InventoryRoot, request.MaxSnapshots)
	if err != nil {
		// Reuse is only an optimization. Preserve the historical intake path so
		// capacity reclamation and its exact errors still run when the inventory
		// cannot be scanned completely.
		return Record{}, "", false, nil
	}
	candidates = matchingCleanArchiveCandidates(candidates, incoming)
	if len(candidates) == 0 {
		return Record{}, "", false, nil
	}
	if err := ImportSnapshotBundle(ctx, request.Repository, archivePath, incoming, request.MaxArchiveBytes); err != nil {
		return Record{}, "", false, err
	}
	defer func() {
		_ = deleteRecoveryRef(context.WithoutCancel(ctx), request.Repository, incoming.Ref, incoming.SnapshotSHA)
	}()
	incomingParent, incomingClean, err := cleanSnapshotParent(ctx, request.Repository, incoming.SnapshotSHA)
	if err != nil {
		return Record{}, "", false, err
	}
	if !incomingClean {
		return Record{}, "", false, nil
	}
	for _, candidate := range candidates {
		parent, clean, err := cleanSnapshotParent(ctx, request.Repository, candidate.Record.SnapshotSHA)
		if err != nil || !clean || parent != incomingParent {
			continue
		}
		return candidate.Record, candidate.RecordPath, true, nil
	}
	return Record{}, "", false, nil
}

func matchingCleanArchiveCandidates(entries []InventoryEntry, incoming Record) []InventoryEntry {
	var candidates []InventoryEntry
	for _, entry := range entries {
		record := entry.Record
		if record.RunID == incoming.RunID &&
			record.RepositoryKey == incoming.RepositoryKey &&
			record.BaseSHA == incoming.BaseSHA &&
			record.PatchDigest == incoming.PatchDigest &&
			record.SnapshotSHA != incoming.SnapshotSHA {
			candidates = append(candidates, entry)
		}
	}
	return candidates
}

func cleanSnapshotParent(ctx context.Context, repository, snapshot string) (string, bool, error) {
	var line boundedRefOutput
	if err := recoveryGit(ctx, repository, &line, "rev-list", "--parents", "-n", "1", snapshot); err != nil {
		return "", false, err
	}
	fields := strings.Fields(line.String())
	if len(fields) != 2 || fields[0] != snapshot || !gitObjectID.MatchString(fields[1]) {
		return "", false, nil
	}
	var snapshotTree, parentTree boundedRefOutput
	if err := recoveryGit(ctx, repository, &snapshotTree, "rev-parse", snapshot+"^{tree}"); err != nil {
		return "", false, err
	}
	if err := recoveryGit(ctx, repository, &parentTree, "rev-parse", fields[1]+"^{tree}"); err != nil {
		return "", false, err
	}
	return fields[1], strings.TrimSpace(snapshotTree.String()) == strings.TrimSpace(parentTree.String()), nil
}

func deleteRecoveryRef(ctx context.Context, repository, ref, old string) error {
	if ref == "" || old == "" {
		return nil
	}
	return recoveryGit(ctx, repository, io.Discard, "update-ref", "--no-deref", "-d", ref, old)
}
