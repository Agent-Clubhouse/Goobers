package journal

import (
	"errors"
	"path/filepath"
	"testing"
)

func TestTryRecoverRefusesBusyWriterAndPublisherThenRecovers(t *testing.T) {
	run, root := newRun(t)
	t.Cleanup(func() { _ = run.Close() })
	dir := filepath.Join(root, testIdentity().RunID)
	if second, _, err := TryRecover(dir); !errors.Is(err, ErrRecoveryBusy) || second != nil {
		if second != nil {
			_ = second.Close()
		}
		t.Fatalf("busy writer not refused immediately: writer=%v err=%v", second, err)
	}
	if err := run.Close(); err != nil {
		t.Fatal(err)
	}
	publication, err := acquireRunPublicationLock(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { releaseJournalLock(publication) })
	if second, _, err := TryRecover(dir); !errors.Is(err, ErrRecoveryBusy) || second != nil {
		if second != nil {
			_ = second.Close()
		}
		t.Fatalf("busy publication not refused immediately: writer=%v err=%v", second, err)
	}
	releaseJournalLock(publication)
	publication = nil
	recovered, _, err := TryRecover(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = recovered.Close() }()
	if err := recovered.Append(Event{Type: EventRefTouched, ExternalRef: &ExternalRef{Provider: "github", Kind: "pull-request", ID: "9"}}); err != nil {
		t.Fatal(err)
	}
}
