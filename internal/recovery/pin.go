package recovery

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// PinCommit protects already committed objects from ordinary branch deletion
// and Git garbage collection. The coordinator must verify that repository is
// the repository identified by record.RepositoryKey. This does not preserve
// dirty files or protect against removal of the entire
// object database; an independent durable archive is still required before
// destructive worktree/repository cleanup may be acknowledged.
func PinCommit(ctx context.Context, repository string, record Record) error {
	if err := record.validateSnapshot(); err != nil {
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
	trustedPath, err := filepath.Abs(repository)
	if err != nil {
		return fmt.Errorf("resolve recovery repository: %w", err)
	}
	trustedPath, err = filepath.EvalSymlinks(trustedPath)
	if err != nil {
		return fmt.Errorf("resolve recovery repository: %w", err)
	}
	// Recovery callers explicitly select this repository. Stage mounts may
	// belong to the host UID, so replace ambient ownership exemptions with
	// this exact resolved path, never '*' or an inherited GIT_CONFIG_* value.
	command := exec.CommandContext(ctx, "git", append([]string{"-C", trustedPath, "--no-replace-objects", "-c", "core.hooksPath=" + os.DevNull,
		"-c", "safe.directory=", "-c", "safe.directory=" + trustedPath}, args...)...)
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
	// These operations are all local (no network transport; GIT_* stripped
	// above; safe.directory pinned to trustedPath), so stderr cannot carry a
	// remote credential or URL the way a clone/fetch failure could. Capture
	// it bounded, rather than discarding it, so a failure names the git
	// subcommand, the exit code, and the diagnostic instead of a bare exit
	// status (#5352). The bound protects the journal, not a secret.
	var stderr boundedCaptureStderr
	command.Stderr = &stderr
	if err := command.Run(); err != nil {
		exitCode := -1
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			exitCode = exitErr.ExitCode()
		}
		return newCaptureError(args, exitCode, stderr.String(), err)
	}
	return nil
}

// boundedCaptureStderr accumulates a command's stderr without holding more
// than captureStderrBound bytes plus one write's worth of overrun; the exact
// tail is trimmed by boundedTail once the command has finished. Unlike
// boundedRefOutput this never fails the write — a truncated diagnostic is
// still useful, and a lost error message here would leave the caller with
// less evidence than before this fix.
type boundedCaptureStderr struct {
	buf []byte
}

func (b *boundedCaptureStderr) Write(data []byte) (int, error) {
	if len(b.buf) < captureStderrBound*2 {
		b.buf = append(b.buf, data...)
	}
	return len(data), nil
}

func (b *boundedCaptureStderr) String() string { return string(b.buf) }
