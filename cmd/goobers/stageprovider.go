package main

import (
	"fmt"

	"strings"

	"github.com/goobers/goobers/internal/capability"
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
	// mutationRecorder is an explicitly supplied journal mutation recorder.
	// It wins over mutationKind, which is the common case's shorthand for
	// "record through the sidecar under this kind": a caller that already
	// holds a recorder (post-merge reconcile, whose branch cleanup records
	// kind="branch" separately from the merge's kind="pr") hands it over
	// intact rather than being forced to re-derive one from a kind string.
	mutationRecorder providers.MutationRecorder
	openPR           bool
	noRetries        bool
	observeToken     func(string)
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

// withStageProviderMutationRecorder threads a caller-owned mutation recorder
// through the seam, for stages that build their recorder themselves rather
// than naming a sidecar kind.
func withStageProviderMutationRecorder(recorder providers.MutationRecorder) stageProviderOption {
	return func(cfg *stageProviderConfig) {
		cfg.mutationRecorder = recorder
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

// Merge-review stages share this seam so provider selection cannot drift between
// the GitHub and ADO operation paths.
func newMergeReviewProvider(root string, repo providers.RepositoryRef, readOnly bool, opts ...stageProviderOption) (providers.Provider, error) {
	return newProviderForStage(root, repo, readOnly, opts...)
}

func newMergeReviewProviderAs[T providers.Provider](root string, repo providers.RepositoryRef, readOnly bool, opts ...stageProviderOption) (T, error) {
	return newProviderForStageAs[T](root, repo, readOnly, opts...)
}

func stageProviderToken(cfg stageProviderConfig) (string, error) {
	var token string
	if cfg.token != "" {
		token = cfg.token
	} else {
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
	if recorder := stageProviderMutationRecorder(cfg); recorder != nil {
		opts = append(opts, providers.WithMutationRecorder(recorder))
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
	return newADOProviderForStage(cfg.root, cfg.repo)
}

func newRegisteredGiteaProviderForStage(cfg stageProviderConfig) (providers.Provider, error) {
	token, err := stageProviderToken(cfg)
	if err != nil {
		return nil, err
	}
	var opts []func(*providers.GiteaProvider)
	if recorder := stageProviderMutationRecorder(cfg); recorder != nil {
		opts = append(opts, providers.WithGiteaMutationRecorder(recorder))
	}
	return newGiteaProviderForStage(cfg.root, cfg.repo, token, opts...)
}

// stageProviderMutationRecorder resolves the recorder a stage records its
// external refs through, identically for every backend: an explicitly
// supplied recorder first, then the sidecar recorder for a declared kind,
// then none. A read-only stage records nothing — it makes no mutations to
// record — which is the pre-existing rule for the kind shorthand and is kept
// for the explicit recorder so the two cannot disagree.
func stageProviderMutationRecorder(cfg stageProviderConfig) providers.MutationRecorder {
	if cfg.readOnly {
		return nil
	}
	if cfg.mutationRecorder != nil {
		return cfg.mutationRecorder
	}
	if cfg.mutationKind != "" {
		return sidecarMutationRecorder{kind: cfg.mutationKind}
	}
	return nil
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
