//go:build integration

package recovery

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/test/testsupport/testdep"
)

func TestIntegrationParentRetentionRestoresOverflowAndPromotion(t *testing.T) {
	testdep.Require(t, "git")
	repo, key, _ := childSnapshotFixture(t)
	at := time.Now().UTC()
	base := recoveryTestGit(t, repo, "rev-parse", "HEAD")
	childSnapshotWrite(t, repo, "tracked.txt", "staged\n")
	recoveryTestGit(t, repo, "add", "tracked.txt")
	childSnapshotWrite(t, repo, "tracked.txt", "dirty\n")
	request := RetentionRequest{Repository: repo, RepositoryKey: key, RunID: "occupant", BaseRef: base, IdentityTime: at, RetainUntil: at.Add(time.Hour), InventoryRoot: t.TempDir(), OverflowRoot: t.TempDir(), CleanupRoots: []string{repo}, MaxSnapshots: 1, MaxArchiveBytes: 1 << 20}
	overflowed := false
	log := retentionJournalFunc(func(event journal.Event) error {
		overflowed = event.Runner["recoveryOverflow"] == true
		return nil
	})
	if _, _, err := Retain(t.Context(), request, log); err != nil {
		t.Fatal(err)
	}
	request.RunID, request.ParentPolicy = "parent", &SnapshotPolicy{}
	record, recordPath, err := Retain(t.Context(), request, log)
	if err != nil || !overflowed || record.ArchiveDigest != "" {
		t.Fatal("full inventory did not use overflow", record, overflowed, err)
	}
	load := func(root string, expected Record) error {
		state, err := LoadRetainedParentState(t.Context(), repo, root, request.OverflowRoot, expected, request.MaxArchiveBytes)
		if err == nil && state.HeadSHA != base {
			t.Fatal("overflow changed original HEAD", state.HeadSHA)
		}
		return err
	}
	if err := load(request.InventoryRoot, record); err != nil {
		t.Fatal("load overflow parent", err)
	}
	recoveryTestGit(t, repo, "update-ref", "-d", record.Ref)
	if err := load(request.InventoryRoot, record); !errors.Is(err, ErrOverflowRefUnresolved) {
		t.Fatal("missing overflow pin admitted", err)
	}
	if err := PinCommit(t.Context(), repo, record); err != nil {
		t.Fatal(err)
	}
	verifyParentRetentionMetadataRefusals(t, recordPath, record, func() error { return load(request.InventoryRoot, record) })
	recoveryTestGit(t, repo, "reset", "--hard", base)
	plan, err := PlanRetainedParentRestore(t.Context(), repo, record, "overflow-restore", request.MaxArchiveBytes)
	if err != nil {
		t.Fatal(err)
	}
	if err := ApplyChildApplication(t.Context(), repo, plan); err != nil {
		t.Fatal(err)
	}
	if got := recoveryTestGit(t, repo, "show", ":tracked.txt"); got != "staged" {
		t.Fatal("overflow lost index", got)
	}
	if got, err := os.ReadFile(filepath.Join(repo, "tracked.txt")); err != nil || string(got) != "dirty\n" {
		t.Fatal("overflow lost working state", string(got), err)
	}
	promotedRoot := t.TempDir()
	promoted, promotedPath, err := PublishToInventoryWithEviction(t.Context(), repo, promotedRoot, []string{repo}, record, 1, request.MaxArchiveBytes, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := load(request.InventoryRoot, promoted); err == nil {
		t.Fatal("bundled receipt silently downgraded to overflow")
	}
	// A declared bundle which is missing/corrupt must not be bypassed using
	// a still-present overflow record left behind by interrupted promotion.
	bundle := filepath.Join(filepath.Dir(promotedPath), BundleFileName)
	if err := os.Rename(bundle, bundle+".missing"); err != nil {
		t.Fatal(err)
	}
	err = load(promotedRoot, record)
	if restoreErr := os.Rename(bundle+".missing", bundle); restoreErr != nil {
		t.Fatal(restoreErr)
	}
	if err == nil {
		t.Fatal("corrupt promotion bypassed bundle verification")
	}
	if err := DeleteOverflowEntry(request.OverflowRoot, record); err != nil {
		t.Fatal(err)
	}
	recoveryTestGit(t, repo, "update-ref", "-d", record.Ref)
	if err := load(promotedRoot, record); err != nil || !HasSnapshotRef(t.Context(), repo, record) {
		t.Fatal("promoted overflow did not import verified bundle", err)
	}
}

func verifyParentRetentionMetadataRefusals(t *testing.T, path string, original Record, load func() error) {
	t.Helper()
	for _, mode := range []string{"expired", "identity", "malformed-tier"} {
		changed := original
		switch mode {
		case "expired":
			changed.CreatedAt, changed.RetainUntil = time.Now().Add(-2*time.Hour), time.Now().Add(-time.Hour)
		case "identity":
			changed.PatchDigest = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		case "malformed-tier":
			changed.ArchiveFormat = archiveFormatFull
		}
		data, err := json.Marshal(changed)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
		if err := load(); err == nil {
			t.Fatal(mode, "retention substitution admitted")
		}
	}
	data, err := json.Marshal(original)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
}

func TestIntegrationParentRetentionHonorsRenewalSidecar(t *testing.T) {
	testdep.Require(t, "git")
	repo, key, _ := childSnapshotFixture(t)
	at := time.Now().UTC().Add(-2 * time.Hour)
	base := recoveryTestGit(t, repo, "rev-parse", "HEAD")
	childSnapshotWrite(t, repo, "tracked.txt", "retained work\n")
	request := RetentionRequest{Repository: repo, RepositoryKey: key, RunID: "renewed-parent", BaseRef: base, IdentityTime: at, RetainUntil: at.Add(time.Hour), InventoryRoot: t.TempDir(), OverflowRoot: t.TempDir(), CleanupRoots: []string{repo}, MaxSnapshots: 1, MaxArchiveBytes: 1 << 20, ParentPolicy: &SnapshotPolicy{}}
	record, path, err := Retain(t.Context(), request, retentionJournalFunc(func(journal.Event) error { return nil }))
	if err != nil {
		t.Fatal(err)
	}
	load := func() error {
		_, err := LoadRetainedParentState(t.Context(), repo, request.InventoryRoot, request.OverflowRoot, record, request.MaxArchiveBytes)
		return err
	}
	if err := load(); err == nil {
		t.Fatal("expired unchanged capture admitted")
	}
	if _, err := RenewRetention(t.Context(), path, time.Now().Add(time.Hour), request.MaxArchiveBytes); err != nil {
		t.Fatal(err)
	}
	if err := load(); err != nil {
		t.Fatal("verified renewal sidecar ignored", err)
	}
}
