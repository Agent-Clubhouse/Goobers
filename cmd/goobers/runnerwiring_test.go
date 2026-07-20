package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/capability"
	"github.com/goobers/goobers/internal/credentials"
	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/internal/gate"
	"github.com/goobers/goobers/internal/harness"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/runner"
	"github.com/goobers/goobers/internal/telemetry"
	"github.com/goobers/goobers/internal/telemetry/rollup"
	"github.com/goobers/goobers/providers"
)

// resolveGrants materializes each grant's ref through the resolver, returning a
// capability->token-value map so tests can assert which token actually backs a
// capability (the whole point of #287's per-capability sourcing/override).
func resolveGrants(t *testing.T, r credentials.Resolver, grants []credentials.Grant) map[string]string {
	t.Helper()
	out := make(map[string]string, len(grants))
	for _, g := range grants {
		if _, dup := out[g.Capability]; dup {
			t.Fatalf("capability %q granted more than once: %+v", g.Capability, grants)
		}
		val, err := r.Resolve(context.Background(), g.Ref)
		if err != nil {
			t.Fatalf("resolve ref %q for %q: %v", g.Ref, g.Capability, err)
		}
		out[g.Capability] = val
	}
	return out
}

// TestBuildEnvCapabilities is #288's wiring: the Copilot adapter's capability→
// env-var map routes agent:model to COPILOT_GITHUB_TOKEN and every org-repo
// capability to GH_TOKEN — two DISTINCT vars, so one subprocess can hold both
// tokens (§3.3). A collision (agent:model sharing GH_TOKEN) would clobber one.
func TestBuildEnvCapabilities(t *testing.T) {
	envCaps := buildEnvCapabilities()
	if got := envCaps["agent:model"]; got != "COPILOT_GITHUB_TOKEN" {
		t.Fatalf("agent:model env = %q, want COPILOT_GITHUB_TOKEN", got)
	}
	for _, c := range credentialedCapabilities {
		if got := envCaps[string(c)]; got != credentialGrantEnv {
			t.Fatalf("capability %s env = %q, want %q", c, got, credentialGrantEnv)
		}
	}
	if envCaps["agent:model"] == credentialGrantEnv {
		t.Fatalf("agent:model must map to a var distinct from the github-tool var %q, else the two tokens collide", credentialGrantEnv)
	}
}

func TestBuildHarnessRegistryMapsGooberHarnessToCopilotAdapter(t *testing.T) {
	envCaps := buildEnvCapabilities()
	registry, err := buildHarnessRegistry(envCaps)
	if err != nil {
		t.Fatalf("buildHarnessRegistry: %v", err)
	}
	adapter, err := registry.Get(string(apiv1.HarnessCopilot))
	if err != nil {
		t.Fatalf("Get(copilot): %v", err)
	}
	copilot, ok := adapter.(*harness.CopilotAdapter)
	if !ok {
		t.Fatalf("registered adapter = %T, want *harness.CopilotAdapter", adapter)
	}
	if copilot.Name() != "copilot-cli" {
		t.Fatalf("adapter Name = %q, want existing diagnostic identity copilot-cli", copilot.Name())
	}
	if copilot.EnvCapabilities[string(capability.AgentModel)] != copilotModelEnv {
		t.Fatalf("agent:model env = %q, want %q", copilot.EnvCapabilities[string(capability.AgentModel)], copilotModelEnv)
	}
	if len(copilot.AuthCheckArgs) == 0 {
		t.Fatal("registered Copilot adapter is missing its authentication preflight")
	}
}

// runValidate and daemon startup both call compiledMachines, so this golden
// list is the automated-check contract for every surviving config admission
// path. A registry change must update this list rather than silently drifting.
func TestValidationAutomatedChecksGolden(t *testing.T) {
	want := []string{
		"ci-status",
		"land-outcome",
		"output-equals",
		"output-matches",
		"output-not-equals",
		"output-numeric-gte",
		"output-numeric-lt",
		"output-numeric-lte",
		"queue-outcome",
		"status-equals",
	}
	if got := knownAutomatedCheckNames(); !slices.Equal(got, want) {
		t.Fatalf("validation automated checks = %v, want %v", got, want)
	}
}

func TestCompiledMachinesRejectsInvalidHarnessConfig(t *testing.T) {
	for _, tc := range []struct {
		name string
		spec apiv1.GooberSpec
		want string
	}{
		{
			name: "unknown model",
			spec: apiv1.GooberSpec{Harness: apiv1.HarnessCopilot, Model: "unknown-model"},
			want: `unknown model "unknown-model"`,
		},
		{
			name: "unknown option",
			spec: apiv1.GooberSpec{
				Harness: apiv1.HarnessCopilot,
				HarnessOptions: map[string]apiextensionsv1.JSON{
					"temperature": {Raw: []byte(`"0.2"`)},
				},
			},
			want: `unknown harness option "temperature"`,
		},
		{
			name: "unsupported model option",
			spec: apiv1.GooberSpec{
				Harness: apiv1.HarnessCopilot,
				Model:   "claude-sonnet-4.5",
				HarnessOptions: map[string]apiextensionsv1.JSON{
					"reasoningEffort": {Raw: []byte(`"high"`)},
				},
			},
			want: `reasoningEffort value "high" is not supported by model "claude-sonnet-4.5"`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := compiledMachines(
				&instance.ConfigSet{},
				map[string]apiv1.GooberSpec{"coder": tc.spec},
			)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("compiledMachines error = %v, want %q", err, tc.want)
			}
		})
	}
}

// TestBuildCredentialsDefault: with no credentials: block, the first repo's
// token backs every credentialed capability and agent:model is absent (it must
// be sourced explicitly, never defaulted to the repo token).
func TestBuildCredentialsDefault(t *testing.T) {
	t.Setenv("GH_TOKEN_A", "tokenA")
	cfg := &instance.Config{Repos: []instance.RepoRef{
		{Provider: "github", Owner: "acme", Name: "web", Token: instance.TokenRef{Env: "GH_TOKEN_A"}},
	}}
	resolver, grants, err := buildCredentials(cfg)
	if err != nil {
		t.Fatalf("buildCredentials: %v", err)
	}

	got := resolveGrants(t, resolver, grants)
	for _, c := range credentialedCapabilities {
		if got[string(c)] != "tokenA" {
			t.Fatalf("capability %s = %q, want repo token tokenA", c, got[string(c)])
		}
	}
	if _, ok := got["agent:model"]; ok {
		t.Fatalf("agent:model must not be granted without a credentials: entry, got %+v", got)
	}
}

func TestWorkflowRuntimeIndexesUseGaggleAndName(t *testing.T) {
	workflowDefinition := func(gaggle, command string) apiv1.Workflow {
		return apiv1.Workflow{
			ObjectMeta: metav1.ObjectMeta{Name: "deploy"},
			Spec: apiv1.WorkflowSpec{
				Gaggle:   gaggle,
				Triggers: []apiv1.Trigger{{Type: apiv1.TriggerManual}},
				Start:    "deploy",
				Tasks: []apiv1.Task{{
					Name: "deploy",
					Type: apiv1.TaskDeterministic,
					Goal: "Deploy.",
					Run:  &apiv1.DeterministicRun{Command: []string{"sh", "-c", "printf " + command}, Workspace: apiv1.WorkspaceScratch},
				}},
			},
		}
	}
	set := &instance.ConfigSet{
		Gaggles: []apiv1.Gaggle{
			{ObjectMeta: metav1.ObjectMeta{Name: "alpha"}, Spec: apiv1.GaggleSpec{Project: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "example", Name: "alpha"}}},
			{ObjectMeta: metav1.ObjectMeta{Name: "beta"}, Spec: apiv1.GaggleSpec{Project: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "example", Name: "beta"}}},
		},
		Workflows: []apiv1.Workflow{
			workflowDefinition("alpha", "alpha-deploy"),
			workflowDefinition("beta", "beta-deploy"),
		},
	}

	machines, err := compiledMachines(set, map[string]apiv1.GooberSpec{})
	if err != nil {
		t.Fatal(err)
	}
	refs, err := repoRefsByWorkflow(set)
	if err != nil {
		t.Fatal(err)
	}
	alpha := localscheduler.WorkflowIdentity{Gaggle: "alpha", Workflow: "deploy"}
	beta := localscheduler.WorkflowIdentity{Gaggle: "beta", Workflow: "deploy"}
	if len(machines) != 2 || machines[alpha] == nil || machines[beta] == nil {
		t.Fatalf("compiled machines = %+v", machines)
	}
	if len(refs) != 2 || refs[alpha].Name != "alpha" || refs[beta].Name != "beta" {
		t.Fatalf("workflow repo refs = %+v", refs)
	}

	layout := instance.NewLayout(t.TempDir())
	for _, gaggle := range []string{"alpha", "beta"} {
		if err := layout.EnsureGaggleRuntime(gaggle); err != nil {
			t.Fatal(err)
		}
	}
	log, _, err := journal.OpenInstanceLog(layout.SchedulerDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = log.Close() })
	var wg sync.WaitGroup
	definitions, err := buildSchedulerDefinitions(
		layout,
		&instance.Config{},
		set,
		nil,
		&wg,
		nil,
		nil,
		log,
		journal.NewRegistryScrubber(),
		nil,
		localscheduler.NewProviderQuotaState(),
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if definitions.WorktreesByGaggle["alpha"].Root == definitions.WorktreesByGaggle["beta"].Root {
		t.Fatal("gaggles share a workcopy root")
	}
	for i, identity := range []localscheduler.WorkflowIdentity{alpha, beta} {
		runID, err := telemetry.NewRunID()
		if err != nil {
			t.Fatal(err)
		}
		result, err := definitions.Runners[identity.Gaggle].Start(context.Background(), runner.StartInput{
			RunID:   runID,
			Machine: definitions.Machines[identity],
			Gaggle:  identity.Gaggle,
		})
		if err != nil || result.Phase != journal.PhaseCompleted {
			t.Fatalf("start %s run %d: phase=%s err=%v", identity.Gaggle, i, result.Phase, err)
		}
		if _, err := os.Stat(filepath.Join(layout.ForGaggle(identity.Gaggle).RunsDir(), runID, "run.yaml")); err != nil {
			t.Fatalf("%s run journal: %v", identity.Gaggle, err)
		}
	}
}

func TestLegacyClaimNamespaceUsesOwningRunIdentity(t *testing.T) {
	layout := instance.NewLayout(t.TempDir())
	providers := map[string]apiv1.Provider{
		"alpha": apiv1.ProviderGitHub,
		"beta":  apiv1.ProviderADO,
	}
	for _, test := range []struct {
		runID    string
		gaggle   string
		provider string
	}{
		{runID: "run-alpha", gaggle: "alpha", provider: "github"},
		{runID: "run-beta", gaggle: "beta", provider: "ado"},
	} {
		run, err := journal.Create(layout.RunsDir(), journal.RunIdentity{
			RunID:     test.runID,
			Workflow:  "deploy",
			Gaggle:    test.gaggle,
			StartedAt: time.Now(),
		}, nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := run.Close(); err != nil {
			t.Fatal(err)
		}

		namespace, err := legacyClaimNamespace(layout, providers, localscheduler.ClaimEntry{RunID: test.runID})
		if err != nil {
			t.Fatal(err)
		}
		if namespace.Gaggle != test.gaggle || namespace.Provider != test.provider {
			t.Fatalf("namespace for %s = %+v, want gaggle %q provider %q", test.runID, namespace, test.gaggle, test.provider)
		}
	}
}

func TestBuildSchedulerSetupMigratesLiveLegacyClaimForRemovedWorkflow(t *testing.T) {
	root := initDeterministicDemo(t)
	layout := instance.NewLayout(root)
	const runID = "removed-workflow-run"

	run, err := journal.Create(layout.RunsDir(), journal.RunIdentity{
		RunID: runID, Workflow: "removed-workflow", WorkflowVersion: 1, Gaggle: "example",
		Trigger: journal.Trigger{Kind: journal.TriggerManual},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := run.Close(); err != nil {
		t.Fatal(err)
	}
	ledgerPath := filepath.Join(layout.SchedulerDir(), claimLedgerFileName)
	ledger, err := localscheduler.OpenClaimLedger(ledgerPath)
	if err != nil {
		t.Fatal(err)
	}
	if ok, _, err := ledger.Claim("159", runID, "removed-workflow", time.Hour); err != nil || !ok {
		t.Fatalf("seed legacy claim: ok=%v err=%v", ok, err)
	}

	var wg sync.WaitGroup
	setup, err := buildSchedulerSetup(context.Background(), layout, &wg)
	if err != nil {
		t.Fatalf("buildSchedulerSetup: %v", err)
	}
	defer setup.Shutdown(context.Background())

	reopened, err := localscheduler.OpenClaimLedger(ledgerPath)
	if err != nil {
		t.Fatal(err)
	}
	key := localscheduler.ClaimKey{Gaggle: "example", Provider: "github", ExternalID: "159"}
	entry, ok := reopened.LookupScoped(key)
	if !ok || entry.RunID != runID {
		t.Fatalf("migrated claim = %+v, %v; want claim scoped from the run's gaggle", entry, ok)
	}
	if _, ok := reopened.Lookup("159"); ok {
		t.Fatal("item-only legacy claim remained after ownership was resolved without the removed workflow")
	}
}

// TestBuildCredentialsAgentModel: a credentials: entry for agent:model adds a
// grant sourced from its own token, leaving the repo-backed capabilities intact
// — the two-tokens-one-subprocess case (#287).
func TestBuildCredentialsAgentModel(t *testing.T) {
	t.Setenv("GH_TOKEN_A", "tokenA")
	t.Setenv("COPILOT_PAT", "copilottok")
	cfg := &instance.Config{
		Repos: []instance.RepoRef{
			{Provider: "github", Owner: "acme", Name: "web", Token: instance.TokenRef{Env: "GH_TOKEN_A"}},
		},
		Credentials: []instance.CredentialGrant{
			{Capability: "agent:model", Token: instance.TokenRef{Env: "COPILOT_PAT"}},
		},
	}
	resolver, grants, err := buildCredentials(cfg)
	if err != nil {
		t.Fatalf("buildCredentials: %v", err)
	}
	got := resolveGrants(t, resolver, grants)
	if got["agent:model"] != "copilottok" {
		t.Fatalf("agent:model = %q, want copilottok", got["agent:model"])
	}
	for _, c := range credentialedCapabilities {
		if got[string(c)] != "tokenA" {
			t.Fatalf("capability %s = %q, want repo token tokenA", c, got[string(c)])
		}
	}
}

// TestBuildCredentialsOverride is #287 AC1/AC3: a credentials: entry for a
// capability the repo token would otherwise back OVERRIDES that grant — it
// resolves to the entry's token, and it stays a single grant (not duplicated).
func TestBuildCredentialsOverride(t *testing.T) {
	t.Setenv("GH_TOKEN_A", "tokenA")
	t.Setenv("PUSH_TOKEN_B", "tokenB")
	cfg := &instance.Config{
		Repos: []instance.RepoRef{
			{Provider: "github", Owner: "acme", Name: "web", Token: instance.TokenRef{Env: "GH_TOKEN_A"}},
		},
		Credentials: []instance.CredentialGrant{
			{Capability: "repo:push", Token: instance.TokenRef{Env: "PUSH_TOKEN_B"}},
		},
	}
	resolver, grants, err := buildCredentials(cfg)
	if err != nil {
		t.Fatalf("buildCredentials: %v", err)
	}
	got := resolveGrants(t, resolver, grants)
	if got["repo:push"] != "tokenB" {
		t.Fatalf("repo:push = %q, want override tokenB", got["repo:push"])
	}
	// The other repo-backed capabilities are untouched by the override.
	if got["github:issues:write"] != "tokenA" || got["github:pr:write"] != "tokenA" {
		t.Fatalf("non-overridden capabilities changed: %+v", got)
	}
}

func TestBuildGooberCredentialGrantsScopesSourcesToIdentity(t *testing.T) {
	sources := []credentials.Grant{
		{Capability: "agent:model", Ref: "model-token"},
		{Capability: "github:issues:write", Ref: "issues-token"},
	}
	grants := buildGooberCredentialGrants(
		"curator",
		[]string{"agent:model", "telemetry:read", "agent:model"},
		sources,
	)
	if len(grants) != 1 {
		t.Fatalf("grants = %+v, want one credential-backed grant", grants)
	}
	if got := grants[0]; got.Goober != "curator" || got.Capability != "agent:model" || got.Ref != "model-token" {
		t.Fatalf("grant = %+v, want curator/agent:model/model-token", got)
	}
}

// TestIngestRunTelemetryLogsForcedFailure is issue #246's third fix: a
// swallowed rollup-ingest error used to leave nothing but a bare `_ =` — no
// visible trace anywhere that the rollup silently fell behind. This forces
// IngestRun to fail (a closed *rollup.DB) and asserts the failure is visible
// in the instance log, not merely absorbed.
func TestIngestRunTelemetryLogsForcedFailure(t *testing.T) {
	root := t.TempDir()
	l := instance.NewLayout(root)

	db, err := rollup.Open(filepath.Join(root, "telemetry.db"))
	if err != nil {
		t.Fatal(err)
	}
	// Force IngestRun/IngestSchedulerLog to fail deterministically, without
	// relying on any particular on-disk run-directory shape.
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	log, _, err := journal.OpenInstanceLog(l.SchedulerDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = log.Close() })

	ingestRunTelemetry(nil, db, l, "run-forced-failure", log)

	events, err := journal.ReadInstanceLog(l.SchedulerDir())
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, ev := range events {
		if ev.Type == journal.EventError && ev.RunID == "run-forced-failure" && ev.Error != nil &&
			strings.Contains(ev.Error.Code, "telemetry_ingest") {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected a telemetry_ingest_* error event for run-forced-failure, got: %+v", events)
	}
}

// TestIngestRunTelemetryNilLogDoesNotPanic proves logIngestFailure's nil-log
// guard holds — ingestRunTelemetry is called from contexts (tests, a
// standalone db) where no instance log may be wired.
func TestIngestRunTelemetryNilLogDoesNotPanic(t *testing.T) {
	root := t.TempDir()
	l := instance.NewLayout(root)
	db, err := rollup.Open(filepath.Join(root, "telemetry.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	ingestRunTelemetry(nil, db, l, "run-nil-log", nil)
}

// --- #312: escalation-notifier wiring ---

type escTestRegistrar struct{ registered [][]byte }

func (r *escTestRegistrar) Register(secret []byte) {
	r.registered = append(r.registered, append([]byte(nil), secret...))
}

type ciPollTestRecorder struct{}

func (ciPollTestRecorder) RecordArtifact(name string, data []byte) (journal.Ref, error) {
	return journal.Ref{Path: name, Digest: journal.Digest(data), Size: int64(len(data))}, nil
}

type ciPollTestFallback struct{}

func (ciPollTestFallback) Run(context.Context, apiv1.InvocationEnvelope, apiv1.DeterministicRun) (apiv1.ResultEnvelope, error) {
	return apiv1.ResultEnvelope{}, errors.New("unexpected fallback invocation")
}

type ciPollFakePoller struct{ called bool }

func (p *ciPollFakePoller) PollPullRequest(context.Context, providers.PullRequestPollRequest) (providers.PullRequestPollResult, error) {
	p.called = true
	return providers.PullRequestPollResult{CheckState: providers.CheckStatePassing}, nil
}

func newCIPollWiringTestExecutor(t *testing.T, reg *escTestRegistrar) invoke.Deterministic {
	t.Helper()
	t.Setenv("CI_POLL_TOKEN", "ci-poll-token-value")
	cfg := repoConfig()
	cfg.Repos[0].Token.Env = "CI_POLL_TOKEN"
	resolver, grants, err := buildCredentials(cfg)
	if err != nil {
		t.Fatalf("buildCredentials: %v", err)
	}
	injector, err := credentials.NewInjector(resolver, grants, reg)
	if err != nil {
		t.Fatalf("NewInjector: %v", err)
	}
	deterministic, err := buildCIPollExecutor(cfg, injector, ciPollTestFallback{}, ciPollTestRecorder{})
	if err != nil {
		t.Fatalf("buildCIPollExecutor: %v", err)
	}
	return deterministic
}

func ciPollTestEnvelope(capabilities []string) apiv1.InvocationEnvelope {
	return apiv1.InvocationEnvelope{
		RepoRef:      apiv1.RepoRef{Owner: "acme", Name: "web"},
		Capabilities: capabilities,
		Inputs: map[string]interface{}{
			executor.InputKind:     executor.KindCIPoll,
			executor.InputPRNumber: "401",
		},
	}
}

func TestCIPollCredentialRequiresDeclaredCapability(t *testing.T) {
	deterministic := newCIPollWiringTestExecutor(t, &escTestRegistrar{})
	called := false
	prev := newPRPoller
	newPRPoller = func(string) executor.PRPoller {
		called = true
		return &ciPollFakePoller{}
	}
	t.Cleanup(func() { newPRPoller = prev })

	_, err := deterministic.Run(context.Background(), ciPollTestEnvelope(nil), apiv1.DeterministicRun{})
	if !errors.Is(err, credentials.ErrUndeclaredCapability) {
		t.Fatalf("Run error = %v, want ErrUndeclaredCapability", err)
	}
	if called {
		t.Fatal("PR poller constructed before capability admission")
	}
}

func TestCIPollCredentialAdmitsDeclaredCapability(t *testing.T) {
	reg := &escTestRegistrar{}
	deterministic := newCIPollWiringTestExecutor(t, reg)
	poller := &ciPollFakePoller{}
	var gotToken string
	prev := newPRPoller
	newPRPoller = func(token string) executor.PRPoller {
		gotToken = token
		return poller
	}
	t.Cleanup(func() { newPRPoller = prev })

	result, err := deterministic.Run(context.Background(), ciPollTestEnvelope([]string{string(capability.GitHubPRWrite)}), apiv1.DeterministicRun{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if gotToken != "ci-poll-token-value" {
		t.Fatalf("poller token = %q, want declared capability token", gotToken)
	}
	if !poller.called {
		t.Fatal("PR poller was not called")
	}
	if result.Outputs[executor.OutputCIStatus] != string(providers.CheckStatePassing) {
		t.Fatalf("outputs = %+v, want ciStatus=%q", result.Outputs, providers.CheckStatePassing)
	}
	if len(reg.registered) != 1 || string(reg.registered[0]) != "ci-poll-token-value" {
		t.Fatalf("registered secrets = %q, want the ci-poll token", reg.registered)
	}
}

type escFakeCommenter struct {
	gotReq providers.UpdateWorkItemRequest
}

func (f *escFakeCommenter) UpdateWorkItem(_ context.Context, req providers.UpdateWorkItemRequest) (providers.WorkItem, error) {
	f.gotReq = req
	return providers.WorkItem{}, nil
}

// TestBuildEscalationNotifier is #312: the notifier is wired at the composition
// root for a repo-backed instance (so runner.Config.Escalation is no longer
// always nil), and nil for a repo-less instance (nothing to comment on).
func TestBuildEscalationNotifier(t *testing.T) {
	t.Run("nil for a repo-less instance", func(t *testing.T) {
		if n := buildEscalationNotifier(&instance.Config{}, nil, nil); n != nil {
			t.Fatalf("expected a nil notifier for no repos, got %+v", n)
		}
	})
	t.Run("wired for a repo-backed instance", func(t *testing.T) {
		cfg := &instance.Config{Repos: []instance.RepoRef{
			{Provider: "github", Owner: "acme", Name: "web", Token: instance.TokenRef{Env: "ESC_TOK"}},
		}}
		resolver, err := credentials.NewResolver([]credentials.TokenRef{{Name: "acme/web", Env: "ESC_TOK"}})
		if err != nil {
			t.Fatalf("NewResolver: %v", err)
		}
		n := buildEscalationNotifier(cfg, resolver, &escTestRegistrar{})
		if n == nil {
			t.Fatal("expected a non-nil notifier for a repo-backed instance")
		}
		if n.Poster == nil {
			t.Fatal("expected a non-nil escalation poster")
		}
	})
}

// TestEscalationCommenterResolvesTokenPerCall is #312's rotation-safety +
// scrubbing property plus #544's multi-repo regression: the commenter resolves
// the request repository's token on each call (not captured at startup),
// registers it for scrubbing, and posts through a freshly-authenticated
// provider.
func TestEscalationCommenterResolvesTokenPerCall(t *testing.T) {
	t.Setenv("ESC_PRIMARY_TOK", "primary-token-value")
	t.Setenv("ESC_SECONDARY_TOK", "secondary-token-value")
	resolver, err := credentials.NewResolver([]credentials.TokenRef{
		{Name: "acme/web", Env: "ESC_PRIMARY_TOK"},
		{Name: "acme/api", Env: "ESC_SECONDARY_TOK"},
	})
	if err != nil {
		t.Fatalf("NewResolver: %v", err)
	}
	reg := &escTestRegistrar{}

	fake := &escFakeCommenter{}
	var gotToken string
	prev := newEscalationPoster
	newEscalationPoster = func(token string) gate.Commenter { gotToken = token; return fake }
	t.Cleanup(func() { newEscalationPoster = prev })

	c := &escalationCommenter{resolver: resolver, reg: reg}
	repository := providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "acme", Name: "api"}
	if _, err := c.UpdateWorkItem(context.Background(), providers.UpdateWorkItemRequest{
		Repository: repository,
		ID:         "281",
		Comment:    "escalated",
	}); err != nil {
		t.Fatalf("UpdateWorkItem: %v", err)
	}
	if gotToken != "secondary-token-value" {
		t.Fatalf("poster built with token %q, want the secondary repository token", gotToken)
	}
	if fake.gotReq.Repository != repository || fake.gotReq.ID != "281" || fake.gotReq.Comment != "escalated" {
		t.Fatalf("posted request = %+v", fake.gotReq)
	}
	var registered bool
	for _, s := range reg.registered {
		if string(s) == "secondary-token-value" {
			registered = true
		}
	}
	if !registered {
		t.Fatalf("resolved token not registered for scrubbing; registered=%v", reg.registered)
	}
}

// --- #353: open-PR-count refresher wiring ---

func cappedWorkflows() []apiv1.Workflow {
	return []apiv1.Workflow{{Spec: apiv1.WorkflowSpec{Readiness: apiv1.ReadinessConditions{MaxOpenPRs: 1}}}}
}

func repoConfig() *instance.Config {
	return &instance.Config{Repos: []instance.RepoRef{
		{Provider: "github", Owner: "acme", Name: "web", Token: instance.TokenRef{Env: "OPENPR_TOK"}},
	}}
}

// TestBuildOpenPRRefresher is #353: the refresher is built only when a repo is
// configured AND some workflow opts into the MaxOpenPRs cap — so an instance
// that doesn't use the cap grows no GitHub poller.
func TestBuildOpenPRRefresher(t *testing.T) {
	t.Run("nil for a repo-less instance", func(t *testing.T) {
		r, err := buildOpenPRRefresher(&instance.Config{}, cappedWorkflows(), &escTestRegistrar{})
		if err != nil || r != nil {
			t.Fatalf("want nil,nil; got %v,%v", r, err)
		}
	})
	t.Run("nil when no workflow opts into the cap", func(t *testing.T) {
		wfs := []apiv1.Workflow{{Spec: apiv1.WorkflowSpec{Readiness: apiv1.ReadinessConditions{MaxConcurrentRuns: 1}}}}
		r, err := buildOpenPRRefresher(repoConfig(), wfs, &escTestRegistrar{})
		if err != nil || r != nil {
			t.Fatalf("want nil,nil; got %v,%v", r, err)
		}
	})
	t.Run("built when a repo and a capped workflow are present", func(t *testing.T) {
		r, err := buildOpenPRRefresher(repoConfig(), cappedWorkflows(), &escTestRegistrar{})
		if err != nil {
			t.Fatalf("buildOpenPRRefresher: %v", err)
		}
		if r == nil {
			t.Fatal("expected a non-nil refresher for a repo-backed, capped instance")
		}
	})
}

type fakeHeadLister struct{ heads []string }

func (f *fakeHeadLister) ListOpenPullRequests(context.Context, providers.RepositoryRef) ([]providers.OpenPRSummary, error) {
	prs := make([]providers.OpenPRSummary, 0, len(f.heads))
	for _, h := range f.heads {
		prs = append(prs, providers.OpenPRSummary{Head: h})
	}
	return prs, nil
}

// TestResolvingOpenPRListerResolvesTokenPerCall is #353's rotation-safety +
// scrubbing property: the lister resolves the org-repo token on each poll (not
// captured at startup), registers it for scrubbing, and lists through a freshly
// authenticated provider.
func TestResolvingOpenPRListerResolvesTokenPerCall(t *testing.T) {
	t.Setenv("OPENPR_TOK", "list-token-value")
	resolver, err := credentials.NewResolver([]credentials.TokenRef{{Name: "acme/web", Env: "OPENPR_TOK"}})
	if err != nil {
		t.Fatalf("NewResolver: %v", err)
	}
	reg := &escTestRegistrar{}

	fake := &fakeHeadLister{heads: []string{"goobers/implementation/run-1"}}
	var gotToken string
	prev := newOpenPRProvider
	newOpenPRProvider = func(token string) localscheduler.OpenPRLister { gotToken = token; return fake }
	t.Cleanup(func() { newOpenPRProvider = prev })

	l := &resolvingOpenPRLister{ref: "acme/web", resolver: resolver, reg: reg}
	prs, err := l.ListOpenPullRequests(context.Background(), providers.RepositoryRef{Owner: "acme", Name: "web"})
	if err != nil {
		t.Fatalf("ListOpenPullRequests: %v", err)
	}
	if gotToken != "list-token-value" {
		t.Fatalf("provider built with token %q, want the resolved token", gotToken)
	}
	if len(prs) != 1 || prs[0].Head != "goobers/implementation/run-1" {
		t.Fatalf("prs = %v", prs)
	}
	var registered bool
	for _, s := range reg.registered {
		if string(s) == "list-token-value" {
			registered = true
		}
	}
	if !registered {
		t.Fatalf("resolved token not registered for scrubbing; registered=%v", reg.registered)
	}
}

// blockedHandlerFakeCommenter records every UpdateWorkItem call (unlike
// escFakeCommenter, which only keeps the last) — buildBlockedHandler's
// multi-item fallback path needs every call visible.
type blockedHandlerFakeCommenter struct {
	calls []providers.UpdateWorkItemRequest
}

func (f *blockedHandlerFakeCommenter) UpdateWorkItem(_ context.Context, req providers.UpdateWorkItemRequest) (providers.WorkItem, error) {
	f.calls = append(f.calls, req)
	return providers.WorkItem{}, nil
}

func blockedHandlerTestResolver(t *testing.T) credentials.Resolver {
	t.Helper()
	t.Setenv("BLOCKED_TOK", "blocked-token-value")
	resolver, err := credentials.NewResolver([]credentials.TokenRef{{Name: "acme/web", Env: "BLOCKED_TOK"}})
	if err != nil {
		t.Fatalf("NewResolver: %v", err)
	}
	return resolver
}

// TestBuildBlockedHandlerNilForRepoLessInstance mirrors
// TestBuildEscalationNotifier's repo-less case: no repo configured, no
// driving issue to comment on.
func TestBuildBlockedHandlerNilForRepoLessInstance(t *testing.T) {
	if h := buildBlockedHandler(instance.NewLayout(t.TempDir()), &instance.Config{}, nil, nil); h != nil {
		t.Fatalf("expected a nil handler for no repos, got %+v", h)
	}
}

// TestBuildBlockedHandlerKnownBlockersRecordsAndParks retains #552's learned
// dependency guard while applying #544's needs-human parking disposition.
func TestBuildBlockedHandlerKnownBlockersRecordsAndParks(t *testing.T) {
	fake := &blockedHandlerFakeCommenter{}
	var gotToken string
	prev := newEscalationPoster
	newEscalationPoster = func(token string) gate.Commenter {
		gotToken = token
		return fake
	}
	t.Cleanup(func() { newEscalationPoster = prev })

	l := instance.NewLayout(t.TempDir())
	if err := os.MkdirAll(l.SchedulerDir(), 0o755); err != nil {
		t.Fatalf("mkdir scheduler dir: %v", err)
	}
	t.Setenv("BLOCKED_SECONDARY_TOK", "blocked-secondary-token")
	cfg := &instance.Config{Repos: []instance.RepoRef{
		{Provider: "github", Owner: "acme", Name: "web", Token: instance.TokenRef{Env: "BLOCKED_TOK"}},
		{Provider: "github", Owner: "acme", Name: "api", Token: instance.TokenRef{Env: "BLOCKED_SECONDARY_TOK"}},
	}}
	t.Setenv("BLOCKED_TOK", "blocked-primary-token")
	resolver, err := credentials.NewResolver([]credentials.TokenRef{
		{Name: "acme/web", Env: "BLOCKED_TOK"},
		{Name: "acme/api", Env: "BLOCKED_SECONDARY_TOK"},
	})
	if err != nil {
		t.Fatalf("NewResolver: %v", err)
	}
	h := buildBlockedHandler(l, cfg, resolver, &escTestRegistrar{})
	if h == nil {
		t.Fatal("expected a non-nil handler for a repo-backed instance")
	}

	err = h(context.Background(), runner.BlockedOutcome{
		RunID:   "run-1",
		RepoRef: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "api", Branch: "main"},
		Stage:   "implement", ItemID: "510",
		Reason: "DEPENDENCY_NOT_MET: unmet prerequisite", Blockers: []string{"441", "442"},
	})
	if err != nil {
		t.Fatalf("handler: %v", err)
	}

	if len(fake.calls) != 1 {
		t.Fatalf("parking calls = %d, want 1", len(fake.calls))
	}
	got := fake.calls[0]
	if got.ID != "510" {
		t.Fatalf("request ID = %q, want 510", got.ID)
	}
	wantRepo := providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "acme", Name: "api"}
	if got.Repository != wantRepo {
		t.Fatalf("request repository = %+v, want secondary repository %+v", got.Repository, wantRepo)
	}
	if gotToken != "blocked-secondary-token" {
		t.Fatalf("parking token = %q, want secondary repository token", gotToken)
	}
	if len(got.AddLabels) != 1 || got.AddLabels[0] != providers.LabelNeedsHuman {
		t.Fatalf("AddLabels = %v, want [%s]", got.AddLabels, providers.LabelNeedsHuman)
	}
	wantRemoved := []string{providers.LabelReady, providers.LabelClaimed}
	if !slices.Equal(got.RemoveLabels, wantRemoved) {
		t.Fatalf("RemoveLabels = %v, want %v", got.RemoveLabels, wantRemoved)
	}
	if got.Comment != "" {
		t.Fatalf("comment = %q, want empty (the shared escalation notifier owns the comment)", got.Comment)
	}

	recs, err := loadBlockedRecords(blockedRecordsPath(l))
	if err != nil {
		t.Fatalf("loadBlockedRecords: %v", err)
	}
	rec, ok := recs["510"]
	if !ok {
		t.Fatal("expected a blocked.json record for item 510")
	}
	if len(rec.Blockers) != 2 || rec.RunID != "run-1" {
		t.Fatalf("record = %+v, want blockers [441 442] from run-1", rec)
	}
}

func TestBuildBlockedHandlerRecordFailureStillParks(t *testing.T) {
	fake := &blockedHandlerFakeCommenter{}
	prev := newEscalationPoster
	newEscalationPoster = func(string) gate.Commenter { return fake }
	t.Cleanup(func() { newEscalationPoster = prev })

	l := instance.NewLayout(t.TempDir())
	if err := os.WriteFile(l.SchedulerDir(), []byte("not a directory"), 0o644); err != nil {
		t.Fatalf("write scheduler blocker: %v", err)
	}
	cfg := &instance.Config{Repos: []instance.RepoRef{
		{Provider: "github", Owner: "acme", Name: "web", Token: instance.TokenRef{Env: "BLOCKED_TOK"}},
	}}
	h := buildBlockedHandler(l, cfg, blockedHandlerTestResolver(t), &escTestRegistrar{})

	err := h(context.Background(), runner.BlockedOutcome{
		RunID: "run-1", RepoRef: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web"},
		Stage: "implement", ItemID: "510",
		Reason: "DEPENDENCY_NOT_MET: unmet prerequisite", Blockers: []string{"441"},
	})
	if err == nil || !strings.Contains(err.Error(), "record block for 510") {
		t.Fatalf("handler error = %v, want blocked-record failure", err)
	}
	if len(fake.calls) != 1 {
		t.Fatalf("parking calls = %d, want 1 despite blocked-record failure", len(fake.calls))
	}
	got := fake.calls[0]
	wantRemoved := []string{providers.LabelReady, providers.LabelClaimed}
	if len(got.AddLabels) != 1 || got.AddLabels[0] != providers.LabelNeedsHuman ||
		!slices.Equal(got.RemoveLabels, wantRemoved) {
		t.Fatalf("parking request = %+v, want needs-human added and ready/claimed removed", got)
	}
}

// TestBuildBlockedHandlerNoBlockersParksNeedsHuman covers the unattributed
// path: no blocked.json record, but the same #539 parking disposition.
func TestBuildBlockedHandlerNoBlockersParksNeedsHuman(t *testing.T) {
	fake := &blockedHandlerFakeCommenter{}
	prev := newEscalationPoster
	newEscalationPoster = func(string) gate.Commenter { return fake }
	t.Cleanup(func() { newEscalationPoster = prev })

	l := instance.NewLayout(t.TempDir())
	cfg := &instance.Config{Repos: []instance.RepoRef{
		{Provider: "github", Owner: "acme", Name: "web", Token: instance.TokenRef{Env: "BLOCKED_TOK"}},
	}}
	h := buildBlockedHandler(l, cfg, blockedHandlerTestResolver(t), &escTestRegistrar{})

	err := h(context.Background(), runner.BlockedOutcome{
		RunID: "run-1", RepoRef: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web"},
		Stage: "implement", ItemID: "520",
		Reason: "waiting on an external dependency",
	})
	if err != nil {
		t.Fatalf("handler: %v", err)
	}

	if len(fake.calls) != 1 {
		t.Fatalf("parking calls = %d, want 1", len(fake.calls))
	}
	got := fake.calls[0]
	if got.ID != "520" {
		t.Fatalf("request ID = %q, want 520", got.ID)
	}
	if len(got.AddLabels) != 1 || got.AddLabels[0] != providers.LabelNeedsHuman {
		t.Fatalf("AddLabels = %v, want [%s]", got.AddLabels, providers.LabelNeedsHuman)
	}
	wantRemoved := []string{providers.LabelReady, providers.LabelClaimed}
	if !slices.Equal(got.RemoveLabels, wantRemoved) {
		t.Fatalf("RemoveLabels = %v, want %v", got.RemoveLabels, wantRemoved)
	}
	if got.Comment != "" {
		t.Fatalf("comment = %q, want empty (the shared escalation notifier owns the comment)", got.Comment)
	}

	recs, err := loadBlockedRecords(blockedRecordsPath(l))
	if err != nil {
		t.Fatalf("loadBlockedRecords: %v", err)
	}
	if len(recs) != 0 {
		t.Fatalf("blocked.json = %+v, want empty — nothing for #552 to skip/self-heal on an unattributed block", recs)
	}
}

// TestBuildBlockedHandlerResolvesItemFromClaimLedgerWhenEmpty proves a run
// started without StartInput.Item (scheduled/fan-out implementation runs
// claim their item mid-run) still notifies the right issue: the handler
// falls back to the claim ledger by RunID, since the run's claims are still
// held at the point the handler runs (before FinalizeTerminal releases them).
func TestBuildBlockedHandlerResolvesItemFromClaimLedgerWhenEmpty(t *testing.T) {
	fake := &blockedHandlerFakeCommenter{}
	prev := newEscalationPoster
	newEscalationPoster = func(string) gate.Commenter { return fake }
	t.Cleanup(func() { newEscalationPoster = prev })

	l := instance.NewLayout(t.TempDir())
	if err := os.MkdirAll(l.SchedulerDir(), 0o755); err != nil {
		t.Fatalf("mkdir scheduler dir: %v", err)
	}
	ledger, err := localscheduler.OpenClaimLedger(filepath.Join(l.SchedulerDir(), claimLedgerFileName))
	if err != nil {
		t.Fatalf("OpenClaimLedger: %v", err)
	}
	if ok, _, err := ledger.Claim("530", "run-fanout", "implementation", time.Hour); err != nil || !ok {
		t.Fatalf("seed claim: ok=%v err=%v", ok, err)
	}

	cfg := &instance.Config{Repos: []instance.RepoRef{
		{Provider: "github", Owner: "acme", Name: "web", Token: instance.TokenRef{Env: "BLOCKED_TOK"}},
	}}
	h := buildBlockedHandler(l, cfg, blockedHandlerTestResolver(t), &escTestRegistrar{})

	err = h(context.Background(), runner.BlockedOutcome{
		RunID: "run-fanout", RepoRef: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web"},
		Stage: "implement", Reason: "blocked", Blockers: []string{"441"},
	})
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	if len(fake.calls) != 1 || fake.calls[0].ID != "530" {
		t.Fatalf("calls = %+v, want exactly one call for item 530 (resolved via the claim ledger)", fake.calls)
	}
}

// TestPRClaimBlockedFlowNormalizesProviderID proves the claim ledger and
// blocked-record store retain the namespaced PR key while provider operations
// use GitHub's bare issue/PR number.
func TestPRClaimBlockedFlowNormalizesProviderID(t *testing.T) {
	fake := &blockedHandlerFakeCommenter{}
	prev := newEscalationPoster
	newEscalationPoster = func(string) gate.Commenter { return fake }
	t.Cleanup(func() { newEscalationPoster = prev })

	l := instance.NewLayout(t.TempDir())
	if err := os.MkdirAll(l.SchedulerDir(), 0o755); err != nil {
		t.Fatalf("mkdir scheduler dir: %v", err)
	}
	ledger, err := localscheduler.OpenClaimLedger(filepath.Join(l.SchedulerDir(), claimLedgerFileName))
	if err != nil {
		t.Fatalf("OpenClaimLedger: %v", err)
	}
	if ok, _, err := ledger.Claim("pr/955", "run-remediate", "pr-remediation", time.Hour); err != nil || !ok {
		t.Fatalf("seed PR claim: ok=%v err=%v", ok, err)
	}

	cfg := &instance.Config{Repos: []instance.RepoRef{
		{Provider: "github", Owner: "acme", Name: "web", Token: instance.TokenRef{Env: "BLOCKED_TOK"}},
	}}
	resolver := blockedHandlerTestResolver(t)
	reg := &escTestRegistrar{}
	h := buildBlockedHandler(l, cfg, resolver, reg)
	outcome := runner.BlockedOutcome{
		RunID: "run-remediate", RepoRef: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web"},
		Stage: "implement", Reason: "blocked on issue 956", Blockers: []string{"956"},
	}
	if err := h(context.Background(), outcome); err != nil {
		t.Fatalf("blocked handler: %v", err)
	}

	ids, err := claimedItemIDsForRun(l, "run-remediate")
	if err != nil {
		t.Fatalf("claimedItemIDsForRun: %v", err)
	}
	if !slices.Equal(ids, []string{"pr/955"}) {
		t.Fatalf("claimed item IDs = %v, want [pr/955]", ids)
	}
	notifier := buildEscalationNotifier(cfg, resolver, reg)
	repository := providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "acme", Name: "web"}
	if err := notifier.NotifyStageEscalated(context.Background(), repository, ids[0], outcome.Stage, outcome.Reason); err != nil {
		t.Fatalf("NotifyStageEscalated: %v", err)
	}

	if len(fake.calls) != 2 {
		t.Fatalf("provider calls = %+v, want parking and notification", fake.calls)
	}
	if fake.calls[0].ID != "955" || fake.calls[1].ID != "955" {
		t.Fatalf("provider IDs = [%q %q], want bare PR number 955", fake.calls[0].ID, fake.calls[1].ID)
	}
	if fake.calls[1].Comment == "" {
		t.Fatal("notification comment is empty")
	}
	recs, err := loadBlockedRecords(blockedRecordsPath(l))
	if err != nil {
		t.Fatalf("loadBlockedRecords: %v", err)
	}
	if _, ok := recs["pr/955"]; !ok {
		t.Fatalf("blocked records = %+v, want namespaced key pr/955", recs)
	}
	if _, ok := recs["955"]; ok {
		t.Fatalf("blocked records = %+v, bare provider ID must not replace the claim key", recs)
	}
}

// TestBuildBlockedHandlerNoClaimIsANoop proves a producer/schedule-triggered
// run (no Item, no claim to resolve) is a clean no-op — the journaled
// blocked_by_agent cause and escalated phase are the whole story; nothing to
// notify.
func TestBuildBlockedHandlerNoClaimIsANoop(t *testing.T) {
	fake := &blockedHandlerFakeCommenter{}
	prev := newEscalationPoster
	newEscalationPoster = func(string) gate.Commenter { return fake }
	t.Cleanup(func() { newEscalationPoster = prev })

	l := instance.NewLayout(t.TempDir())
	if err := os.MkdirAll(l.SchedulerDir(), 0o755); err != nil {
		t.Fatalf("mkdir scheduler dir: %v", err)
	}
	cfg := &instance.Config{Repos: []instance.RepoRef{
		{Provider: "github", Owner: "acme", Name: "web", Token: instance.TokenRef{Env: "BLOCKED_TOK"}},
	}}
	h := buildBlockedHandler(l, cfg, blockedHandlerTestResolver(t), &escTestRegistrar{})

	err := h(context.Background(), runner.BlockedOutcome{RunID: "run-producer", Stage: "curate", Reason: "blocked"})
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	if len(fake.calls) != 0 {
		t.Fatalf("calls = %+v, want none (no driving item anywhere)", fake.calls)
	}
}
