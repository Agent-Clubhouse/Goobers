package gagglehealth

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

func TestStorePersistsIsolatedProjectionsAndRebuildsOnRestart(t *testing.T) {
	now := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	root := t.TempDir()
	store, err := openStore(root, 24*time.Hour, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}

	alphaIdentity := apiv1.GaggleHealthIdentity{Gaggle: "alpha", Run: "run-alpha"}
	alphaKey, err := EpisodeKey(FindingNoProgress, alphaIdentity)
	if err != nil {
		t.Fatal(err)
	}
	alpha := finding(FindingNoProgress, alphaKey, alphaIdentity, now, apiv1.GaggleHealthStalled)
	if _, err := store.Append(event(1, now, apiv1.GaggleHealthFindingOpened, alpha)); err != nil {
		t.Fatal(err)
	}

	betaIdentity := apiv1.GaggleHealthIdentity{Gaggle: "beta", Workflow: "curation"}
	betaKey, err := EpisodeKey(FindingTriggerSilence, betaIdentity)
	if err != nil {
		t.Fatal(err)
	}
	beta := finding(FindingTriggerSilence, betaKey, betaIdentity, now, apiv1.GaggleHealthDegraded)
	if _, err := store.Append(event(2, now, apiv1.GaggleHealthFindingOpened, beta)); err != nil {
		t.Fatal(err)
	}

	alphaSnapshot, err := store.Snapshot("alpha")
	if err != nil {
		t.Fatal(err)
	}
	if len(alphaSnapshot.Active) != 1 || alphaSnapshot.Active[0].Identity.Gaggle != "alpha" || alphaSnapshot.LastSequence != 1 {
		t.Fatalf("alpha projection leaked another partition: %+v", alphaSnapshot)
	}
	if err := os.Remove(filepath.Join(root, "health", "gaggles", "alpha", "state.json")); err != nil {
		t.Fatal(err)
	}

	restarted, err := openStore(root, 24*time.Hour, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	rebuilt, err := restarted.Snapshot("alpha")
	if err != nil {
		t.Fatal(err)
	}
	if rebuilt.State != apiv1.GaggleHealthStalled || len(rebuilt.Active) != 1 {
		t.Fatalf("restart projection = %+v", rebuilt)
	}
	data, err := os.ReadFile(filepath.Join(root, "health", "gaggles", "alpha", "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	var persisted apiv1.GaggleHealthSnapshot
	if err := json.Unmarshal(data, &persisted); err != nil {
		t.Fatal(err)
	}
	if persisted.Gaggle != "alpha" || len(persisted.Active) != 1 {
		t.Fatalf("persisted projection = %+v", persisted)
	}
}

func TestStoreAppliesDurationBasedHistoryRetention(t *testing.T) {
	now := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	store, err := openStore(t.TempDir(), 24*time.Hour, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	identity := apiv1.GaggleHealthIdentity{Gaggle: "alpha", Run: "old-run"}
	key, err := EpisodeKey(FindingNoProgress, identity)
	if err != nil {
		t.Fatal(err)
	}
	openedAt := now.Add(-48 * time.Hour)
	opened := finding(FindingNoProgress, key, identity, openedAt, apiv1.GaggleHealthStalled)
	if _, err := store.Append(event(1, openedAt, apiv1.GaggleHealthFindingOpened, opened)); err != nil {
		t.Fatal(err)
	}
	resolvedAt := now.Add(-25 * time.Hour)
	resolved := opened
	resolved.ResolvedAt = &resolvedAt
	resolved.ResolutionEvidence = []apiv1.GaggleHealthEvidence{{Kind: "journal-event", Run: identity.Run, Sequence: 2}}
	resolved.Repair.FollowUp = apiv1.GaggleHealthFollowUpResolved
	snapshot, err := store.Append(event(2, resolvedAt, apiv1.GaggleHealthFindingResolved, resolved))
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Active) != 0 || len(snapshot.History) != 0 {
		t.Fatalf("expired history was retained: %+v", snapshot)
	}
}

func TestStoreRejectsJournalSequenceGaps(t *testing.T) {
	now := time.Now().UTC()
	store, err := openStore(t.TempDir(), 24*time.Hour, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	identity := apiv1.GaggleHealthIdentity{Gaggle: "alpha"}
	key, err := EpisodeKey(FindingNoProgress, identity)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Append(event(2, now, apiv1.GaggleHealthFindingOpened,
		finding(FindingNoProgress, key, identity, now, apiv1.GaggleHealthStalled))); err == nil {
		t.Fatal("Append() accepted a non-contiguous first sequence")
	}
}
