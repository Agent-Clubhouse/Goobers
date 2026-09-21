// Package history retains a fixed-size, private local diagnostic window. It is
// observational evidence, never a scheduler journal or an execution ledger.
package history

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/goobers/goobers/internal/fleetdiagnostics"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/platform/durability"
	"github.com/goobers/goobers/internal/platform/lock"
	"github.com/goobers/goobers/internal/platform/safeopen"
	"github.com/goobers/goobers/internal/telemetry"
)

const (
	// MaxBytes caps both the committed snapshot and its single fixed scratch.
	MaxBytes = 4 << 20
	// MaxRecords bounds the retained observation population.
	MaxRecords = 4096
	// MaxRecordBytes bounds one encoded, scrubbed observation.
	MaxRecordBytes = 64 << 10
	// MaxBatch bounds one pulse before serialization or allocation.
	MaxBatch     = 256
	snapshotName = "history.json"
	scratchName  = "history.next.json"
	lockName     = "history.lock"
)

// Metadata distinguishes local history omissions from transport loss counters.
type Metadata struct {
	StoredAt           time.Time `json:"storedAt"`
	EvictedRecords     uint64    `json:"evictedRecords"`
	OmittedRecords     uint64    `json:"omittedRecords"`
	KnownWriteFailures uint64    `json:"knownWriteFailures"`
	Reset              bool      `json:"reset"`
}

// Record retains only a validated fleet contract and its observation timestamp.
type Record struct {
	Name       string         `json:"name"`
	Time       time.Time      `json:"time"`
	Attributes map[string]any `json:"attributes"`
}

// Snapshot contains a retained window, never a claim of continuous coverage.
type Snapshot struct {
	Metadata
	Records []Record
}

type wireSnapshot struct {
	Schema int `json:"schema"`
	Metadata
	Records json.RawMessage `json:"records"`
}

// Store owns one nonblocking writer lock for its lifetime. Readers need no lock
// because each pulse replaces one complete snapshot atomically.
type Store struct {
	dir      string
	root     *os.Root
	owner    *lock.Handle
	scrubber journal.Scrubber
	mu       sync.Mutex
	metadata Metadata
	records  []json.RawMessage
	failures atomic.Uint64
	closed   bool
}

// Open validates bounded existing data and removes only the fixed crash scratch.
// Corrupt history starts an explicitly reset window; unsafe paths fail closed.
func Open(ctx context.Context, dir string, scrubber journal.Scrubber) (*Store, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	info, err := os.Lstat(dir)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("unsafe diagnostic history directory")
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	owner, err := acquireOwner(root, dir)
	if err != nil {
		_ = root.Close()
		return nil, err
	}
	if scrubber == nil {
		scrubber = journal.NewPatternScrubber()
	}
	store := &Store{dir: dir, root: root, owner: owner, scrubber: scrubber}
	if info, err := root.Lstat(snapshotName); err == nil {
		if !info.Mode().IsRegular() {
			_ = store.Close()
			return nil, errors.New("unsafe diagnostic history snapshot")
		}
		if info.Size() > MaxBytes {
			if err := root.Remove(snapshotName); err != nil {
				_ = store.Close()
				return nil, err
			}
			store.metadata.Reset = true
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		_ = store.Close()
		return nil, err
	}

	if err := root.Remove(scratchName); err != nil && !errors.Is(err, os.ErrNotExist) {
		_ = store.Close()
		return nil, err
	}
	snapshot, raw, err := readSnapshot(root)
	if err == nil {
		store.metadata, store.records = snapshot.Metadata, raw
		store.failures.Store(snapshot.KnownWriteFailures)
	} else if !errors.Is(err, os.ErrNotExist) {
		store.metadata.Reset = true
	}
	return store, nil
}
func acquireOwner(root *os.Root, dir string) (*lock.Handle, error) {
	file, err := root.OpenFile(lockName, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err == nil {
		err = file.Close()
	} else if errors.Is(err, os.ErrExist) {
		err = nil
	}
	if err != nil {
		return nil, err
	}
	before, err := root.Lstat(lockName)
	if err != nil || !before.Mode().IsRegular() || before.Size() != 0 {
		return nil, errors.New("unsafe diagnostic history lock")
	}
	owner, err := lock.TryAcquireExisting(filepath.Join(dir, lockName))
	if err != nil {
		return nil, err
	}
	after, err := owner.File().Stat()
	if err != nil || !after.Mode().IsRegular() || !os.SameFile(before, after) {
		_ = owner.Release()
		return nil, errors.New("diagnostic history lock changed")
	}
	return owner, nil
}

// Close releases the writer lock. The last committed snapshot remains readable.
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	return errors.Join(s.owner.Release(), s.root.Close())
}

// Append writes one scrubbed pulse with one file fsync and atomic replacement.
// Failures before replacement preserve the previous snapshot. Directory sync
// failures retain the published candidate; known failures persist next time.
func (s *Store) Append(ctx context.Context, records []telemetry.DiagnosticRecord) error {
	if !s.mu.TryLock() {
		s.failures.Add(1)
		return errors.New("diagnostic history writer busy")
	}
	defer s.mu.Unlock()
	if s.closed {
		return errors.New("diagnostic history closed")
	}
	if err := ctx.Err(); err != nil {
		s.failures.Add(1)
		return err
	}
	if len(records) > MaxBatch {
		s.failures.Add(1)
		return errors.New("diagnostic history batch exceeds bound")
	}
	next := append([]json.RawMessage(nil), s.records...)
	metadata := s.metadata
	for _, record := range records {
		if err := ctx.Err(); err != nil {
			s.failures.Add(1)
			return err
		}
		encoded, err := encodeRecord(record, s.scrubber)
		if err != nil {
			metadata.OmittedRecords++
			continue
		}
		next = append(next, encoded)
	}
	size := 0
	for _, record := range next {
		size += len(record) + 1
	}
	for len(next) > MaxRecords || size > MaxBytes-1024 {
		size -= len(next[0]) + 1
		// Header space is reserved before serialization, avoiding quadratic remarshal.
		next = next[1:]
		metadata.EvictedRecords++
	}
	metadata.StoredAt, metadata.KnownWriteFailures = time.Now().UTC(), s.failures.Load()
	data, err := encodeSnapshot(metadata, next)
	if err == nil {
		var published bool
		published, err = s.commit(ctx, data)
		if published {
			s.metadata, s.records = metadata, next
		}
	}
	if err != nil {
		s.failures.Add(1)
		return err
	}
	s.metadata, s.records = metadata, next
	return nil
}
func encodeRecord(record telemetry.DiagnosticRecord, scrubber journal.Scrubber) ([]byte, error) {
	r := Record{Name: record.Name, Time: record.Time, Attributes: record.Attributes}
	if err := validateRecord(r); err != nil {
		return nil, err
	}
	data, err := json.Marshal(r)
	if err != nil {
		return nil, err
	}
	if scrubber != nil {
		data = scrubber.Scrub(data)
	}
	if len(data) > MaxRecordBytes {
		return nil, errors.New("diagnostic record exceeds bound")
	}
	var checked Record
	if err := json.Unmarshal(data, &checked); err != nil {
		return nil, err
	}
	if err := validateRecord(checked); err != nil {
		return nil, err
	}
	return data, nil
}
func validateRecord(record Record) error {
	if record.Time.IsZero() {
		return errors.New("missing diagnostic timestamp")
	}
	switch record.Name {
	case fleetdiagnostics.HeartbeatEvent:
		_, err := fleetdiagnostics.DecodeHeartbeat(record.Attributes)
		return err
	case fleetdiagnostics.FeatureEvent:
		_, err := fleetdiagnostics.DecodeFeatureUsage(record.Attributes)
		return err
	default:
		return errors.New("unknown diagnostic record")
	}
}
func encodeSnapshot(metadata Metadata, records []json.RawMessage) ([]byte, error) {
	if records == nil {
		records = []json.RawMessage{}
	}
	raw, err := json.Marshal(records)
	if err != nil {
		return nil, err
	}
	return json.Marshal(wireSnapshot{Schema: 1, Metadata: metadata, Records: raw})
}
func (s *Store) commit(ctx context.Context, data []byte) (bool, error) {
	if len(data) > MaxBytes {
		return false, errors.New("diagnostic snapshot exceeds bound")
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	file, err := s.root.OpenFile(scratchName, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return false, err
	}
	defer func() { _ = file.Close(); _ = s.root.Remove(scratchName) }()
	if _, err := file.Write(data); err != nil {
		return false, err
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if err := file.Sync(); err != nil {
		return false, err
	}
	if err := file.Close(); err != nil {
		return false, err
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if err := durability.ReplaceFile(filepath.Join(s.dir, scratchName), filepath.Join(s.dir, snapshotName)); err != nil {
		return false, err
	}
	// Even if directory sync fails, keep the published in-memory candidate so a
	// later recovery write never silently discards the just-published records.
	return true, durability.SyncDir(s.dir)
}

// Read inspects at most MaxBytes and MaxRecords without contacting a daemon.
func Read(dir string) (Snapshot, error) {
	info, err := os.Lstat(dir)
	if err != nil {
		return Snapshot{}, err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return Snapshot{}, errors.New("unsafe diagnostic history directory")
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return Snapshot{}, err
	}
	defer func() { _ = root.Close() }()
	snapshot, _, err := readSnapshot(root)
	return snapshot, err
}
func readSnapshot(root *os.Root) (Snapshot, []json.RawMessage, error) {
	info, err := root.Lstat(snapshotName)
	if err != nil {
		return Snapshot{}, nil, err
	}
	if !info.Mode().IsRegular() {
		return Snapshot{}, nil, errors.New("unsafe diagnostic history snapshot")
	}
	file, err := safeopen.OpenRegularInRoot(root, snapshotName)
	if err != nil {
		return Snapshot{}, nil, err
	}
	defer func() { _ = file.Close() }()
	info, err = file.Stat()
	if err != nil || info.Size() > MaxBytes {
		return Snapshot{}, nil, errors.New("diagnostic snapshot exceeds bound")
	}
	data, err := io.ReadAll(io.LimitReader(file, MaxBytes+1))
	if err != nil || len(data) > MaxBytes {
		return Snapshot{}, nil, errors.New("diagnostic snapshot unreadable or oversized")
	}
	var wire wireSnapshot
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&wire); err != nil || wire.Schema != 1 || wire.StoredAt.IsZero() {
		return Snapshot{}, nil, errors.New("invalid diagnostic snapshot")
	}
	if decoder.Decode(new(any)) != io.EOF {
		return Snapshot{}, nil, errors.New("trailing diagnostic snapshot data")
	}
	return decodeRecords(wire)
}
func decodeRecords(wire wireSnapshot) (Snapshot, []json.RawMessage, error) {
	result := Snapshot{Metadata: wire.Metadata}
	decoder := json.NewDecoder(bytes.NewReader(wire.Records))
	token, err := decoder.Token()
	if err != nil || token != json.Delim('[') {
		return Snapshot{}, nil, errors.New("invalid diagnostic records")
	}
	var raw []json.RawMessage
	for decoder.More() {
		if len(raw) >= MaxRecords {
			return Snapshot{}, nil, errors.New("diagnostic record count exceeds bound")
		}
		var data json.RawMessage
		if err := decoder.Decode(&data); err != nil || len(data) > MaxRecordBytes {
			return Snapshot{}, nil, errors.New("invalid diagnostic record size")
		}
		var record Record
		if err := json.Unmarshal(data, &record); err != nil {
			return Snapshot{}, nil, err
		}
		if err := validateRecord(record); err != nil {
			return Snapshot{}, nil, err
		}
		raw = append(raw, data)
		result.Records = append(result.Records, record)
	}
	if _, err := decoder.Token(); err != nil {
		return Snapshot{}, nil, err
	}
	return result, raw, nil
}
