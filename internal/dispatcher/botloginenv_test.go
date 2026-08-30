package dispatcher

import (
	"encoding/json"
	"errors"
	"slices"
	"testing"

	"github.com/goobers/goobers/internal/instance"
)

// botloginenv_test.go is Goobers#3914's dispatcher-side evidence: the login a
// stage's forge credential authenticates AS is resolved HERE, where the
// instance config is readable, and stamped into the pod as dispatcher-owned
// run identity — non-spoofable, kept by a goobers-CLI stage, stripped from
// every other one.
//
// The failure it exists to prevent is the one #3914 measured: a stage pod has
// no instance root, so the CLI's own fail-open lookup always resolved "" there
// and providers.WithConfiguredLogin was never applied. Under GitHub App auth
// that is a silent regression to GET /user — an endpoint an installation token
// cannot call — for every stage in a pod that consults AuthenticatedLogin.

// botLoginConfig is a dispatcher wired the way workerdispatch.go wires it from
// an instance whose github repo authenticates as a GitHub App.
func botLoginConfig() Config {
	cfg := testConfig()
	cfg.BotLogins = map[string]string{
		instance.GitHubBotLoginKey("Agent-Clubhouse", "Goobers"): "goobersbot[bot]",
	}
	return cfg
}

// routedCLIAttempt is a goobers-CLI attempt routed to the repository the
// config above declares a login for, with the RunContext the engine builds.
func routedCLIAttempt() Attempt {
	attempt := cliAttempt()
	attempt.RunContext = map[string]string{
		executorRepoProviderEnv: "github",
		executorRepoOwnerEnv:    "Agent-Clubhouse",
		executorRepoNameEnv:     "Goobers",
	}
	return attempt
}

// The headline: a rendered goobers-CLI stage pod carries the routed
// repository's configured bot login.
func TestRenderedCLIStagePodCarriesTheConfiguredBotLogin(t *testing.T) {
	pod, err := RenderPod(botLoginConfig(), routedCLIAttempt(), linuxRunner())
	if err != nil {
		t.Fatalf("RenderPod: %v", err)
	}
	if got := podEnvMap(pod)[ProviderBotLoginEnv]; got != "goobersbot[bot]" {
		t.Fatalf("%s = %q, want %q — without it the pod's provider falls back to GET /user, which a GitHub App installation token cannot call",
			ProviderBotLoginEnv, got, "goobersbot[bot]")
	}
}

// STAMPED UNCONDITIONALLY for a CLI stage, empty value included. In a pod
// these arrive as ordinary container variables, so a runner IMAGE exporting
// GOOBERS_PROVIDER_BOT_LOGIN would otherwise be inherited verbatim by the one
// stage class that keeps the name — the inherited-environment spoof. A value
// the dispatcher always stamps is a value the image cannot supply, and an
// empty stamp is a MEANINGFUL answer on the far side ("resolved: none
// declared") rather than the absence that fails closed.
func TestCLIStagePodAlwaysCarriesTheBotLoginStamp(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*Config, *Attempt)
	}{
		{"no logins configured at all", func(c *Config, _ *Attempt) { c.BotLogins = nil }},
		{"routed repo declares none", func(_ *Config, a *Attempt) {
			a.RunContext[executorRepoNameEnv] = "some-other-repo"
		}},
		{"routed repo is not github", func(_ *Config, a *Attempt) {
			a.RunContext[executorRepoProviderEnv] = "ado"
		}},
		{"no routed repo", func(_ *Config, a *Attempt) { a.RunContext = map[string]string{} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, attempt := botLoginConfig(), routedCLIAttempt()
			tc.mutate(&cfg, &attempt)
			pod, err := RenderPod(cfg, attempt, linuxRunner())
			if err != nil {
				t.Fatalf("RenderPod: %v", err)
			}
			value, stamped := podEnvMap(pod)[ProviderBotLoginEnv]
			if !stamped {
				t.Fatalf("%s was not stamped; an absent stamp fails the stage's identity CLOSED, and a runner image could supply the name instead", ProviderBotLoginEnv)
			}
			if value != "" {
				t.Fatalf("%s = %q, want empty — nothing resolvable was configured for this attempt", ProviderBotLoginEnv, value)
			}
		})
	}
}

// A non-CLI stage never sees it: the name is in DispatcherControlEnv, so
// __dispatch-exec strips it — including a value a runner image baked in.
func TestBotLoginIsNotStampedForANonCLIStage(t *testing.T) {
	attempt := routedCLIAttempt()
	attempt.CLIStage = false
	pod, err := RenderPod(botLoginConfig(), attempt, linuxRunner())
	if err != nil {
		t.Fatalf("RenderPod: %v", err)
	}
	if value, stamped := podEnvMap(pod)[ProviderBotLoginEnv]; stamped {
		t.Fatalf("%s = %q on a non-CLI stage; only a goobers-CLI stage builds providers", ProviderBotLoginEnv, value)
	}
	if !slices.Contains(DispatcherControlEnv, ProviderBotLoginEnv) {
		t.Fatalf("%s is not in DispatcherControlEnv, so a non-CLI stage would inherit an image-baked value", ProviderBotLoginEnv)
	}
	if !slices.Contains(DispatcherRunIdentityEnv, ProviderBotLoginEnv) {
		t.Fatalf("%s is not run identity, so a goobers-CLI stage would be stripped of the login it is stamped to use", ProviderBotLoginEnv)
	}
}

// SPOOF REFUSAL by name and by $(VAR) dereference. Both are properties of the
// control-env category and are asserted generically elsewhere
// (TestWorkflowEnvCannotOverrideAControlVariable /
// TestWorkflowEnvCannotDereferenceAControlVariable); restated here by name
// because "the bot login cannot be authored by a workflow" is the acceptance
// criterion of #3914 and must fail as itself if the category ever changes.
func TestWorkflowEnvCannotAuthorTheBotLogin(t *testing.T) {
	for _, tc := range []struct {
		name string
		env  map[string]string
		want func(*ControlEnvOverrideError) bool
	}{
		{
			name: "by name",
			env:  map[string]string{ProviderBotLoginEnv: "attacker[bot]"},
			want: func(e *ControlEnvOverrideError) bool { return e.Key == ProviderBotLoginEnv },
		},
		{
			name: "by $(VAR) dereference",
			env:  map[string]string{"MINE": "$(" + ProviderBotLoginEnv + ")"},
			want: func(e *ControlEnvOverrideError) bool { return e.Dereferences == ProviderBotLoginEnv },
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			attempt := routedCLIAttempt()
			attempt.Env = tc.env
			_, err := RenderPod(botLoginConfig(), attempt, linuxRunner())
			var override *ControlEnvOverrideError
			if !errors.As(err, &override) || !tc.want(override) {
				t.Fatalf("RenderPod err = %v, want a ControlEnvOverrideError for %s", err, ProviderBotLoginEnv)
			}
		})
	}
}

// Inputs cannot reach it (GOOBERS_INPUT_ prefixing), and the operator
// passthrough carries NAMES, never values — so neither can change the stamped
// login.
func TestInputsAndPassthroughCannotChangeTheBotLogin(t *testing.T) {
	cfg := botLoginConfig()
	cfg.EnvPassthrough = append([]string{"OPERATOR_VAR"}, DispatcherControlEnv...)
	attempt := routedCLIAttempt()
	attempt.Inputs = map[string]string{"provider-bot-login": "attacker[bot]", "PROVIDER_BOT_LOGIN": "attacker[bot]"}

	pod, err := RenderPod(cfg, attempt, envDenyRunner())
	if err != nil {
		t.Fatalf("RenderPod: %v", err)
	}
	if got := podEnvMap(pod)[ProviderBotLoginEnv]; got != "goobersbot[bot]" {
		t.Fatalf("%s = %q after hostile inputs and a passthrough naming the control plane, want %q", ProviderBotLoginEnv, got, "goobersbot[bot]")
	}
}

// A CLI stage on a class enforcing env:default-deny must KEEP the login: the
// in-pod rebuild from GOOBERS_STAGE_ENV_ALLOW runs before the CLI/non-CLI
// strip, so a name absent from the allowlist is gone before the split can
// decide to keep it — and a lost login is #3914 again, restriction-conditional.
func TestEnvDefaultDenyAllowlistCarriesTheBotLogin(t *testing.T) {
	pod, err := RenderPod(botLoginConfig(), routedCLIAttempt(), envDenyRunner())
	if err != nil {
		t.Fatalf("RenderPod: %v", err)
	}
	var allow []string
	if err := json.Unmarshal([]byte(podEnvMap(pod)[EnvStageEnvAllow]), &allow); err != nil {
		t.Fatalf("decode %s: %v", EnvStageEnvAllow, err)
	}
	if !slices.Contains(allow, ProviderBotLoginEnv) {
		t.Fatalf("%s is not in the env:default-deny allowlist; a CLI stage on this class would lose its provider identity", ProviderBotLoginEnv)
	}
}

// The stamp names the ROUTED repository's login and nothing else. The routed
// repo is read from the attempt's own dispatcher-built RunContext — the same
// map GOOBERS_REPO_* is stamped from — so the login and the repository it
// belongs to can never describe two different repositories.
func TestBotLoginFollowsTheRoutedRepository(t *testing.T) {
	cfg := botLoginConfig()
	cfg.BotLogins[instance.GitHubBotLoginKey("Agent-Clubhouse", "Goobernetes-Infra")] = "infrabot[bot]"
	attempt := routedCLIAttempt()
	attempt.RunContext[executorRepoNameEnv] = "Goobernetes-Infra"

	pod, err := RenderPod(cfg, attempt, linuxRunner())
	if err != nil {
		t.Fatalf("RenderPod: %v", err)
	}
	env := podEnvMap(pod)
	if got := env[ProviderBotLoginEnv]; got != "infrabot[bot]" {
		t.Fatalf("%s = %q, want the routed repository's login %q", ProviderBotLoginEnv, got, "infrabot[bot]")
	}
	if env[executorRepoNameEnv] != "Goobernetes-Infra" {
		t.Fatalf("the login and the routed repo disagree: %s = %q", executorRepoNameEnv, env[executorRepoNameEnv])
	}
}

// GitHub is case-insensitive about owner and name, so a config author's
// capitalization must not decide whether a stage finds its own login. Both
// halves go through instance.GitHubBotLoginKey for exactly this reason.
func TestBotLoginLookupIsCaseInsensitive(t *testing.T) {
	attempt := routedCLIAttempt()
	attempt.RunContext[executorRepoOwnerEnv] = "agent-clubhouse"
	attempt.RunContext[executorRepoNameEnv] = "GOOBERS"

	pod, err := RenderPod(botLoginConfig(), attempt, linuxRunner())
	if err != nil {
		t.Fatalf("RenderPod: %v", err)
	}
	if got := podEnvMap(pod)[ProviderBotLoginEnv]; got != "goobersbot[bot]" {
		t.Fatalf("%s = %q for a differently-cased routed repo, want %q", ProviderBotLoginEnv, got, "goobersbot[bot]")
	}
}

// The index the dispatcher is wired with is the one the local substrate reads,
// not a second lookup that agrees until one of them is edited.
func TestBotLoginIndexIsTheInstanceConfigLookup(t *testing.T) {
	cfg := &instance.Config{Repos: []instance.RepoRef{
		{Provider: "github", Owner: "Agent-Clubhouse", Name: "Goobers", Auth: &instance.RepoAuthConfig{Kind: instance.GitHubAuthApp, Slug: "goobersbot"}},
		{Provider: "github", Owner: "Agent-Clubhouse", Name: "pat-repo"},
		{Provider: "ado", Owner: "contoso", Project: "p", Name: "r"},
	}}
	index := cfg.GitHubBotLogins()
	for _, tc := range []struct{ owner, name, want string }{
		{"Agent-Clubhouse", "Goobers", "goobersbot[bot]"},
		{"agent-clubhouse", "goobers", "goobersbot[bot]"},
		{"Agent-Clubhouse", "pat-repo", ""},
		{"contoso", "r", ""},
		{"nobody", "nothing", ""},
	} {
		if got := index[instance.GitHubBotLoginKey(tc.owner, tc.name)]; got != tc.want {
			t.Errorf("index[%s/%s] = %q, want %q", tc.owner, tc.name, got, tc.want)
		}
		if got := cfg.GitHubBotLogin(tc.owner, tc.name); got != tc.want {
			t.Errorf("GitHubBotLogin(%s/%s) = %q, want %q — the two halves must not disagree", tc.owner, tc.name, got, tc.want)
		}
	}
}
