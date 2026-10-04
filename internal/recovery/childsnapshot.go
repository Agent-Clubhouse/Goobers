package recovery

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"
)

// ErrWorkspaceChanged refuses a capture or disposition against a moved parent.
var ErrWorkspaceChanged = errors.New("parent workspace changed since the expected snapshot")

// ChildSnapshot is a filtered workspace receipt. Record binds the exact HEAD
// prerequisite and immutable snapshot; IndexDigest separately detects staging
// changes that produce the same working tree. Policy omissions must never be
// treated as deletions when this snapshot is applied back to the parent.
type ChildSnapshot struct {
	Record      Record         `json:"record"`
	TreeSHA     string         `json:"treeSha"`
	IndexDigest string         `json:"indexDigest"`
	Policy      SnapshotPolicy `json:"policy"`
}

// CaptureChildSnapshot captures a parent's current permitted working state.
// The caller holds exclusive workspace custody. HEAD, real index, branch and
// working files remain unchanged. The receipt is not a durable carrier until
// WriteChildSnapshotBundle succeeds and the caller durably stores its bytes.
func CaptureChildSnapshot(ctx context.Context, repository, repositoryKey, runID string, identityTime, retainUntil time.Time, policy SnapshotPolicy) (ChildSnapshot, error) {
	if err := policy.Validate(); err != nil {
		return ChildSnapshot{}, err
	}
	if !validRepositoryKey(repositoryKey) || identityTime.IsZero() || !retainUntil.After(identityTime) {
		return ChildSnapshot{}, fmt.Errorf("invalid child snapshot identity or retention window")
	}
	policy.ExcludedPaths = slices.Clone(policy.ExcludedPaths)
	slices.Sort(policy.ExcludedPaths)
	head, index, err := workspaceSnapshotIdentity(ctx, repository)
	if err != nil {
		return ChildSnapshot{}, err
	}
	snapshot, err := captureSnapshot(ctx, repository, runID, identityTime, &policy)
	if err != nil {
		return ChildSnapshot{}, err
	}
	afterHead, afterIndex, err := workspaceSnapshotIdentity(ctx, repository)
	if err != nil {
		return ChildSnapshot{}, err
	}
	if head != afterHead || index != afterIndex {
		return ChildSnapshot{}, ErrWorkspaceChanged
	}
	if err := verifySnapshotPolicy(ctx, repository, snapshot, policy); err != nil {
		return ChildSnapshot{}, err
	}
	tree, err := snapshotObject(ctx, repository, snapshot+"^{tree}")
	if err != nil {
		return ChildSnapshot{}, err
	}
	digest, err := WriteSnapshotPatch(ctx, repository, head, snapshot, io.Discard)
	if err != nil {
		return ChildSnapshot{}, err
	}
	ref, err := RefForSnapshot(runID, snapshot)
	if err != nil {
		return ChildSnapshot{}, err
	}
	record := Record{Version: 1, RunID: runID, RepositoryKey: repositoryKey, Ref: ref, BaseRef: head, BaseSHA: head, SnapshotSHA: snapshot, PatchDigest: digest, CreatedAt: identityTime, RetainUntil: retainUntil}
	return ChildSnapshot{Record: record, TreeSHA: tree, IndexDigest: index, Policy: policy}, nil
}

func workspaceSnapshotIdentity(ctx context.Context, repository string) (string, string, error) {
	head, err := snapshotObject(ctx, repository, "HEAD^{commit}")
	if err != nil {
		return "", "", err
	}
	hash := sha256.New()
	output := &archiveBudgetWriter{destination: hash, remaining: maxSnapshotIndexBytes}
	if err := recoveryGit(ctx, repository, output, "ls-files", "--stage", "-z"); err != nil {
		return "", "", err
	}
	// Include sparse/assume-unchanged flags as well as logical staged entries.
	if err := recoveryGit(ctx, repository, output, "ls-files", "-v", "-z"); err != nil {
		return "", "", err
	}
	return head, fmt.Sprintf("sha256:%x", hash.Sum(nil)), nil
}

func snapshotObject(ctx context.Context, repository, ref string) (string, error) {
	var out boundedRefOutput
	if err := recoveryGit(ctx, repository, &out, "rev-parse", "--verify", ref); err != nil {
		return "", err
	}
	value := strings.TrimSpace(out.String())
	if !gitObjectID.MatchString(value) {
		return "", fmt.Errorf("invalid snapshot object identity")
	}
	return value, nil
}

// WriteChildSnapshotBundle emits a bounded delta using the normal recovery
// carrier. It deliberately never falls back to ancestor-history transfer:
// the receiving mirror must already hold Record.BaseSHA through normal source
// acquisition or workspace continuity. Discard partial bytes after any error.
func WriteChildSnapshotBundle(ctx context.Context, repository string, snapshot ChildSnapshot, destination io.Writer, maxBytes int64) (ChildSnapshot, error) {
	if err := snapshot.validate(); err != nil {
		return ChildSnapshot{}, err
	}
	if destination == nil {
		return ChildSnapshot{}, fmt.Errorf("child snapshot destination is required")
	}
	if err := verifySnapshotPolicy(ctx, repository, snapshot.Record.SnapshotSHA, snapshot.Policy); err != nil {
		return ChildSnapshot{}, err
	}
	counter := &snapshotCountingWriter{destination: destination}
	digest, format, err := WriteSnapshotBundle(ctx, repository, snapshot.Record, counter, maxBytes)
	if err != nil {
		return ChildSnapshot{}, err
	}
	if format != archiveFormatDelta {
		return ChildSnapshot{}, fmt.Errorf("child snapshot requires its exact base prerequisite")
	}
	snapshot.Record.ArchiveDigest = digest
	snapshot.Record.ArchiveFormat = format
	snapshot.Record.ArchiveBytes = counter.bytes
	return snapshot, nil
}

type snapshotCountingWriter struct {
	destination io.Writer
	bytes       int64
}

func (w *snapshotCountingWriter) Write(data []byte) (int, error) {
	n, err := w.destination.Write(data)
	w.bytes += int64(n)
	return n, err
}

func (s ChildSnapshot) validate() error {
	if err := s.Policy.Validate(); err != nil {
		return err
	}
	if err := s.Record.validateRestorable(); err != nil {
		return err
	}
	if s.Record.BaseRef != s.Record.BaseSHA || !gitObjectID.MatchString(s.TreeSHA) || !patchDigest.MatchString(s.IndexDigest) {
		return fmt.Errorf("invalid child snapshot receipt")
	}
	return nil
}

// ImportChildSnapshot verifies and imports the child carrier without moving a
// checkout or branch. Parent-child authorization is the coordinator's job;
// receipts must come from its durable state, not from the proposing agent.
func ImportChildSnapshot(ctx context.Context, repository, archive string, snapshot ChildSnapshot, maxBytes int64) error {
	if err := snapshot.validate(); err != nil {
		return err
	}
	if snapshot.Record.ArchiveFormat != archiveFormatDelta {
		return fmt.Errorf("child snapshot carrier must be a delta")
	}
	if err := ImportSnapshotBundle(ctx, repository, archive, snapshot.Record, maxBytes); err != nil {
		return err
	}
	tree, err := snapshotObject(ctx, repository, snapshot.Record.SnapshotSHA+"^{tree}")
	if err != nil {
		return err
	}
	if tree != snapshot.TreeSHA {
		return fmt.Errorf("child snapshot tree receipt mismatch")
	}
	return verifySnapshotPolicy(ctx, repository, snapshot.Record.SnapshotSHA, snapshot.Policy)
}

// CheckChildSnapshotCurrent checks both content and staged state under the
// caller's exclusive workspace lease. A match never authorizes mutation alone.
func CheckChildSnapshotCurrent(ctx context.Context, repository string, expected ChildSnapshot) error {
	if err := expected.validate(); err != nil {
		return err
	}
	current, err := CaptureChildSnapshot(ctx, repository, expected.Record.RepositoryKey, expected.Record.RunID, expected.Record.CreatedAt, expected.Record.RetainUntil, expected.Policy)
	if err != nil {
		return err
	}
	if current.Record.SnapshotSHA != expected.Record.SnapshotSHA || current.TreeSHA != expected.TreeSHA || current.IndexDigest != expected.IndexDigest {
		return ErrWorkspaceChanged
	}
	return nil
}

func readSnapshotTree(ctx context.Context, repository, snapshot string) ([]byte, error) {
	var entries bytes.Buffer
	writer := &archiveBudgetWriter{destination: &entries, remaining: maxSnapshotIndexBytes}
	if err := recoveryGit(ctx, repository, writer, "ls-tree", "-r", "-z", snapshot); err != nil {
		return nil, err
	}
	return entries.Bytes(), nil
}
