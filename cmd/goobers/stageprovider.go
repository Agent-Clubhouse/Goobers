package main

import (
	"fmt"
	"os"

	"strings"

	"github.com/goobers/goobers/internal/adoauth"
	"github.com/goobers/goobers/internal/capability"
	"github.com/goobers/goobers/internal/credentials"
	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/providers"
)

type stageProviderConfig struct {
	root         string
	repo         providers.RepositoryRef
	readOnly     bool
	capability   capability.Capability
	token        string
	cached       bool
	mutationKind string
	openPR       bool
	noRetries    bool
	observeToken func(string)
	// connectionRef names the gaggle connection whose credential this provider
	// must authenticate as, instead of the stage's capability-scoped default.
	connectionRef string
}

type stageProviderOption func(*stageProviderConfig)

func withStageProviderCapability(cap capability.Capability) stageProviderOption {
	return func(cfg *stageProviderConfig) {
		cfg.capability = cap
	}
}

func withStageProviderToken(token string) stageProviderOption {
	return func(cfg *stageProviderConfig) {
		cfg.token = token
	}
}

func withStageProviderCache() stageProviderOption {
	return func(cfg *stageProviderConfig) {
		cfg.cached = true
	}
}

func withStageProviderMutations(kind string) stageProviderOption {
	return func(cfg *stageProviderConfig) {
		cfg.mutationKind = kind
	}
}

func withStageProviderOpenPR() stageProviderOption {
	return func(cfg *stageProviderConfig) {
		cfg.openPR = true
	}
}

func withStageProviderRetriesDisabled() stageProviderOption {
	return func(cfg *stageProviderConfig) {
		cfg.noRetries = true
	}
}

func withStageProviderTokenObserver(observer func(string)) stageProviderOption {
	return func(cfg *stageProviderConfig) {
		cfg.observeToken = observer
	}
}

// withStageProviderConnection authenticates this provider as the named gaggle
// connection rather than with the stage's capability-scoped token. It is how a
// backlog that lives under different ownership than the target repository gets
// its own credential (spec.backlog.connectionRef): the runner injects the
// connection's token alongside — not instead of — the capability tokens, so one
// stage can hold a project credential and a backlog credential simultaneously.
//
// An empty name is a no-op, keeping every gaggle that declares no connectionRef
// on exactly its previous credential path.
func withStageProviderConnection(name string) stageProviderOption {
	return func(cfg *stageProviderConfig) {
		cfg.connectionRef = name
	}
}

type stageProviderFactory func(stageProviderConfig) (providers.Provider, error)

var stageProviderFactories = map[providers.ProviderKind]stageProviderFactory{
	providers.ProviderGitHub: newGitHubProviderForStage,
	providers.ProviderADO:    newRegisteredADOProviderForStage,
	providers.ProviderGitea:  newRegisteredGiteaProviderForStage,
}

func newProviderForStage(root string, repo providers.RepositoryRef, readOnly bool, opts ...stageProviderOption) (providers.Provider, error) {
	cfg := stageProviderConfig{
		root:       root,
		repo:       repo,
		readOnly:   readOnly,
		capability: capability.GitHubIssuesWrite,
	}
	if readOnly {
		cfg.capability = capability.GitHubIssuesRead
	}
	for _, opt := range opts {
		opt(&cfg)
	}
	factory, ok := stageProviderFactories[repo.Provider]
	if !ok {
		return nil, fmt.Errorf("repository provider %q is not registered for stages", repo.Provider)
	}
	return factory(cfg)
}

func newProviderForStageAs[T providers.Provider](root string, repo providers.RepositoryRef, readOnly bool, opts ...stageProviderOption) (T, error) {
	var zero T
	provider, err := newProviderForStage(root, repo, readOnly, opts...)
	if err != nil {
		return zero, err
	}
	typed, ok := provider.(T)
	if !ok {
		return zero, fmt.Errorf("repository provider %q does not support this stage operation", repo.Provider)
	}
	return typed, nil
}

func stageProviderToken(cfg stageProviderConfig) (string, error) {
	var token string
	switch {
	case cfg.token != "":
		token = cfg.token
	case cfg.connectionRef != "":
		var err error
		token, err = connectionToken(cfg.connectionRef, cfg.capability)
		if err != nil {
			return "", err
		}
	default:
		var err error
		token, err = providerToken(cfg.capability)
		if err != nil {
			return "", err
		}
	}
	if cfg.observeToken != nil {
		cfg.observeToken(token)
	}
	return token, nil
}

// connectionToken reads the credential the runner injected for a named gaggle
// connection. It falls back to the capability-scoped credential when the
// instance declares no credentials: entry for the connection — a gaggle may
// legitimately name a connection purely for documentation/validation while both
// project and backlog live under one account and one token.
func connectionToken(name string, cap capability.Capability) (string, error) {
	if token := os.Getenv(executor.CredentialEnvVar(credentials.ConnectionCredentialKey(name))); token != "" {
		return token, nil
	}
	token, err := providerToken(cap)
	if err != nil {
		return "", fmt.Errorf("no credential for connection %q and no capability credential to fall back on: %w", name, err)
	}
	return token, nil
}

func newGitHubProviderForStage(cfg stageProviderConfig) (providers.Provider, error) {
	token, err := stageProviderToken(cfg)
	if err != nil {
		return nil, err
	}
	var opts []func(*providers.GitHubProvider)
	// Under GitHub App auth the minted installation token cannot self-report
	// its login (GET /user is PAT-only), so every trusted-comment check —
	// claim markers first among them — needs the login declared in config
	// and threaded here (#3343). Best-effort: a missing/unreadable config or
	// an absent slug leaves the GET /user path in place, which is correct
	// for PATs and fails with the actionable 403 for undeclared App auth.
	if login := stageProviderConfiguredLogin(cfg.root, cfg.repo); login != "" {
		opts = append(opts, providers.WithConfiguredLogin(login))
	}
	if !cfg.readOnly && cfg.mutationKind != "" {
		opts = append(opts, providers.WithMutationRecorder(sidecarMutationRecorder{kind: cfg.mutationKind}))
	}
	if cfg.noRetries {
		opts = append(opts, providers.WithMaxRateLimitRetries(0), providers.WithMaxTransientRetries(0))
	}
	if cfg.cached {
		return newCachedGitHubProvider(cfg.root, token, opts...), nil
	}
	return newGitHubProvider(token, opts...), nil
}

func newRegisteredADOProviderForStage(cfg stageProviderConfig) (providers.Provider, error) {
	if cfg.openPR {
		return newADOProviderForOpenPR(cfg.root, cfg.repo)
	}
	if cfg.connectionRef != "" {
		return newADOProviderForConnection(cfg.root, cfg.repo, cfg.connectionRef)
	}
	return newADOProviderForStage(cfg.root, cfg.repo)
}

// newADOProviderForConnection builds the ADO provider for a stage that must
// authenticate as a named connection rather than with the routed repo's own
// default credential — the backlog half of a gaggle whose backlog project is
// governed by different credentials than its code repository.
//
// Only PAT auth is redirected: the Entra-backed kinds (azure-cli, workload and
// managed identity) authenticate as an ambient identity with no token ref to
// substitute, so they are left exactly as configured. When the connection
// credential is absent the repo's configured auth stands, which keeps a gaggle
// that names a connection but shares one credential working unchanged.
func newADOProviderForConnection(root string, routed providers.RepositoryRef, connectionRef string) (*providers.ADOProvider, error) {
	repo, err := adoRepoRefForStage(root, routed)
	if err != nil {
		return nil, err
	}
	kind := instance.ADOAuthPAT
	if repo.Auth != nil {
		kind = repo.Auth.Kind
	}
	envVar := executor.CredentialEnvVar(credentials.ConnectionCredentialKey(connectionRef))
	if kind == instance.ADOAuthPAT && os.Getenv(envVar) != "" {
		repo.Token = instance.TokenRef{Env: envVar}
	}
	return adoauth.Provider(repo, nil, nil, nil, nil, nil)
}

func newRegisteredGiteaProviderForStage(cfg stageProviderConfig) (providers.Provider, error) {
	token, err := stageProviderToken(cfg)
	if err != nil {
		return nil, err
	}
	var opts []func(*providers.GiteaProvider)
	if !cfg.readOnly && cfg.mutationKind != "" {
		opts = append(opts, providers.WithGiteaMutationRecorder(sidecarMutationRecorder{kind: cfg.mutationKind}))
	}
	return newGiteaProviderForStage(cfg.root, cfg.repo, token, opts...)
}

// stageProviderConfiguredLogin resolves the config-declared bot login for the
// stage's target repository — the repos[] entry matching owner/name whose
// auth block declares a GitHub App slug (#3343). Best-effort by design: any
// load failure returns "" and the provider falls back to GET /user.
func stageProviderConfiguredLogin(root string, repo providers.RepositoryRef) string {
	if repo.Provider != providers.ProviderGitHub || root == "" {
		return ""
	}
	cfg, err := instance.LoadConfig(instance.NewLayout(root).ConfigFile())
	if err != nil {
		return ""
	}
	for _, r := range cfg.Repos {
		if r.Provider == "github" && strings.EqualFold(r.Owner, repo.Owner) && strings.EqualFold(r.Name, repo.Name) {
			return r.Auth.BotLogin()
		}
	}
	return ""
}
