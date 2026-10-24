// Package recoveryindex is the recovery record-index conformance suite. Every
// recovery.RecordIndex implementation runs it, so the cloud index keeps the
// semantics of the local inventory, the reference.
package recoveryindex

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/recovery"
	"github.com/goobers/goobers/providers"
)

// Backend opens views of one durable index. Every Open after the first must
// observe what earlier views committed, as a restarted process would.
// StageBundle makes data durable where the backend keeps bundles and returns
// the location Publish takes.
type Backend struct {
	Open        func(t *testing.T) recovery.RecordIndex
	StageBundle func(t *testing.T, data []byte) string
}

// Run executes the suite. newBackend returns an empty index per subtest.
func Run(t *testing.T, newBackend func(t *testing.T) Backend) {
	cases := map[string]func(*testing.T, Backend){
		"publish-sets-retention":     publishSetsRetention,
		"identical-retry-unchanged":  identicalRetryUnchanged,
		"conflicting-identity":       conflictingIdentity,
		"several-snapshots-per-run":  severalSnapshotsPerRun,
		"renew-never-shrinks":        renewNeverShrinks,
		"unknown-key":                unknownKey,
		"invalid-publication":        invalidPublication,
		"reap-expired-only":          reapExpiredOnly,
		"reap-limit":                 reapLimit,
		"durable-across-reopen":      durableAcrossReopen,
		"exact-capture-time-retains": exactCaptureTime,
		"bundle-belongs-to-one":      bundleBelongsToOne,
		"lost-reap-result-retired":   lostReapResultRetired,
	}
	for name, run := range cases {
		t.Run(name, func(t *testing.T) { run(t, newBackend(t)) })
	}
}

// fixture is a valid record for run and snapshot whose archive binding
// matches data.
func fixture(run string, snapshot byte, data []byte) recovery.Record {
	ref, _ := recovery.RefForRun(run)
	sum := sha256.Sum256(data)
	return recovery.Record{
		Version:       1,
		RunID:         run,
		RepositoryKey: providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "acme", Name: "app"}.CanonicalKey(),
		Ref:           ref,
		BaseSHA:       strings.Repeat("a", 40),
		SnapshotSHA:   strings.Repeat(string(snapshot), 40),
		PatchDigest:   "sha256:" + strings.Repeat("c", 64),
		ArchiveDigest: fmt.Sprintf("sha256:%x", sum),
		ArchiveBytes:  int64(len(data)),
		ArchiveFormat: "full",
		CreatedAt:     time.Now().Add(-time.Minute).UTC(),
	}
}

func publish(t *testing.T, backend Backend, index recovery.RecordIndex, record recovery.Record, data []byte, retention time.Duration) recovery.IndexedRecord {
	t.Helper()
	indexed, err := index.Publish(t.Context(), record, backend.StageBundle(t, data), retention)
	if err != nil {
		t.Fatalf("Publish(%s): %v", record.RunID, err)
	}
	return indexed
}

// within checks a deadline set retention past the backend clock, allowing for
// skew between that clock and the test's.
func within(t *testing.T, deadline, before, after time.Time, retention time.Duration) {
	t.Helper()
	const skew = 5 * time.Second
	if deadline.Before(before.Add(retention-skew)) || deadline.After(after.Add(retention+skew)) {
		t.Fatalf("RetainUntil %s is not %s past the publication window [%s, %s]", deadline, retention, before, after)
	}
}

func publishSetsRetention(t *testing.T, backend Backend) {
	index, data := backend.Open(t), []byte("bundle")
	record := fixture("run-1", 'b', data)
	before := time.Now()
	indexed := publish(t, backend, index, record, data, time.Hour)
	within(t, indexed.Record.RetainUntil, before, time.Now(), time.Hour)
	if !recovery.SameIdentity(indexed.Record, record) || indexed.Bundle == "" {
		t.Fatalf("published %+v, want the identity of %+v and a bundle", indexed, record)
	}
	got, err := index.Get(t.Context(), record.Key())
	if err != nil {
		t.Fatal(err)
	}
	if got.Record != indexed.Record || got.Bundle != indexed.Bundle {
		t.Fatalf("Get = %+v, want %+v", got, indexed)
	}
}

func identicalRetryUnchanged(t *testing.T, backend Backend) {
	index, data := backend.Open(t), []byte("bundle")
	record := fixture("run-1", 'b', data)
	first := publish(t, backend, index, record, data, time.Hour)
	retry := publish(t, backend, index, record, data, 48*time.Hour)
	if retry.Record != first.Record || retry.Bundle != first.Bundle {
		t.Fatalf("identical retry = %+v, want the stored %+v unchanged", retry, first)
	}
}

func conflictingIdentity(t *testing.T, backend Backend) {
	index, data := backend.Open(t), []byte("bundle")
	record := fixture("run-1", 'b', data)
	publish(t, backend, index, record, data, time.Hour)
	changed := record
	changed.PatchDigest = "sha256:" + strings.Repeat("d", 64)
	if _, err := index.Publish(t.Context(), changed, backend.StageBundle(t, data), time.Hour); !errors.Is(err, recovery.ErrRecordConflict) {
		t.Fatalf("different record for the same key = %v, want ErrRecordConflict", err)
	}
}

func severalSnapshotsPerRun(t *testing.T, backend Backend) {
	index, data := backend.Open(t), []byte("bundle")
	first, second := fixture("run-1", 'b', data), fixture("run-1", 'e', data)
	publish(t, backend, index, first, data, time.Hour)
	publish(t, backend, index, second, data, time.Hour)
	for _, record := range []recovery.Record{first, second} {
		if got, err := index.Get(t.Context(), record.Key()); err != nil || got.Record.SnapshotSHA != record.SnapshotSHA {
			t.Fatalf("Get(%s) = %+v, %v", record.SnapshotSHA, got, err)
		}
	}
}

func renewNeverShrinks(t *testing.T, backend Backend) {
	index, data := backend.Open(t), []byte("bundle")
	record := fixture("run-1", 'b', data)
	published := publish(t, backend, index, record, data, time.Hour)
	shorter, err := index.Renew(t.Context(), record.Key(), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if !shorter.Record.RetainUntil.Equal(published.Record.RetainUntil) {
		t.Fatalf("shorter renewal moved RetainUntil from %s to %s", published.Record.RetainUntil, shorter.Record.RetainUntil)
	}
	before := time.Now()
	longer, err := index.Renew(t.Context(), record.Key(), 48*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	within(t, longer.Record.RetainUntil, before, time.Now(), 48*time.Hour)
	if !recovery.SameIdentity(longer.Record, record) {
		t.Fatal("renewal changed the record identity")
	}
	got, err := index.Get(t.Context(), record.Key())
	if err != nil || !got.Record.RetainUntil.Equal(longer.Record.RetainUntil) {
		t.Fatalf("Get after renewal = %+v, %v; want the renewed deadline", got, err)
	}
}

func unknownKey(t *testing.T, backend Backend) {
	index := backend.Open(t)
	key := fixture("missing", 'b', []byte("x")).Key()
	if _, err := index.Get(t.Context(), key); !errors.Is(err, recovery.ErrRecordNotFound) {
		t.Fatalf("Get of unknown key = %v, want ErrRecordNotFound", err)
	}
	if _, err := index.Renew(t.Context(), key, time.Hour); !errors.Is(err, recovery.ErrRecordNotFound) {
		t.Fatalf("Renew of unknown key = %v, want ErrRecordNotFound", err)
	}
}

func invalidPublication(t *testing.T, backend Backend) {
	index, data := backend.Open(t), []byte("bundle")
	preset := fixture("run-1", 'b', data)
	preset.RetainUntil = time.Now().Add(time.Hour)
	invalid := fixture("run-2", 'b', data)
	invalid.BaseSHA = "not-a-sha"
	for name, record := range map[string]recovery.Record{"preset-deadline": preset, "invalid-record": invalid} {
		if _, err := index.Publish(t.Context(), record, backend.StageBundle(t, data), time.Hour); err == nil {
			t.Fatalf("%s: Publish accepted an invalid publication", name)
		}
		if _, err := index.Get(t.Context(), record.Key()); !errors.Is(err, recovery.ErrRecordNotFound) {
			t.Fatalf("%s: rejected publication was indexed: %v", name, err)
		}
	}
	if _, err := index.Publish(t.Context(), fixture("run-3", 'b', data), backend.StageBundle(t, data), 0); err == nil {
		t.Fatal("Publish accepted a zero retention")
	}
}

func reapExpiredOnly(t *testing.T, backend Backend) {
	index, data := backend.Open(t), []byte("bundle")
	short, long := fixture("short", 'b', data), fixture("long", 'b', data)
	shortIndexed := publish(t, backend, index, short, data, time.Millisecond)
	publish(t, backend, index, long, data, time.Hour)
	time.Sleep(50 * time.Millisecond)
	reaped, err := index.Reap(t.Context(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(reaped) != 1 || reaped[0].Record != shortIndexed.Record || reaped[0].Bundle != shortIndexed.Bundle {
		t.Fatalf("reaped %+v, want only %+v", reaped, shortIndexed)
	}
	if _, err := index.Get(t.Context(), short.Key()); !errors.Is(err, recovery.ErrRecordNotFound) {
		t.Fatalf("reaped record still indexed: %v", err)
	}
	if _, err := index.Get(t.Context(), long.Key()); err != nil {
		t.Fatalf("unexpired record: %v", err)
	}
	if again, err := index.Reap(t.Context(), 10); err != nil || len(again) != 0 {
		t.Fatalf("second reap = %d records, %v; want none", len(again), err)
	}
	if err := index.ReleaseBundle(t.Context(), reaped[0].Bundle); err != nil {
		t.Fatalf("ReleaseBundle after deleting a reaped bundle: %v", err)
	}
}

func reapLimit(t *testing.T, backend Backend) {
	index, data := backend.Open(t), []byte("bundle")
	for _, run := range []string{"run-1", "run-2", "run-3"} {
		publish(t, backend, index, fixture(run, 'b', data), data, time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond)
	first, err := index.Reap(t.Context(), 2)
	if err != nil || len(first) != 2 {
		t.Fatalf("Reap(2) = %d records, %v", len(first), err)
	}
	rest, err := index.Reap(t.Context(), 2)
	if err != nil || len(rest) != 1 {
		t.Fatalf("second Reap(2) = %d records, %v; want the last one", len(rest), err)
	}
}

func durableAcrossReopen(t *testing.T, backend Backend) {
	data := []byte("bundle")
	record := fixture("run-1", 'b', data)
	published := publish(t, backend, backend.Open(t), record, data, time.Hour)
	renewed, err := backend.Open(t).Renew(t.Context(), record.Key(), 48*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	got, err := backend.Open(t).Get(t.Context(), record.Key())
	if err != nil || got.Record != renewed.Record || got.Bundle != published.Bundle {
		t.Fatalf("reopened Get = %+v, %v; want the renewed record", got, err)
	}
}

// exactCaptureTime keeps the capture time to the nanosecond, so a retry of a
// record captured with full clock precision is identical, not a conflict.
func exactCaptureTime(t *testing.T, backend Backend) {
	index, data := backend.Open(t), []byte("bundle")
	record := fixture("run-1", 'b', data)
	record.CreatedAt = time.Date(2026, 10, 1, 12, 0, 0, 123456789, time.UTC)
	published := publish(t, backend, index, record, data, time.Hour)
	if !published.Record.CreatedAt.Equal(record.CreatedAt) {
		t.Fatalf("CreatedAt = %s, want %s exactly", published.Record.CreatedAt, record.CreatedAt)
	}
	publish(t, backend, index, record, data, time.Hour)
}

// bundleBelongsToOne: a location held by one record cannot be published for
// another, so a reaped record's bundle is never one a live record still uses.
func bundleBelongsToOne(t *testing.T, backend Backend) {
	index, data := backend.Open(t), []byte("bundle")
	short, long := fixture("short", 'b', data), fixture("long", 'b', data)
	shared := backend.StageBundle(t, data)
	if _, err := index.Publish(t.Context(), short, shared, time.Millisecond); err != nil {
		t.Fatal(err)
	}
	if _, err := index.Publish(t.Context(), long, shared, time.Hour); err == nil {
		t.Fatal("a second record was published at a location another record holds")
	}
	publish(t, backend, index, long, data, time.Hour)
	time.Sleep(50 * time.Millisecond)
	reaped, err := index.Reap(t.Context(), 10)
	if err != nil || len(reaped) != 1 {
		t.Fatalf("Reap = %d records, %v; want the expired one", len(reaped), err)
	}
	live, err := index.Get(t.Context(), long.Key())
	if err != nil {
		t.Fatal(err)
	}
	if live.Bundle == reaped[0].Bundle {
		t.Fatalf("Reap handed back %q for deletion while a live record still references it", live.Bundle)
	}
}

// lostReapResultRetired: a Reap whose commit succeeded but whose result never
// reached the caller (#8163). The reaper drives deletion from RetiredBundles,
// which must list the reaped bundle unless the index deleted it during Reap,
// and draining it (delete, then ReleaseBundle) leaves nothing retired.
func lostReapResultRetired(t *testing.T, backend Backend) {
	index, data := backend.Open(t), []byte("bundle")
	record := fixture("lost", 'b', data)
	bundle := backend.StageBundle(t, data)
	if _, err := index.Publish(t.Context(), record, bundle, time.Millisecond); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	if _, err := index.Reap(t.Context(), 10); err != nil {
		t.Fatal(err)
	}
	// The result is discarded, as a caller that lost the reply would.
	retired, err := index.RetiredBundles(t.Context(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(retired) > 1 || (len(retired) == 1 && retired[0] != bundle) {
		t.Fatalf("retired bundles = %v, want none (deleted during Reap) or exactly %q", retired, bundle)
	}
	for _, retiredBundle := range retired {
		if err := index.ReleaseBundle(t.Context(), retiredBundle); err != nil {
			t.Fatal(err)
		}
	}
	if left, err := index.RetiredBundles(t.Context(), 10); err != nil || len(left) != 0 {
		t.Fatalf("after draining, retired bundles = %v, %v; want none", left, err)
	}
}
