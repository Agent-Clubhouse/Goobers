package recovery

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
)

// PinCommit protects already committed objects from ordinary branch deletion
// and Git garbage collection. The coordinator must verify that repository is
// the repository identified by record.RepositoryKey. This does not preserve
// dirty files or protect against removal of the entire
// object database; an independent durable archive is still required before
// destructive worktree/repository cleanup may be acknowledged.
func PinCommit(ctx context.Context, repository string, record Record) error {
	if err := record.Validate(); err != nil {
		return err
	}
	for _, object := range []string{record.BaseSHA, record.SnapshotSHA} {
		if err := recoveryGit(ctx, repository, io.Discard, "cat-file", "-e", object+"^{commit}"); err != nil {
			return fmt.Errorf("recovery commit is unavailable: %w", err)
		}
	}
	if err := recoveryGit(ctx, repository, io.Discard, "merge-base", "--is-ancestor", record.BaseSHA, record.SnapshotSHA); err != nil {
		return fmt.Errorf("recovery snapshot does not descend from its base: %w", err)
	}
	digest, err := WriteSnapshotPatch(ctx, repository, record.BaseSHA, record.SnapshotSHA, io.Discard)
	if err != nil {
		return err
	}
	if digest != record.PatchDigest {
		return fmt.Errorf("recovery patch digest does not match snapshot")
	}
	// The all-zero old value makes creation compare-and-swap. No retry can
	// clobber a conflicting ref, including a concurrent publisher's ref.
	// Never dereference a symbolic ref: a retention pin must be independent
	// of branches that can advance or disappear, and must not write through
	// an alias into an operator's branch.
	zero := strings.Repeat("0", len(record.SnapshotSHA))
	if err := recoveryGit(ctx, repository, io.Discard, "update-ref", "--no-deref", record.Ref, record.SnapshotSHA, zero); err == nil {
		return nil
	}
	var current boundedRefOutput
	if err := recoveryGit(ctx, repository, &current, "rev-parse", "--verify", record.Ref); err != nil {
		return fmt.Errorf("create or inspect recovery ref: %w", err)
	}
	if strings.TrimSpace(current.String()) != record.SnapshotSHA {
		return ErrRecordConflict
	}
	// Reassert the expected old value on retries; a concurrent replacement
	// after inspection must not be accepted as a successful publication.
	if err := recoveryGit(ctx, repository, io.Discard, "update-ref", "--no-deref", record.Ref, record.SnapshotSHA, record.SnapshotSHA); err != nil {
		return fmt.Errorf("confirm recovery ref: %w", err)
	}
	return nil
}

// Only one object ID is expected; do not buffer arbitrary Git output.
type boundedRefOutput struct{ bytes.Buffer }

func (b *boundedRefOutput) Write(data []byte) (int, error) {
	if b.Len()+len(data) > 128 {
		return 0, fmt.Errorf("oversized recovery ref output")
	}
	return b.Buffer.Write(data)
}

func recoveryGit(ctx context.Context, repository string, stdout io.Writer, args ...string) error {
	return recoveryGitWithEnv(ctx, repository, stdout, nil, args...)
}

func recoveryGitWithEnv(ctx context.Context, repository string, stdout io.Writer, environment []string, args ...string) error {
	return recoveryGitIO(ctx, repository, stdout, nil, environment, args...)
}

func recoveryGitIO(ctx context.Context, repository string, stdout io.Writer, stdin io.Reader, environment []string, args ...string) error {
	command := exec.CommandContext(ctx, "git", append([]string{"-C", repository, "--no-replace-objects", "-c", "core.hooksPath=" + os.DevNull}, args...)...)
	// These local object/ref operations need no inherited Git transport or
	// repository overrides. In particular GIT_DIR must not defeat -C.
	for _, entry := range os.Environ() {
		if !strings.HasPrefix(strings.ToUpper(entry), "GIT_") {
			command.Env = append(command.Env, entry)
		}
	}
	command.Env = append(command.Env, environment...)
	command.Stdout = stdout
	command.Stdin = stdin
	// Git diagnostics can contain local paths or credential-bearing remote
	// URLs. Return the operation's exit error without echoing those bytes.
	command.Stderr = io.Discard
	return command.Run()
}
