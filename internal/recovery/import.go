package recovery

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// ImportSnapshotBundle imports a verified independent archive without fetching
// any remote or overwriting a branch. The record must come from trusted
// recovery metadata, not from hashing an untrusted file on demand. The source
// is copied into private staging and checked before Git reads it, so a later
// replacement of the source path cannot change the imported bytes.
func ImportSnapshotBundle(ctx context.Context, repository, path string, record Record, maxBytes int64) error {
	return withVerifiedArchive(ctx, path, record, maxBytes, func(staged string) error {
		var heads snapshotPathOutput
		if err := recoveryGit(ctx, repository, &heads, "-c", "transfer.fsckObjects=true", "bundle", "unbundle", staged); err != nil {
			return fmt.Errorf("import recovery objects: %w", err)
		}
		if strings.TrimSpace(heads.String()) != record.SnapshotSHA+" "+record.Ref {
			return fmt.Errorf("imported recovery identity does not match record")
		}
		return PinCommit(ctx, repository, record)
	})
}

func withVerifiedArchive(ctx context.Context, path string, record Record, maxBytes int64, consume func(string) error) error {
	if err := record.Validate(); err != nil {
		return err
	}
	if record.ArchiveBytes > maxBytes {
		return fmt.Errorf("recorded recovery archive exceeds byte budget")
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
		return fmt.Errorf("recovery archive digest mismatch")
	}
	if info, err := os.Stat(staged); err != nil || info.Size() != record.ArchiveBytes {
		return fmt.Errorf("recovery archive size does not match record")
	}
	if err := verifyBundleHeader(staged, record); err != nil {
		return err
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
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = file.Close() }()
	header, err := io.ReadAll(io.LimitReader(file, 4096))
	if err != nil {
		return err
	}
	end := bytes.Index(header, []byte("\n\n"))
	if end < 0 || string(header[:end+2]) != expectedBundleHeader(record) {
		return fmt.Errorf("recovery bundle does not contain the expected self-contained snapshot")
	}
	return nil
}
