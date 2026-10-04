// Package credentialoverride is `goobers validate --check-repos`' preflight of
// instance-level credentials: capability overrides (#2744).
//
// A credentials: entry is unqualified: it backs its capability in EVERY
// gaggle. In a multi-gaggle instance a token meant for one gaggle's repository
// therefore routes into every other gaggle's stages too, and only a probe of
// each gaggle's own repository with that token shows whether it can work.
//
// The runner's grant materialization (repo bindings, storage keys, backlog
// roles, grant filtering) and the network probes live in cmd/goobers; they
// are injected through Runner and Prober so the plan here mirrors exactly
// what a run receives.
package credentialoverride

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/capability"
	"github.com/goobers/goobers/internal/credentials"
	"github.com/goobers/goobers/internal/instance"
)

// Probe is one gaggle's use of one instance-level credentials: entry
// (#2744): the entry at Credential replaces the repo default for Capability
// in Gaggle's runner grants, and that capability acts on Repo — the gaggle's
// project repository, or its backlog repository for a cross-provider backlog
// role.
type Probe struct {
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

// Runner is the runner's own credential-grant materialization
// (buildRoleCredentials' helpers), injected so Probes builds each gaggle's
// grants exactly as a run does.
type Runner struct {
	// RepoCapabilities is the repository-acting capability names
	// (repoCredentialedCapabilityNames).
	RepoCapabilities []string
	// StorageKey is a credentials: entry's harness-scoped storage key.
	StorageKey func(instance.CredentialGrant) (string, error)
	// BacklogRole is a gaggle's cross-provider backlog role, if any.
	BacklogRole func(apiv1.RepoRef, apiv1.BacklogRef) *credentials.BacklogRole
	// FilterGrants drops grants a non-ADO repo cannot hold.
	FilterGrants func([]instance.RepoRef, []credentials.Grant) []credentials.Grant
	// ProjectRepo is the repos[] entry for a gaggle's project.
	ProjectRepo func(*instance.Config, apiv1.RepoRef) (instance.RepoRef, bool)
	// MintsCredential reports whether an ADO repo mints its own credential.
	MintsCredential func(instance.RepoRef) bool
}

// Probes materializes credentials.RunnerGrants for every gaggle exactly as
// the runner does (buildRoleCredentials) and returns, per gaggle, each
// repository-acting capability whose effective grant comes from a
// credentials: entry rather than the gaggle's own repo token.
//
// Only token-sourced capability entries for repository-acting capabilities
// (RepoCapabilities) are probed: an agent:model or BYO MCP credential never
// touches the target repository, and a githubApp entry is restricted to
// agent:model. A target repository not in repos[] is skipped;
// checkGaggleRepositoryBindings reports that gap.
func (r Runner) Probes(cfg *instance.Config, set *instance.ConfigSet) []Probe {
	repoCapabilities := make(map[string]bool)
	for _, name := range r.RepoCapabilities {
		repoCapabilities[name] = true
	}
	overrides := make([]credentials.Grant, 0, len(cfg.Credentials))
	credentialByRef := make(map[string]int, len(cfg.Credentials))
	for i, cg := range cfg.Credentials {
		if cg.MCP != "" || cg.GitHubApp != nil || !cg.Token.Configured() || !repoCapabilities[cg.Capability] {
			continue
		}
		key, err := r.StorageKey(cg)
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
	bindings := r.repoBindings(cfg.Repos)
	var probes []Probe
	for _, gaggle := range set.Gaggles {
		project := gaggle.Spec.Project
		role := r.BacklogRole(project, gaggle.Spec.Backlog)
		owner := project.Owner
		if project.Provider == apiv1.ProviderADO && project.Project != "" {
			owner += "/" + project.Project
		}
		grants := r.FilterGrants(cfg.Repos, credentials.RunnerGrants(bindings, owner, project.Name, role, r.RepoCapabilities, overrides))
		probes = append(probes, r.gaggleProbes(cfg, gaggle.Name, project, role, grants, credentialByRef)...)
	}
	return probes
}

// gaggleProbes returns one probe per override grant in one gaggle's
// materialized grants.
func (r Runner) gaggleProbes(cfg *instance.Config, gaggle string, project apiv1.RepoRef, role *credentials.BacklogRole, grants []credentials.Grant, credentialByRef map[string]int) []Probe {
	backlogCapabilities := make(map[string]bool)
	if role != nil {
		for _, c := range role.Capabilities {
			backlogCapabilities[c] = true
		}
	}
	var probes []Probe
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
			repo, found = r.ProjectRepo(cfg, project)
		}
		if !found || !capabilityActsOnRepo(capabilityName, repo) {
			continue
		}
		probes = append(probes, Probe{
			Gaggle: gaggle, Credential: i, Capability: capabilityName, Repo: repo,
			Metadata: repo.Provider == string(apiv1.ProviderGitHub) && capabilityName != string(capability.RepoPush),
		})
	}
	return probes
}

// capabilityActsOnRepo reports whether a stage holding capabilityName
// would use it against repo. A provider-specific capability is only exercised
// against its own provider's repositories — an ado:* override is never used
// by a GitHub gaggle, nor a github:* one by an Azure DevOps project — and
// ado:work-items:write acts on the Boards project, not the repository (its
// reachability is checkADOBacklogProjects' concern), and ado:packaging:read
// acts on package feeds, so neither is probed as git repository access.
func capabilityActsOnRepo(capabilityName string, repo instance.RepoRef) bool {
	switch {
	case capabilityName == string(capability.ADOWorkItemsWrite), capabilityName == string(capability.ADOPackagingRead):
		return false
	case strings.HasPrefix(capabilityName, "github:"):
		return repo.Provider == string(apiv1.ProviderGitHub)
	case strings.HasPrefix(capabilityName, "ado:"):
		return repo.Provider == string(apiv1.ProviderADO)
	default:
		return true
	}
}

// repoBindings mirrors buildRoleCredentials' repo bindings without
// registering any credential source: a repo with a credential binds its
// owner/name ref, one without binds no token.
func (r Runner) repoBindings(repos []instance.RepoRef) []credentials.RepoBinding {
	bindings := make([]credentials.RepoBinding, 0, len(repos))
	for _, repo := range repos {
		owner := repo.Owner
		if repo.Provider == string(apiv1.ProviderADO) && repo.Project != "" {
			owner += "/" + repo.Project
		}
		tokenRef := ""
		if repo.Token.Configured() || repo.GitHubAppAuth() || r.MintsCredential(repo) {
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

// Prober is the network side of the preflight, injected from cmd/goobers'
// repository preflight seams.
type Prober struct {
	// Timeout bounds each token resolution and each probe
	// (repositoryPreflightTimeout).
	Timeout time.Duration
	// Reachable is the git access probe (targetRepositoryReachable).
	Reachable func(ctx context.Context, repo instance.RepoRef, token string, stores credentials.StoreResolver) error
	// Size is the GitHub repository metadata probe (targetRepositorySize).
	Size func(ctx context.Context, repo instance.RepoRef, token string) (int64, error)
	// Scrub redacts token from a probe error (scrubRepositoryError).
	Scrub func(err error, token string) string
}

// Check is `validate --check-repos`' preflight of the grants a run will
// actually receive (#2744): for every gaggle, each credentials: override that
// replaces a repository-acting capability's default must reach that gaggle's
// repository. A token that cannot is a validation error naming the config
// line, not a later 403 attributed to a stage; fail records it at the
// credentials: entry's JSON pointer. Each (credential, repository) pair is
// probed at most once, under the same per-probe timeout as the repos[]
// preflight.
func (p Prober) Check(cfg *instance.Config, probes []Probe, stores credentials.StoreResolver, stdout io.Writer, fail func(pointer, message string)) bool {
	if len(probes) == 0 {
		return true
	}
	tokens := make(map[int]string)
	tokenErrs := make(map[int]error)
	results := make(map[string]error)
	ok := true
	for _, probe := range probes {
		cg := cfg.Credentials[probe.Credential]
		token, err := p.resolveToken(probe.Credential, cg, stores, tokens, tokenErrs)
		label := fmt.Sprintf("credentials[%d] (%s) for Gaggle/%s", probe.Credential, probe.Capability, probe.Gaggle)
		target := repoDisplayName(probe.Repo)
		if err == nil {
			key := fmt.Sprintf("%d\x00%s\x00%s\x00%t", probe.Credential, probe.Repo.Provider, target, probe.Metadata)
			var probed bool
			if err, probed = results[key]; !probed {
				err = p.probe(probe, cg, token, stores)
				results[key] = err
			}
		}
		if err != nil {
			message := fmt.Sprintf("%s: token cannot reach target repository %s: %s", label, target, p.Scrub(err, token))
			_, _ = fmt.Fprintf(stdout, "CREDENTIAL %s\n", message)
			_, _ = fmt.Fprintf(stdout, "  This credentials: entry replaces the repository token for every gaggle; scope it to a token that can reach each gaggle's repository, or remove it.\n")
			fail(fmt.Sprintf("/credentials/%d", probe.Credential), message)
			ok = false
			continue
		}
		_, _ = fmt.Fprintf(stdout, "CREDENTIAL %s: reaches %s\n", label, target)
	}
	return ok
}

func (p Prober) resolveToken(i int, cg instance.CredentialGrant, stores credentials.StoreResolver, tokens map[int]string, errs map[int]error) (string, error) {
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
		ctx, cancel := context.WithTimeout(context.Background(), p.Timeout)
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

// probe checks the probe's repository with the override's token in place of
// the repository's own credential: the configured auth is dropped so the
// token is used as a PAT, exactly as a stage holding the override grant
// would.
func (p Prober) probe(probe Probe, cg instance.CredentialGrant, token string, stores credentials.StoreResolver) error {
	repo := probe.Repo
	repo.Token = cg.Token
	repo.Auth = nil
	ctx, cancel := context.WithTimeout(context.Background(), p.Timeout)
	defer cancel()
	if probe.Metadata {
		_, err := p.Size(ctx, repo, token)
		return err
	}
	return p.Reachable(ctx, repo, token, stores)
}

// repoDisplayName is repo's owner[/project]/name as the preflight reports it.
func repoDisplayName(repo instance.RepoRef) string {
	if repo.Project != "" {
		return repo.Owner + "/" + repo.Project + "/" + repo.Name
	}
	return repo.Owner + "/" + repo.Name
}
