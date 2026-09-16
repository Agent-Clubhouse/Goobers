package worktree

import (
	"context"
	"errors"
	"os"
	"strings"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/credentials"
	"github.com/goobers/goobers/internal/workspacerevision"
)

// RemoteBranchAccess is a backend-resolved grant. Authorized must be explicit
// even for an intentionally anonymous source or an offline fixture remote.
// Tokens never leave the trusted Git operation's child environment.
type RemoteBranchAccess struct {
	Authorized bool
	Token      string
	Username   string
}

// RemoteBranchOptions contains only configured URLs and generated ownership.
// Callers must authorize the source against the run's configured repositories.
type RemoteBranchOptions struct {
	SourceURL   string
	TargetURL   string
	Binding     apiv1.WorkspaceBranchBinding
	SourceRead  RemoteBranchAccess
	TargetWrite RemoteBranchAccess
	TempDir     string
}

// EstablishRemoteBranch transfers a verified exact commit into a create-only
// remote ref. Its only persistent mutation is the generated target ref; neither
// source refs nor local stage repositories are ever modified.
func EstablishRemoteBranch(ctx context.Context, opts RemoteBranchOptions) (err error) {
	return transferRemoteBranch(ctx, opts, "")
}

// PublishRemoteBranch imports stage commits into a sterile backend repository,
// verifies ancestry, and compare-and-swaps only the durably owned remote ref.
// The stage's Git config and hooks never receive target credentials.
func PublishRemoteBranch(ctx context.Context, opts RemoteBranchOptions, workspace string) error {
	if workspace == "" {
		return revisionFailure(workspacerevision.CodeInvalid, "publication requires a workspace", nil)
	}
	return transferRemoteBranch(ctx, opts, workspace)
}

// VerifyOwnedWorkspace checks continuity without resolving any moving source
// ref. Local commits are allowed, but they must retain the immutable root.
func VerifyOwnedWorkspace(ctx context.Context, path string, binding apiv1.WorkspaceBranchBinding) error {
	if err := binding.Validate(); err != nil {
		return revisionFailure(workspacerevision.CodeInvalid, "invalid workspace ownership", err)
	}
	branch, err := gitOutput(ctx, path, "--no-replace-objects", "--no-lazy-fetch", "symbolic-ref", "-q", "HEAD")
	if err != nil || branch != binding.Ref {
		return revisionFailure(workspacerevision.CodeConflict, "workspace is not on the owned branch", err)
	}
	if _, err := gitOutput(ctx, path, "--no-replace-objects", "--no-lazy-fetch", "merge-base", "--is-ancestor", binding.StartingSHA, "HEAD"); err != nil {
		return revisionFailure(workspacerevision.CodeConflict, "workspace no longer descends from the owned starting SHA", err)
	}
	return nil
}

func validateOwnedStartingSHA(sha, branch string, syncBase bool) error {
	if sha == "" {
		return nil
	}
	if err := apiv1.ValidateCommitSHA(sha); err != nil {
		return revisionFailure(workspacerevision.CodeInvalid, "invalid owned starting SHA", err)
	}
	if branch == "" || syncBase {
		return revisionFailure(workspacerevision.CodeConflict, "owned workspaces require an attached branch without base synchronization", nil)
	}
	return nil
}

func transferRemoteBranch(ctx context.Context, opts RemoteBranchOptions, workspace string) (err error) {
	if err := opts.Binding.Validate(); err != nil {
		return revisionFailure(workspacerevision.CodeInvalid, "invalid remote ownership", err)
	}
	if opts.SourceURL == "" || opts.TargetURL == "" || !opts.SourceRead.Authorized || !opts.TargetWrite.Authorized {
		return revisionFailure(workspacerevision.CodeUnauthorized, "explicit source read and target write grants are required", nil)
	}
	dir, err := os.MkdirTemp(opts.TempDir, "goobers-remote-branch-")
	if err != nil {
		return revisionFailure(workspacerevision.CodeAcquisition, "create sterile transfer directory", err)
	}
	defer func() { err = errors.Join(err, os.RemoveAll(dir)) }()
	script, err := credentials.WriteAskpassScript(dir)
	if err != nil {
		return revisionFailure(workspacerevision.CodeAcquisition, "prepare transfer authentication", err)
	}
	local := sterileBranchEnv(dir, script, RemoteBranchAccess{})
	source := sterileBranchEnv(dir, script, opts.SourceRead)
	target := sterileBranchEnv(dir, script, opts.TargetWrite)
	run := func(env []string, args ...string) (string, error) {
		prefix := []string{"--no-replace-objects", "--no-lazy-fetch", "-c", "credential.helper=",
			"-c", "http.followRedirects=false", "-c", "protocol.allow=never",
			"-c", "protocol.https.allow=always", "-c", "protocol.http.allow=always",
			"-c", "protocol.file.allow=always", "-c", "fetch.recurseSubmodules=false",
			"-c", "submodule.recurse=false"}
		out, err := rawGitOutput(ctx, dir, env, append(prefix, args...)...)
		return strings.TrimSpace(string(out)), err
	}
	format := "sha1"
	if len(opts.Binding.StartingSHA) == 64 {
		format = "sha256"
	}
	if _, err := run(local, "init", "--bare", "--template=", "--object-format="+format, "."); err != nil {
		return revisionFailure(workspacerevision.CodeAcquisition, "initialize sterile transfer repository", err)
	}
	readTip := func() (string, error) {
		out, err := run(target, "ls-remote", "--refs", "--", opts.TargetURL, opts.Binding.Ref)
		if err != nil {
			return "", revisionFailure(workspacerevision.CodeAcquisition, "inspect remote workspace branch", err)
		}
		if out == "" {
			return "", nil
		}
		fields := strings.Fields(out)
		if len(fields) != 2 || fields[1] != opts.Binding.Ref || apiv1.ValidateCommitSHA(fields[0]) != nil {
			return "", revisionFailure(workspacerevision.CodeSHAMismatch, "remote returned a substituted ref", nil)
		}
		return fields[0], nil
	}
	tip, err := readTip()
	if err != nil {
		return err
	}
	sha := opts.Binding.StartingSHA
	if workspace == "" && tip != "" && tip != sha {
		return revisionFailure(workspacerevision.CodeConflict, "remote workspace branch already points to another commit", nil)
	}
	fetchURL, fetchEnv := opts.SourceURL, source
	if workspace != "" {
		if tip == "" {
			return revisionFailure(workspacerevision.CodeConflict, "owned remote branch disappeared before publication", nil)
		}
		out, readErr := rawGitOutput(ctx, workspace, local, "--no-replace-objects", "--no-lazy-fetch",
			"-c", "safe.directory="+workspace, "rev-parse", "--verify", "HEAD")
		sha = strings.TrimSpace(string(out))
		if readErr != nil || apiv1.ValidateCommitSHA(sha) != nil {
			return revisionFailure(workspacerevision.CodeSHAMismatch, "publication has no exact commit", readErr)
		}
		fetchURL, fetchEnv = workspace, local
	} else if tip == sha {
		// Reconcile a crash after create, without requiring the source still
		// to serve the object. Verify the target object before recording it.
		fetchURL, fetchEnv = opts.TargetURL, target
	}
	if _, err := run(fetchEnv, "fetch", "--no-tags", "--no-recurse-submodules", "--", fetchURL, sha); err != nil {
		return revisionFailure(workspacerevision.CodeAcquisition, "fetch exact authorized commit", err)
	}
	fetched, err := run(local, "rev-parse", "--verify", "FETCH_HEAD")
	if err != nil || fetched != sha {
		return revisionFailure(workspacerevision.CodeSHAMismatch, "source supplied another object", err)
	}
	kind, err := run(local, "cat-file", "-t", sha)
	if err != nil {
		return revisionFailure(workspacerevision.CodeAcquisition, "read transferred object", err)
	}
	if kind != "commit" {
		return revisionFailure(workspacerevision.CodeObjectType, "selected object is not a commit", nil)
	}
	commit, err := run(local, "rev-parse", "--verify", sha+"^{commit}")
	if err != nil || commit != sha {
		return revisionFailure(workspacerevision.CodeSHAMismatch, "transferred commit identity differs", err)
	}
	expectedOld := ""
	if workspace != "" {
		if _, err := run(target, "fetch", "--no-tags", "--no-recurse-submodules", "--", opts.TargetURL, tip); err != nil {
			return revisionFailure(workspacerevision.CodeAcquisition, "acquire current owned branch tip", err)
		}
		for _, ancestor := range []string{opts.Binding.StartingSHA, tip} {
			if _, err := run(local, "merge-base", "--is-ancestor", ancestor, sha); err != nil {
				return revisionFailure(workspacerevision.CodeConflict, "publication must descend from both starting SHA and current owned tip", err)
			}
		}
		expectedOld = tip
	}
	if tip == sha {
		return nil
	}
	_, pushErr := run(target, "push", "--porcelain", "--no-verify",
		"--force-with-lease="+opts.Binding.Ref+":"+expectedOld, "--", opts.TargetURL, sha+":"+opts.Binding.Ref)
	// A lost response or a concurrent identical creator is success. A
	// conflicting creator is never overwritten, even if push failed oddly.
	tip, err = readTip()
	if err != nil {
		return err
	}
	if tip == sha {
		return nil
	}
	if tip != "" && (workspace == "" || tip != expectedOld) {
		return revisionFailure(workspacerevision.CodeConflict, "remote workspace branch creation lost its lease", nil)
	}
	return revisionFailure(workspacerevision.CodeAcquisition, "remote workspace branch update did not complete", pushErr)
}

func sterileBranchEnv(home, script string, access RemoteBranchAccess) []string {
	env := make([]string, 0, 20)
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		switch strings.ToUpper(key) {
		case "PATH", "SYSTEMROOT", "WINDIR", "COMSPEC", "TEMP", "TMP", "TMPDIR":
			env = append(env, entry)
		}
	}
	env = append(env, "HOME="+home, "USERPROFILE="+home, "XDG_CONFIG_HOME="+home,
		"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_SYSTEM="+os.DevNull, "GIT_CONFIG_GLOBAL="+os.DevNull,
		"GIT_NO_REPLACE_OBJECTS=1", "GIT_LFS_SKIP_SMUDGE=1", "GCM_INTERACTIVE=never",
		"LC_ALL=C", "GOOBERS_GIT_USERNAME="+access.Username)
	return append(env, credentials.GitEnv(script, access.Token)...)
}
