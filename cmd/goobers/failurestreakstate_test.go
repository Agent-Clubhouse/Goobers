package main

import (
	"context"
	"os"
	"testing"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/stateclient"
	"github.com/goobers/goobers/providers"
)

func newFailureStreakTestLayout(t *testing.T) instance.Layout {
	t.Helper()
	l := instance.NewLayout(t.TempDir())
	if err := os.MkdirAll(l.SchedulerDir(), 0o755); err != nil {
		t.Fatalf("mkdir scheduler dir: %v", err)
	}
	return l
}

// TestFailureStreakRepositoryIdentityIsolation is Goobers#3025's acceptance
// criterion that identical item numbers in different repositories must never
// collide: two repos' issue #1 must keep independent streaks.
func TestFailureStreakRepositoryIdentityIsolation(t *testing.T) {
	l := newFailureStreakTestLayout(t)
	repoA := providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "acme", Name: "web"}
	repoB := providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "acme", Name: "api"}

	if err := writeFailureStreakCount(l, repoA, "1", 3, "run-a", "implement"); err != nil {
		t.Fatalf("write repoA: %v", err)
	}
	if err := writeFailureStreakCount(l, repoB, "1", 0, "run-b", ""); err != nil {
		t.Fatalf("write repoB: %v", err)
	}

	fake := &blockedHandlerFakeCommenter{}
	gotA, err := loadFailureStreakCount(context.Background(), fake, l, repoA, "1")
	if err != nil || gotA != 3 {
		t.Fatalf("repoA#1 = %d, %v; want 3", gotA, err)
	}
	gotB, err := loadFailureStreakCount(context.Background(), fake, l, repoB, "1")
	if err != nil || gotB != 0 {
		t.Fatalf("repoB#1 = %d, %v; want 0 (independent of repoA's identically-numbered issue)", gotB, err)
	}

	// Cross-provider collision check: same owner/name, different provider.
	repoC := providers.RepositoryRef{Provider: providers.ProviderADO, Owner: "acme", Name: "web", Project: "proj"}
	if err := writeFailureStreakCount(l, repoC, "1", 7, "run-c", "implement"); err != nil {
		t.Fatalf("write repoC: %v", err)
	}
	gotAAfter, err := loadFailureStreakCount(context.Background(), fake, l, repoA, "1")
	if err != nil || gotAAfter != 3 {
		t.Fatalf("repoA#1 after ADO repoC write = %d, %v; want unchanged 3", gotAAfter, err)
	}
}

// TestFailureStreakCASConflictRetries is Goobers#3025's compare-and-swap
// acceptance criterion: a concurrent writer's Update must not lose the other
// writer's increment. updateFailureStreakRecord's fn is re-run per the
// stateclient.Store contract; this exercises that a hand-rolled interleaving
// (read old, then two competing writers each apply their own delta through
// Update) still lands both deltas rather than the second clobbering the
// first.
func TestFailureStreakCASConflictRetries(t *testing.T) {
	l := newFailureStreakTestLayout(t)
	repo := providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "acme", Name: "web"}
	store, err := openStageStateStore(l)
	if err != nil {
		t.Fatalf("open state store: %v", err)
	}
	key := failureStreakKey(repo, "9")
	ctx := context.Background()

	const writers = 8
	errCh := make(chan error, writers)
	for range writers {
		go func() {
			errCh <- updateFailureStreakRecord(ctx, store, key, func(r failureStreakRecord) (failureStreakRecord, bool, error) {
				r.Count++
				return r, true, nil
			})
		}()
	}
	for range writers {
		if err := <-errCh; err != nil {
			t.Fatalf("concurrent update: %v", err)
		}
	}

	got, err := loadFailureStreakCount(ctx, nil, l, repo, "9")
	if err != nil || got != writers {
		t.Fatalf("count after %d concurrent increments = %d, %v; want %d (no lost update)", writers, got, err, writers)
	}
}

// TestFailureStreakSurvivesCommentDeletionAndEditing is Goobers#3025's core
// invariant: once the authoritative key exists, deleting or editing the
// provider comment cannot change what a read reports.
func TestFailureStreakSurvivesCommentDeletionAndEditing(t *testing.T) {
	l := newFailureStreakTestLayout(t)
	repo := providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "acme", Name: "web"}

	if err := writeFailureStreakCount(l, repo, "5", 2, "run-1", "implement"); err != nil {
		t.Fatalf("write: %v", err)
	}

	// A commenter that reports NO comments (the marker was deleted) and would
	// error if the record were re-derived from the marker's absence.
	deleted := &blockedHandlerFakeCommenter{}
	got, err := loadFailureStreakCount(context.Background(), deleted, l, repo, "5")
	if err != nil || got != 2 {
		t.Fatalf("count after comment deletion = %d, %v; want 2 (unchanged — comments are a projection)", got, err)
	}

	// A commenter whose marker was hand-edited to a bogus, much higher count.
	edited := &blockedHandlerFakeCommenter{comments: []providers.Comment{
		{ID: "1", Body: "Goobers: **99 consecutive terminal failure(s)**.\n\n<!-- goobers:failure-streak data-count=\"99\" -->"},
	}}
	got, err = loadFailureStreakCount(context.Background(), edited, l, repo, "5")
	if err != nil || got != 2 {
		t.Fatalf("count after comment edited to 99 = %d, %v; want 2 (the KV record is authoritative)", got, err)
	}
}

// TestFailureStreakMigratesFromLegacyCommentMarker is Goobers#3025's
// one-release migration-on-read: an item whose scheduler-state key has never
// been written picks up its prior streak from the legacy provider-comment
// marker, and that value is durably written into the key so it survives a
// future comment edit or deletion.
func TestFailureStreakMigratesFromLegacyCommentMarker(t *testing.T) {
	l := newFailureStreakTestLayout(t)
	repo := providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "acme", Name: "web"}
	legacy := &blockedHandlerFakeCommenter{comments: []providers.Comment{
		{ID: "1", Body: "Goobers: **2 consecutive terminal failure(s)**.\n\n<!-- goobers:failure-streak data-count=\"2\" -->"},
	}}

	got, err := loadFailureStreakCount(context.Background(), legacy, l, repo, "11")
	if err != nil || got != 2 {
		t.Fatalf("migrated count = %d, %v; want 2", got, err)
	}

	// Now delete the legacy comment and confirm the migrated value is
	// durable — a second read must not need the provider at all.
	unavailable := &blockedHandlerFakeCommenter{listErr: context.DeadlineExceeded}
	got, err = loadFailureStreakCount(context.Background(), unavailable, l, repo, "11")
	if err != nil || got != 2 {
		t.Fatalf("count after migration with provider unavailable = %d, %v; want 2 (already durable)", got, err)
	}
}

// TestFailureStreakMalformedLegacyValueDoesNotMigrate covers the fail-closed
// side of migration-on-read: a marker present but carrying an unparsable
// data-count must never be treated as a legitimate zero, and must not poison
// the key with a bogus value.
func TestFailureStreakMalformedLegacyValueDoesNotMigrate(t *testing.T) {
	l := newFailureStreakTestLayout(t)
	repo := providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "acme", Name: "web"}
	malformed := &blockedHandlerFakeCommenter{comments: []providers.Comment{
		{ID: "1", Body: "Goobers: streak comment with a corrupted marker.\n\n<!-- goobers:failure-streak data-count=\"not-a-number\" -->"},
	}}

	got, err := loadFailureStreakCount(context.Background(), malformed, l, repo, "12")
	if err != nil || got != 0 {
		t.Fatalf("count with malformed marker = %d, %v; want 0, no error (nothing to migrate)", got, err)
	}

	store, err := openStageStateStore(l)
	if err != nil {
		t.Fatalf("open state store: %v", err)
	}
	value, err := store.Get(context.Background(), failureStreakStateKey(failureStreakKey(repo, "12")))
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if value.Exists() {
		t.Fatalf("a malformed legacy marker must not create a scheduler-state record, got %+v", value)
	}
}

// TestFailureStreakRestartDurability confirms the record survives a fresh
// process re-opening the same instance layout (no in-memory cache to lose).
func TestFailureStreakRestartDurability(t *testing.T) {
	l := newFailureStreakTestLayout(t)
	repo := providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "acme", Name: "web"}
	if err := writeFailureStreakCount(l, repo, "20", 3, "run-1", "implement"); err != nil {
		t.Fatalf("write: %v", err)
	}

	// A fresh Layout value over the same directory, as a restarted daemon
	// would construct.
	restarted := instance.NewLayout(l.Root)
	got, err := loadFailureStreakCount(context.Background(), nil, restarted, repo, "20")
	if err != nil || got != 3 {
		t.Fatalf("count after restart = %d, %v; want 3", got, err)
	}
}

// TestFailureStreakDecodeRejectsForeignKeyedRecord guards the integrity
// posture failureStreakDocument shares with remediationNoopDocument: a
// document whose embedded key does not match the key it was read at is a
// decode error, never silently accepted.
func TestFailureStreakDecodeRejectsForeignKeyedRecord(t *testing.T) {
	data, err := encodeFailureStreakRecord("github/acme/web#1", failureStreakRecord{Count: 5})
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if _, err := decodeFailureStreakRecord(stateclient.Value{Data: data, ETag: "x"}, "github/acme/web#2"); err == nil {
		t.Fatal("decode with mismatched key = nil error, want a mis-keyed-record error")
	}
}
