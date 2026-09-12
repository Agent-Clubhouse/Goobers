package recovery

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
)

// emptyPatchDigest is WriteSnapshotPatch's digest when the snapshot tree
// matches its base: Git emits no patch bytes at all, so the digest covers the
// empty byte stream. It identifies a capture that would protect no work.
const emptyPatchDigest = "sha256:e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

// WriteSnapshotPatch streams a binary-capable patch without buffering it in
// memory. The digest covers exactly the bytes delivered to destination. A
// caller must discard partial output on error and durably publish successful
// output before acknowledging cleanup. This captures committed tree changes,
// not dirty or untracked work; those must first become a snapshot commit.
func WriteSnapshotPatch(ctx context.Context, repository, base, snapshot string, destination io.Writer) (string, error) {
	if !gitObjectID.MatchString(base) || !gitObjectID.MatchString(snapshot) || len(base) != len(snapshot) {
		return "", fmt.Errorf("invalid recovery patch object identity")
	}
	if destination == nil {
		return "", fmt.Errorf("recovery patch destination is required")
	}
	digest := sha256.New()
	// Explicit presentation flags prevent ordinary user diff preferences from
	// changing the retained bytes. Disable executable diff drivers/textconv;
	// recovery must retain binary content, not a lossy display conversion.
	err := recoveryGit(ctx, repository, io.MultiWriter(destination, digest),
		"-c", "core.quotePath=true", "-c", "diff.suppressBlankEmpty=false",
		"diff", "--binary", "--full-index", "--no-ext-diff", "--no-textconv",
		"--no-color", "--no-renames", "--no-relative", "--src-prefix=a/", "--dst-prefix=b/",
		"--unified=3", "--inter-hunk-context=0",
		"--diff-algorithm=myers", "--no-indent-heuristic", "--ignore-submodules=none",
		"--submodule=short", "-O", os.DevNull, base, snapshot, "--")
	if err != nil {
		return "", fmt.Errorf("capture recovery patch: %w", err)
	}
	return fmt.Sprintf("sha256:%x", digest.Sum(nil)), nil
}
