package main

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"path"
	"strings"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/adoauth"
	"github.com/goobers/goobers/internal/capability"
	"github.com/goobers/goobers/internal/credentials"
	"github.com/goobers/goobers/internal/githubapp"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/mcpconfig"
	"github.com/goobers/goobers/providers"
)

// buildCredentials is the composition root for the secret-resolver seam. It
// selects the local env/file implementation; a tier-3 deployment substitutes
// its SEC-010 Key Vault Resolver here while all downstream wiring stays
// unchanged. By default the first configured repo's token backs every
// credentialed capability (V0 single-target-repo simplification, ARCHITECTURE.md
// §6). instance.yaml's credentials: block then sources individual capabilities
// or named BYO MCP credentials from their own token refs. Capability entries
// override repo-token defaults; BYO entries remain unreachable until a goober's
// MCP server explicitly references the matching name.
// The returned Grants are runner-owned (empty Goober); buildGooberCredentialGrants
// binds these sources to each goober's own declared capability and MCP keys
// before an agentic injector can use them.
// buildCredentials builds the resolver and runner-owned grants for one gaggle,
// whose project repo is (gaggleOwner, gaggleName). Repo capabilities are granted
// that gaggle's OWN repo token (per-repo credential scoping, MGV-5 #1012) rather
// than an instance-wide default, so a gaggle's stages only ever hold a token for
// that gaggle's repo. An empty (gaggleOwner, gaggleName) — an instance-level
// caller, or a single-repo/legacy instance — falls back to the first repo's
// token, byte-identical to the prior instance-global behavior. agent:model and
// other cfg.Credentials entries stay unqualified (the shared token every gaggle
// uses), overriding the repo-default grant for their capability (#287).
// stores resolves store-backed token refs (#683) — built once per composition
// root (daemon setup, or a one-shot command's own scope) so every consumer
// shares one TTL cache; a store ref with a nil stores fails closed at
// resolver construction rather than degrading into an unconfigured token.
//
// A github-app repo (#686) contributes a minting dynamic source under the same
// ref name a static token would use, so every consumer that resolves the repo
// ref — capability grants, ci-poll, the open-PR lister, worktree git auth —
// receives short-lived installation tokens with no further wiring. registrar
// receives every minted token (and the App key) at mint time; nil is only for
// display-path callers that never write journals.
//
// additionalRepos are the gaggle's read-only reference repos (MGV-10, #1285):
// each gains only a repo-qualified contents:read grant from its own token, never
// a write capability. Pass nil for instance-level or single-repo callers.
func buildCredentials(cfg *instance.Config, stores credentials.StoreResolver, gaggleOwner, gaggleName string, additionalRepos []apiv1.RepoRef, registrar credentials.SecretRegistrar) (credentials.Resolver, []credentials.Grant, error) {
	refs := make([]credentials.TokenRef, 0, len(cfg.Repos)+len(cfg.Credentials))
	bindings := make([]credentials.RepoBinding, 0, len(cfg.Repos))
	var sources map[string]credentials.ExpiringResolveFunc
	for _, r := range cfg.Repos {
		owner := r.Owner
		if r.Provider == "ado" && r.Project != "" {
			owner += "/" + r.Project
		}
		ref := owner + "/" + r.Name
		tokenRef := ""
		if r.GitHubAppAuth() {
			// Fail closed on a duplicate owner/name (as a static-token repo does
			// at NewResolverWith's duplicate-ref check): silently overwriting the
			// minting source would let a second entry hijack the first's grants.
			if _, dup := sources[ref]; dup {
				return nil, nil, fmt.Errorf("build credentials: repo %s: duplicate repository reference", ref)
			}
			mint, err := newGitHubAppTokenSource(r, registrar, stores)
			if err != nil {
				return nil, nil, fmt.Errorf("build credentials: repo %s: %w", ref, err)
			}
			if sources == nil {
				sources = make(map[string]credentials.ExpiringResolveFunc)
			}
			sources[ref] = mint
			tokenRef = ref
		} else if r.Token.Configured() {
			// The full token ref (env|file|store) is appended; a store-backed ref
			// resolves through stores below (#683) and fails closed there if no
			// store resolver is configured.
			tokenRef = ref
			refs = append(refs, r.Token.CredentialTokenRef(ref))
		}
		bindings = append(bindings, credentials.RepoBinding{Owner: owner, Name: r.Name, TokenRef: tokenRef})
	}
	// Daemon identity (#1780/#1295): when configured, sources a single named
	// ref backing the standard daemon-mutation capability set — one place
	// instead of one credentials: entry per capability. Built before the
	// explicit credentials: loop below so those entries, appended after,
	// still win per RunnerGrants' last-wins-per-capability semantics
	// (matches every explicit CredentialGrant's existing precedence over a
	// repo-default grant).
	var daemonIdentityOverrides []credentials.Grant
	if cfg.DaemonIdentity != nil {
		if cfg.DaemonIdentity.GitHubApp() {
			mint, err := newDaemonIdentityGitHubAppTokenSource(cfg.DaemonIdentity, gaggleOwner, gaggleName, registrar, stores)
			if err != nil {
				return nil, nil, fmt.Errorf("build credentials: daemonIdentity: %w", err)
			}
			if sources == nil {
				sources = make(map[string]credentials.ExpiringResolveFunc)
			}
			sources[daemonIdentityRefName] = mint
		} else {
			refs = append(refs, cfg.DaemonIdentity.Token.CredentialTokenRef(daemonIdentityRefName))
		}
		daemonIdentityOverrides = make([]credentials.Grant, len(daemonIdentityCapabilities))
		for i, c := range daemonIdentityCapabilities {
			daemonIdentityOverrides[i] = credentials.Grant{Capability: string(c), Ref: daemonIdentityRefName}
		}
	}
	// Explicit credential refs: each sources one capability or named BYO MCP
	// credential from its own token, namespaced away from repo refs.
	for _, cg := range cfg.Credentials {
		key, err := credentialGrantKey(cg)
		if err != nil {
			return nil, nil, fmt.Errorf("build credentials: %w", err)
		}
		refs = append(refs, cg.Token.CredentialTokenRef(credentialRefName(key)))
	}
	// The expiring-source form threads each minted value's stated expiry
	// through to the materialized Set (DS10): the credential plane's mint
	// responses carry it, so a stage pod never treats a snapshot as unbounded.
	resolver, err := credentials.NewResolverWithExpiring(refs, stores, nil, sources)
	if err != nil {
		return nil, nil, fmt.Errorf("build credential resolver: %w", err)
	}

	caps := make([]string, len(credentialedCapabilities))
	for i, c := range credentialedCapabilities {
		caps[i] = string(c)
	}
	overrides := make([]credentials.Grant, 0, len(daemonIdentityOverrides)+len(cfg.Credentials))
	overrides = append(overrides, daemonIdentityOverrides...)
	for _, cg := range cfg.Credentials {
		key, err := credentialGrantKey(cg)
		if err != nil {
			return nil, nil, fmt.Errorf("build credentials: %w", err)
		}
		overrides = append(overrides, credentials.Grant{Capability: key, Ref: credentialRefName(key)})
	}
	grants := credentials.RunnerGrants(bindings, gaggleOwner, gaggleName, caps, overrides)
	// Read-only reference repos (MGV-10, #1285): each of the gaggle's
	// AdditionalRepos is granted only a repo-qualified contents:read token, drawn
	// from that repo's own configured token binding. These runner-owned grants
	// authenticate the reference-repo checkout at provision time (MGV-11); no
	// write capability is ever produced for an additional repo, so a stage cannot
	// obtain a write token for one — reference repos are read-only by construction.
	additionalBindings := make([]credentials.RepoBinding, 0, len(additionalRepos))
	for _, r := range additionalRepos {
		owner := r.Owner
		if r.Provider == apiv1.ProviderADO && r.Project != "" {
			owner += "/" + r.Project
		}
		additionalBindings = append(additionalBindings, credentials.RepoBinding{Owner: owner, Name: r.Name})
	}
	grants = append(grants, credentials.AdditionalReadGrants(bindings, additionalBindings, string(capability.ContentsRead))...)
	return resolver, grants, nil
}

// buildGooberCredentialGrants binds the configured credential sources to one
// goober's definition-level capability and MCP credential keys. The resulting
// grants carry the goober identity, so a forged stage envelope cannot make this
// injector reach a key granted only to another goober.
func buildGooberCredentialGrants(gooberName string, keys []string, sources []credentials.Grant) []credentials.Grant {
	refs := make(map[string]string, len(sources))
	for _, source := range sources {
		if source.Goober == "" {
			refs[source.Capability] = source.Ref
		}
	}
	grants := make([]credentials.Grant, 0, len(keys))
	seen := make(map[string]bool, len(keys))
	for _, key := range keys {
		if seen[key] {
			continue
		}
		seen[key] = true
		if !capability.StageDeclarable(key) && !mcpconfig.IsBYOCredentialKey(key) {
			continue
		}
		if ref, ok := refs[key]; ok {
			grants = append(grants, credentials.Grant{
				Goober:     gooberName,
				Capability: key,
				Ref:        ref,
			})
		}
	}
	return grants
}

// deterministicCredentialGrants excludes named BYO MCP credential sources.
// Those sources are reachable only after buildGooberCredentialGrants binds
// them to a goober that explicitly references the named MCP server.
func deterministicCredentialGrants(sources []credentials.Grant) []credentials.Grant {
	grants := make([]credentials.Grant, 0, len(sources))
	for _, source := range sources {
		if !mcpconfig.IsBYOCredentialKey(source.Capability) {
			grants = append(grants, source)
		}
	}
	return grants
}

func credentialGrantKey(grant instance.CredentialGrant) (string, error) {
	switch {
	case grant.Capability != "" && grant.MCP == "":
		if !capability.StageDeclarable(grant.Capability) {
			return "", fmt.Errorf("capability %q cannot be stage-scoped", grant.Capability)
		}
		return grant.Capability, nil
	case grant.Capability == "" && mcpconfig.ValidBYOCredentialName(grant.MCP):
		return mcpconfig.BYOCredentialKey(grant.MCP), nil
	default:
		return "", errors.New("credential grant must set exactly one valid capability or mcp name")
	}
}

// credentialRefName is the resolver ref name for an explicit credentials entry,
// namespaced so it can never collide with a repo ref (owner/name).
func credentialRefName(key string) string { return "credential:" + key }

// newGitHubAppTokenSource builds the installation-token minting source for a
// github-app repo (#686). A package var so CLI tests substitute an
// httptest-backed source (mirrors newPRPoller / newOpenPRProvider); the
// production source caches until near expiry and single-flights refreshes.
var newGitHubAppTokenSource = func(repo instance.RepoRef, registrar credentials.SecretRegistrar, stores credentials.StoreResolver) (credentials.ExpiringResolveFunc, error) {
	source, err := githubapp.Source(repo, registrar, stores)
	if err != nil {
		return nil, err
	}
	return source.TokenWithExpiry, nil
}

// newDaemonIdentityGitHubAppTokenSource builds the installation-token minting
// source for a github-app-kind DaemonIdentity (#1780/#1779), scoped down to
// this one gaggle's repo exactly like a repo's own github-app auth already
// is (MGV-5 #1012) — a shared App installation must not hand one gaggle's
// stages a token that reaches a sibling gaggle's repo. A package var, like
// newGitHubAppTokenSource, so CLI tests substitute an httptest-backed source.
var newDaemonIdentityGitHubAppTokenSource = func(d *instance.DaemonIdentityConfig, gaggleOwner, gaggleRepoName string, registrar credentials.SecretRegistrar, stores credentials.StoreResolver) (credentials.ExpiringResolveFunc, error) {
	const keyRefName = "daemon-identity-private-key"
	// #3415: which installation mints depends on which owner this gaggle acts
	// on, and that is known HERE -- buildCredentials receives the gaggle's
	// owner and builds one resolver per gaggle. Selecting at wiring time is
	// why the per-owner form needs no change to credentials.ResolveFunc, which
	// takes only a context and could not otherwise learn the target.
	//
	// A gaggle whose owner has no binding is refused rather than defaulted:
	// falling back to some other owner's installation reproduces exactly the
	// 422-at-first-use this feature exists to prevent, only later and with a
	// less obvious cause. Config validation already rejects this shape at
	// load, so reaching here means the two disagree.
	installationID, ok := d.InstallationForOwner(gaggleOwner)
	if !ok {
		return nil, fmt.Errorf(
			"daemon identity has no GitHub App installation bound for owner %q; "+
				"add it to daemonIdentity.installations", gaggleOwner)
	}
	keyResolver, err := credentials.NewResolverWith([]credentials.TokenRef{d.PrivateKey.CredentialTokenRef(keyRefName)}, stores, nil)
	if err != nil {
		return nil, fmt.Errorf("configure daemon identity App key source: %w", err)
	}
	source, err := githubapp.New(githubapp.Config{
		AppID:          string(d.AppID),
		InstallationID: string(installationID),
		Repositories:   []string{gaggleRepoName},
		Key: func(ctx context.Context) (string, error) {
			return keyResolver.Resolve(ctx, keyRefName)
		},
		Registrar: registrar,
	})
	if err != nil {
		return nil, err
	}
	return source.TokenWithExpiry, nil
}

// newWorkflowSourceAppTokenSource builds the installation-token minting source
// for a github-app workflowSource (#3274). The daemon's config-repo fetch is
// wired here — the composition root — because internal/instance owns the
// gitsource seam but cannot import internal/githubapp (which imports it); the
// gitsource receives the returned instance.GitTokenSource instead. A package
// var, like newGitHubAppTokenSource, so CLI tests substitute a fake minter.
var newWorkflowSourceAppTokenSource = func(source instance.WorkflowSource, registrar credentials.SecretRegistrar, stores credentials.StoreResolver) (instance.GitTokenSource, error) {
	if !source.GitHubAppAuth() {
		return nil, errors.New("workflowSource does not use github-app auth")
	}
	repository, err := workflowSourceRepositoryName(source.URL)
	if err != nil {
		return nil, err
	}
	const keyRefName = "workflow-source-private-key"
	keyResolver, err := credentials.NewResolverWith([]credentials.TokenRef{source.Auth.PrivateKey.CredentialTokenRef(keyRefName)}, stores, nil)
	if err != nil {
		return nil, fmt.Errorf("configure workflow-source App key source: %w", err)
	}
	return githubapp.New(githubapp.Config{
		AppID:          string(source.Auth.AppID),
		InstallationID: string(source.Auth.InstallationID),
		// Down-scope every mint to the config repo itself — the same per-repo
		// scoping discipline as repos[] App auth (MGV-5 #1012): the App
		// installation may cover more repositories than the definitions tree,
		// and this token only ever needs to read one of them.
		Repositories: []string{repository},
		Key: func(ctx context.Context) (string, error) {
			return keyResolver.Resolve(ctx, keyRefName)
		},
		Registrar: registrar,
	})
}

// workflowSourceRepositoryName derives the repository name the minted
// installation token is down-scoped to from the remote config URL's last path
// segment (GitHub clone URLs end /<owner>/<repo>[.git]). Fail closed on a URL
// with no derivable name: minting unscoped instead would silently widen the
// token to the installation's whole repository set.
func workflowSourceRepositoryName(rawURL string) (string, error) {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return "", fmt.Errorf("parse workflowSource url: %w", err)
	}
	name := path.Base(strings.TrimSuffix(parsed.Path, ".git"))
	if name == "" || name == "." || name == "/" {
		return "", fmt.Errorf("cannot derive the config repository name from workflowSource url %q for installation-token down-scoping", rawURL)
	}
	return name, nil
}

// buildWorktreeGitEnv builds the worktree Manager's per-repo git-auth resolver
// (MGV-11 #1286). Keyed on the clone URL the runner hands WorkingCopy, it backs:
//   - each read-only reference repo (additionalRepos) with that repo's own
//     contents:read token (MGV-10/#1285), as an x-access-token http.extraheader
//     scoped to that URL — a read credential, never a push one;
//   - an ADO project repo with its Entra/PAT source, stores-aware (#683);
//   - a GitHub project repo with its github-app installation token or a
//     static/store-backed token (#667/#686), via githubWorktreeGitEnvironment,
//     scoped to the project repo's own clone URL;
//   - every other URL with the ambient git environment (nil return).
//
// Returns (nil, nil) when nothing bespoke is needed — a GitHub-only gaggle whose
// project repo and reference repos carry no configured tokens — so the Manager
// keeps its plain ambient behavior and a single-gaggle instance is unchanged.
func buildWorktreeGitEnv(cfg *instance.Config, workcopiesDir string, gaggleProject apiv1.RepoRef, additionalRepos []apiv1.RepoRef, resolver credentials.Resolver, grants []credentials.Grant, cloneURL func(apiv1.RepoRef) (string, error), reg providers.SecretRegistrar, stores credentials.StoreResolver) (func(context.Context, string) ([]string, error), error) {
	grantRef := make(map[string]string, len(grants))
	for _, g := range grants {
		grantRef[g.Capability] = g.Ref
	}
	readRefByURL := make(map[string]string, len(additionalRepos))
	for _, repo := range additionalRepos {
		owner := repo.Owner
		if repo.Provider == apiv1.ProviderADO && repo.Project != "" {
			owner += "/" + repo.Project
		}
		ref, ok := grantRef[credentials.RepoScopedCapability(string(capability.ContentsRead), owner, repo.Name)]
		if !ok {
			// No configured read token for this reference repo — a public repo
			// clones fine anonymously, so fall through to the ambient env.
			continue
		}
		url, err := cloneURL(repo)
		if err != nil {
			return nil, fmt.Errorf("resolve reference repo %q clone URL: %w", repo.Name, err)
		}
		readRefByURL[url] = ref
	}

	var adoSource providers.ADOCredentialSource
	if adoRepo, ok := adoRepoForGaggle(cfg, gaggleProject); ok {
		source, err := adoauth.Source(adoRepo, nil, stores)
		if err != nil {
			return nil, fmt.Errorf("configure ADO worktree authentication: %w", err)
		}
		adoSource = source
	}

	// GitHub project-repo authentication (#667/#686): github-app installation
	// tokens or a static/store-backed token for the gaggle's own GitHub repo,
	// scoped to its clone URL. nil when the project repo is not an authenticated
	// GitHub repo (public, or a non-GitHub provider).
	var githubProjectEnv func(context.Context, string) ([]string, error)
	if githubRepo, ok := githubRepoForGaggle(cfg, gaggleProject); ok {
		env, err := githubWorktreeGitEnvironment(workcopiesDir, githubRepo, reg, stores)
		if err != nil {
			return nil, fmt.Errorf("configure GitHub worktree authentication: %w", err)
		}
		githubProjectEnv = env
	}

	// Gitea project-repo authentication: a static/store-backed token for the
	// gaggle's own Gitea repo, scoped to its clone URL, so run-branch push (and
	// private-repo clone/fetch) authenticate. nil when the project repo is not
	// an authenticated Gitea repo (public read, or another provider). Unlike
	// GitHub, Gitea has no app-install auth — only a configured token.
	var giteaProjectEnv func(context.Context, string) ([]string, error)
	if giteaRepo, ok := giteaRepoForGaggle(cfg, gaggleProject); ok {
		env, err := giteaWorktreeGitEnvironment(giteaRepo, reg, stores)
		if err != nil {
			return nil, fmt.Errorf("configure Gitea worktree authentication: %w", err)
		}
		giteaProjectEnv = env
	}

	if len(readRefByURL) == 0 && adoSource == nil && githubProjectEnv == nil && giteaProjectEnv == nil {
		return nil, nil // nothing bespoke — keep the Manager's ambient behavior
	}
	return func(ctx context.Context, repoURL string) ([]string, error) {
		if ref, ok := readRefByURL[repoURL]; ok {
			token, err := resolver.Resolve(ctx, ref)
			if err != nil {
				return nil, fmt.Errorf("resolve reference-repo read token: %w", err)
			}
			return providers.GitHubGitAuthEnvironment(token, repoURL, reg), nil
		}
		if adoSource != nil {
			return providers.ADOGitAuthEnvironment(ctx, adoSource, reg, repoURL)
		}
		if githubProjectEnv != nil {
			// Scoped to the project repo's clone URL; returns nil (ambient) for
			// any other URL.
			return githubProjectEnv(ctx, repoURL)
		}
		if giteaProjectEnv != nil {
			// Scoped to the Gitea project repo's clone URL; nil (ambient) elsewhere.
			return giteaProjectEnv(ctx, repoURL)
		}
		return nil, nil // ambient
	}, nil
}
