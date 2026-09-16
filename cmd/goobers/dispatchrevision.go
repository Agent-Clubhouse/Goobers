package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/dispatcher"
	"github.com/goobers/goobers/internal/workspacerevision"
)

func podRevisionFailure(code, message string, cause error) error {
	return &workspacerevision.Error{Code: code, Message: message, Cause: cause}
}

func decodePodRevisionJSON(raw string, target any) error {
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return podRevisionFailure(workspacerevision.CodeInvalid, "invalid selected revision transport", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return podRevisionFailure(workspacerevision.CodeInvalid, "trailing selected revision transport", err)
	}
	return nil
}

func podWorkspaceRevision() (*apiv1.WorkspaceRevision, *dispatcher.WorkspaceCheckout, error) {
	raw, transport := os.Getenv(dispatcher.EnvWorkspaceRevision), os.Getenv(dispatcher.EnvWorkspaceCheckout)
	if raw == "" && os.Getenv(dispatcher.EnvWorkspaceBranchBinding) != "" {
		if _, _, err := podWorkspaceBranch(); err != nil {
			return nil, nil, err
		}
		return nil, nil, nil
	}
	if raw == "" && transport == "" {
		return nil, nil, nil
	}
	if raw == "" {
		return nil, nil, podRevisionFailure(workspacerevision.CodeInvalid, "configured checkout has no selected revision", nil)
	}
	if transport == "" {
		return nil, nil, podRevisionFailure(workspacerevision.CodeUnauthorized, "selected revision has no configured checkout authority", nil)
	}
	var revision apiv1.WorkspaceRevision
	var checkout dispatcher.WorkspaceCheckout
	if err := decodePodRevisionJSON(raw, &revision); err != nil {
		return nil, nil, err
	}
	if err := decodePodRevisionJSON(transport, &checkout); err != nil {
		return nil, nil, err
	}
	if err := revision.Validate(); err != nil {
		return nil, nil, podRevisionFailure(workspacerevision.CodeInvalid, "invalid selected revision identity", err)
	}
	if os.Getenv(dispatcher.EnvStageWorkspace) != string(apiv1.WorkspaceRepoReadOnly) {
		return nil, nil, podRevisionFailure(workspacerevision.CodeInvalid, "selected revision requires repo-readonly", nil)
	}
	if os.Getenv(dispatcher.EnvWorkspaceBranch) != "" || os.Getenv(dispatcher.EnvWorkspaceDelta) != "" ||
		os.Getenv(dispatcher.EnvStageSyncBase) != "" || os.Getenv(dispatcher.EnvCheckoutCapability) != "" {
		return nil, nil, podRevisionFailure(workspacerevision.CodeConflict, "selected revision cannot use writable provisioning controls", nil)
	}
	// Base provenance was authorized at dispatch. Recheck only that selected
	// identity matches the separately stamped configured source transport.
	sourceRevision := *revision.DeepCopy()
	sourceRevision.BaseRepository = nil
	if _, err := workspacerevision.Resolve(sourceRevision, checkout.Repository, nil); err != nil {
		return nil, nil, err
	}
	return &revision, &checkout, nil
}

func checkoutSelectedRevision(ctx context.Context, dir string, creds []dispatcher.MintedCredential, revision *apiv1.WorkspaceRevision, checkout dispatcher.WorkspaceCheckout) error {
	if _, err := os.Lstat(filepath.Join(dir, ".git")); !os.IsNotExist(err) {
		return podRevisionFailure(workspacerevision.CodeAcquisition, "selected checkout requires a fresh repository directory", err)
	}
	// Business-stage credentials are never a substitute for the source-scoped
	// checkout grant, even when they happen to be able to read the fork.
	var selected []dispatcher.MintedCredential
	for _, cred := range creds {
		if cred.Capability == dispatcher.WorkspaceRevisionCheckoutCapability {
			selected = append(selected, cred)
		}
	}
	if len(selected) != 1 || (selected[0].Value == "") != selected[0].Anonymous {
		return podRevisionFailure(workspacerevision.CodeUnauthorized, "exactly one authorized selected-source checkout grant is required", nil)
	}
	selected[0].Capability = "repo:read"
	sourceURL, err := checkoutCloneURL(checkout.Repository)
	if err != nil {
		return podRevisionFailure(workspacerevision.CodeInvalid, "invalid configured source transport", err)
	}
	auth, err := checkoutGitAuthEnv(dir, selected)
	if err != nil {
		return podRevisionFailure(workspacerevision.CodeAcquisition, "prepare selected-source authentication", err)
	}
	if selected[0].Anonymous {
		auth = anonymousRevisionEnvironment()
	}
	git := selectedRevisionGit{dir: dir, env: sterileRevisionEnvironment(auth)}
	format := "sha1"
	if len(revision.CommitSHA) == 64 {
		format = "sha256"
	}
	commands := [][]string{
		{"init", "--quiet", "--template=", "--object-format=" + format, "."},
		{"remote", "add", "origin", sourceURL},
	}
	if checkout.PartialClone {
		commands = append(commands, []string{"config", "remote.origin.promisor", "true"},
			[]string{"config", "remote.origin.partialclonefilter", "blob:none"})
	}
	for _, command := range commands {
		if _, err := git.output(ctx, command...); err != nil {
			return podRevisionFailure(workspacerevision.CodeAcquisition, "initialize selected-source checkout", err)
		}
	}
	// libcurl can read .netrc independently of Git credential helpers. Keep
	// both authenticated and anonymous acquisition out of the image's HOME.
	home, err := filepath.Abs(filepath.Join(dir, ".git", "goobers-auth-home"))
	if err != nil {
		return podRevisionFailure(workspacerevision.CodeAcquisition, "resolve isolated checkout home", err)
	}
	if err := os.Mkdir(home, 0o700); err != nil {
		return podRevisionFailure(workspacerevision.CodeAcquisition, "create isolated checkout home", err)
	}
	git.env = revisionHomeEnvironment(git.env, home)
	fetch := []string{"fetch", "--refetch", "--no-tags", "--no-recurse-submodules"}
	if checkout.PartialClone {
		fetch = append(fetch, "--filter=blob:none")
	}
	fetch = append(fetch, "--", "origin", revision.CommitSHA)
	if _, err := git.output(ctx, fetch...); err != nil {
		return podRevisionFailure(workspacerevision.CodeAcquisition, "acquire selected source object", err)
	}
	if fetched, err := git.output(ctx, "--no-lazy-fetch", "rev-parse", "--verify", "FETCH_HEAD"); err != nil || fetched != revision.CommitSHA {
		return podRevisionFailure(workspacerevision.CodeSHAMismatch, "source did not supply the selected SHA", err)
	}
	kind, err := git.output(ctx, "--no-lazy-fetch", "cat-file", "-t", revision.CommitSHA)
	if err != nil {
		return podRevisionFailure(workspacerevision.CodeAcquisition, "selected object is unavailable", err)
	}
	if kind != "commit" {
		return podRevisionFailure(workspacerevision.CodeObjectType, "selected object is not a commit", nil)
	}
	if checkout.Repository.Checkout != nil && len(checkout.Repository.Checkout.Sparse) > 0 {
		args := append([]string{"sparse-checkout", "set", "--cone", "--"}, checkout.Repository.Checkout.Sparse...)
		if _, err := git.output(ctx, args...); err != nil {
			return podRevisionFailure(workspacerevision.CodeAcquisition, "configure selected sparse checkout", err)
		}
	}
	if _, err := git.output(ctx, "checkout", "--quiet", "--detach", "--force", revision.CommitSHA); err != nil {
		return podRevisionFailure(workspacerevision.CodeAcquisition, "materialize selected commit", err)
	}
	return git.verifyHEAD(ctx, revision.CommitSHA)
}

// A fresh template-free repository and isolated config make every undeclared
// custom filter inert, including arbitrary process filters and URL rewrites.
func sterileRevisionEnvironment(auth []string) []string {
	env := make([]string, 0, len(auth))
	for _, entry := range auth {
		name, _, _ := strings.Cut(entry, "=")
		upper := strings.ToUpper(name)
		if strings.HasPrefix(upper, "GIT_") && upper != "GIT_ASKPASS" {
			continue
		}

		env = append(env, entry)
	}
	return append(env, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_SYSTEM="+os.DevNull,
		"GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_TERMINAL_PROMPT=0", "GIT_LFS_SKIP_SMUDGE=1")
}

func anonymousRevisionEnvironment() []string {
	var env []string
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		upper := strings.ToUpper(name)
		if upper == "GIT_ASKPASS" || upper == "SSH_ASKPASS" || strings.HasPrefix(upper, "GOOBERS_GIT_") {
			continue
		}
		env = append(env, entry)
	}
	return env
}

func revisionHomeEnvironment(base []string, home string) []string {
	var env []string
	for _, entry := range base {
		name, _, _ := strings.Cut(entry, "=")
		switch strings.ToUpper(name) {
		case "HOME", "USERPROFILE", "CURL_HOME":
			continue
		}
		env = append(env, entry)
	}
	return append(env, "HOME="+home, "USERPROFILE="+home, "CURL_HOME="+home)
}

type selectedRevisionGit struct {
	dir string
	env []string
}

func (g selectedRevisionGit) output(ctx context.Context, args ...string) (string, error) {
	prefix := []string{"--no-replace-objects", "-c", "safe.directory=" + workspaceGitEnv(g.dir).path,
		"-c", "core.hooksPath=" + os.DevNull, "-c", "core.fsmonitor=false",
		"-c", "credential.helper=", "-c", "submodule.recurse=false", "-c", "fetch.recurseSubmodules=false",
		"-c", "filter.lfs.clean=", "-c", "filter.lfs.smudge=", "-c", "filter.lfs.process=", "-c", "filter.lfs.required=false",
		"-c", "gc.auto=0", "-c", "maintenance.auto=false"}
	cmd := exec.CommandContext(ctx, "git", append(prefix, args...)...)
	cmd.Dir, cmd.Env = g.dir, g.env
	// Do not emit transport errors containing credentials to pod logs.
	out, err := cmd.Output()
	return strings.TrimSpace(string(out)), err
}

func (g selectedRevisionGit) verifyHEAD(ctx context.Context, sha string) error {
	head, err := g.output(ctx, "--no-lazy-fetch", "rev-parse", "--verify", "HEAD")
	if err != nil || head != sha {
		return podRevisionFailure(workspacerevision.CodeSHAMismatch, "workspace HEAD differs from selected SHA", err)
	}
	_, err = g.output(ctx, "symbolic-ref", "-q", "HEAD")
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 1 {
		return podRevisionFailure(workspacerevision.CodeSHAMismatch, "selected workspace is not detached", err)
	}
	return nil
}

func podWorkspaceFailureCode(fallback string, err error) string {
	var refusal *workspacerevision.Error
	if errors.As(err, &refusal) {
		return refusal.StageErrorCode()
	}
	return fallback
}

func podWorkspaceFailure(fallback string, err error) apiv1.ResultEnvelope {
	result := failureEnvelope(podWorkspaceFailureCode(fallback, err), err.Error())
	var refusal *workspacerevision.Error
	if errors.As(err, &refusal) {
		result.Error.Retryable = !refusal.NonRetryable()
	}
	return result
}

func podResultWorkspaceRevision(data []byte) (*apiv1.WorkspaceRevision, error) {
	var outputs map[string]json.RawMessage
	if err := json.Unmarshal(data, &outputs); err != nil {
		return nil, nil
	}
	raw, ok := outputs["workspaceRevision"]
	if !ok {
		return nil, nil
	}
	var revision *apiv1.WorkspaceRevision
	if err := decodePodRevisionJSON(string(raw), &revision); err != nil {
		return nil, err
	}
	if revision == nil {
		return nil, podRevisionFailure(workspacerevision.CodeInvalid, "workspaceRevision result control must be an object", nil)
	}
	if err := revision.Validate(); err != nil {
		return nil, podRevisionFailure(workspacerevision.CodeInvalid, "invalid workspaceRevision result control", err)
	}
	return revision, nil
}
