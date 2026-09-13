package recovery

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// errRestoreArchiveInvalid, errRestoreBaseMissing and errRestoreImportFailed
// classify ImportSnapshotBundle failures into the bounded
// RestoreFailureReason vocabulary (#5028) via errors.Is, without changing
// any returned error's message.
var (
	errRestoreArchiveInvalid = errors.New("recovery restore archive invalid")
	errRestoreBaseMissing    = errors.New("recovery restore base commit missing")
	errRestoreImportFailed   = errors.New("recovery restore import failed")
)

// ImportSnapshotBundle imports a verified independent archive without fetching
// any remote or overwriting a branch. The record must come from trusted
// recovery metadata, not from hashing an untrusted file on demand. The source
// is copied into private staging and checked before Git reads it, so a later
// replacement of the source path cannot change the imported bytes.
func ImportSnapshotBundle(ctx context.Context, repository, path string, record Record, maxBytes int64) (err error) {
	if observer := observeSnapshot(ctx); observer != nil {
		defer func() {
			if err != nil {
				observer.SnapshotRestoreFailed(string(classifyRestoreFailure(err)))
			}
		}()
	}
	return withVerifiedArchive(ctx, path, record, maxBytes, func(staged string) error {
		if record.archiveFormat() == archiveFormatDelta {
			if err := recoveryGit(ctx, repository, io.Discard, "cat-file", "-e", record.BaseSHA+"^{commit}"); err != nil {
				return fmt.Errorf("recovery delta bundle requires its base commit %s to already be present in %q: %w: %w", record.BaseSHA, repository, errRestoreBaseMissing, err)
			}
		}
		var heads snapshotPathOutput
		if err := recoveryGit(ctx, repository, &heads, "-c", "transfer.fsckObjects=true", "bundle", "unbundle", staged); err != nil {
			return fmt.Errorf("import recovery objects: %w: %w", errRestoreImportFailed, err)
		}
		if strings.TrimSpace(heads.String()) != record.SnapshotSHA+" "+record.Ref {
			return fmt.Errorf("imported recovery identity does not match record: %w", errRestoreImportFailed)
		}
		if err := PinCommit(ctx, repository, record); err != nil {
			return fmt.Errorf("%w: %w", errRestoreImportFailed, err)
		}
		return nil
	})
}

// classifyRestoreFailure maps an ImportSnapshotBundle error onto the bounded
// RestoreFailureReason vocabulary. An error matching none of the classified
// sentinels (an I/O failure staging the archive, for instance) still counts
// as an import failure: every ImportSnapshotBundle error is one class of
// restore failure or another.
func classifyRestoreFailure(err error) RestoreFailureReason {
	switch {
	case errors.Is(err, errRestoreBaseMissing):
		return RestoreFailureReasonBaseMissing
	case errors.Is(err, errRestoreArchiveInvalid):
		return RestoreFailureReasonArchiveInvalid
	default:
		return RestoreFailureReasonImportFailed
	}
}

func withVerifiedArchive(ctx context.Context, path string, record Record, maxBytes int64, consume func(string) error) error {
	if err := record.Validate(); err != nil {
		return fmt.Errorf("%w: %w", errRestoreArchiveInvalid, err)
	}
	if record.ArchiveBytes > maxBytes {
		return fmt.Errorf("recorded recovery archive exceeds byte budget: %w", errRestoreArchiveInvalid)
	}
	directory, err := os.MkdirTemp("", "goobers-recovery-import-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(directory) }()
	staged := filepath.Join(directory, "snapshot.bundle")
	digest, err := publishArchive(ctx, staged, maxBytes, func(w io.Writer) error {
		return copyRecoveryArchive(path, w)
	})
	if err != nil {
		return err
	}
	if digest != record.ArchiveDigest {
		return fmt.Errorf("recovery archive digest mismatch: %w", errRestoreArchiveInvalid)
	}
	if info, err := os.Stat(staged); err != nil || info.Size() != record.ArchiveBytes {
		return fmt.Errorf("recovery archive size does not match record: %w", errRestoreArchiveInvalid)
	}
	if err := verifyBundleHeader(staged, record); err != nil {
		return fmt.Errorf("%w: %w", errRestoreArchiveInvalid, err)
	}
	return consume(staged)
}

func copyRecoveryArchive(path string, destination io.Writer) error {
	before, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !before.Mode().IsRegular() {
		return fmt.Errorf("recovery archive is not a regular file")
	}
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = file.Close() }()
	after, err := file.Stat()
	if err != nil || !os.SameFile(before, after) {
		return fmt.Errorf("recovery archive changed while opening")
	}
	_, err = io.Copy(destination, file)
	return err
}

func verifyBundleHeader(path string, record Record) error {
	header, err := readBundleHeader(path)
	if err != nil {
		return err
	}
	return verifyBundleHeaderBytes(header, record, record.archiveFormat())
}

// readBundleHeader returns the bundle header including its terminating blank
// line, without buffering the rest of the (potentially large) archive.
func readBundleHeader(path string) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()
	header, err := io.ReadAll(io.LimitReader(file, 4096))
	if err != nil {
		return nil, err
	}
	end := bytes.Index(header, []byte("\n\n"))
	if end < 0 {
		return nil, fmt.Errorf("recovery bundle header not found")
	}
	return header[:end+2], nil
}

// inspectBundleFormat reports the format an already-written bundle file
// declares in its own header, for a crash-resumed publication that must
// record the format the earlier attempt actually chose (#4862). It infers
// shape only; the caller still verifies the header against the trusted
// record via verifyBundleHeader/ImportSnapshotBundle before relying on it.
func inspectBundleFormat(path string) (string, error) {
	header, err := readBundleHeader(path)
	if err != nil {
		return "", err
	}
	trimmed, ok := strings.CutSuffix(string(header), "\n\n")
	if !ok {
		return "", fmt.Errorf("recovery bundle header not found")
	}
	switch strings.Count(trimmed, "\n") {
	case 2:
		return archiveFormatFull, nil
	case 3:
		return archiveFormatDelta, nil
	default:
		return "", fmt.Errorf("recovery bundle header has an unexpected shape")
	}
}
