//go:build integration

package recovery

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

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
	// The managed mirror this represents ordinarily already tracks the
	// repository's base branch (the worker's workspace shares its object
	// store), which is what makes the delta bundle PrepareRecord/
	// PublishRetainedState just produced above restorable here.
	recoveryTestGit(t, host, "fetch", source, "main")
	request := RetentionRequest{Repository: host, RepositoryKey: record.RepositoryKey, RunID: record.RunID, IdentityTime: record.CreatedAt, RetainUntil: record.RetainUntil, InventoryRoot: inventory, CleanupRoots: []string{host}, MaxSnapshots: 1, MaxArchiveBytes: 1 << 20}
	// The host supplies its own run identity time and retention policy, not
	// whatever dates the worker placed in its otherwise valid envelope.
	request.IdentityTime = request.IdentityTime.Add(-time.Hour)
	request.RetainUntil = request.RetainUntil.Add(30 * 24 * time.Hour)
	fullRoot := t.TempDir()
	if err := os.Mkdir(filepath.Join(fullRoot, "occupied"), 0o700); err != nil {
		t.Fatal(err)
	}
	full := request
	full.InventoryRoot = fullRoot
	if _, _, err := AcceptArchive(ctx, bytes.NewReader(wire.Bytes()), full, retentionJournalFunc(func(journal.Event) error { t.Fatal("full inventory acknowledged intake"); return nil })); !errors.Is(err, ErrInventoryFull) {
		t.Fatalf("full inventory accepted intake: %v", err)
	}
	if refs := recoveryTestGit(t, host, "for-each-ref", "--format=%(refname)"); refs != "" {
		t.Fatalf("intake created refs before reserving capacity: %s", refs)
	}
	// Regression for #5211: publication used the configured cap while its
	// post-publication readers still treated 128 as full. Exercise the actual
	// archive intake path with 129 valid existing records under a cap of 500.
	largeHost, largeInventory := t.TempDir(), t.TempDir()
	recoveryTestGit(t, largeHost, "init", "--bare")
	recoveryTestGit(t, largeHost, "fetch", source, "main")
	for i := range 129 {
		seed := storageTestRecord()
		seed.RunID = fmt.Sprintf("existing-%03d", i)
		seed.SnapshotSHA = fmt.Sprintf("%040x", i+1)
		seed.Ref, err = RefForSnapshot(seed.RunID, seed.SnapshotSHA)
		if err != nil {
			t.Fatal(err)
		}
		seedInventoryRecord(t, largeInventory, seed)
	}
	large := request
	large.Repository = largeHost
	large.InventoryRoot = largeInventory
	large.CleanupRoots = []string{largeHost}
	large.MaxSnapshots = 500
	if _, _, err := AcceptArchive(ctx, bytes.NewReader(wire.Bytes()), large, retentionJournalFunc(func(journal.Event) error { return nil })); err != nil {
		t.Fatalf("publication with 129 of 500 configured slots: %v", err)
	}
	if entries, err := ReadInventory(ctx, largeInventory, 500); err != nil || len(entries) != 130 {
		t.Fatalf("post-publication inventory: entries=%d err=%v, want 130", len(entries), err)
	}
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
	if !got.CreatedAt.Equal(request.IdentityTime) || !got.RetainUntil.Equal(request.RetainUntil) {
		t.Fatal("intake trusted worker retention dates")
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

func TestIntegrationArchiveIntakeReusesCleanSnapshotAlreadyInCustody(t *testing.T) {
	testdep.Require(t, "git")
	ctx := context.Background()
	source, host, inventory := t.TempDir(), t.TempDir(), t.TempDir()
	recoveryTestGit(t, source, "init", "--initial-branch=main")
	recoveryTestGit(t, source, "commit", "--allow-empty", "-m", "base")
	recoveryTestGit(t, source, "checkout", "-b", "run")
	writeRestoreFixture(t, source, "implementation", "worker implementation")
	recoveryTestGit(t, source, "add", "implementation")
	recoveryTestGit(t, source, "commit", "-m", "work")
	recoveryTestGit(t, host, "init", "--bare")
	recoveryTestGit(t, host, "fetch", source, "main")

	template := storageTestRecord()
	request := RetentionRequest{
		Repository: host, RepositoryKey: template.RepositoryKey, RunID: template.RunID,
		IdentityTime: template.CreatedAt, RetainUntil: template.RetainUntil,
		InventoryRoot: inventory, CleanupRoots: []string{host},
		MaxSnapshots: 1, MaxArchiveBytes: 1 << 20,
	}
	envelope := func(identity time.Time) []byte {
		t.Helper()
		archive := t.TempDir()
		record, err := PrepareRecord(ctx, source, template.RepositoryKey, template.RunID, "main", identity, identity.Add(24*time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		retained, err := PublishRetainedState(ctx, source, archive, []string{source}, record, 1<<20)
		if err != nil {
			t.Fatal(err)
		}
		var wire bytes.Buffer
		if err := WriteArchiveEnvelope(ctx, filepath.Join(archive, BundleFileName), retained, 1<<20, &wire); err != nil {
			t.Fatal(err)
		}
		return wire.Bytes()
	}

	var acknowledgements []Record
	first, firstPath, err := AcceptArchive(ctx, bytes.NewReader(envelope(template.CreatedAt)), request, retentionJournalFunc(func(event journal.Event) error {
		records, err := RecordsFromEvents([]journal.Event{event}, request.RunID)
		if err != nil {
			t.Fatal(err)
		}
		acknowledgements = append(acknowledgements, records[0])
		return nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	second, secondPath, err := AcceptArchive(ctx, bytes.NewReader(envelope(template.CreatedAt.Add(time.Hour))), request, retentionJournalFunc(func(event journal.Event) error {
		records, err := RecordsFromEvents([]journal.Event{event}, request.RunID)
		if err != nil {
			t.Fatal(err)
		}
		acknowledgements = append(acknowledgements, records[0])
		return nil
	}))
	if err != nil {
		t.Fatalf("clean duplicate at capacity was not reused: %v", err)
	}
	if firstPath == "" || secondPath != firstPath || second != first {
		t.Fatalf("duplicate intake created a new record: first=%+v %q second=%+v %q", first, firstPath, second, secondPath)
	}
	entries, err := ReadInventory(ctx, inventory, 1)
	if err != nil || len(entries) != 1 || entries[0].Record != first {
		t.Fatalf("duplicate intake consumed another slot: entries=%+v err=%v", entries, err)
	}
	if len(acknowledgements) != 2 || acknowledgements[0] != first || acknowledgements[1] != first {
		t.Fatalf("duplicate intake acknowledged different custody: %+v", acknowledgements)
	}
}
