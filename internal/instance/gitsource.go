package instance

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"sync"
)

const (
	defaultGitSourceRef = "main"
	configSourcesDir    = "config-sources"
)

// GitSourceOptions identifies a Git-backed workflow configuration source.
type GitSourceOptions struct {
	// InstanceRoot owns the managed mirror and materialized snapshots.
	InstanceRoot string
	// Repository is either a local Git repository path or a remote Git URL.
	Repository string
	// Ref is the branch to track. Empty defaults to main.
	Ref string
}

// GitSource reads workflow configuration from the committed tree of a branch.
// Reuse a source across snapshots so remote fetches and materialization are
// serialized against its managed mirror.
type GitSource struct {
	repository    string
	ref           string
	local         bool
	repositoryDir string
	mirror        string

	mu sync.Mutex
}

// ConfigSnapshot is an immutable, disposable view of a configuration source.
type ConfigSnapshot struct {
	// Dir contains the materialized committed tree.
	Dir string
	// Revision is the commit ID from which Dir was materialized.
	Revision string

	cleanupDir string
	closeOnce  sync.Once
	closeErr   error
}

// NewGitSource constructs a Git-backed configuration source.
func NewGitSource(opts GitSourceOptions) (*GitSource, error) {
	if strings.TrimSpace(opts.InstanceRoot) == "" {
		return nil, errors.New("git config source: instance root is required")
	}
	if strings.TrimSpace(opts.Repository) == "" {
		return nil, errors.New("git config source: repository is required")
	}

	instanceRoot, err := filepath.Abs(opts.InstanceRoot)
	if err != nil {
		return nil, fmt.Errorf("git config source: resolve instance root: %w", err)
	}
	managedRoot := filepath.Join(instanceRoot, configSourcesDir)
	if err := os.MkdirAll(managedRoot, 0o755); err != nil {
		return nil, fmt.Errorf("git config source: create managed root: %w", err)
	}

	ref, err := normalizeGitSourceRef(opts.Ref)
	if err != nil {
		return nil, err
	}

	repository := opts.Repository
	local := false
	switch info, statErr := os.Stat(repository); {
	case statErr == nil && !info.IsDir():
		return nil, errors.New("git config source: local repository is not a directory")
	case statErr == nil:
		repository, err = filepath.Abs(repository)
		if err != nil {
			return nil, fmt.Errorf("git config source: resolve local repository: %w", err)
		}
		local = true
	case !os.IsNotExist(statErr):
		return nil, fmt.Errorf("git config source: inspect repository: %w", statErr)
	}

	sum := sha256.Sum256([]byte(repository))
	repositoryDir := filepath.Join(managedRoot, hex.EncodeToString(sum[:])[:16])
	source := &GitSource{
		repository:    repository,
		ref:           ref,
		local:         local,
		repositoryDir: repositoryDir,
	}
	if !local {
		source.mirror = filepath.Join(repositoryDir, "repo.git")
	}
	return source, nil
}

// Snapshot fetches the source when needed and materializes the configured
// branch's committed tree without consulting a local repository's checkout.
func (s *GitSource) Snapshot(ctx context.Context) (*ConfigSnapshot, error) {
	if s == nil {
		return nil, errors.New("git config source: nil source")
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if _, err := s.gitOutput(ctx, "validate tracked ref", "check-ref-format", s.ref); err != nil {
		return nil, fmt.Errorf("git config source: invalid tracked ref: %w", err)
	}

	repoArgs := []string{"-C", s.repository}
	if !s.local {
		if err := s.refreshMirror(ctx); err != nil {
			return nil, err
		}
		repoArgs = []string{"--git-dir=" + s.mirror}
	}

	revisionBytes, err := s.gitOutput(
		ctx,
		"resolve tracked ref",
		append(repoArgs, "rev-parse", "--verify", "--end-of-options", s.ref+"^{commit}")...,
	)
	if err != nil {
		return nil, fmt.Errorf("git config source: resolve tracked ref %q: %w", s.ref, err)
	}
	revision := strings.TrimSpace(string(revisionBytes))

	snapshotsDir := filepath.Join(s.repositoryDir, "snapshots")
	if err := os.MkdirAll(snapshotsDir, 0o755); err != nil {
		return nil, fmt.Errorf("git config source: create snapshots directory: %w", err)
	}
	dir, err := os.MkdirTemp(snapshotsDir, "tree-")
	if err != nil {
		return nil, fmt.Errorf("git config source: create snapshot: %w", err)
	}
	if err := s.extractRevision(ctx, repoArgs, revision, dir); err != nil {
		return nil, errors.Join(err, os.RemoveAll(dir))
	}
	return &ConfigSnapshot{Dir: dir, Revision: revision, cleanupDir: dir}, nil
}

// Close removes the materialized snapshot. It is safe to call more than once.
func (s *ConfigSnapshot) Close() error {
	if s == nil {
		return nil
	}
	s.closeOnce.Do(func() {
		if s.cleanupDir != "" {
			s.closeErr = os.RemoveAll(s.cleanupDir)
		}
	})
	return s.closeErr
}

func normalizeGitSourceRef(ref string) (string, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		ref = defaultGitSourceRef
	}
	switch {
	case strings.HasPrefix(ref, "refs/heads/"):
		return ref, nil
	case strings.HasPrefix(ref, "refs/"):
		return "", fmt.Errorf("git config source: ref %q is not a branch", ref)
	default:
		return "refs/heads/" + ref, nil
	}
}

func (s *GitSource) refreshMirror(ctx context.Context) error {
	switch _, err := os.Stat(s.mirror); {
	case os.IsNotExist(err):
		if err := s.cloneMirror(ctx); err != nil {
			return err
		}
	case err != nil:
		return fmt.Errorf("git config source: inspect managed mirror: %w", err)
	default:
		if _, err := s.gitOutput(
			ctx,
			"fetch managed mirror",
			"--git-dir="+s.mirror,
			"fetch",
			"--prune",
			"origin",
			"+refs/*:refs/*",
		); err != nil {
			return fmt.Errorf("git config source: fetch managed mirror: %w", err)
		}
	}
	return nil
}

func (s *GitSource) cloneMirror(ctx context.Context) error {
	if err := os.MkdirAll(s.repositoryDir, 0o755); err != nil {
		return fmt.Errorf("git config source: create mirror directory: %w", err)
	}
	staging, err := os.MkdirTemp(s.repositoryDir, "clone-")
	if err != nil {
		return fmt.Errorf("git config source: create mirror staging directory: %w", err)
	}
	defer func() { _ = os.RemoveAll(staging) }()

	stagedMirror := filepath.Join(staging, "repo.git")
	if _, err := s.gitOutput(
		ctx,
		"clone managed mirror",
		"clone",
		"--mirror",
		"--",
		s.repository,
		stagedMirror,
	); err != nil {
		return fmt.Errorf("git config source: clone managed mirror: %w", err)
	}
	if err := os.Rename(stagedMirror, s.mirror); err != nil {
		if _, statErr := os.Stat(s.mirror); statErr == nil {
			return nil
		}
		return fmt.Errorf("git config source: install managed mirror: %w", err)
	}
	return nil
}

func (s *GitSource) extractRevision(ctx context.Context, repoArgs []string, revision, destination string) error {
	args := append(append([]string{}, repoArgs...), "archive", "--format=tar", revision)
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("git config source: open archive stream: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("git config source: start archive: %w", err)
	}
	extractErr := extractGitArchive(stdout, destination)
	if extractErr != nil {
		_ = stdout.Close()
	}
	waitErr := cmd.Wait()
	if extractErr != nil {
		return fmt.Errorf("git config source: extract revision %s: %w", revision, extractErr)
	}
	if waitErr != nil {
		return s.commandError("archive tracked revision", waitErr, stderr.String())
	}
	return nil
}

func extractGitArchive(r io.Reader, destination string) error {
	tr := tar.NewReader(r)
	for {
		header, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}

		archivePath, target, err := archiveTarget(destination, header.Name)
		if err != nil {
			return err
		}
		switch header.Typeflag {
		case tar.TypeXGlobalHeader, tar.TypeXHeader:
			continue
		case tar.TypeDir:
			if err := os.MkdirAll(target, os.FileMode(header.Mode)&0o777); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			mode := os.FileMode(header.Mode) & 0o777
			file, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
			if err != nil {
				return err
			}
			_, copyErr := io.Copy(file, tr)
			closeErr := file.Close()
			if err := errors.Join(copyErr, closeErr); err != nil {
				return err
			}
		case tar.TypeSymlink:
			if strings.ContainsRune(header.Linkname, '\\') {
				return fmt.Errorf("archive symlink %q has a non-portable target", header.Name)
			}
			linkPath := path.Clean(path.Join(path.Dir(archivePath), header.Linkname))
			if path.IsAbs(header.Linkname) ||
				filepath.IsAbs(filepath.FromSlash(header.Linkname)) ||
				linkPath == ".." ||
				strings.HasPrefix(linkPath, "../") {
				return fmt.Errorf("archive symlink %q escapes snapshot", header.Name)
			}
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			if err := os.Symlink(filepath.FromSlash(header.Linkname), target); err != nil {
				return err
			}
		default:
			return fmt.Errorf("unsupported archive entry type %d for %q", header.Typeflag, header.Name)
		}
	}
}

func archiveTarget(root, name string) (string, string, error) {
	if strings.ContainsRune(name, '\\') {
		return "", "", fmt.Errorf("archive path %q is not portable", name)
	}
	clean := path.Clean(name)
	if clean == "." || path.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, "../") {
		return "", "", fmt.Errorf("archive path %q escapes snapshot", name)
	}
	return clean, filepath.Join(root, filepath.FromSlash(clean)), nil
}

func (s *GitSource) gitOutput(ctx context.Context, operation string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	output, err := cmd.Output()
	if err != nil {
		return nil, s.commandError(operation, err, stderr.String())
	}
	return output, nil
}

func (s *GitSource) commandError(operation string, cause error, stderr string) error {
	detail := strings.TrimSpace(strings.ReplaceAll(stderr, s.repository, "<repository>"))
	if len(detail) > 4096 {
		detail = detail[:4096] + "..."
	}
	if detail == "" {
		return fmt.Errorf("%s: %w", operation, cause)
	}
	return fmt.Errorf("%s: %w: %s", operation, cause, detail)
}
