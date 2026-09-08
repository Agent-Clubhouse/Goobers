//go:build integration

package recovery

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/test/testsupport/testdep"
)

func TestIntegrationArchiveIntakeRequiresVerifiedDurableAcknowledgement(t *testing.T) {
	testdep.Require(t, "git")
	ctx := context.Background()
	source, archive, host, inventory := t.TempDir(), t.TempDir(), t.TempDir(), t.TempDir()
	recoveryTestGit(t, source, "init", "--initial-branch=main")
	recoveryTestGit(t, source, "commit", "--allow-empty", "-m", "base")
	writeRestoreFixture(t, source, "implementation", "worker implementation")
	template := storageTestRecord()
	prepared, err := PrepareRecord(ctx, source, template.RepositoryKey, template.RunID, "main", template.CreatedAt, template.RetainUntil)
	if err != nil {
		t.Fatal(err)
	}
	record, err := PublishRetainedState(ctx, source, archive, []string{source}, prepared, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	var wire bytes.Buffer
	if err := WriteArchiveEnvelope(ctx, filepath.Join(archive, BundleFileName), record, 1<<20, &wire); err != nil {
		t.Fatal(err)
	}
	recoveryTestGit(t, host, "init", "--bare")
	request := RetentionRequest{Repository: host, RepositoryKey: record.RepositoryKey, RunID: record.RunID, IdentityTime: record.CreatedAt, RetainUntil: record.RetainUntil, InventoryRoot: inventory, CleanupRoots: []string{host}, MaxSnapshots: 1, MaxArchiveBytes: 1 << 20}
	denied := errors.New("journal acknowledgement failed")
	got, path, err := AcceptArchive(ctx, bytes.NewReader(wire.Bytes()), request, retentionJournalFunc(func(journal.Event) error { return denied }))
	if !errors.Is(err, denied) || got != (Record{}) || path != "" {
		t.Fatalf("false acknowledgement: %+v %q %v", got, path, err)
	}
	for range 2 {
		got, path, err = AcceptArchive(ctx, bytes.NewReader(wire.Bytes()), request, retentionJournalFunc(func(event journal.Event) error {
			if event.RunID != request.RunID || event.Runner["recoveryCapture"] != true {
				t.Fatalf("custody event: %+v", event)
			}
			return nil
		}))
		if err != nil {
			t.Fatal(err)
		}
	}
	entries, err := ReadInventory(ctx, inventory, 1)
	if err != nil || len(entries) != 1 {
		t.Fatalf("retry inventory: %v %v", entries, err)
	}
	if got.SnapshotSHA != record.SnapshotSHA || got.PatchDigest != record.PatchDigest {
		t.Fatal("host changed implementation identity")
	}
	if data := recoveryTestGit(t, host, "show", got.Ref+":implementation"); data != "worker implementation" {
		t.Fatalf("host pin: %q", data)
	}
	destination := t.TempDir()
	recoveryTestGit(t, destination, "init", "--bare")
	if err := ImportSnapshotBundle(ctx, destination, filepath.Join(filepath.Dir(path), BundleFileName), got, 1<<20); err != nil {
		t.Fatal(err)
	}
}
