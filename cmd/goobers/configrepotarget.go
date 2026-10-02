package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path"
	"strings"
	"time"

	"github.com/goobers/goobers/internal/capability"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/providers"
)

// The config-repo target (TUT-A8, #1220; docs/design/tutor-redesign.md §4.8).
//
// A tutor workflow runs in one gaggle but its improvement PRs belong in the
// instance CONFIG repository, the one workflowSource loads definitions from.
// push-branch and open-pr gain an opt-in `target: config-repo` input that
// re-aims them at that repository and authenticates them with the stage's
// declared configrepo:write credential — a credential minted from
// workflowSource's own auth, never from a gaggle's product-repo token. The two
// credentials are not interchangeable in either direction: a config-repo
// stage never reads GOOBERS_CRED_REPO_PUSH or provider:pr:write, and a
// product-repo stage never reads GOOBERS_CRED_CONFIGREPO_WRITE.

const (
	// configRepoTargetInput names the opt-in input; its only non-empty value.
	configRepoTargetInput = "target"
	configRepoTargetValue = "config-repo"
	// configRepoInput / configRepoBaseInput address the config repository
	// where no instance config is readable (a stage pod): owner/name and the
	// base branch. They are verified against workflowSource when it IS
	// readable, and the installation token behind them is down-scoped to the
	// config repository, so a wrong value fails at the forge rather than
	// reaching another repo.
	configRepoInput     = "configRepo"
	configRepoBaseInput = "configRepoBase"
	// configRepoDirInput names the config-repo checkout directory, relative to
	// the stage workspace.
	configRepoDirInput   = "configRepoDir"
	defaultConfigRepoDir = "config-repo"
	configRepoHost       = "github.com"
)

// configRepoTarget is the config repository a config-repo-targeted stage
// addresses, resolved from workflowSource.
type configRepoTarget struct {
	Repo providers.RepositoryRef
	// Base is the branch PRs target and checkouts start from: workflowSource.ref.
	Base string
}

// CloneURL is the repository's HTTPS clone URL (no credential embedded).
func (t configRepoTarget) CloneURL() string {
	return "https://" + configRepoHost + "/" + t.Repo.Owner + "/" + t.Repo.Name + ".git"
}

// providerTargetIsConfigRepo reports whether the stage opted into the
// config-repo target, rejecting any other non-empty value so a typo cannot
// silently fall back to the product repository.
func providerTargetIsConfigRepo() (bool, error) {
	switch v := strings.TrimSpace(providerInput(configRepoTargetInput, "")); v {
	case "":
		return false, nil
	case configRepoTargetValue:
		return true, nil
	default:
		return false, fmt.Errorf("unknown target %q: the only supported value is %q (omit it for the gaggle's repository)", v, configRepoTargetValue)
	}
}

func configRepoDir() string {
	if d := strings.TrimSpace(providerInput(configRepoDirInput, "")); d != "" {
		return d
	}
	return defaultConfigRepoDir
}

// pushBranchConfigTarget resolves the config target (push-branch, open-pr) when
// the stage opted in; root is where the instance config is looked up.
func pushBranchConfigTarget(root string) (configRepoTarget, bool, error) {
	isConfig, err := providerTargetIsConfigRepo()
	if err != nil || !isConfig {
		return configRepoTarget{}, false, err
	}
	target, err := resolveConfigRepoTarget(root)
	return target, true, err
}

// parseConfigRepoURL derives owner/name from a workflowSource git URL. Only
// github.com is supported: the write credential is a GitHub App installation
// token, and a URL with no derivable owner/name fails closed.
func parseConfigRepoURL(rawURL string) (owner, name string, err error) {
	parsed, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		return "", "", fmt.Errorf("parse config repository url: %w", err)
	}
	if !strings.EqualFold(parsed.Hostname(), configRepoHost) {
		return "", "", fmt.Errorf("config repository %q: only %s is supported as a configrepo:write target", rawURL, configRepoHost)
	}
	trimmed := strings.Trim(strings.TrimSuffix(strings.Trim(parsed.Path, "/"), ".git"), "/")
	parts := strings.Split(trimmed, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" || path.Base(trimmed) != parts[1] {
		return "", "", fmt.Errorf("cannot derive owner/name of the config repository from %q", rawURL)
	}
	return parts[0], parts[1], nil
}

// configRepoTargetFromSource derives the target from an instance's
// workflowSource. No workflowSource, a local one, or one without a URL means
// there is no config repository to write to.
func configRepoTargetFromSource(source *instance.WorkflowSource) (configRepoTarget, error) {
	if source == nil {
		return configRepoTarget{}, errors.New("no workflowSource is configured, so there is no config repository to target; set workflowSource in instance.yaml")
	}
	if source.Kind != instance.WorkflowSourceKindGit || strings.TrimSpace(source.URL) == "" {
		return configRepoTarget{}, fmt.Errorf("workflowSource kind %q is not a remote git repository, so it cannot be a config-repo target", source.Kind)
	}
	owner, name, err := parseConfigRepoURL(source.URL)
	if err != nil {
		return configRepoTarget{}, err
	}
	return configRepoTarget{
		Repo: providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: owner, Name: name},
		Base: source.TrackedRef(),
	}, nil
}

// resolveConfigRepoTarget resolves the config repository. The instance's
// workflowSource is authoritative when its config is readable from root; the
// configRepo/configRepoBase inputs serve a stage pod, which carries no
// instance config, and must agree with workflowSource wherever both exist.
func resolveConfigRepoTarget(root string) (configRepoTarget, error) {
	inputRepo := strings.TrimSpace(providerInput(configRepoInput, ""))
	inputBase := strings.TrimSpace(providerInput(configRepoBaseInput, ""))

	cfg, loadErr := instance.LoadConfig(instance.NewLayout(root).ConfigFile())
	if loadErr == nil {
		target, err := configRepoTargetFromSource(cfg.WorkflowSource)
		if err != nil {
			return configRepoTarget{}, err
		}
		if inputRepo != "" && !strings.EqualFold(inputRepo, target.Repo.Owner+"/"+target.Repo.Name) {
			return configRepoTarget{}, fmt.Errorf("input %s %q does not match workflowSource repository %s/%s", configRepoInput, inputRepo, target.Repo.Owner, target.Repo.Name)
		}
		if inputBase != "" && inputBase != target.Base {
			return configRepoTarget{}, fmt.Errorf("input %s %q does not match workflowSource ref %q", configRepoBaseInput, inputBase, target.Base)
		}
		return target, nil
	}
	if inputRepo == "" {
		return configRepoTarget{}, fmt.Errorf("resolve config repository: no instance config is readable (%w) and no %s input names it", loadErr, configRepoInput)
	}
	owner, name, ok := strings.Cut(inputRepo, "/")
	if !ok || owner == "" || name == "" || strings.Contains(name, "/") {
		return configRepoTarget{}, fmt.Errorf("input %s must be owner/name, got %q", configRepoInput, inputRepo)
	}
	if inputBase == "" {
		inputBase = instance.DefaultWorkflowSourceRef
	}
	return configRepoTarget{
		Repo: providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: owner, Name: name},
		Base: inputBase,
	}, nil
}

// configRepoWriteToken returns the configrepo:write credential the stage was
// delivered. It reads exactly that capability's variable — the config grant
// can never be satisfied by (or satisfy) a product-repo capability — and fails
// closed with an operator-facing remedy when it is absent.
func configRepoWriteToken() (string, error) {
	token, err := providerToken(capability.ConfigRepoWrite)
	if err != nil {
		return "", fmt.Errorf("%w; the instance mints it from a github-app workflowSource, or from a credentials: entry for %s", err, capability.ConfigRepoWrite)
	}
	return token, nil
}

// configRepoOriginMatches reports whether remote names the target repository.
func configRepoOriginMatches(remote string, target configRepoTarget) bool {
	owner, name, err := parseConfigRepoURL(remote)
	if err != nil {
		return false
	}
	return strings.EqualFold(owner, target.Repo.Owner) && strings.EqualFold(name, target.Repo.Name)
}

// configRepoPushAuth is push-branch's per-invocation credential for the
// config-repo target. The origin is verified once: a checkout whose origin is
// anything but the config repository is refused before the credential is read.
func configRepoPushAuth(dir string, target configRepoTarget) (pushBranchAuthEnv, error) {
	remote, err := originURL(dir)
	if err != nil {
		return nil, err
	}
	if !configRepoOriginMatches(remote, target) {
		return nil, fmt.Errorf("refusing to push %s with %s: its origin is not the config repository %s/%s", dir, capability.ConfigRepoWrite, target.Repo.Owner, target.Repo.Name)
	}
	if _, err := configRepoWriteToken(); err != nil {
		return nil, err
	}
	return func() ([]string, error) {
		token, err := configRepoWriteToken()
		if err != nil {
			return nil, err
		}
		return gitAuthEnvFor(capability.ConfigRepoWrite, token), nil
	}, nil
}

// withWorkingDir runs fn with dir as the process working directory, restoring
// it after. open-pr's git inspections (write-boundary confinement, Tutor change
// classification) read the process cwd, so the config-repo target points them
// at the config checkout this way.
func withWorkingDir(dir string, fn func() error) (err error) {
	prev, err := os.Getwd()
	if err != nil {
		return err
	}
	if err := os.Chdir(dir); err != nil {
		return err
	}
	defer func() {
		if restoreErr := os.Chdir(prev); restoreErr != nil {
			err = errors.Join(err, restoreErr)
		}
	}()
	return fn()
}

const configCheckoutHelp = "Usage: goobers config-checkout [dir]\n\n" +
	"Clone the instance CONFIG repository (the workflowSource repository) at\n" +
	"its tracked ref into [dir] (default: the configRepoDir input, else\n" +
	"\"config-repo\", relative to the stage workspace) and check out the run's\n" +
	"branch there, so a stage can edit the config tree and push-branch/open-pr\n" +
	"(input target: config-repo) can publish it. Requires the stage to declare\n" +
	"configrepo:write. The credential authenticates the clone through the git\n" +
	"environment only; it is never written to .git/config.\n\n" +
	"Inputs: head (branch to create/reuse; default the run's stable branch),\n" +
	"configRepoDir, and for stage pods without instance config, configRepo\n" +
	"(owner/name) and configRepoBase.\n" +
	"A pre-existing branch on the remote (a repass) is checked out and\n" +
	"continued rather than recreated. A non-empty [dir] is refused.\n" +
	"Exit codes: 0 = checked out, 1 = business error, 2 = usage/IO error.\n"

const configCheckoutTimeout = 5 * time.Minute

func runConfigCheckout(args []string, stdout, stderr io.Writer) int {
	fs := newCLIFlagSet("config-checkout", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = helpUsage(stderr, "config-checkout")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 1 {
		fs.Usage()
		return 2
	}
	dir := configRepoDir()
	if fs.NArg() == 1 {
		dir = fs.Arg(0)
	}
	root := providerStageRoot("")
	target, err := resolveConfigRepoTarget(root)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	token, err := configRepoWriteToken()
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	runID, workflow, err := providerRunContext()
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	head := providerInput("head", preferredOpenPRHead(root, runID, workflow))

	if entries, readErr := os.ReadDir(dir); readErr == nil && len(entries) > 0 {
		pf(stderr, "error: %s already exists and is not empty\n", dir)
		return 1
	}
	ctx, cancel := context.WithTimeout(context.Background(), configCheckoutTimeout)
	defer cancel()
	if err := checkoutConfigRepo(ctx, dir, target, head, token); err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	pf(stdout, "checked out %s/%s@%s into %s on branch %s\n", target.Repo.Owner, target.Repo.Name, target.Base, dir, head)
	return 0
}

// checkoutConfigRepo clones the config repository's base branch into dir and
// puts head checked out there: the remote head when it already exists (a
// repass), otherwise a new branch off base.
func checkoutConfigRepo(ctx context.Context, dir string, target configRepoTarget, head, token string) error {
	authEnv := append(gitAuthEnvFor(capability.ConfigRepoWrite, token), "GIT_TERMINAL_PROMPT=0")
	clone := exec.CommandContext(ctx, "git", "clone", "--quiet", "--branch", target.Base, "--single-branch", target.CloneURL(), dir)
	clone.Env = authEnv
	if out, err := clone.CombinedOutput(); err != nil {
		return fmt.Errorf("clone config repository %s/%s@%s: %w: %s", target.Repo.Owner, target.Repo.Name, target.Base, err, strings.TrimSpace(string(out)))
	}
	git := func(env []string, args ...string) ([]byte, error) {
		cmd := workspaceGitCommand(dir, args...)
		cmd.Env = composeGitEnv(dir, env)
		return workspaceGitCombinedOutput(cmd)
	}
	// A repass: continue the branch a previous attempt already pushed.
	if _, err := git(authEnv, "fetch", "--quiet", "origin", head); err == nil {
		if out, err := git(nil, "checkout", "--quiet", "-B", head, "FETCH_HEAD"); err != nil {
			return fmt.Errorf("check out existing branch %q: %w: %s", head, err, strings.TrimSpace(string(out)))
		}
		return nil
	}
	if out, err := git(nil, "checkout", "--quiet", "-b", head); err != nil {
		return fmt.Errorf("create branch %q: %w: %s", head, err, strings.TrimSpace(string(out)))
	}
	return nil
}
