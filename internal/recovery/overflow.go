package recovery

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/platform/durability"
)

// The overflow tier is what makes capacity gate bundle publication rather
// than worktree teardown (#5370).
//
// A snapshot's objects live in the repository's object store BEFORE any
// inventory slot is consulted: PrepareRecord writes the snapshot commit and
// pin.go creates its retention ref, and only then does publication ask the
// inventory for a directory to bundle into. The scarce resource is therefore
// the bundle directory, never the work. Refusing the publish when the
// inventory is legitimately full of unique, unlanded, in-window captures
// protects nothing and stops the instance: the cleanup is refused, the run
// branch cannot be reacquired, and unrelated runs then fail at
// `create worktree` with an error naming neither recovery nor the run that
// filled the inventory.
//
// An overflow entry is the same identity a retained entry carries, minus the
// bundle: one directory holding record.json alone, a few hundred bytes, for
// objects that already exist. It sits exactly one durability tier below a
// bundle — restorable while the mirror holds the ref, not self-contained —
// and the retention pass promotes it to a real bundle as soon as a slot
// frees, so steady state converges back to the intended contract. It is
// therefore deliberately uncapped: capping it would reintroduce the refusal
// it exists to remove, for a tier whose cost is metadata.
//
// Retirement rules apply to overflow entries exactly as to retained ones, so
// nothing is ever discarded for sitting in this tier (#5354 bullet 7).

// OverflowRootName is the overflow tier's directory under the instance root,
// a sibling of the inventory rather than an entry inside it: an entry inside
// would consume the very slot this tier exists because there is none of.
const OverflowRootName = "recovery-overflow"

// PublishOverflow pins prepared's snapshot ref in repository and publishes its
// identity into the overflow tier, with no bundle. repository must be the
// repository that holds the snapshot objects — for a linked worktree that is
// the managed mirror, which is where the pin already lives.
//
// Success means the ref is pinned and the record is durable, which is what
// authorizes the caller's cleanup. It is not proof the objects survive an
// operator pruning the mirror; only a bundle is that, which is why promotion
// exists.
func PublishOverflow(ctx context.Context, repository, root string, prepared Record) (Record, string, error) {
	if err := prepared.validateSnapshot(); err != nil {
		return Record{}, "", err
	}
	// Pin first: a record naming a ref that does not exist is worse than no
	// record, because it reports recoverable work that is not.
	if err := PinCommit(ctx, repository, prepared); err != nil {
		return Record{}, "", err
	}
	directory := filepath.Join(root, inventoryDirectoryName(prepared))
	if err := prepareOverflowDirectory(root, directory); err != nil {
		return Record{}, "", err
	}
	path := filepath.Join(directory, RecordFileName)
	published, err := publishOverflowRecord(path, prepared)
	if err != nil {
		return Record{}, "", err
	}
	return published, path, nil
}

func prepareOverflowDirectory(root, directory string) error {
	if err := os.MkdirAll(root, 0o700); err != nil {
		return fmt.Errorf("create recovery overflow root: %w", err)
	}
	if err := os.Mkdir(directory, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
		return fmt.Errorf("reserve recovery overflow entry: %w", err)
	}
	info, err := os.Lstat(directory)
	if err != nil || !info.IsDir() {
		return fmt.Errorf("recovery overflow entry must be a real directory")
	}
	return durability.SyncDir(root)
}

// publishOverflowRecord refuses to replace a different identity for the same
// directory, exactly as PublishRecord does for a retained record. The one
// field it may move is RetainUntil, and only forward: the retained tier
// extends a deadline through a retention.json sidecar, and an overflow entry
// republished by a later capture of the same immutable snapshot must be able
// to do the same rather than being refused as a conflict.
func publishOverflowRecord(path string, record Record) (Record, error) {
	existing, err := ReadOverflowRecord(path)
	if err == nil {
		if record.RetainUntil.Before(existing.RetainUntil) {
			record.RetainUntil = existing.RetainUntil
		}
		comparison := existing
		comparison.RetainUntil = record.RetainUntil
		if comparison != record {
			return Record{}, ErrRecordConflict
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return Record{}, err
	}
	data, err := encodeOverflowRecord(record)
	if err != nil {
		return Record{}, err
	}
	if err := journal.WriteFileAtomic(path, data, 0o600); err != nil {
		return Record{}, fmt.Errorf("publish recovery overflow record: %w", err)
	}
	if err := durability.SyncDir(filepath.Dir(path)); err != nil {
		return Record{}, err
	}
	return record, nil
}

// encodeOverflowRecord serialises the SAME Record schema a retained record
// uses. The archive fields are the only difference and they are empty, which
// is not a new field or a new version — an overflow entry is a retained
// entry's identity without its bundle, and a record that claimed archive
// bytes it does not have would be a lie an importer could act on.
func encodeOverflowRecord(record Record) ([]byte, error) {
	if err := record.validateSnapshot(); err != nil {
		return nil, err
	}
	if record.ArchiveDigest != "" || record.ArchiveBytes != 0 || record.ArchiveFormat != "" {
		return nil, fmt.Errorf("recovery overflow record must declare no archive")
	}
	data, err := json.Marshal(record)
	if err != nil {
		return nil, fmt.Errorf("encode recovery overflow record: %w", err)
	}
	if len(data) > MaxRecordBytes {
		return nil, fmt.Errorf("recovery record exceeds byte limit")
	}
	return data, nil
}

// ReadOverflowRecord reads one overflow record. It validates identity but not
// archive binding, since an overflow entry has no archive to bind to.
func ReadOverflowRecord(path string) (Record, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return Record{}, err
	}
	if !info.Mode().IsRegular() {
		return Record{}, fmt.Errorf("recovery overflow record is not a regular file")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return Record{}, err
	}
	if len(data) > MaxRecordBytes {
		return Record{}, fmt.Errorf("recovery record exceeds byte limit")
	}
	if err := uniqueRecordFields(data); err != nil {
		return Record{}, err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var record Record
	if err := decoder.Decode(&record); err != nil {
		return Record{}, fmt.Errorf("decode recovery overflow record: %w", err)
	}
	if err := record.validateSnapshot(); err != nil {
		return Record{}, err
	}
	if record.ArchiveDigest != "" || record.ArchiveBytes != 0 {
		return Record{}, fmt.Errorf("recovery overflow record must declare no archive")
	}
	return record, nil
}

// ReadOverflow lists the overflow tier oldest capture first, tolerating debris
// the way ReadInventoryTolerant does: an unreadable entry is reported, never
// allowed to take the whole scan down with it. A missing root is empty and is
// not created by this read.
func ReadOverflow(ctx context.Context, root string) ([]InventoryEntry, []UnreadableEntry, error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	info, err := os.Lstat(root)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil, nil
	}
	if err != nil || !info.IsDir() {
		return nil, nil, fmt.Errorf("recovery overflow root must be a real directory")
	}
	names, err := overflowNames(root, info)
	if err != nil {
		return nil, nil, err
	}
	var entries []InventoryEntry
	var unreadable []UnreadableEntry
	for _, name := range names {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		entry, err := readOverflowEntry(root, name)
		if err != nil {
			unreadable = append(unreadable, UnreadableEntry{Name: name, Err: err})
			continue
		}
		entries = append(entries, entry)
	}
	// Oldest capture first, which is the order promotion and reporting both
	// want; the directory names are identity hashes and carry no order.
	slices.SortStableFunc(entries, func(a, b InventoryEntry) int {
		return a.Record.CreatedAt.Compare(b.Record.CreatedAt)
	})
	return entries, unreadable, nil
}

func overflowNames(root string, before os.FileInfo) ([]string, error) {
	file, err := os.Open(root)
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()
	after, err := file.Stat()
	if err != nil || !os.SameFile(before, after) {
		return nil, fmt.Errorf("recovery overflow root changed while opening")
	}
	names, err := file.Readdirnames(MaxInventoryEntries + 1)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	slices.Sort(names)
	return names, nil
}

func readOverflowEntry(root, name string) (InventoryEntry, error) {
	directory := filepath.Join(root, name)
	info, err := os.Lstat(directory)
	if err != nil || !info.IsDir() {
		return InventoryEntry{}, fmt.Errorf("overflow entry must be a real directory")
	}
	path := filepath.Join(directory, RecordFileName)
	record, err := ReadOverflowRecord(path)
	if err != nil {
		return InventoryEntry{}, err
	}
	if name != inventoryDirectoryName(record) {
		return InventoryEntry{}, ErrRecordConflict
	}
	return InventoryEntry{Record: record, RecordPath: path}, nil
}

// HasSnapshotRef reports whether repository still holds the exact pin the
// record names. Promotion and restore both need to pick the managed copy that
// actually has the objects: a snapshot captured into the mirror may never
// have reached a pinned clone, so "the first repository" is not good enough.
func HasSnapshotRef(ctx context.Context, repository string, record Record) bool {
	if record.validateSnapshot() != nil {
		return false
	}
	var output boundedRefOutput
	if err := recoveryGit(ctx, repository, &output, "rev-parse", "--verify", record.Ref+"^{commit}"); err != nil {
		return false
	}
	return strings.TrimSpace(output.String()) == record.SnapshotSHA
}

// RetireOverflow removes an overflow entry that an already-authorized
// retention decision has retired: it unpins the exact owned ref in every
// supplied repository and then deletes the record. The caller must have
// established eligibility (landing proof, explicit abandonment, retain floor
// or a contentless justification) exactly as for a retained entry — this
// helper is the mechanism, not the decision.
func RetireOverflow(ctx context.Context, root string, expected Record, repositories []string) error {
	if err := expected.validateSnapshot(); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	path := filepath.Join(root, inventoryDirectoryName(expected), RecordFileName)
	current, err := ReadOverflowRecord(path)
	if err != nil {
		return err
	}
	if current != expected {
		return ErrRecordConflict
	}
	for _, repository := range repositories {
		if err := deleteExactRecoveryRef(ctx, repository, current.Ref, current.SnapshotSHA); err != nil {
			return err
		}
	}
	return DeleteOverflowEntry(root, current)
}

// DeleteOverflowEntry removes only the record directory. Promotion uses it
// once the same identity is durable as a bundle; retirement uses it after the
// ref is unpinned.
func DeleteOverflowEntry(root string, record Record) error {
	if err := record.validateSnapshot(); err != nil {
		return err
	}
	directory := filepath.Join(root, inventoryDirectoryName(record))
	if err := os.RemoveAll(directory); err != nil {
		return fmt.Errorf("remove recovery overflow entry: %w", err)
	}
	return durability.SyncDir(root)
}

// ImportSnapshotFromRepository makes an overflow entry's objects available in
// destination when there is no bundle to import from. source must be the
// repository holding the pin — the managed mirror for the record's repository.
//
// When destination IS that repository the objects are already local and only
// the ref is verified: fetching a repository into itself is not a no-op in
// Git, and doing it anyway would be a second way for this to fail.
func ImportSnapshotFromRepository(ctx context.Context, destination, source string, record Record) error {
	if err := record.validateSnapshot(); err != nil {
		return err
	}
	if source == "" || strings.HasPrefix(source, "-") || strings.ContainsAny(source, "\x00\r\n") {
		return fmt.Errorf("invalid recovery overflow source repository")
	}
	if !sameRecoveryRepository(destination, source) {
		// --no-tags/--no-write-fetch-head for the same reason FetchCurrentBase
		// uses them: this must leave FETCH_HEAD and the tag namespace of an
		// operator's checkout untouched.
		if err := recoveryGit(ctx, destination, io.Discard, "fetch", "--no-tags", "--no-write-fetch-head", "--no-recurse-submodules", "--",
			source, record.Ref+":"+record.Ref); err != nil {
			return fmt.Errorf("fetch recovery overflow objects: %w: %w", errRestoreImportFailed, err)
		}
	}
	var output boundedRefOutput
	if err := recoveryGit(ctx, destination, &output, "rev-parse", "--verify", record.Ref+"^{commit}"); err != nil {
		return fmt.Errorf("resolve recovery overflow ref: %w: %w", errRestoreImportFailed, err)
	}
	if strings.TrimSpace(output.String()) != record.SnapshotSHA {
		return fmt.Errorf("recovery overflow ref does not match its record: %w", errRestoreImportFailed)
	}
	if err := PinCommit(ctx, destination, record); err != nil {
		return fmt.Errorf("%w: %w", errRestoreImportFailed, err)
	}
	return nil
}

func sameRecoveryRepository(a, b string) bool {
	left, err := filepath.EvalSymlinks(a)
	if err != nil {
		return false
	}
	right, err := filepath.EvalSymlinks(b)
	if err != nil {
		return false
	}
	left, err = filepath.Abs(left)
	if err != nil {
		return false
	}
	right, err = filepath.Abs(right)
	if err != nil {
		return false
	}
	return left == right
}
