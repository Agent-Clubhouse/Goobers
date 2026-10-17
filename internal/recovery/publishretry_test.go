package recovery

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	platformlock "github.com/goobers/goobers/internal/platform/lock"
)

func TestResumeArchivePublicationBindsOriginalArchive(t *testing.T) {
	for _, format := range []string{archiveFormatFull, archiveFormatDelta} {
		t.Run(format, func(t *testing.T) {
			directory, prepared, existing, archive := retryTestArchive(t, format)
			repository := t.TempDir()
			calls := 0
			importArchive := func(ctx context.Context, repo, path string, record Record, maxBytes int64) error {
				calls++
				if ctx != t.Context() || repo != repository || path != filepath.Join(directory, BundleFileName) || record != existing || maxBytes != 1<<20 {
					t.Fatalf("incorrect import arguments: %q %q %+v %d", repo, path, record, maxBytes)
				}
				assertRetryEvidence(t, directory, archive, nil)
				return retryTestVerifyArchive(ctx, repo, path, record, maxBytes)
			}
			got, err := resumeArchivePublicationWithImporter(t.Context(), repository, directory, prepared, 1<<20, importArchive)
			if err != nil || got != existing || calls != 1 {
				t.Fatalf("resume = %+v, %v; import calls = %d", got, err, calls)
			}
			assertRetryEvidence(t, directory, archive, &existing)
		})
	}
}

func TestRepublishRetainedStatePreservesPublishedIdentity(t *testing.T) {
	for _, format := range []string{"", archiveFormatFull, archiveFormatDelta} {
		t.Run("format="+format, func(t *testing.T) {
			directory, prepared, existing, archive := retryTestArchive(t, format)
			if err := PublishRecord(filepath.Join(directory, RecordFileName), existing); err != nil {
				t.Fatal(err)
			}
			// Equivalent timestamps and freshly computed transport fields must
			// not replace the exact identity already published before the crash.
			zone := time.FixedZone("retry", 3600)
			prepared.CreatedAt = prepared.CreatedAt.In(zone)
			prepared.RetainUntil = prepared.RetainUntil.In(zone)
			prepared.ArchiveDigest, prepared.ArchiveBytes, prepared.ArchiveFormat = "new packing", 42, "new format"
			calls := 0
			importArchive := func(ctx context.Context, repo, path string, record Record, maxBytes int64) error {
				calls++
				if record != existing {
					t.Fatalf("retry verified changed identity: %+v", record)
				}
				assertRetryEvidence(t, directory, archive, &existing)
				return retryTestVerifyArchive(ctx, repo, path, record, maxBytes)
			}
			got, err := republishRetainedStateWithImporter(t.Context(), t.TempDir(), directory, prepared, existing, 1<<20, importArchive)
			if err != nil || got != existing || calls != 1 {
				t.Fatalf("republish = %+v, %v; import calls = %d", got, err, calls)
			}
			assertRetryEvidence(t, directory, archive, &existing)
		})
	}
}

func TestRepublishRetainedStateRejectsIdentityConflicts(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*Record)
	}{
		{"capture time", func(r *Record) { r.CreatedAt = r.CreatedAt.Add(time.Second) }},
		{"retention deadline", func(r *Record) { r.RetainUntil = r.RetainUntil.Add(time.Second) }},
		{"version", func(r *Record) { r.Version++ }},
		{"owner", func(r *Record) { r.RunID = "another-run" }},
		{"repository", func(r *Record) { r.RepositoryKey = "github|||team|other|" }},
		{"ref", func(r *Record) { r.Ref = "refs/goobers/recovery/another-run" }},
		{"base ref", func(r *Record) { r.BaseRef = "refs/heads/other" }},
		{"base commit", func(r *Record) { r.BaseSHA = strings.Repeat("e", 40) }},
		{"snapshot commit", func(r *Record) { r.SnapshotSHA = strings.Repeat("e", 40) }},
		{"patch", func(r *Record) { r.PatchDigest = "sha256:" + strings.Repeat("e", 64) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			directory, prepared, existing, archive := retryTestArchive(t, archiveFormatFull)
			if err := PublishRecord(filepath.Join(directory, RecordFileName), existing); err != nil {
				t.Fatal(err)
			}
			tc.change(&prepared)
			importArchive := func(context.Context, string, string, Record, int64) error {
				t.Fatal("conflicting identity reached archive import")
				return nil
			}
			got, err := republishRetainedStateWithImporter(t.Context(), t.TempDir(), directory, prepared, existing, 1<<20, importArchive)
			if !errors.Is(err, ErrRecordConflict) || got != (Record{}) {
				t.Fatalf("conflicting identity acknowledged: %+v %v", got, err)
			}
			assertRetryEvidence(t, directory, archive, &existing)
		})
	}
}

func TestResumeArchivePublicationRejectsInvalidEvidence(t *testing.T) {
	for _, name := range []string{"missing", "directory", "over budget", "missing header", "unexpected header shape", "wrong snapshot"} {
		t.Run(name, func(t *testing.T) {
			directory, prepared, _, archive := retryTestArchive(t, archiveFormatFull)
			path := filepath.Join(directory, BundleFileName)
			maxBytes := int64(1 << 20)
			switch name {
			case "missing", "directory":
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				if name == "directory" {
					if err := os.Mkdir(path, 0o700); err != nil {
						t.Fatal(err)
					}
				}
			case "over budget":
				maxBytes = int64(len(archive) - 1)
			case "missing header":
				archive = "interrupted header"
			case "unexpected header shape":
				archive = "# v3 git bundle\n\n"
			case "wrong snapshot":
				prepared.SnapshotSHA = strings.Repeat("e", 40)
			}
			if name != "missing" && name != "directory" {
				if err := os.WriteFile(path, []byte(archive), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			// These invalid inputs must fail in the production path before Git.
			got, err := resumeArchivePublication(t.Context(), t.TempDir(), directory, prepared, maxBytes)
			if err == nil || got != (Record{}) {
				t.Fatalf("invalid evidence acknowledged: %+v %v", got, err)
			}
			if _, err := os.Lstat(filepath.Join(directory, RecordFileName)); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("unverified archive received metadata: %v", err)
			}
			if name != "missing" && name != "directory" {
				assertRetryEvidence(t, directory, archive, nil)
			}
		})
	}
}

func TestRepublishRetainedStateRejectsInvalidArchiveBinding(t *testing.T) {
	for _, name := range []string{"digest", "size", "format", "budget"} {
		t.Run(name, func(t *testing.T) {
			directory, prepared, existing, archive := retryTestArchive(t, archiveFormatFull)
			maxBytes := int64(1 << 20)
			switch name {
			case "digest":
				existing.ArchiveDigest = "sha256:" + strings.Repeat("0", 64)
			case "size":
				existing.ArchiveBytes++
			case "format":
				existing.ArchiveFormat = archiveFormatDelta
			case "budget":
				maxBytes = existing.ArchiveBytes - 1
			}
			if err := PublishRecord(filepath.Join(directory, RecordFileName), existing); err != nil {
				t.Fatal(err)
			}
			got, err := republishRetainedState(t.Context(), t.TempDir(), directory, prepared, existing, maxBytes)
			if !errors.Is(err, errRestoreArchiveInvalid) || got != (Record{}) {
				t.Fatalf("invalid binding acknowledged: %+v %v", got, err)
			}
			assertRetryEvidence(t, directory, archive, &existing)
		})
	}
}

func TestResumeArchivePublicationRetriesAfterMetadataFailure(t *testing.T) {
	directory, prepared, existing, archive := retryTestArchive(t, archiveFormatDelta)
	lock, err := platformlock.TryAcquire(filepath.Join(directory, RecordFileName) + ".lock")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = lock.Release() })
	got, err := resumeArchivePublicationWithImporter(t.Context(), t.TempDir(), directory, prepared, 1<<20, retryTestVerifyArchive)
	if !errors.Is(err, platformlock.ErrHeld) || got != (Record{}) {
		t.Fatalf("metadata failure acknowledged: %+v %v", got, err)
	}
	assertRetryEvidence(t, directory, archive, nil)
	if err := lock.Release(); err != nil {
		t.Fatal(err)
	}
	got, err = resumeArchivePublicationWithImporter(t.Context(), t.TempDir(), directory, prepared, 1<<20, retryTestVerifyArchive)
	if err != nil || got != existing {
		t.Fatalf("retry after metadata failure = %+v, %v", got, err)
	}
	assertRetryEvidence(t, directory, archive, &existing)
}

func TestRepublishRetainedStatePreservesEvidenceOnFailure(t *testing.T) {
	importFailure := errors.New("snapshot verification failed")
	for _, name := range []string{"import", "archive lock", "metadata lock", "cancel after import", "archive changed after import"} {
		t.Run(name, func(t *testing.T) {
			directory, prepared, existing, archive := retryTestArchive(t, archiveFormatFull)
			if err := PublishRecord(filepath.Join(directory, RecordFileName), existing); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			var wantErr error
			switch name {
			case "import":
				wantErr = importFailure
			case "archive lock", "metadata lock":
				filename := BundleFileName
				if name == "metadata lock" {
					filename = RecordFileName
				}
				lock, err := platformlock.TryAcquire(filepath.Join(directory, filename) + ".lock")
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = lock.Release() })
				wantErr = platformlock.ErrHeld
			case "cancel after import":
				wantErr = context.Canceled
			}
			calls := 0
			importArchive := func(ctx context.Context, repo, path string, record Record, maxBytes int64) error {
				calls++
				if err := retryTestVerifyArchive(ctx, repo, path, record, maxBytes); err != nil {
					return err
				}
				switch name {
				case "import":
					return importFailure
				case "cancel after import":
					cancel()
				case "archive changed after import":
					archive += "changed after verification"
					return os.WriteFile(path, []byte(archive), 0o600)
				}
				return nil
			}
			got, err := republishRetainedStateWithImporter(ctx, t.TempDir(), directory, prepared, existing, 1<<20, importArchive)
			if err == nil || got != (Record{}) || calls != 1 {
				t.Fatalf("failed retry acknowledged: %+v %v; import calls = %d", got, err, calls)
			}
			if wantErr != nil && !errors.Is(err, wantErr) {
				t.Fatalf("retry error = %v, want %v", err, wantErr)
			}
			if name == "archive changed after import" && !strings.Contains(err.Error(), "retained archive changed during retry") {
				t.Fatalf("archive replacement was not detected: %v", err)
			}
			assertRetryEvidence(t, directory, archive, &existing)
		})
	}
}

func retryTestArchive(t *testing.T, format string) (string, Record, Record, string) {
	t.Helper()
	directory := t.TempDir()
	prepared := storageTestRecord()
	prepared.ArchiveDigest, prepared.ArchiveBytes, prepared.ArchiveFormat = "", 0, ""
	header := expectedBundleHeader(prepared)
	if format == archiveFormatDelta {
		// Multiple prerequisites are legal; the recorded base need not be first.
		header = "# v3 git bundle\n@object-format=sha1\n-" + strings.Repeat("e", 40) + " earlier boundary\n-" + prepared.BaseSHA + " base\n" + prepared.SnapshotSHA + " " + prepared.Ref + "\n\n"
	}
	archive := header + "opaque original pack bytes"
	if err := os.WriteFile(filepath.Join(directory, BundleFileName), []byte(archive), 0o600); err != nil {
		t.Fatal(err)
	}
	existing := prepared
	existing.ArchiveDigest = fmt.Sprintf("sha256:%x", sha256.Sum256([]byte(archive)))
	existing.ArchiveBytes, existing.ArchiveFormat = int64(len(archive)), format
	return directory, prepared, existing, archive
}

// Keep real staging, digest, size and header verification. Only Git object
// import/pinning is faked; actual Git bundle semantics remain integration tests.
func retryTestVerifyArchive(ctx context.Context, _ string, path string, record Record, maxBytes int64) error {
	return withVerifiedArchive(ctx, path, record, maxBytes, func(string) error { return nil })
}

func assertRetryEvidence(t *testing.T, directory, archive string, metadata *Record) {
	t.Helper()
	if got, err := os.ReadFile(filepath.Join(directory, BundleFileName)); err != nil || string(got) != archive {
		t.Fatalf("retained archive changed: %q %v", got, err)
	}
	got, err := ReadRecord(filepath.Join(directory, RecordFileName))
	if metadata == nil {
		if !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("metadata published before successful verification: %+v %v", got, err)
		}
	} else if err != nil || got != *metadata {
		t.Fatalf("retained metadata changed: %+v %v, want %+v", got, err, *metadata)
	}
}
