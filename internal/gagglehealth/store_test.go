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
	store, err := openStore(root, fixedRetention(24*time.Hour), func() time.Time { return now })
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
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	restarted, err := openStore(root, fixedRetention(24*time.Hour), func() time.Time { return now })
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
	if err := restarted.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestStoreAppliesPerGaggleHistoryRetentionOnRestart(t *testing.T) {
	now := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	root := t.TempDir()
	retentions := map[string]time.Duration{
		"alpha": 24 * time.Hour,
		"beta":  72 * time.Hour,
	}
	resolveRetention := func(gaggle string) (time.Duration, error) {
		return retentions[gaggle], nil
	}
	store, err := openStore(root, resolveRetention, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	openedAt := now.Add(-48 * time.Hour)
	resolvedAt := now.Add(-25 * time.Hour)
	var sequence uint64
	for _, gaggle := range []string{"alpha", "beta"} {
		identity := apiv1.GaggleHealthIdentity{Gaggle: gaggle, Run: "old-run"}
		key, err := EpisodeKey(FindingNoProgress, identity)
		if err != nil {
			t.Fatal(err)
		}
		opened := finding(FindingNoProgress, key, identity, openedAt, apiv1.GaggleHealthStalled)
		sequence++
		if _, err := store.Append(event(sequence, openedAt, apiv1.GaggleHealthFindingOpened, opened)); err != nil {
			t.Fatal(err)
		}
		resolved := opened
		resolved.ResolvedAt = &resolvedAt
		resolved.ResolutionEvidence = []apiv1.GaggleHealthEvidence{{Kind: "journal-event", Run: identity.Run, Sequence: sequence + 1}}
		resolved.Repair.FollowUp = apiv1.GaggleHealthFollowUpResolved
		sequence++
		if _, err := store.Append(event(sequence, resolvedAt, apiv1.GaggleHealthFindingResolved, resolved)); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	restarted, err := openStore(root, resolveRetention, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	alpha, err := restarted.Snapshot("alpha")
	if err != nil {
		t.Fatal(err)
	}
	beta, err := restarted.Snapshot("beta")
	if err != nil {
		t.Fatal(err)
	}
	if len(alpha.History) != 0 {
		t.Fatalf("alpha expired history was retained: %+v", alpha)
	}
	if len(beta.History) != 1 || beta.History[0].Identity.Gaggle != "beta" {
		t.Fatalf("beta history did not use its retention: %+v", beta)
	}
	if err := restarted.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestStoreRejectsJournalSequenceGaps(t *testing.T) {
	now := time.Now().UTC()
	store, err := openStore(t.TempDir(), fixedRetention(24*time.Hour), func() time.Time { return now })
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
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestStoreRepairsTornFinalAppendOnRestart(t *testing.T) {
	now := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	root := t.TempDir()
	store, err := openStore(root, fixedRetention(24*time.Hour), func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	identity := apiv1.GaggleHealthIdentity{Gaggle: "alpha"}
	key, err := EpisodeKey(FindingNoProgress, identity)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Append(event(1, now, apiv1.GaggleHealthFindingOpened,
		finding(FindingNoProgress, key, identity, now, apiv1.GaggleHealthStalled))); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	path := filepath.Join(root, "health", "events.jsonl")
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString(`{"schemaVersion":"goobers.dev/gaggle-health/v1alpha1"`); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}

	restarted, err := openStore(root, fixedRetention(24*time.Hour), func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if after.Size() != before.Size() {
		t.Fatalf("repaired journal size = %d, want %d", after.Size(), before.Size())
	}
	if _, err := restarted.Append(event(2, now.Add(time.Minute), apiv1.GaggleHealthFindingUpdated,
		finding(FindingNoProgress, key, identity, now.Add(time.Minute), apiv1.GaggleHealthStalled))); err != nil {
		t.Fatal(err)
	}
	if err := restarted.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := openStore(root, fixedRetention(24*time.Hour), func() time.Time { return now })
	if err != nil {
		t.Fatalf("second restart after append: %v", err)
	}
	if err := reopened.Close(); err != nil {
		t.Fatal(err)
	}
}

func fixedRetention(retention time.Duration) RetentionResolver {
	return func(string) (time.Duration, error) {
		return retention, nil
	}
}
