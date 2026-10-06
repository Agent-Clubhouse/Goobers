package recovery

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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
	hermetic, cleanup, err := hermeticPatchRepository(ctx, repository, len(base))
	if err != nil {
		return "", fmt.Errorf("capture recovery patch: %w", err)
	}
	defer cleanup()
	digest := sha256.New()
	// Explicit presentation flags prevent ordinary user diff preferences from
	// changing the retained bytes. Disable executable diff drivers/textconv;
	// recovery must retain binary content, not a lossy display conversion.
	// The diff runs in a hermetic repository so a worktree's .gitattributes
	// (e.g. diff=csharp hunk headers) cannot make a pod's digest differ from
	// the host mirror's recomputation (#6306).
	err = recoveryGitWithEnv(ctx, hermetic, io.MultiWriter(destination, digest), hermeticPatchEnvironment(),
		"-c", "core.attributesFile="+os.DevNull, "-c", "core.quotePath=true", "-c", "diff.suppressBlankEmpty=false",
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

// hermeticPatchRepository creates a temporary bare repository that borrows
// repository's objects through alternates. It has no working tree, index,
// in-tree or info attributes, so diff presentation depends only on objects.
func hermeticPatchRepository(ctx context.Context, repository string, idLength int) (string, func(), error) {
	var objects bytes.Buffer
	if err := recoveryGit(ctx, repository, &objects, "rev-parse", "--path-format=absolute", "--git-path", "objects"); err != nil {
		return "", nil, err
	}
	objectDir := strings.TrimSpace(objects.String())
	if objectDir == "" || strings.ContainsAny(objectDir, "\n\r") {
		return "", nil, fmt.Errorf("resolve recovery object directory")
	}
	directory, err := os.MkdirTemp("", "goobers-recovery-patch-*")
	if err != nil {
		return "", nil, err
	}
	cleanup := func() { _ = os.RemoveAll(directory) }
	format := "sha1"
	if idLength == 64 {
		format = "sha256"
	}
	command := exec.CommandContext(ctx, "git", "init", "--bare", "--quiet", "--template=", "--object-format="+format, directory)
	command.Env = append(nonGitEnvironment(), hermeticPatchEnvironment()...)
	if output, err := command.CombinedOutput(); err != nil {
		cleanup()
		return "", nil, fmt.Errorf("initialize hermetic patch repository: %w: %s", err, strings.TrimSpace(string(output)))
	}
	info := filepath.Join(directory, "objects", "info")
	if err := os.MkdirAll(info, 0o700); err != nil {
		cleanup()
		return "", nil, err
	}
	if err := os.WriteFile(filepath.Join(info, "alternates"), []byte(filepath.ToSlash(objectDir)+"\n"), 0o600); err != nil {
		cleanup()
		return "", nil, err
	}
	return directory, cleanup, nil
}

func hermeticPatchEnvironment() []string {
	return []string{"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=" + os.DevNull, "GIT_ATTR_NOSYSTEM=1"}
}

func nonGitEnvironment() []string {
	var environment []string
	for _, entry := range os.Environ() {
		if !strings.HasPrefix(strings.ToUpper(entry), "GIT_") {
			environment = append(environment, entry)
		}
	}
	return environment
}
