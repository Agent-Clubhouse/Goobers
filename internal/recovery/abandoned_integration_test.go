//go:build integration

package recovery

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/test/testsupport/testdep"
)

func TestIntegrationAbandonedPreparationRequiresDurableHandoff(t *testing.T) {
	testdep.Require(t, "git")
	repository, inventory := t.TempDir(), t.TempDir()
	recoveryTestGit(t, repository, "init", "--initial-branch=receiving")
	recoveryTestGit(t, repository, "commit", "--allow-empty", "-m", "base")
	base := recoveryTestGit(t, repository, "rev-parse", "HEAD")
	template := storageTestRecord()
	branch, err := PreparedRestoreBranch(template.RunID)
	if err != nil {
		t.Fatal(err)
	}
	recoveryTestGit(t, repository, "checkout", "-b", branch)
	writeRestoreFixture(t, repository, "implementation", "retained work")
	recoveryTestGit(t, repository, "add", "implementation")
	recoveryTestGit(t, repository, "commit", "-m", "prepared")
	commit := recoveryTestGit(t, repository, "rev-parse", "HEAD")
	recoveryTestGit(t, repository, "checkout", "receiving")
	request := RetentionRequest{Repository: repository, RepositoryKey: template.RepositoryKey, RunID: template.RunID, IdentityTime: template.CreatedAt, RetainUntil: template.RetainUntil, InventoryRoot: inventory, CleanupRoots: []string{repository}, MaxSnapshots: 1, MaxArchiveBytes: 1 << 20}
	blocked := errors.New("publication journal unavailable")
	log := retentionJournalFunc(func(journal.Event) error { return blocked })
	if err := RetainAbandonedPreparation(context.Background(), request, log); !errors.Is(err, blocked) {
		t.Fatalf("journal failure ignored: %v", err)
	}
	if got := recoveryTestGit(t, repository, "rev-parse", "refs/heads/"+branch); got != commit {
		t.Fatal("preparation deleted before acknowledgement")
	}
	entries, err := ReadInventory(context.Background(), inventory, 1)
	if err != nil || len(entries) != 1 {
		t.Fatalf("inspect interrupted publication fixture: %+v %v", entries, err)
	}
	if err := os.Remove(entries[0].RecordPath); err != nil {
		t.Fatal(err)
	}
	if err := RetainAbandonedPreparation(context.Background(), request, log); !errors.Is(err, blocked) {
		t.Fatalf("missing record prevented deterministic publication repair: %v", err)
	}
	if repaired, err := ReadInventory(context.Background(), inventory, 1); err != nil || len(repaired) != 1 {
		t.Fatalf("repaired inventory = %+v, error %v", repaired, err)
	}
	if got := recoveryTestGit(t, repository, "rev-parse", "refs/heads/"+branch); got != commit {
		t.Fatal("preparation deleted before repaired publication acknowledgement")
	}
	foreign := filepath.Join(inventory, "foreign-partial")
	if err := os.Mkdir(foreign, 0o700); err != nil {
		t.Fatal(err)
	}
	request.MaxSnapshots = 2
	if err := RetainAbandonedPreparation(context.Background(), request, log); err == nil || errors.Is(err, blocked) {
		t.Fatalf("foreign partial reservation did not remain fail-closed: %v", err)
	}
	if err := os.Remove(foreign); err != nil {
		t.Fatal(err)
	}
	request.MaxSnapshots = 1
	// Terminal cleanup may retry a failed stage acknowledgement with a new
	// capture window. Reuse immutable custody metadata and extend retention.
	request.IdentityTime = request.IdentityTime.Add(time.Hour)
	request.RetainUntil = request.RetainUntil.Add(30 * 24 * time.Hour)
	log = retentionJournalFunc(func(event journal.Event) error {
		if event.Runner["recoveryCapture"] != true {
			t.Fatal("handoff lost capture ordering")
		}
		return nil
	})
	remoteFailure := errors.New("remote custody unavailable")
	request.AcknowledgeArchive = func(_ context.Context, record Record, archive string) error {
		if record.SnapshotSHA != commit || filepath.Base(archive) != BundleFileName {
			t.Fatalf("invalid remote handoff: %+v %s", record, archive)
		}
		return remoteFailure
	}
	if err := RetainAbandonedPreparation(context.Background(), request, log); !errors.Is(err, remoteFailure) {
		t.Fatalf("remote custody failure ignored: %v", err)
	}
	if got := recoveryTestGit(t, repository, "rev-parse", "refs/heads/"+branch); got != commit {
		t.Fatal("preparation deleted before remote acknowledgement")
	}
	acknowledged := false
	request.AcknowledgeArchive = func(context.Context, Record, string) error {
		acknowledged = true
		return nil
	}
	if err := RetainAbandonedPreparation(context.Background(), request, log); err != nil {
		t.Fatal(err)
	}
	if !acknowledged {
		t.Fatal("cleanup bypassed remote acknowledgement")
	}
	if err := RetainAbandonedPreparation(context.Background(), request, log); err != nil {
		t.Fatalf("cleanup retry: %v", err)
	}
	entries, err = ReadInventory(context.Background(), inventory, 1)
	if err != nil || len(entries) != 1 {
		t.Fatalf("bounded retained handoff: %d %v", len(entries), err)
	}
	effective, err := ReadRetainedRecord(entries[0].RecordPath)
	if err != nil || !effective.RetainUntil.Equal(request.RetainUntil) {
		t.Fatalf("terminal recovery window was not renewed: %+v %v", effective, err)
	}
	destination := t.TempDir()
	recoveryTestGit(t, destination, "init", "--initial-branch=main")
	if err := ImportSnapshotBundle(context.Background(), destination, filepath.Join(filepath.Dir(entries[0].RecordPath), BundleFileName), entries[0].Record, 1<<20); err != nil {
		t.Fatal(err)
	}
	if got := recoveryTestGit(t, destination, "show", entries[0].Record.Ref+":implementation"); got != "retained work" {
		t.Fatalf("archive lost implementation: %q", got)
	}
	if recoveryTestGit(t, repository, "rev-parse", "HEAD") != base {
		t.Fatal("handoff changed receiving branch")
	}
}
