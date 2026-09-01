package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/goobers/goobers/internal/capability"
	"github.com/goobers/goobers/internal/dispatcher"
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
	provider, err := factory(cfg)
	if err != nil {
		return nil, err
	}
	configureStageAttribution(provider, root)
	return provider, nil
}

func configureStageAttribution(provider providers.Provider, root string) {
	configurer, ok := provider.(providers.AttributionConfigurer)
	if !ok {
		return
	}
	attribution, ok := stageAttribution(root)
	if !ok {
		return
	}
	configurer.SetAttribution(attribution)
}

func stageAttribution(root string) (providers.Attribution, bool) {
	runID := strings.TrimSpace(os.Getenv("GOOBERS_RUN_ID"))
	gaggle := strings.TrimSpace(os.Getenv("GOOBERS_GAGGLE"))
	workflow := strings.TrimSpace(os.Getenv("GOOBERS_WORKFLOW"))
	task := strings.TrimSpace(os.Getenv(executor.TaskEnvVar))
	goober := strings.TrimSpace(os.Getenv(executor.GooberEnvVar))
	if runID == "" || gaggle == "" || workflow == "" || task == "" {
		return providers.Attribution{}, false
	}
	if goober == "" {
		goober = "deterministic"
	}
	instanceName := ""
	if clean := filepath.Clean(strings.TrimSpace(root)); clean != "." && clean != "" {
		instanceName = filepath.Base(clean)
	}
	return providers.Attribution{
		Schema:   1,
		Goobers:  true,
		Instance: instanceName,
		Gaggle:   gaggle,
		Workflow: workflow,
		Task:     task,
		Goober:   goober,
		Run:      runID,
	}, true
}

func newProviderForStageAs[T providers.Provider](root string, repo providers.RepositoryRef, readOnly bool, opts ...stageProviderOption) (T, error) {
	return newProviderForStageSurface[T](root, repo, readOnly, opts...)
}

// newProviderForStageSurface is newProviderForStageAs for a stage surface that
// is not itself a superset of providers.Provider — a lane interface naming
// only the calls one stage makes (prCommentWatchProvider, remediationProvider).
// Those exist so a stage declares its own needs, and they must not be a reason
// to construct a backend directly: an off-seam constructor is one with no
// configured login, which is #3885/#3890 on the local substrate and #3914 in a
// pod.
func newProviderForStageSurface[T any](root string, repo providers.RepositoryRef, readOnly bool, opts ...stageProviderOption) (T, error) {
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
	// and threaded here (#3343). In a stage pod there IS no config, so the
	// dispatcher resolves it daemon-side and stamps it as run identity; this
	// seam is where both substrates converge (#3914). An unresolved identity
	// no longer degrades to GET /user in a pod — it makes AuthenticatedLogin
	// refuse, which leaves a provider whose stage never asks for a login
	// working exactly as before.
	opts = append(opts, stageProviderConfiguredLogin(cfg.root, cfg.repo).options()...)
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
// auth block declares a GitHub App slug (#3343) — on the LOCAL substrate,
// where the instance config is readable, or from the dispatcher's stamped run
// identity in a stage pod, where it is not (#3914).
//
// It returns a resolution rather than a string because "" is two different
// answers and conflating them is the bug this exists to fix. See
// stageProviderLogin.
//
// PRECEDENCE: the dispatcher-stamped identity WINS over any config file it
// could read. In a pod the only config reachable at all is one inside the
// workspace — content the run itself may have authored — so a stamp that
// yielded to a file on disk would hand a workflow the bot identity it is
// specifically not allowed to author. On the local substrate the variable is
// not stamped (the executor never sets it, and procenv's default-deny
// allowlist does not carry the name), so this arm simply does not arise and
// local behavior is the pre-existing config lookup exactly.
func stageProviderConfiguredLogin(root string, repo providers.RepositoryRef) stageProviderLogin {
	if repo.Provider != providers.ProviderGitHub {
		return stageProviderLogin{}
	}
	if stamped, ok := os.LookupEnv(dispatcher.ProviderBotLoginEnv); ok {
		return stageProviderStampedLogin(stamped, repo)
	}
	cfg, err := instance.LoadConfig(instance.NewLayout(root).ConfigFile())
	if err == nil {
		// The config is authoritative here, INCLUDING when it declares no bot
		// login for this repo: that is the PAT posture, and GET /user is the
		// right answer for it.
		return stageProviderLogin{login: cfg.GitHubBotLogin(repo.Owner, repo.Name)}
	}
	if strings.TrimSpace(os.Getenv(executor.InstanceRootEnvVar)) == "" {
		// No instance root and no stamp: this is a stage pod (the same
		// signal dispatchexec.go's instance-root backstop keys off, and true
		// in a pod because the dispatcher never stamps that variable) served
		// by a dispatcher that does not stamp the login yet — version skew,
		// or a hand-built attempt. Fail the identity CLOSED. Falling through
		// to GET /user is what #3914 is: correct under a PAT, and under App
		// auth a request that cannot succeed, failing far away with a forge
		// error for a platform fault.
		return stageProviderLogin{refusal: fmt.Sprintf(
			"this stage is running in a pod (%s is unset) and the dispatcher stamped no %s for %s/%s; the instance config that declares the bot login is not readable here, so the login cannot be resolved (Goobers#3914)",
			executor.InstanceRootEnvVar, dispatcher.ProviderBotLoginEnv, repo.Owner, repo.Name)}
	}
	// An instance root IS declared and its config did not load: the
	// pre-existing best-effort local posture, unchanged. A PAT self-reports,
	// and undeclared App auth still fails with the actionable 403.
	return stageProviderLogin{}
}

// stageProviderLogin is what identity resolution produced for one provider
// construction, and it has three states because the far side has three cases
// (#3914):
//
//   - login set        — apply providers.WithConfiguredLogin;
//   - both empty       — no declared login; GET /user is correct (a PAT);
//   - refusal set      — identity could not be resolved AND the fallback is
//     known-unsafe, so AuthenticatedLogin must fail closed instead of asking
//     an endpoint an App installation token cannot call.
//
// The refusal is deliberately NOT a construction error: a provider whose stage
// never consults AuthenticatedLogin is unaffected by an unresolved login and
// must keep working — "genuinely unconfigured provider" is a supported posture
// and this must not turn it into a refused stage. The refusal travels INTO the
// provider and fires only if the login is actually asked for.
type stageProviderLogin struct {
	login   string
	refusal string
}

// options renders the resolution as GitHub provider options.
func (r stageProviderLogin) options() []func(*providers.GitHubProvider) {
	if r.login != "" {
		return []func(*providers.GitHubProvider){providers.WithConfiguredLogin(r.login)}
	}
	if r.refusal != "" {
		return []func(*providers.GitHubProvider){providers.WithLoginSelfReportRefused(r.refusal)}
	}
	return nil
}

// stageProviderStampedLogin interprets the dispatcher's stamped identity for
// the repository a provider is being built for.
//
// The stamp names ONE repository — the one the run was routed to — so it is
// applied only to that repository. A stage building a provider for some other
// repo (an additional repo, a cross-repo read) has no resolved identity for
// it, and in a pod there is no config to resolve one from, so that fails
// closed too rather than borrowing the routed repo's login and acting under
// an identity the operator never declared for it.
//
// The routed repository is read from the dispatcher's own GOOBERS_REPO_*
// variables, which sit in the same control-env category as the stamp itself:
// a workflow cannot author either, so the binding cannot be bypassed by
// declaring a repository.
func stageProviderStampedLogin(stamped string, repo providers.RepositoryRef) stageProviderLogin {
	routedOwner := os.Getenv(executor.RepoOwnerEnvVar)
	routedName := os.Getenv(executor.RepoNameEnvVar)
	if !strings.EqualFold(instance.GitHubBotLoginKey(routedOwner, routedName), instance.GitHubBotLoginKey(repo.Owner, repo.Name)) ||
		strings.TrimSpace(routedOwner) == "" || strings.TrimSpace(routedName) == "" {
		return stageProviderLogin{refusal: fmt.Sprintf(
			"%s was stamped for the routed repository %s/%s, not for %s/%s; this stage has no resolved provider identity for that repository (Goobers#3914)",
			dispatcher.ProviderBotLoginEnv, routedOwner, routedName, repo.Owner, repo.Name)}
	}
	login := strings.TrimSpace(stamped)
	if login == "" {
		// Resolved, and nothing declared: the PAT posture. The local
		// substrate returns "" for exactly the same repository, so the two
		// substrates agree — which is the parity #3914 asks for, not merely
		// the App case working.
		return stageProviderLogin{}
	}
	if !githubLoginPattern.MatchString(login) {
		return stageProviderLogin{refusal: fmt.Sprintf(
			"%s is malformed (%q is not a GitHub login); refusing to act under an unusable identity rather than falling back to GET /user (Goobers#3914)",
			dispatcher.ProviderBotLoginEnv, stamped)}
	}
	return stageProviderLogin{login: login}
}

// githubLoginPattern is the shape a GitHub login can have: alphanumerics and
// hyphens, optionally with the "[bot]" suffix an App's login carries. It is a
// SANITY bound, not an authorization: the stamp is dispatcher-owned, so this
// catches a mis-stamped or truncated value (a whole config blob, a value with
// a newline, an empty "[bot]") before it is compared against comment authors,
// where a malformed identity matches nothing and every trusted-comment check
// silently reads as "not mine".
var githubLoginPattern = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9-]{0,38})(?:\[bot\])?$`)
