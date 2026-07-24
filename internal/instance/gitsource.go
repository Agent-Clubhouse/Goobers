package instance

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"sync"

	"github.com/goobers/goobers/internal/capability"
	"github.com/goobers/goobers/internal/credentials"
	"github.com/goobers/goobers/internal/procenv"
)

const (
	defaultGitSourceRef         = "main"
	configSourcesDir            = "config-sources"
	workflowSourceCredentialRef = "workflow-source"
)

// GitTokenSource supplies the credential for remote config-source Git
// operations. The workflow-source constructor below narrows this to the
// dedicated configrepo:read grant.
type GitTokenSource interface {
	Token(context.Context) (string, error)
}

// GitSourceOptions identifies a Git-backed workflow configuration source.
type GitSourceOptions struct {
	// InstanceRoot owns the managed mirror and materialized snapshots.
	InstanceRoot string
	// Repository is either a local Git repository path or a remote HTTPS URL.
	Repository string
	// Ref is the branch to track. Empty defaults to main.
	Ref string
	// TokenSource is required for a remote repository and must be nil for a
	// local repository.
	TokenSource GitTokenSource
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
	tokenSource   GitTokenSource
	askpass       string
	createSymlink func(string, string) error

	mu                 sync.Mutex
	warnings           []string
	warningsByRevision map[string][]string
}

var _ ConfigSource = (*GitSource)(nil)

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
	default:
		if urlErr := validateRemoteGitURL(repository); urlErr != nil {
			if !os.IsNotExist(statErr) {
				return nil, fmt.Errorf("git config source: inspect repository: %w", statErr)
			}
			return nil, fmt.Errorf("git config source: %w", urlErr)
		}
	}

	sum := sha256.Sum256([]byte(repository))
	repositoryDir := filepath.Join(managedRoot, hex.EncodeToString(sum[:])[:16])
	source := &GitSource{
		repository:         repository,
		ref:                ref,
		local:              local,
		repositoryDir:      repositoryDir,
		createSymlink:      os.Symlink,
		warningsByRevision: make(map[string][]string),
	}

	if !local {
		source.mirror = filepath.Join(repositoryDir, "repo.git")
		if opts.TokenSource == nil {
			return nil, errors.New("git config source: remote repository requires configrepo:read credentials")
		}
		source.tokenSource = opts.TokenSource
		source.askpass, err = credentials.WriteAskpassScript(filepath.Join(repositoryDir, "auth"))
		if err != nil {
			return nil, fmt.Errorf("git config source: prepare authentication: %w", err)
		}
	} else if opts.TokenSource != nil {
		return nil, errors.New("git config source: local repository must not configure credentials")
	}
	return source, nil
}

func validateRemoteGitURL(repository string) error {
	parsed, err := url.Parse(repository)
	if err != nil {
		if !strings.HasPrefix(strings.ToLower(repository), "https://") {
			return errors.New("remote git url must use https")
		}
		return fmt.Errorf("remote git url is invalid: %w", err)
	}
	if !strings.EqualFold(parsed.Scheme, "https") {
		return errors.New("remote git url must use https")
	}
	if parsed.Opaque != "" || parsed.Host == "" {
		return errors.New("remote git url must be an absolute https url")
	}
	if parsed.User != nil {
		return errors.New("remote git url must not include userinfo")
	}
	if parsed.RawQuery != "" || parsed.Fragment != "" {
		return errors.New("remote git url must not include a query or fragment")
	}
	return nil
}

// NewWorkflowGitSource constructs a Git source from instance.yaml's
// workflowSource block. Remote authentication is isolated behind a dedicated
// configrepo:read grant backed only by workflowSource.token.
func NewWorkflowGitSource(instanceRoot string, source WorkflowSource, registrar credentials.SecretRegistrar) (*GitSource, error) {
	if err := source.Validate(); err != nil {
		return nil, fmt.Errorf("git config source: workflowSource: %w", err)
	}
	if source.Kind != WorkflowSourceKindGit {
		return nil, fmt.Errorf("git config source: workflowSource kind must be %q", WorkflowSourceKindGit)
	}

	repository := source.Path
	var tokenSource GitTokenSource
	if source.URL != "" {
		if registrar == nil {
			return nil, errors.New("git config source: remote workflow source requires a secret registrar")
		}
		repository = source.URL
		resolver, err := credentials.NewResolver([]credentials.TokenRef{{
			Name: workflowSourceCredentialRef,
			Env:  source.Token.Env,
			File: source.Token.File,
		}})
		if err != nil {
			return nil, fmt.Errorf("git config source: build workflow-source resolver: %w", err)
		}
		injector, err := credentials.NewInjector(resolver, []credentials.Grant{{
			Capability: string(capability.ConfigRepoRead),
			Ref:        workflowSourceCredentialRef,
		}}, registrar)
		if err != nil {
			return nil, fmt.Errorf("git config source: build workflow-source grant: %w", err)
		}
		tokenSource = &workflowSourceTokenSource{injector: injector}
	}

	return NewGitSource(GitSourceOptions{
		InstanceRoot: instanceRoot,
		Repository:   repository,
		Ref:          source.Ref,
		TokenSource:  tokenSource,
	})
}

type workflowSourceTokenSource struct {
	injector *credentials.Injector
}

func (s *workflowSourceTokenSource) Token(ctx context.Context) (string, error) {
	set, err := s.injector.Materialize(ctx, []string{string(capability.ConfigRepoRead)})
	if err != nil {
		return "", err
	}
	return set.Token(ctx, string(capability.ConfigRepoRead))
}

// Resolve fetches the source when needed and returns an immutable materialized
// view of the configured branch's committed tree.
func (s *GitSource) Resolve(ctx context.Context) (string, error) {
	if s == nil {
		return "", errors.New("git config source: nil source")
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.warnings = nil

	if _, err := s.gitOutput(ctx, "validate tracked ref", "check-ref-format", s.ref); err != nil {
		return "", fmt.Errorf("git config source: invalid tracked ref: %w", err)
	}

	repoArgs := []string{"-C", s.repository}
	if !s.local {
		if err := s.refreshMirror(ctx); err != nil {
			return "", err
		}
		repoArgs = []string{"--git-dir=" + s.mirror}
	}

	revisionBytes, err := s.gitOutput(
		ctx,
		"resolve tracked ref",
		append(repoArgs, "rev-parse", "--verify", "--end-of-options", s.ref+"^{commit}")...,
	)
	if err != nil {
		return "", fmt.Errorf("git config source: resolve tracked ref %q: %w", s.ref, err)
	}
	revision := strings.TrimSpace(string(revisionBytes))

	destination, warnings, err := s.materializeRevision(ctx, repoArgs, revision)
	if err != nil {
		return "", err
	}
	s.warnings = append([]string(nil), warnings...)
	return destination, nil
}

// Warnings reports non-fatal issues encountered while resolving the most
// recently materialized snapshot.
func (s *GitSource) Warnings() []string {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.warnings...)
}

func (s *GitSource) materializeRevision(ctx context.Context, repoArgs []string, revision string) (string, []string, error) {
	snapshotsDir := filepath.Join(s.repositoryDir, "snapshots")
	if err := os.MkdirAll(snapshotsDir, 0o755); err != nil {
		return "", nil, fmt.Errorf("git config source: create snapshots directory: %w", err)
	}
	destination := filepath.Join(snapshotsDir, revision)
	switch info, err := os.Lstat(destination); {
	case err == nil && !info.IsDir():
		return "", nil, fmt.Errorf("git config source: snapshot path %s is not a directory", destination)
	case err == nil:
		return destination, append([]string(nil), s.warningsByRevision[revision]...), nil
	case !os.IsNotExist(err):
		return "", nil, fmt.Errorf("git config source: inspect snapshot: %w", err)
	}

	staging, err := os.MkdirTemp(snapshotsDir, "tree-")
	if err != nil {
		return "", nil, fmt.Errorf("git config source: create snapshot: %w", err)
	}
	warnings, err := s.extractRevision(ctx, repoArgs, revision, staging)
	if err != nil {
		return "", nil, errors.Join(err, os.RemoveAll(staging))
	}
	renameFailure := os.Rename(staging, destination)
	if renameFailure == nil {
		s.warningsByRevision[revision] = append([]string(nil), warnings...)
		return destination, append([]string(nil), warnings...), nil
	}
	renameErr := fmt.Errorf("git config source: install snapshot: %w", renameFailure)
	switch info, statErr := os.Lstat(destination); {
	case statErr == nil && info.IsDir():
		if removeErr := os.RemoveAll(staging); removeErr != nil {
			return "", nil, errors.Join(renameErr, removeErr)
		}
		s.warningsByRevision[revision] = append([]string(nil), warnings...)
		return destination, append([]string(nil), warnings...), nil
	case statErr == nil:
		return "", nil, errors.Join(renameErr, os.RemoveAll(staging))
	default:
		return "", nil, errors.Join(renameErr, statErr, os.RemoveAll(staging))
	}
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
		if _, err := s.remoteGitOutput(
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
	if _, err := s.remoteGitOutput(
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

type gitTreeEntry struct {
	mode       string
	objectType string
	objectID   string
	name       string
}

func (s *GitSource) extractRevision(ctx context.Context, repoArgs []string, revision, destination string) ([]string, error) {
	output, err := s.gitOutput(
		ctx,
		"list tracked revision",
		append(repoArgs, "ls-tree", "-r", "-z", "--full-tree", revision)...,
	)
	if err != nil {
		return nil, fmt.Errorf("git config source: list revision %s: %w", revision, err)
	}
	var warnings []string

	for len(output) > 0 {
		end := bytes.IndexByte(output, 0)
		if end < 0 {
			return nil, fmt.Errorf("git config source: malformed tree listing for revision %s", revision)
		}
		entry, err := parseGitTreeEntry(output[:end])
		if err != nil {
			return nil, fmt.Errorf("git config source: parse revision %s: %w", revision, err)
		}
		output = output[end+1:]

		treePath, target, err := treeTarget(destination, entry.name)
		if err != nil {
			return nil, err
		}
		if entry.objectType != "blob" {
			return nil, fmt.Errorf(
				"git config source: unsupported tree entry type %q for %q",
				entry.objectType,
				entry.name,
			)
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return nil, fmt.Errorf("git config source: create parent for %q: %w", entry.name, err)
		}

		switch entry.mode {
		case "100644":
			if err := s.writeBlob(ctx, repoArgs, entry.objectID, target, 0o644); err != nil {
				return nil, fmt.Errorf("git config source: materialize %q: %w", entry.name, err)
			}
		case "100755":
			if err := s.writeBlob(ctx, repoArgs, entry.objectID, target, 0o755); err != nil {
				return nil, fmt.Errorf("git config source: materialize %q: %w", entry.name, err)
			}
		case "120000":
			linkTarget, err := s.gitOutput(
				ctx,
				"read symlink blob",
				append(repoArgs, "cat-file", "blob", entry.objectID)...,
			)
			if err != nil {
				return nil, fmt.Errorf("git config source: read symlink %q: %w", entry.name, err)
			}
			if err := validateGitSymlink(treePath, string(linkTarget)); err != nil {
				return nil, err
			}
			if err := s.createSymlink(filepath.FromSlash(string(linkTarget)), target); err != nil {
				if !isSymlinkPrivilegeError(err) {
					return nil, fmt.Errorf("git config source: materialize symlink %q: %w", entry.name, err)
				}
				if writeErr := os.WriteFile(target, linkTarget, 0o644); writeErr != nil {
					return nil, fmt.Errorf("git config source: materialize symlink fallback %q: %w", entry.name, writeErr)
				}
				warnings = append(warnings, fmt.Sprintf(
					"config-source symlink %q was materialized as a plain file because Windows symlink creation requires Developer Mode or elevated rights; its contents are the link target text.",
					treePath,
				))
			}
		default:
			return nil, fmt.Errorf("git config source: unsupported tree mode %q for %q", entry.mode, entry.name)
		}
	}
	return warnings, nil
}

func parseGitTreeEntry(record []byte) (gitTreeEntry, error) {
	metadata, name, ok := bytes.Cut(record, []byte{'\t'})
	if !ok || len(name) == 0 {
		return gitTreeEntry{}, errors.New("tree entry is missing a path")
	}
	fields := bytes.Fields(metadata)
	if len(fields) != 3 {
		return gitTreeEntry{}, errors.New("tree entry has malformed metadata")
	}
	return gitTreeEntry{
		mode:       string(fields[0]),
		objectType: string(fields[1]),
		objectID:   string(fields[2]),
		name:       string(name),
	}, nil
}

func (s *GitSource) writeBlob(
	ctx context.Context,
	repoArgs []string,
	objectID string,
	target string,
	mode os.FileMode,
) error {
	file, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}

	args := append(append([]string{}, repoArgs...), "cat-file", "blob", objectID)
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Env = gitSourceEnv()
	cmd.Stdout = file
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	runErr := cmd.Run()
	closeErr := file.Close()
	if runErr != nil {
		return errors.Join(
			s.commandError("read committed blob", runErr, stderr.String()),
			closeErr,
			os.Remove(target),
		)
	}
	if closeErr != nil {
		return closeErr
	}
	if err := os.Chmod(target, mode); err != nil {
		return err
	}
	return nil
}

func treeTarget(root, name string) (string, string, error) {
	if strings.ContainsRune(name, '\\') {
		return "", "", fmt.Errorf("tree path %q is not portable", name)
	}
	clean := path.Clean(name)
	if clean == "." || path.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, "../") {
		return "", "", fmt.Errorf("tree path %q escapes snapshot", name)
	}
	return clean, filepath.Join(root, filepath.FromSlash(clean)), nil
}

func validateGitSymlink(name, target string) error {
	if strings.ContainsRune(target, '\\') {
		return fmt.Errorf("tree symlink %q has a non-portable target", name)
	}
	linkPath := path.Clean(path.Join(path.Dir(name), target))
	if path.IsAbs(target) ||
		filepath.IsAbs(filepath.FromSlash(target)) ||
		linkPath == ".." ||
		strings.HasPrefix(linkPath, "../") {
		return fmt.Errorf("tree symlink %q escapes snapshot", name)
	}
	return nil
}

// isSymlinkPrivilegeError reports whether err is the Windows
// ERROR_PRIVILEGE_NOT_HELD error returned when symlink creation requires
// Developer Mode or elevated rights.
func isSymlinkPrivilegeError(err error) bool {
	return runtime.GOOS == "windows" && isWindowsSymlinkPrivilegeError(err)
}

func (s *GitSource) gitOutput(ctx context.Context, operation string, args ...string) ([]byte, error) {
	return s.gitOutputWithEnv(ctx, operation, gitSourceEnv(), args...)
}

func (s *GitSource) remoteGitOutput(ctx context.Context, operation string, args ...string) ([]byte, error) {
	token, err := s.tokenSource.Token(ctx)
	if err != nil {
		return nil, fmt.Errorf("resolve %s credential: %w", capability.ConfigRepoRead, err)
	}
	authHome := filepath.Dir(s.askpass)
	env := append(gitSourceEnv(),
		"HOME="+authHome,
		"XDG_CONFIG_HOME="+authHome,
		"USERPROFILE="+authHome,
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL="+os.DevNull,
		"GIT_ALLOW_PROTOCOL=https",
	)
	if caFile := os.Getenv("SSL_CERT_FILE"); caFile != "" {
		env = append(env, "GIT_SSL_CAINFO="+caFile)
	}
	env = append(env, credentials.GitEnv(s.askpass, token)...)
	args = append([]string{
		"-c", "credential.helper=",
		"-c", "http.extraHeader=",
		"-c", "http.cookieFile=",
		"-c", "http.saveCookies=false",
	}, args...)
	return s.gitOutputWithEnv(ctx, operation, env, args...)
}

func (s *GitSource) gitOutputWithEnv(ctx context.Context, operation string, env []string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Env = env
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	output, err := cmd.Output()
	if err != nil {
		return nil, s.commandError(operation, err, stderr.String())
	}
	return output, nil
}

func gitSourceEnv() []string {
	return append(procenv.BaseEnv(), "GIT_TERMINAL_PROMPT=0", "GIT_NO_REPLACE_OBJECTS=1")
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
