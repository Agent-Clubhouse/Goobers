package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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
	deterministic, err := buildCIPollExecutor(cfg, injector, ciPollTestFallback{})
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
	t.Run("wired with the target repo", func(t *testing.T) {
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
		if n.Repository.Provider != providers.ProviderGitHub || n.Repository.Owner != "acme" || n.Repository.Name != "web" {
			t.Fatalf("unexpected Repository: %+v", n.Repository)
		}
	})
}

// TestEscalationCommenterResolvesTokenPerCall is #312's rotation-safety +
// scrubbing property: the commenter resolves the org-repo token on each call
// (not captured at startup), registers it for scrubbing, and posts through a
// freshly-authenticated provider.
func TestEscalationCommenterResolvesTokenPerCall(t *testing.T) {
	t.Setenv("ESC_TOK", "escalation-token-value")
	resolver, err := credentials.NewResolver([]credentials.TokenRef{{Name: "acme/web", Env: "ESC_TOK"}})
	if err != nil {
		t.Fatalf("NewResolver: %v", err)
	}
	reg := &escTestRegistrar{}

	fake := &escFakeCommenter{}
	var gotToken string
	prev := newEscalationPoster
	newEscalationPoster = func(token string) gate.Commenter { gotToken = token; return fake }
	t.Cleanup(func() { newEscalationPoster = prev })

	c := &escalationCommenter{ref: "acme/web", resolver: resolver, reg: reg}
	if _, err := c.UpdateWorkItem(context.Background(), providers.UpdateWorkItemRequest{ID: "281", Comment: "escalated"}); err != nil {
		t.Fatalf("UpdateWorkItem: %v", err)
	}
	if gotToken != "escalation-token-value" {
		t.Fatalf("poster built with token %q, want the resolved token", gotToken)
	}
	if fake.gotReq.ID != "281" || fake.gotReq.Comment != "escalated" {
		t.Fatalf("posted request = %+v", fake.gotReq)
	}
	var registered bool
	for _, s := range reg.registered {
		if string(s) == "escalation-token-value" {
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

func (f *fakeHeadLister) ListOpenPullRequestHeads(context.Context, providers.RepositoryRef) ([]string, error) {
	return f.heads, nil
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
	heads, err := l.ListOpenPullRequestHeads(context.Background(), providers.RepositoryRef{Owner: "acme", Name: "web"})
	if err != nil {
		t.Fatalf("ListOpenPullRequestHeads: %v", err)
	}
	if gotToken != "list-token-value" {
		t.Fatalf("provider built with token %q, want the resolved token", gotToken)
	}
	if len(heads) != 1 || heads[0] != "goobers/implementation/run-1" {
		t.Fatalf("heads = %v", heads)
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

// TestBuildBlockedHandlerKnownBlockersRecordsAndComments is #552's recording
// half: a BlockedOutcome carrying parsed blockers records blocked.json (for
// backlog-query's skip/self-heal filter) and posts a comment — without
// touching goobers:ready/needs-human labels, since a dependency block is not
// a park-for-human disposition.
func TestBuildBlockedHandlerKnownBlockersRecordsAndComments(t *testing.T) {
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
	if h == nil {
		t.Fatal("expected a non-nil handler for a repo-backed instance")
	}

	err := h(context.Background(), runner.BlockedOutcome{
		RunID: "run-1", Stage: "implement", ItemID: "510",
		Reason: "DEPENDENCY_NOT_MET: unmet prerequisite", Blockers: []string{"441", "442"},
	})
	if err != nil {
		t.Fatalf("handler: %v", err)
	}

	if len(fake.calls) != 1 {
		t.Fatalf("comment calls = %d, want 1", len(fake.calls))
	}
	got := fake.calls[0]
	if got.ID != "510" || len(got.AddLabels) != 0 || len(got.RemoveLabels) != 0 {
		t.Fatalf("request = %+v, want ID 510 with no label changes (a dependency block self-heals, it doesn't park)", got)
	}
	if !strings.Contains(got.Comment, "#441") || !strings.Contains(got.Comment, "#442") {
		t.Fatalf("comment = %q, want both blocker issue numbers", got.Comment)
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

// TestBuildBlockedHandlerNoBlockersParksNeedsHuman is #544's park-on-fail-
// attribution path: a blocked result with no parseable blockers gets no
// blocked.json record (there's nothing for #552 to skip/self-heal on) — the
// issue is parked goobers:needs-human instead, per the #539 convention.
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
		RunID: "run-1", Stage: "implement", ItemID: "520",
		Reason: "waiting on an external dependency",
	})
	if err != nil {
		t.Fatalf("handler: %v", err)
	}

	if len(fake.calls) != 1 {
		t.Fatalf("comment calls = %d, want 1", len(fake.calls))
	}
	got := fake.calls[0]
	if got.ID != "520" {
		t.Fatalf("request ID = %q, want 520", got.ID)
	}
	if len(got.AddLabels) != 1 || got.AddLabels[0] != providers.LabelNeedsHuman {
		t.Fatalf("AddLabels = %v, want [%s]", got.AddLabels, providers.LabelNeedsHuman)
	}
	if len(got.RemoveLabels) != 1 || got.RemoveLabels[0] != providers.LabelReady {
		t.Fatalf("RemoveLabels = %v, want [%s]", got.RemoveLabels, providers.LabelReady)
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
		RunID: "run-fanout", Stage: "implement", Reason: "blocked", Blockers: []string{"441"},
	})
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	if len(fake.calls) != 1 || fake.calls[0].ID != "530" {
		t.Fatalf("calls = %+v, want exactly one call for item 530 (resolved via the claim ledger)", fake.calls)
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
