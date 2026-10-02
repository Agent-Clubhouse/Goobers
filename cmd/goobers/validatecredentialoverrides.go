package main

import (
	"context"
	"fmt"
	"io"
	"strings"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/api/validate"
	"github.com/goobers/goobers/internal/capability"
	"github.com/goobers/goobers/internal/credentials"
	"github.com/goobers/goobers/internal/instance"
)

// credentialOverrideProbe is one gaggle's use of one instance-level
// credentials: entry (#2744): the entry at Credential replaces the repo
// default for Capability in Gaggle's runner grants, and that capability acts
// on Repo — the gaggle's project repository, or its backlog repository for a
// cross-provider backlog role.
type credentialOverrideProbe struct {
	Gaggle     string
	Credential int
	Capability string
	Repo       instance.RepoRef
	// Metadata probes GitHub's repository metadata API instead of git: a
	// least-privilege fine-grained token backing an issues or pull-request
	// capability can read the repository's metadata but not its contents, so
	// only repo:push is held to git access.
	Metadata bool
}

// gaggleCredentialOverrideProbes materializes credentials.RunnerGrants for
// every gaggle exactly as the runner does (buildRoleCredentials) and returns,
// per gaggle, each repository-acting capability whose effective grant comes
// from a credentials: entry rather than the gaggle's own repo token.
//
// A credentials: entry is unqualified: it backs its capability in EVERY
// gaggle. In a multi-gaggle instance a token meant for one gaggle's repository
// therefore routes into every other gaggle's stages too, and only a probe of
// each gaggle's own repository with that token shows whether it can work.
//
// Only token-sourced capability entries for repository-acting capabilities
// (repoCredentialedCapabilityNames) are probed: an agent:model or BYO MCP
// credential never touches the target repository, and a githubApp entry is
// restricted to agent:model. A target repository not in repos[] is skipped;
// checkGaggleRepositoryBindings reports that gap.
func gaggleCredentialOverrideProbes(cfg *instance.Config, set *instance.ConfigSet) []credentialOverrideProbe {
	repoCapabilities := make(map[string]bool)
	for _, name := range repoCredentialedCapabilityNames() {
		repoCapabilities[name] = true
	}
	overrides := make([]credentials.Grant, 0, len(cfg.Credentials))
	credentialByRef := make(map[string]int, len(cfg.Credentials))
	for i, cg := range cfg.Credentials {
		if cg.MCP != "" || cg.GitHubApp != nil || !cg.Token.Configured() || !repoCapabilities[cg.Capability] {
			continue
		}
		key, err := credentialGrantStorageKey(cg)
		if err != nil {
			continue
		}
		ref := fmt.Sprintf("credentials[%d]", i)
		credentialByRef[ref] = i
		overrides = append(overrides, credentials.Grant{Capability: key, Ref: ref})
	}
	if len(overrides) == 0 {
		return nil
	}
	bindings := credentialOverrideRepoBindings(cfg.Repos)
	var probes []credentialOverrideProbe
	for _, gaggle := range set.Gaggles {
		project := gaggle.Spec.Project
		role := gaggleBacklogRole(project, gaggle.Spec.Backlog)
		owner := project.Owner
		if project.Provider == apiv1.ProviderADO && project.Project != "" {
			owner += "/" + project.Project
		}
		grants := withoutNonADORepoGrants(cfg.Repos, credentials.RunnerGrants(bindings, owner, project.Name, role, repoCredentialedCapabilityNames(), overrides))
		probes = append(probes, gaggleOverrideProbes(cfg, gaggle.Name, project, role, grants, credentialByRef)...)
	}
	return probes
}

// gaggleOverrideProbes returns one probe per override grant in one gaggle's
// materialized grants.
func gaggleOverrideProbes(cfg *instance.Config, gaggle string, project apiv1.RepoRef, role *credentials.BacklogRole, grants []credentials.Grant, credentialByRef map[string]int) []credentialOverrideProbe {
	backlogCapabilities := make(map[string]bool)
	if role != nil {
		for _, c := range role.Capabilities {
			backlogCapabilities[c] = true
		}
	}
	var probes []credentialOverrideProbe
	for _, grant := range grants {
		i, ok := credentialByRef[grant.Ref]
		if !ok {
			continue
		}
		capabilityName := cfg.Credentials[i].Capability
		var repo instance.RepoRef
		var found bool
		if backlogCapabilities[capabilityName] {
			repo, found = configuredRepoByOwnerName(cfg, role.Owner, role.Name)
		} else {
			repo, found = configuredRepoForProject(cfg, project)
		}
		if !found || !overrideCapabilityActsOnRepo(capabilityName, repo) {
			continue
		}
		probes = append(probes, credentialOverrideProbe{
			Gaggle: gaggle, Credential: i, Capability: capabilityName, Repo: repo,
			Metadata: repo.Provider == string(apiv1.ProviderGitHub) && capabilityName != string(capability.RepoPush),
		})
	}
	return probes
}

// overrideCapabilityActsOnRepo reports whether a stage holding capabilityName
// would use it against repo. A provider-specific capability is only exercised
// against its own provider's repositories — an ado:* override is never used
// by a GitHub gaggle, nor a github:* one by an Azure DevOps project — and
// ado:work-items:write acts on the Boards project, not the repository (its
// reachability is checkADOBacklogProjects' concern), so neither is probed.
func overrideCapabilityActsOnRepo(capabilityName string, repo instance.RepoRef) bool {
	switch {
	case capabilityName == string(capability.ADOWorkItemsWrite):
		return false
	case strings.HasPrefix(capabilityName, "github:"):
		return repo.Provider == string(apiv1.ProviderGitHub)
	case strings.HasPrefix(capabilityName, "ado:"):
		return repo.Provider == string(apiv1.ProviderADO)
	default:
		return true
	}
}

// credentialOverrideRepoBindings mirrors buildRoleCredentials' repo bindings
// without registering any credential source: a repo with a credential binds
// its owner/name ref, one without binds no token.
func credentialOverrideRepoBindings(repos []instance.RepoRef) []credentials.RepoBinding {
	bindings := make([]credentials.RepoBinding, 0, len(repos))
	for _, repo := range repos {
		owner := repo.Owner
		if repo.Provider == string(apiv1.ProviderADO) && repo.Project != "" {
			owner += "/" + repo.Project
		}
		tokenRef := ""
		if repo.Token.Configured() || repo.GitHubAppAuth() || adoRepositoryMintsCredential(repo) {
			tokenRef = owner + "/" + repo.Name
		}
		bindings = append(bindings, credentials.RepoBinding{Owner: owner, Name: repo.Name, TokenRef: tokenRef})
	}
	return bindings
}

func configuredRepoByOwnerName(cfg *instance.Config, owner, name string) (instance.RepoRef, bool) {
	for _, repo := range cfg.Repos {
		if repo.Provider != string(apiv1.ProviderADO) && repo.Owner == owner && repo.Name == name {
			return repo, true
		}
	}
	return instance.RepoRef{}, false
}

// checkGaggleCredentialOverrideAccess is `validate --check-repos`' preflight of
// the grants a run will actually receive (#2744): for every gaggle, each
// credentials: override that replaces a repository-acting capability's
// default must reach that gaggle's repository. A token that cannot is a
// validation error naming the config line, not a later 403 attributed to a
// stage. Each (credential, repository) pair is probed at most once, under the
// same per-probe timeout as the repos[] preflight.
func checkGaggleCredentialOverrideAccess(root, configFile string, cfg *instance.Config, set *instance.ConfigSet, stores credentials.StoreResolver, stdout io.Writer, diagnostics *diagnosticCollector) bool {
	probes := gaggleCredentialOverrideProbes(cfg, set)
	if len(probes) == 0 {
		return true
	}
	file := diagnosticFile(root, configFile)
	tokens := make(map[int]string)
	tokenErrs := make(map[int]error)
	results := make(map[string]error)
	ok := true
	for _, probe := range probes {
		cg := cfg.Credentials[probe.Credential]
		token, err := resolveCredentialOverrideToken(probe.Credential, cg, stores, tokens, tokenErrs)
		label := fmt.Sprintf("credentials[%d] (%s) for Gaggle/%s", probe.Credential, probe.Capability, probe.Gaggle)
		target := repoDisplayName(probe.Repo)
		if err == nil {
			key := fmt.Sprintf("%d\x00%s\x00%s\x00%t", probe.Credential, probe.Repo.Provider, target, probe.Metadata)
			var probed bool
			if err, probed = results[key]; !probed {
				err = probeCredentialOverride(probe, cg, token, stores)
				results[key] = err
			}
		}
		if err != nil {
			message := fmt.Sprintf("%s: token cannot reach target repository %s: %s", label, target, scrubRepositoryError(err, token))
			pf(stdout, "CREDENTIAL %s\n", message)
			pf(stdout, "  This credentials: entry replaces the repository token for every gaggle; scope it to a token that can reach each gaggle's repository, or remove it.\n")
			diagnostics.add(file, fmt.Sprintf("/credentials/%d", probe.Credential), "REPO004", string(validate.Error), message)
			ok = false
			continue
		}
		pf(stdout, "CREDENTIAL %s: reaches %s\n", label, target)
	}
	return ok
}

func resolveCredentialOverrideToken(i int, cg instance.CredentialGrant, stores credentials.StoreResolver, tokens map[int]string, errs map[int]error) (string, error) {
	if token, ok := tokens[i]; ok {
		return token, nil
	}
	if err, ok := errs[i]; ok {
		return "", err
	}
	refName := fmt.Sprintf("validate-credential-%d", i)
	resolver, err := credentials.NewResolverWithStores([]credentials.TokenRef{cg.Token.CredentialTokenRef(refName)}, stores)
	var token string
	if err == nil {
		ctx, cancel := context.WithTimeout(context.Background(), repositoryPreflightTimeout)
		token, err = resolver.Resolve(ctx, refName)
		cancel()
	}
	if err != nil {
		errs[i] = fmt.Errorf("resolve token: %w", err)
		return "", errs[i]
	}
	tokens[i] = token
	return token, nil
}

// probeCredentialOverride checks the probe's repository with the override's
// token in place of the repository's own credential: the configured auth is
// dropped so the token is used as a PAT, exactly as a stage holding the
// override grant would.
func probeCredentialOverride(probe credentialOverrideProbe, cg instance.CredentialGrant, token string, stores credentials.StoreResolver) error {
	repo := probe.Repo
	repo.Token = cg.Token
	repo.Auth = nil
	ctx, cancel := context.WithTimeout(context.Background(), repositoryPreflightTimeout)
	defer cancel()
	if probe.Metadata {
		_, err := targetRepositorySize(ctx, repo, token)
		return err
	}
	return targetRepositoryReachable(ctx, repo, token, stores)
}

func repoDisplayName(repo instance.RepoRef) string {
	if repo.Project != "" {
		return repo.Owner + "/" + repo.Project + "/" + repo.Name
	}
	return repo.Owner + "/" + repo.Name
}
