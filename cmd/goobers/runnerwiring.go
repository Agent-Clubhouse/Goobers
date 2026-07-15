package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/capability"
	"github.com/goobers/goobers/internal/credentials"
	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/internal/gate"
	"github.com/goobers/goobers/internal/harness"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/runner"
	"github.com/goobers/goobers/internal/telemetry"
	"github.com/goobers/goobers/internal/telemetry/rollup"
	"github.com/goobers/goobers/internal/version"
	"github.com/goobers/goobers/internal/workflow"
	"github.com/goobers/goobers/internal/worktree"
	"github.com/goobers/goobers/providers"
)

// buildTelemetryClient constructs the OTel client that spans the runner walk
// (run/task/gate) and scheduler decisions, writing completed spans under
// RunsDir via JournalSpanExporter (issue #126) — the same run journal
// layout goobers trace/telemetry read back through the rollup. Shared by
// up.go/run.go exactly like buildRunnerConfig; each caller owns calling
// Shutdown on the returned client once it's done driving runs.
func buildTelemetryClient(ctx context.Context, l instance.Layout, scrubber journal.Scrubber) (*telemetry.Client, error) {
	return telemetry.New(ctx, telemetry.Config{
		ServiceName:    "goobers",
		ServiceVersion: version.Get().Version,
		SpanExporter:   telemetry.NewJournalSpanExporter(l.RunsDir(), scrubber),
		Batch:          true,
	})
}

// teeRegistrar forwards every registered secret to BOTH a run's own
// SecretRegistrar (feeding that run's journal scrubber) and the instance-global
// shared registry (feeding the span exporter + instance log). It is how a
// per-run secret reaches the two instance-lifetime consumers without changing
// internal/runner's per-run registrar creation (#117 Piece B).
type teeRegistrar struct {
	run    runner.SecretRegistrar
	shared *journal.RegistryScrubber
}

func (t teeRegistrar) Register(secret []byte) {
	t.run.Register(secret)
	t.shared.Register(secret)
}

// ingestRunTelemetry incrementally ingests one finished run, plus a refresh
// of the scheduler decision log, into the local telemetry rollup (issues
// #127/#128) — internal/telemetry/rollup/ingest.go's own doc comment already
// claimed IngestRun is meant to hook a run's completion ("call it once a run
// finishes"), but nothing in cmd/goobers ever called it; every `goobers
// telemetry`/`trace` query instead paid for a full rollup.Rebuild (an
// os.Remove + full rescan) just to stay correct, and scheduler/events.jsonl
// (trigger.fired/tick.skipped/claim.*) was never ingested at all. Called from
// both up.go (every scheduler-dispatched and resumed run) and run.go (the
// one-shot manual run — its scheduler log ingest is a no-op there, since
// `goobers run` never dispatches through the scheduler), regardless of the
// run's own error, so a failed run's errors/stage_attempts still show up in
// `goobers telemetry errors`. Best-effort: the rollup is derived state, never
// the source of truth, so an ingest failure here must never fail the run
// itself.
//
// tel.Flush MUST run before IngestRun reads spans.jsonl: buildTelemetryClient
// sets Batch: true, so completed spans sit in the OTel SDK's in-memory batch
// processor and are only written to disk on the processor's own interval or
// an explicit flush/shutdown — not synchronously when a span ends. Without
// this, IngestRun would race that flush and snapshot a run's spans as empty
// into telemetry.db, with nothing to re-ingest them later even after
// spans.jsonl itself eventually gets written (issue #129's checklist: this
// is exactly the gap that made `goobers trace` depend on a prior
// `goobers telemetry` call, which itself flushed via a DIFFERENT process's
// tel.Shutdown before this fix existed).
func ingestRunTelemetry(tel *telemetry.Client, db *rollup.DB, l instance.Layout, runID string, log *journal.InstanceLog) {
	if tel != nil {
		_ = tel.Flush(context.Background())
	}
	if db == nil {
		return
	}
	// Best-effort (the rollup is derived state, never the source of truth,
	// so a failure here must never fail the run) does NOT mean silent
	// (issue #246): a swallowed error here — e.g. the harness_transcripts PK
	// conflict on re-ingesting a resumed run — left the rollup silently
	// stale with nothing but a blank `_ =` to show for it. logIngestFailure
	// records it to the instance log, matching resumeInterruptedRuns' own
	// resume_unresolvable_workflow convention, without changing the
	// swallow-and-continue control flow.
	if err := db.IngestRun(filepath.Join(l.RunsDir(), runID)); err != nil {
		logIngestFailure(log, runID, "telemetry_ingest_run_failed", err)
	}
	if err := db.IngestSchedulerLog(l.SchedulerDir()); err != nil {
		logIngestFailure(log, runID, "telemetry_ingest_scheduler_log_failed", err)
	}
}

// logIngestFailure appends a best-effort diagnostic event for a failed
// rollup ingest (issue #246) — nil-safe (log may be nil in a test/standalone
// context) and itself swallows its own Append error, since a logging
// failure must not cascade into a second failure mode.
func logIngestFailure(log *journal.InstanceLog, runID, code string, cause error) {
	if log == nil {
		return
	}
	_ = log.Append(journal.Event{
		Type: journal.EventError, RunID: runID,
		Error: &journal.ErrorDetail{Code: code, Message: cause.Error()},
	})
}

// repoCloneURL overrides runner.Config.RepoCloneURL when non-nil. It exists
// purely as a test seam (mirrors internal/localscheduler's swappable newRunID)
// so integration tests can point worktree provisioning at a local git fixture
// instead of a real GitHub clone; production leaves it nil and runner.New
// falls back to its own github.com default.
var repoCloneURL func(apiv1.RepoRef) (string, error)

// newAgenticAdapter overrides how buildRunnerConfig constructs the harness
// adapter for an agentic stage when non-nil. It is a test seam (mirroring
// repoCloneURL above) so the CLI-level acceptance check (acceptance_test.go)
// can substitute a fake for the real Copilot CLI subprocess and drive the full
// agentic loop — implement -> reviewer gate -> local-ci — through `goobers
// run`/`up` offline, extending #29's runner-API-level walking skeleton to the
// CLI entrypoint. Production leaves it nil and buildRunnerConfig uses the real
// CopilotAdapter.
var newAgenticAdapter func(gooberName string, envCaps map[string]string) harness.Adapter

// newPRPoller overrides how buildRunnerConfig constructs the ci-poll stage's
// PRPoller when non-nil. Test seam mirroring repoCloneURL/newAgenticAdapter
// above, so a CLI-level test can point ci-poll at a fake PR provider (an
// httptest.Server, or a bespoke fake) instead of a real GitHub token/network
// (#132). Production leaves it nil and buildRunnerConfig constructs a real
// providers.GitHubProvider over the resolved repo token.
var newPRPoller func(token string) executor.PRPoller

// credentialGrantEnv is the environment variable the Copilot CLI reads a
// credentialed capability's token from (internal/harness.CopilotAdapter's
// EnvCapabilities convention — matches internal/harness/copilot_test.go's
// {"repo:push": "GH_TOKEN"} fixture).
const credentialGrantEnv = "GH_TOKEN"

// copilotModelEnv is the environment variable the Copilot CLI reads its
// model-backend token from. The CLI prefers COPILOT_GITHUB_TOKEN over GH_TOKEN
// for model auth (§3.3), so mapping agent:model to a DISTINCT env var from
// credentialGrantEnv lets one agentic subprocess carry a personal "Copilot
// Requests" PAT for the model (agent:model → COPILOT_GITHUB_TOKEN) AND the
// org-repo token for the github tool (credentialedCapabilities → GH_TOKEN) at
// once — credentialEnv appends both, and because the vars differ neither
// clobbers the other (#288, multi-token credentials 2/3).
const copilotModelEnv = "COPILOT_GITHUB_TOKEN"

// credentialedCapabilities are the canonical capabilities (internal/capability,
// issue #74) a repo's token can satisfy; telemetry:read needs no credential.
var credentialedCapabilities = []capability.Capability{
	capability.RepoPush, capability.GitHubIssuesWrite, capability.GitHubPRWrite,
}

// buildEnvCapabilities maps each capability the Copilot adapter injects to the
// environment variable the CLI reads its token from: the org-repo capabilities
// to GH_TOKEN (the github tool's var) and agent:model to COPILOT_GITHUB_TOKEN
// (the model backend's var, #288, §3.3). The two vars are distinct on purpose —
// credentialEnv appends one entry per declared capability, so a stage declaring
// both agent:model and an org-repo capability carries both tokens in a single
// subprocess with neither clobbering the other.
func buildEnvCapabilities() map[string]string {
	envCaps := make(map[string]string, len(credentialedCapabilities)+1)
	for _, c := range credentialedCapabilities {
		envCaps[string(c)] = credentialGrantEnv
	}
	envCaps[string(capability.AgentModel)] = copilotModelEnv
	return envCaps
}

// buildCredentials builds a Resolver and the capability->ref Grants from
// instance.yaml. By default the first configured repo's token backs every
// credentialed capability (V0 single-target-repo simplification, ARCHITECTURE.md
// §6). instance.yaml's credentials: block then sources individual capabilities
// from their own token refs (#287): a new capability (e.g. agent:model) gains a
// grant, and one the repo token already backs is overridden — so an agentic
// stage can hold a personal Copilot-Requests PAT for the model alongside the
// org-repo token for the github tool, both fail-closed per capability admission.
func buildCredentials(cfg *instance.Config) (*credentials.Resolver, []credentials.Grant, error) {
	refs := make([]credentials.TokenRef, 0, len(cfg.Repos)+len(cfg.Credentials))
	for _, r := range cfg.Repos {
		refs = append(refs, credentials.TokenRef{
			Name: r.Owner + "/" + r.Name,
			Env:  r.Token.Env,
			File: r.Token.File,
		})
	}
	// Per-capability credential refs (#287): each sources one capability from
	// its own token, named distinctly so it never collides with a repo ref.
	for _, cg := range cfg.Credentials {
		refs = append(refs, credentials.TokenRef{
			Name: credentialRefName(cg.Capability),
			Env:  cg.Token.Env,
			File: cg.Token.File,
		})
	}
	resolver, err := credentials.NewResolver(refs)
	if err != nil {
		return nil, nil, fmt.Errorf("build credential resolver: %w", err)
	}

	// Default: the first repo's token backs every credentialed capability.
	grantRef := make(map[string]string, len(credentialedCapabilities)+len(cfg.Credentials))
	var order []string // deterministic grant order: defaults first, then extras
	if len(cfg.Repos) > 0 {
		repoRef := cfg.Repos[0].Owner + "/" + cfg.Repos[0].Name
		for _, c := range credentialedCapabilities {
			grantRef[string(c)] = repoRef
			order = append(order, string(c))
		}
	}
	// instance.yaml credentials: entries source a capability from its own token
	// (#287) — a new capability (e.g. agent:model) is added, and a capability
	// the repo token already backs is OVERRIDDEN to point at its distinct ref.
	for _, cg := range cfg.Credentials {
		if _, exists := grantRef[cg.Capability]; !exists {
			order = append(order, cg.Capability)
		}
		grantRef[cg.Capability] = credentialRefName(cg.Capability)
	}

	grants := make([]credentials.Grant, 0, len(order))
	for _, cap := range order {
		grants = append(grants, credentials.Grant{Capability: cap, Ref: grantRef[cap]})
	}
	return resolver, grants, nil
}

// credentialRefName is the resolver ref name for a per-capability credentials:
// entry (#287) — namespaced so it can never collide with a repo ref (owner/name).
func credentialRefName(cap string) string { return "credential:" + cap }

// buildCIPollExecutor constructs the ci-poll stage's CIPollExecutor over a
// PRPoller for the instance's configured (single, V0-simplification) target
// repo. It returns a nil executor, not an error, when no repo is configured
// or its token can't be resolved: NewDeterministic constructs a run's WHOLE
// deterministic-executor stack lazily on the first deterministic task
// dispatched (internal/runner's executors.deterministic()) — a workflow whose
// first deterministic stage is a plain shell command with no PR involvement
// at all (e.g. `make ci`) must not fail just because ci-poll's own
// credential happens to be unresolvable; only a stage that actually declares
// kind=ci-poll should fail, and TaskExecutor already fails closed on that
// when CIPoll is nil (executor/dispatch.go's CIPoll doc). The resolved token,
// when there is one, is registered with reg so it's scrubbed from anything a
// later stage in this run writes, exactly like credentials.Injector's own
// resolution path (executor/env.go's buildStageEnv).
func buildCIPollExecutor(cfg *instance.Config, resolver *credentials.Resolver, reg runner.SecretRegistrar) (*executor.CIPollExecutor, error) {
	if len(cfg.Repos) == 0 {
		return nil, nil
	}
	ref := cfg.Repos[0].Owner + "/" + cfg.Repos[0].Name
	token, err := resolver.Resolve(context.Background(), ref)
	if err != nil {
		return nil, nil
	}
	reg.Register([]byte(token))
	var poller executor.PRPoller
	if newPRPoller != nil {
		poller = newPRPoller(token)
	} else {
		poller = providers.NewGitHubProvider(token)
	}
	return executor.NewCIPollExecutor(poller)
}

// newEscalationPoster constructs the provider the escalation notifier posts
// through — a package var so tests substitute a fake without a real GitHub
// client (mirrors newPRPoller).
var newEscalationPoster = func(token string) gate.Commenter { return providers.NewGitHubProvider(token) }

// escalationCommenter is the gate.Commenter the runner posts escalation
// comments through (#312). Like buildCIPollExecutor it resolves the org-repo
// token per call — honoring credentials.Resolver's re-read-on-resolve rotation
// contract rather than capturing a token once at daemon startup — registers it
// for scrubbing, then posts through a freshly-authenticated provider.
type escalationCommenter struct {
	ref      string
	resolver *credentials.Resolver
	reg      runner.SecretRegistrar
}

func (c *escalationCommenter) UpdateWorkItem(ctx context.Context, req providers.UpdateWorkItemRequest) (providers.WorkItem, error) {
	token, err := c.resolver.Resolve(ctx, c.ref)
	if err != nil {
		return providers.WorkItem{}, fmt.Errorf("resolve escalation-comment token for %s: %w", c.ref, err)
	}
	c.reg.Register([]byte(token))
	return newEscalationPoster(token).UpdateWorkItem(ctx, req)
}

// buildEscalationNotifier wires the gate.EscalationNotifier (#20) at the
// composition root — a complete, tested implementation that was never
// constructed, so runner.Config.Escalation stayed nil and a repass-budget
// escalation posted nothing to the driving issue (#312, the same "real seam,
// zero production callers" shape as epic #130). Returns nil when no repo is
// configured — a schedule-only producer instance has no driving issue to
// comment on, and NotifyEscalated only fires when in.Item is non-nil anyway.
// Comment-only by deliberate design: the Commenter/UpdateWorkItem seam was
// chosen specifically so escalation never touches the item's status label
// (#63); #20's escalation surfacing is a provider comment on the driving issue,
// not a label change (the goobers:needs-human marker is the curator's output,
// a distinct flow).
func buildEscalationNotifier(cfg *instance.Config, resolver *credentials.Resolver, reg runner.SecretRegistrar) *gate.EscalationNotifier {
	if len(cfg.Repos) == 0 {
		return nil
	}
	repo := cfg.Repos[0]
	return &gate.EscalationNotifier{
		Poster: &escalationCommenter{
			ref:      repo.Owner + "/" + repo.Name,
			resolver: resolver,
			reg:      reg,
		},
		Repository: providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: repo.Owner, Name: repo.Name},
	}
}

// instructionsPath resolves a goober's Instructions field to an absolute
// file path. Instructions is documented as "relative to the goober
// definition directory" (api/v1alpha1.GooberSpec), which config-as-code
// objects don't retain after instance.LoadConfigDir flattens them into a
// ConfigSet — but every shipped config (internal/instance/starter,
// config-examples/, selfhost/) lays goobers out at the same fixed path, so
// that layout convention is reproduced here rather than widening ConfigSet's
// shape for this one field.
func instructionsPath(configDir string, spec apiv1.GooberSpec, gooberName string) string {
	return filepath.Join(configDir, "gaggles", spec.Gaggle, "goobers", gooberName, spec.Instructions)
}

// buildRunnerConfig assembles the runner.Config the daemon (`goobers up`) and
// `goobers run` share: real worktrees, the real Copilot harness adapter and
// shell executor, credentials scoped to instance.yaml's configured repo(s).
// One Config serves every workflow/run — runner.Runner is not bound to a
// single compiled machine. Also returns the *worktree.Manager directly (not
// just embedded in the Config) so the daemon can call Reap on the exact same
// Manager instance the runner itself dispatches through (issue #136) —
// constructing a second, separate Manager over the same root would give Reap
// its own independent repoLocks map, defeating the per-repo git-operation
// serialization both share Root for in the first place.
//
// tel may be nil (instance.yaml's telemetry.enabled: false, issue #129) —
// deliberately NOT assigned to the returned Config.Telemetry field in that
// case. runner.Config.Telemetry is the SpanStarter INTERFACE; a nil
// *telemetry.Client assigned to it would produce a non-nil interface value
// wrapping a nil pointer, so the runner's own `r.cfg.Telemetry == nil` guard
// would incorrectly evaluate false and panic on first use — Go's classic
// typed-nil-in-interface trap. Leaving the field unset keeps the interface
// itself nil.
func buildRunnerConfig(l instance.Layout, cfg *instance.Config, goobers map[string]apiv1.GooberSpec, tel *telemetry.Client, sharedReg *journal.RegistryScrubber) (runner.Config, *worktree.Manager, error) {
	wtMgr, err := worktree.NewManager(l.WorkcopiesDir())
	if err != nil {
		return runner.Config{}, nil, fmt.Errorf("new worktree manager: %w", err)
	}
	resolver, grants, err := buildCredentials(cfg)
	if err != nil {
		return runner.Config{}, nil, err
	}
	instanceRoot, err := filepath.Abs(l.Root)
	if err != nil {
		return runner.Config{}, nil, fmt.Errorf("resolve instance root: %w", err)
	}
	// The running daemon's own binary path, substituted for a bare "goobers"
	// command token in deterministic stages — a fresh stage worktree never
	// contains the goobers binary, so a bare name fails at exec (#229). Fail
	// closed here rather than let every deterministic stage fail at exec time.
	selfBin, err := os.Executable()
	if err != nil {
		return runner.Config{}, nil, fmt.Errorf("resolve goobers binary path: %w", err)
	}

	envCaps := buildEnvCapabilities()

	// An agentic gate's reviewer has no stage-level capabilities of its own, so
	// the runner sources them from the reviewer goober's definition (#294). Map
	// each goober to its declared grants for that lookup; only agentic gates
	// consult it (task stages carry their own stage-level capabilities).
	gateGooberCaps := make(map[string][]string, len(goobers))
	for name, spec := range goobers {
		if len(spec.Capabilities) > 0 {
			gateGooberCaps[name] = append([]string(nil), spec.Capabilities...)
		}
	}

	rc := runner.Config{
		NewDeterministic: func(rec runner.ArtifactRecorder, reg runner.SecretRegistrar) (invoke.Deterministic, error) {
			// Register resolved secrets into the run's own registrar AND the
			// instance-global shared registry, so they are scrubbed from the run
			// journal (via reg) and from the span exporter / instance log (via
			// sharedReg) alike (#117 Piece B).
			reg = teeRegistrar{run: reg, shared: sharedReg}
			injector, err := credentials.NewInjector(resolver, grants, reg)
			if err != nil {
				return nil, err
			}
			shell, err := executor.NewShellExecutor(injector, rec)
			if err != nil {
				return nil, err
			}
			// GOOBERS_INSTANCE_ROOT — the only way a `goobers` CLI subcommand
			// invoked as a stage's shell command (its cwd is the stage's
			// worktree, not the instance root) locates instance.yaml/config/
			// scheduler (#131/#132's backlog-query/open-pr/issue-close-out).
			shell.InstanceRoot = instanceRoot
			// Resolve a bare "goobers" command token to the running daemon's own
			// binary, so a deterministic stage execs it from its fresh worktree
			// clone (which never contains the binary) rather than failing (#229).
			shell.SelfBin = selfBin

			ciPoll, err := buildCIPollExecutor(cfg, resolver, reg)
			if err != nil {
				return nil, err
			}
			return executor.NewTaskExecutor(shell, ciPoll)
		},
		NewAgentic: func(gooberName string, rec runner.ArtifactRecorder, reg runner.SecretRegistrar) (invoke.Goober, error) {
			spec, ok := goobers[gooberName]
			if !ok {
				return nil, fmt.Errorf("goober %q not found in config", gooberName)
			}
			// The injector registers resolved secrets into the run's registrar AND
			// the shared instance registry (#117 Piece B). reg (not the tee) is
			// kept below for the journal.Scrubber assertion — it still accumulates
			// every secret, since the tee forwards to it.
			injector, err := credentials.NewInjector(resolver, grants, teeRegistrar{run: reg, shared: sharedReg})
			if err != nil {
				return nil, err
			}
			instructions, err := os.ReadFile(instructionsPath(l.ConfigDir(), spec, gooberName))
			if err != nil {
				return nil, fmt.Errorf("read goober %q instructions: %w", gooberName, err)
			}
			var adapter harness.Adapter = &harness.CopilotAdapter{Command: []string{"copilot"}, EnvCapabilities: envCaps}
			if newAgenticAdapter != nil {
				adapter = newAgenticAdapter(gooberName, envCaps)
			}
			recorder, ok := rec.(harness.SpanRecorder)
			if !ok {
				return nil, fmt.Errorf("runner artifact recorder does not implement harness.SpanRecorder")
			}
			artifacts, ok := rec.(harness.ArtifactRecorder)
			if !ok {
				return nil, fmt.Errorf("runner artifact recorder does not implement harness.ArtifactRecorder")
			}
			// harness.NewContextResolver pairs rec's own Dir() (same-run
			// resolution, #121) with the instance's RunsDir (cross-run
			// resolution, #103/T3) — rec (a *journal.Run) has no notion of
			// sibling runs on its own, only l (the instance layout) does.
			direr, ok := rec.(interface{ Dir() string })
			if !ok {
				return nil, fmt.Errorf("runner artifact recorder does not implement Dir() string")
			}
			contextResolver := harness.NewContextResolver(direr, l.RunsDir())
			registryScrubber, ok := reg.(journal.Scrubber)
			if !ok {
				return nil, fmt.Errorf("runner secret registrar does not implement journal.Scrubber")
			}
			scrubber := journal.Chain(registryScrubber, journal.NewPatternScrubber())
			return harness.NewExecutor(adapter, injector, recorder, artifacts, contextResolver, scrubber, string(instructions))
		},
		Automated:              gate.NewAutomatedEvaluator(),
		Worktrees:              wtMgr,
		RunsDir:                l.RunsDir(),
		RepoCloneURL:           repoCloneURL,
		GateGooberCapabilities: gateGooberCaps,
		// Wire the escalation notifier (#312) so a repass-budget escalation
		// actually comments on the driving issue; nil for a repo-less instance.
		Escalation: buildEscalationNotifier(cfg, resolver, sharedReg),
	}
	if tel != nil {
		rc.Telemetry = tel
	}
	return rc, wtMgr, nil
}

// goobersByName indexes set's Goobers by name for workflow.WithGoobers
// admission and NewAgentic's instructions/harness lookup.
func goobersByName(set *instance.ConfigSet) map[string]apiv1.GooberSpec {
	out := make(map[string]apiv1.GooberSpec, len(set.Goobers))
	for _, g := range set.Goobers {
		out[g.Name] = g.Spec
	}
	return out
}

// knownAutomatedCheckNames returns the automated check names actually
// registered (internal/gate.DefaultChecks()'s keys) for
// workflow.WithKnownChecks — every real automated gate resolves its Check
// against this exact registry (internal/gate.AutomatedEvaluator.Evaluate), so
// a typo here is caught at compile time instead of failing only when a run
// actually reaches that gate (#124).
func knownAutomatedCheckNames() []string {
	checks := gate.DefaultChecks()
	names := make([]string, 0, len(checks))
	for name := range checks {
		names = append(names, name)
	}
	return names
}

// compiledMachines compiles every workflow in set, admission-checked against
// goobers (capabilities, harness, gate-outcome coverage, and known automated
// check names — #124), keyed by workflow name. WorkflowVersion is registry-
// assigned (per-name monotonic, WF-016); no registry is wired at the instance
// level yet, so this pins version 1 for every workflow, matching run.go's
// existing limitation until a follow-up introduces one.
func compiledMachines(set *instance.ConfigSet, goobers map[string]apiv1.GooberSpec) (map[string]*workflow.Machine, error) {
	const workflowVersion = 1
	knownChecks := knownAutomatedCheckNames()
	machines := make(map[string]*workflow.Machine, len(set.Workflows))
	for i := range set.Workflows {
		wf := &set.Workflows[i]
		m, err := workflow.Compile(
			workflow.Definition{Name: wf.Name, Version: workflowVersion, Spec: wf.Spec},
			workflow.WithGoobers(goobers), workflow.WithKnownChecks(knownChecks),
		)
		if err != nil {
			return nil, fmt.Errorf("compile workflow %q: %w", wf.Name, err)
		}
		machines[wf.Name] = m
	}
	return machines, nil
}

// repoRefsByWorkflow resolves each workflow's RepoRef via its Gaggle's
// declared project (apiv1.GaggleSpec.Project) — a workflow only names its
// gaggle, not a repo directly.
func repoRefsByWorkflow(set *instance.ConfigSet) (map[string]apiv1.RepoRef, error) {
	gagglesByName := make(map[string]apiv1.Gaggle, len(set.Gaggles))
	for _, g := range set.Gaggles {
		gagglesByName[g.Name] = g
	}
	refs := make(map[string]apiv1.RepoRef, len(set.Workflows))
	for i := range set.Workflows {
		wf := &set.Workflows[i]
		g, ok := gagglesByName[wf.Spec.Gaggle]
		if !ok {
			return nil, fmt.Errorf("workflow %q references unknown gaggle %q", wf.Name, wf.Spec.Gaggle)
		}
		refs[wf.Name] = g.Spec.Project
	}
	return refs, nil
}
