package recovery

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// PortableSnapshot carries a filtered tree on a synthetic root commit. It
// contains no source history, remotes, hooks or credential helpers. It is a
// transport object only; callers must never publish its synthetic ancestry.
type PortableSnapshot struct {
	Record  Record         `json:"record"`
	TreeSHA string         `json:"treeSha"`
	Policy  SnapshotPolicy `json:"policy"`
}

// WritePortableSnapshot emits a bounded standalone Git carrier of one already
// captured tree. This uses the existing recovery bundle verifier and leaves
// the real branch, index and working files unchanged.
func WritePortableSnapshot(ctx context.Context, repository string, snapshot ChildSnapshot, destination io.Writer, maxBytes int64) (PortableSnapshot, error) {
	if destination == nil || maxBytes <= 0 {
		return PortableSnapshot{}, fmt.Errorf("portable workspace requires a bounded destination")
	}
	if err := snapshot.validate(); err != nil {
		return PortableSnapshot{}, err
	}
	if err := verifySnapshotPolicy(ctx, repository, snapshot.Record.SnapshotSHA, snapshot.Policy); err != nil {
		return PortableSnapshot{}, err
	}
	root, err := portableRoot(ctx, repository, snapshot)
	if err != nil {
		return PortableSnapshot{}, err
	}
	ref, err := RefForSnapshot(snapshot.Record.RunID, root)
	if err != nil {
		return PortableSnapshot{}, err
	}
	patch, err := WriteSnapshotPatch(ctx, repository, root, root, io.Discard)
	if err != nil {
		return PortableSnapshot{}, err
	}
	record := Record{Version: 1, RunID: snapshot.Record.RunID, RepositoryKey: snapshot.Record.RepositoryKey, Ref: ref, BaseSHA: root, SnapshotSHA: root, PatchDigest: patch, CreatedAt: snapshot.Record.CreatedAt, RetainUntil: snapshot.Record.RetainUntil}
	count := &snapshotCountingWriter{destination: destination}
	digest, format, err := WriteSnapshotBundle(ctx, repository, record, count, maxBytes)
	if err != nil {
		return PortableSnapshot{}, err
	}
	record.ArchiveDigest, record.ArchiveFormat, record.ArchiveBytes = digest, format, count.bytes
	out := PortableSnapshot{Record: record, TreeSHA: snapshot.TreeSHA, Policy: snapshot.Policy}
	return out, out.Validate()
}

func portableRoot(ctx context.Context, repository string, snapshot ChildSnapshot) (string, error) {
	date := snapshot.Record.CreatedAt.UTC().Format(time.RFC3339)
	env := []string{"GIT_AUTHOR_NAME=Goobers Transport", "GIT_AUTHOR_EMAIL=transport@goobers.invalid", "GIT_COMMITTER_NAME=Goobers Transport", "GIT_COMMITTER_EMAIL=transport@goobers.invalid", "GIT_AUTHOR_DATE=" + date, "GIT_COMMITTER_DATE=" + date}
	message := "Filtered child workspace transport for " + snapshot.Record.RunID + "\n\nCapture identity: " + snapshot.Record.CreatedAt.UTC().Format(time.RFC3339Nano)
	var output boundedRefOutput
	if err := recoveryGitWithEnv(ctx, repository, &output, env, "-c", "commit.gpgsign=false", "commit-tree", snapshot.TreeSHA, "-m", message); err != nil {
		return "", err
	}
	root := strings.TrimSpace(output.String())
	if !gitObjectID.MatchString(root) {
		return "", fmt.Errorf("invalid portable workspace commit")
	}
	return root, nil
}

// Validate checks the closed portable receipt shape, not object availability.
func (s PortableSnapshot) Validate() error {
	if err := s.Record.Validate(); err != nil {
		return err
	}
	if err := s.Policy.Validate(); err != nil {
		return err
	}
	if s.Record.ArchiveFormat != archiveFormatFull || s.Record.BaseRef != "" || s.Record.BaseSHA != s.Record.SnapshotSHA || !gitObjectID.MatchString(s.TreeSHA) {
		return fmt.Errorf("portable workspace requires a standalone root tree")
	}
	return nil
}

// ImportPortableSnapshot verifies bytes, root ancestry, tree and exclusions
// before making the received objects available. It moves no branch or index.
func ImportPortableSnapshot(ctx context.Context, repository, archive string, snapshot PortableSnapshot, maxBytes int64) error {
	if err := snapshot.Validate(); err != nil {
		return err
	}
	if err := ImportSnapshotBundle(ctx, repository, archive, snapshot.Record, maxBytes); err != nil {
		return err
	}
	return verifyPortableObjects(ctx, repository, snapshot)
}

func verifyPortableObjects(ctx context.Context, repository string, snapshot PortableSnapshot) error {
	var parents boundedRefOutput
	if err := recoveryGit(ctx, repository, &parents, "rev-list", "--parents", "-n", "1", snapshot.Record.SnapshotSHA); err != nil {
		return err
	}
	if strings.TrimSpace(parents.String()) != snapshot.Record.SnapshotSHA {
		return fmt.Errorf("portable workspace unexpectedly contains source history")
	}
	tree, err := snapshotObject(ctx, repository, snapshot.Record.SnapshotSHA+"^{tree}")
	if err != nil {
		return err
	}
	if tree != snapshot.TreeSHA {
		return fmt.Errorf("portable workspace tree does not match receipt")
	}
	return verifySnapshotPolicy(ctx, repository, snapshot.Record.SnapshotSHA, snapshot.Policy)
}

// InitializePortableWorkspace provisions an empty private pod volume without
// any remote transport or source credentials. Its branch is deliberately not a
// provider-visible branch. Pod changes return as a tree, not this commit log.
func InitializePortableWorkspace(ctx context.Context, repository, archive string, snapshot PortableSnapshot, maxBytes int64) error {
	entries, err := os.ReadDir(repository)
	if err != nil || len(entries) != 0 {
		return fmt.Errorf("portable workspace requires an empty private directory")
	}
	if err := recoveryGit(ctx, repository, io.Discard, "init", "--template=", "--initial-branch=goobers-child-transport", "."); err != nil {
		return err
	}
	if err := ImportPortableSnapshot(ctx, repository, archive, snapshot, maxBytes); err != nil {
		return err
	}
	if err := recoveryGit(ctx, repository, io.Discard, "-c", "core.hooksPath="+os.DevNull, "checkout", "-B", "goobers-child-transport", snapshot.Record.SnapshotSHA); err != nil {
		return err
	}
	// Persistent configuration contains no transports or inherited helpers.
	return recoveryGit(ctx, repository, io.Discard, "config", "core.hooksPath", filepath.ToSlash(os.DevNull))
}
