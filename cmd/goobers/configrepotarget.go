package main

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"path"
	"strings"

	"github.com/goobers/goobers/internal/capability"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/providers"
)

// The config-repo target (TUT-A8, #1220; docs/design/tutor-redesign.md §4.8).
//
// A tutor workflow runs in one gaggle but its improvement PRs belong in the
// instance CONFIG repository, the one workflowSource loads definitions from.
// push-branch and open-pr gain an opt-in --config-repo flag that
// re-aims them at that repository and authenticates them with the stage's
// declared configrepo:write credential — a credential minted from
// workflowSource's own auth, never from a gaggle's product-repo token. The two
// credentials are not interchangeable in either direction: a config-repo
// stage never reads GOOBERS_CRED_REPO_PUSH or provider:pr:write, and a
// product-repo stage never reads GOOBERS_CRED_CONFIGREPO_WRITE.

const (
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

func configRepoDir() string {
	if d := strings.TrimSpace(providerInput("configRepoDir", "")); d != "" {
		return d
	}
	return defaultConfigRepoDir
}

// configRepoFlag is the opt-in flag name push-branch and open-pr share. It is a
// flag rather than an input so the provider-stage manifest, which sees a
// stage's args, can swap the stage's required capability from the product
// repo's (repo:push / provider:pr:write) to configrepo:write at admission.
const configRepoFlag = "config-repo"

// configRepoTargetFor resolves the config target (push-branch, open-pr) when
// the stage passed --config-repo; root is where the instance config is looked up.
func configRepoTargetFor(enabled bool, root string) (configRepoTarget, bool, error) {
	if !enabled {
		return configRepoTarget{}, false, nil
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
	inputRepo := strings.TrimSpace(providerInput("configRepo", ""))
	inputBase := strings.TrimSpace(providerInput("configRepoBase", ""))

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
