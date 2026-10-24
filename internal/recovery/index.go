package recovery

import (
	"context"
	"errors"
	"time"
)

var (
	// ErrRecordNotFound means a record index has no record for the key.
	ErrRecordNotFound = errors.New("recovery record not found")
	// ErrBundleUnavailable means a bundle location is held by another record
	// or was reaped and not yet released.
	ErrBundleUnavailable = errors.New("recovery bundle location is in use or awaiting deletion")
)

// RecordKey identifies one retained snapshot. A run may retain several.
type RecordKey struct {
	RepositoryKey string
	RunID         string
	SnapshotSHA   string
}

// Key returns the record's index identity.
func (r Record) Key() RecordKey {
	return RecordKey{RepositoryKey: r.RepositoryKey, RunID: r.RunID, SnapshotSHA: r.SnapshotSHA}
}

// IndexedRecord is a retained record with its effective (renewed) RetainUntil
// and the backend-specific location of its bundle.
type IndexedRecord struct {
	Record Record
	Bundle string
}

// RecordIndex is the storage-independent index of retained snapshots. The
// local inventory is the reference; the cloud PostgreSQL index satisfies it
// too, and test/testsupport/conformance/recoveryindex verifies they agree.
//
// Deadlines come from the index's own durable clock (the database clock in
// cloud), so Publish requires a zero record.RetainUntil and sets it retention
// past that clock. An identical retry returns the stored record unchanged; a
// different record for the same key is ErrRecordConflict. Renew never
// shortens a deadline.
//
// A bundle location belongs to one record: Publish refuses a location another
// record holds. Reap removes records whose deadline has passed and returns
// them. A returned bundle is referenced by no record, and its location stays
// retired (Publish refuses it with ErrBundleUnavailable) until ReleaseBundle,
// which the caller invokes once it has deleted the bundle. A backend that
// deletes bundles itself during Reap (the local inventory) treats
// ReleaseBundle as a no-op.
type RecordIndex interface {
	Publish(ctx context.Context, record Record, bundle string, retention time.Duration) (IndexedRecord, error)
	Renew(ctx context.Context, key RecordKey, retention time.Duration) (IndexedRecord, error)
	Get(ctx context.Context, key RecordKey) (IndexedRecord, error)
	Reap(ctx context.Context, limit int) ([]IndexedRecord, error)
	// RetiredBundles lists up to limit bundle locations a Reap retired and no
	// ReleaseBundle has released yet: the bundles still to delete. It, not
	// Reap's result, is what a reaper deletes from, so a Reap whose commit
	// succeeded but whose result was lost is finished on the next pass.
	RetiredBundles(ctx context.Context, limit int) ([]string, error)
	ReleaseBundle(ctx context.Context, bundle string) error
}

// SameIdentity reports whether two records differ only in RetainUntil.
func SameIdentity(a, b Record) bool {
	a.RetainUntil, b.RetainUntil = time.Time{}, time.Time{}
	a.CreatedAt, b.CreatedAt = a.CreatedAt.UTC(), b.CreatedAt.UTC()
	return a == b
}
