package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/capability"
	"github.com/goobers/goobers/internal/credentials"
	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/internal/gate"
	"github.com/goobers/goobers/internal/gooberassets"
	"github.com/goobers/goobers/internal/harness"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/runner"
	"github.com/goobers/goobers/internal/telemetry"
	"github.com/goobers/goobers/internal/telemetry/rollup"
	"github.com/goobers/goobers/internal/version"
	"github.com/goobers/goobers/internal/workflow"
	"github.com/goobers/goobers/internal/worktree"
	"github.com/goobers/goobers/providers"
)

// buildTelemetryClient constructs the OTel client that spans the runner walk
// (run/task/gate) and scheduler decisions. Run spans land under RunsDir;
// scheduler spans land in the instance scheduler journal. Both are projected
// into the rollup. Shared by up.go/run.go exactly like buildRunnerConfig; each
// caller owns calling Shutdown on the returned client once it is done driving
// runs.
func buildTelemetryClient(ctx context.Context, l instance.Layout, scrubber journal.Scrubber) (*telemetry.Client, error) {
	return telemetry.New(ctx, telemetry.Config{
		ServiceName:    "goobers",
		ServiceVersion: version.Get().Version,
		SpanExporter:   telemetry.NewPerGaggleJournalSpanExporter(l.Root, scrubber),
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
// of the scheduler decision log and spans, into the local telemetry rollup (issues
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

// newAgenticAdapter overrides the adapter selected from the harness Registry
// for an agentic stage when non-nil. It is a test seam (mirroring
// repoCloneURL above) so the CLI-level acceptance check (acceptance_test.go)
// can substitute a fake for the real Copilot CLI subprocess and drive the full
// agentic loop — implement -> reviewer gate -> local-ci — through `goobers
// run`/`up` offline, extending #29's runner-API-level walking skeleton to the
// CLI entrypoint. Production leaves it nil.
var newAgenticAdapter func(gooberName string, envCaps map[string]string) harness.Adapter

// newPRPoller overrides how buildRunnerConfig constructs the ci-poll stage's
// PRPoller when non-nil. Test seam mirroring repoCloneURL/newAgenticAdapter
// above, so a CLI-level test can point ci-poll at a fake PR provider (an
// httptest.Server, or a bespoke fake) instead of a real GitHub token/network
// (#132). Production leaves it nil and buildRunnerConfig constructs a real
// providers.GitHubProvider over the resolved repo token.
var newPRPoller func(token string) executor.PRPoller

// credentialGrantEnv is the environment variable the Copilot CLI reads most
// credentialed capabilities' tokens from (internal/harness.CopilotAdapter's
// EnvCapabilities convention — matches internal/harness/copilot_test.go's
// {"repo:push": "GH_TOKEN"} fixture).
const credentialGrantEnv = "GH_TOKEN"

// copilotModelEnv is the environment variable the Copilot CLI reads its
// model-backend token from. The CLI prefers COPILOT_GITHUB_TOKEN over GH_TOKEN
// for model auth (§3.3), so mapping agent:model to a DISTINCT env var from
// credentialGrantEnv lets one agentic subprocess carry a personal "Copilot
// Requests" PAT for the model (agent:model → COPILOT_GITHUB_TOKEN) AND the
// org-repo token for the github tool (ordinary repo capabilities → GH_TOKEN)
// at once — credentialEnv appends both, and because the vars differ neither
// clobbers the other (#288, multi-token credentials 2/3).
const copilotModelEnv = "COPILOT_GITHUB_TOKEN"

// credentialedCapabilities are the canonical capabilities (internal/capability,
// issue #74) a repo's token can satisfy; telemetry:read needs no credential.
var credentialedCapabilities = []capability.Capability{
	capability.RepoPush, capability.GitHubIssuesWrite, capability.GitHubIssuesApprove, capability.GitHubPRWrite, capability.GitHubPRReview, capability.GitHubBranchDelete, capability.GitHubPRMerge,
}

// buildEnvCapabilities maps each capability the Copilot adapter injects to the
// environment variable that consumes its token. General org-repo capabilities
// use GH_TOKEN (the github tool's var), github:issues:approve uses its dedicated
// GOOBERS_CRED_* variable so the nominator can detect and exercise that opt-in
// separately, and agent:model uses COPILOT_GITHUB_TOKEN (the model backend's
// var, #288, §3.3).
func buildEnvCapabilities() map[string]string {
	envCaps := make(map[string]string, len(credentialedCapabilities)+1)
	for _, c := range credentialedCapabilities {
		envCaps[string(c)] = credentialGrantEnv
	}
	envCaps[string(capability.GitHubIssuesApprove)] = executor.CredentialEnvVar(string(capability.GitHubIssuesApprove))
	envCaps[string(capability.AgentModel)] = copilotModelEnv
	return envCaps
}

// buildHarnessRegistry is the production harness composition point. Registry
// keys are goober spec.harness values; adapter names remain their diagnostic
// identities, so Copilot continues to report "copilot-cli" in spans and errors.
func buildHarnessRegistry(envCaps map[string]string) (*harness.Registry, error) {
	registry := harness.NewRegistry()
	adapter := &harness.CopilotAdapter{
		Command:         []string{"copilot"},
		AuthCheckArgs:   copilotAuthCheckArgs,
		EnvCapabilities: envCaps,
	}
	if err := registry.RegisterAs(string(apiv1.HarnessCopilot), adapter); err != nil {
		return nil, fmt.Errorf("register Copilot harness: %w", err)
	}
	return registry, nil
}

// buildCredentials is the composition root for the secret-resolver seam. It
// selects the local env/file implementation; a tier-3 deployment substitutes
// its SEC-010 Key Vault Resolver here while all downstream wiring stays
// unchanged. By default the first configured repo's token backs every
// credentialed capability (V0 single-target-repo simplification, ARCHITECTURE.md
// §6). instance.yaml's credentials: block then sources individual capabilities
// from their own token refs (#287): a new capability (e.g. agent:model) gains a
// grant, and one the repo token already backs is overridden — so an agentic
// stage can hold a personal Copilot-Requests PAT for the model alongside the
// org-repo token for the github tool, both fail-closed per capability admission.
// The returned Grants are runner-owned (empty Goober); buildGooberCredentialGrants
// binds these sources to each goober's own declared capabilities before an
// agentic injector can use them.
func buildCredentials(cfg *instance.Config) (credentials.Resolver, []credentials.Grant, error) {
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

// buildGooberCredentialGrants binds the configured credential sources to one
// goober's definition-level capability grants. The resulting grants carry the
// goober identity, so a forged stage envelope cannot make this injector reach a
// capability granted only to another goober.
func buildGooberCredentialGrants(gooberName string, capabilities []string, sources []credentials.Grant) []credentials.Grant {
	refs := make(map[string]string, len(sources))
	for _, source := range sources {
		if source.Goober == "" {
			refs[source.Capability] = source.Ref
		}
	}
	grants := make([]credentials.Grant, 0, len(capabilities))
	seen := make(map[string]bool, len(capabilities))
	for _, capability := range capabilities {
		if seen[capability] {
			continue
		}
		seen[capability] = true
		if ref, ok := refs[capability]; ok {
			grants = append(grants, credentials.Grant{
				Goober:     gooberName,
				Capability: capability,
				Ref:        ref,
			})
		}
	}
	return grants
}

// credentialRefName is the resolver ref name for a per-capability credentials:
// entry (#287) — namespaced so it can never collide with a repo ref (owner/name).
func credentialRefName(cap string) string { return "credential:" + cap }

// ciPollTaskExecutor admits ci-poll's credential against each invocation's
// declared capabilities. Other deterministic kinds retain TaskExecutor's
// existing dispatch behavior without materializing the PR credential.
type ciPollTaskExecutor struct {
	fallback invoke.Deterministic
	injector *credentials.Injector
	recorder executor.ArtifactRecorder
}

func (e *ciPollTaskExecutor) Run(ctx context.Context, env apiv1.InvocationEnvelope, run apiv1.DeterministicRun) (apiv1.ResultEnvelope, error) {
	kind, _ := env.Inputs[executor.InputKind].(string)
	if kind != executor.KindCIPoll {
		return e.fallback.Run(ctx, env, run)
	}

	set, err := e.injector.Materialize(ctx, env.Capabilities)
	if err != nil {
		return apiv1.ResultEnvelope{}, fmt.Errorf("resolve ci-poll credentials: %w", err)
	}
	token, err := set.Token(ctx, string(capability.GitHubPRWrite))
	if err != nil {
		return apiv1.ResultEnvelope{}, fmt.Errorf("resolve ci-poll credential: %w", err)
	}
	var poller executor.PRPoller
	if newPRPoller != nil {
		poller = newPRPoller(token)
	} else {
		poller = providers.NewGitHubProvider(token)
	}
	ciPoll, err := executor.NewCIPollExecutor(poller, e.recorder)
	if err != nil {
		return apiv1.ResultEnvelope{}, err
	}
	pollCfg, err := executor.CIPollConfigFromEnvelope(env)
	if err != nil {
		return apiv1.ResultEnvelope{}, err
	}
	return ciPoll.Run(ctx, pollCfg)
}

// buildCIPollExecutor wraps the deterministic dispatcher for a repo-backed
// instance. Credential resolution stays lazy so a non-ci-poll stage never
// requires the PR capability or token.
func buildCIPollExecutor(cfg *instance.Config, injector *credentials.Injector, fallback invoke.Deterministic, recorder executor.ArtifactRecorder) (invoke.Deterministic, error) {
	if len(cfg.Repos) == 0 {
		return fallback, nil
	}
	if injector == nil {
		return nil, fmt.Errorf("build ci-poll executor: credential injector is nil")
	}
	if fallback == nil {
		return nil, fmt.Errorf("build ci-poll executor: fallback executor is nil")
	}
	if recorder == nil {
		return nil, fmt.Errorf("build ci-poll executor: artifact recorder is nil")
	}
	return &ciPollTaskExecutor{fallback: fallback, injector: injector, recorder: recorder}, nil
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
	resolver credentials.Resolver
	reg      runner.SecretRegistrar
}

func (c *escalationCommenter) UpdateWorkItem(ctx context.Context, req providers.UpdateWorkItemRequest) (providers.WorkItem, error) {
	ref := req.Repository.Owner + "/" + req.Repository.Name
	token, err := c.resolver.Resolve(ctx, ref)
	if err != nil {
		return providers.WorkItem{}, fmt.Errorf("resolve escalation-comment token for %s: %w", ref, err)
	}
	c.reg.Register([]byte(token))
	// PR remediation uses pr/<number> as its internal claim key; provider work
	// item endpoints use the shared bare issue/PR number.
	req.ID = blockedLookupID(req.ID)
	return newEscalationPoster(token).UpdateWorkItem(ctx, req)
}

// buildEscalationNotifier wires the gate.EscalationNotifier (#20) at the
// composition root — a complete, tested implementation that was never
// constructed, so runner.Config.Escalation stayed nil and a repass-budget
// escalation posted nothing to the driving issue (#312, the same "real seam,
// zero production callers" shape as epic #130). Returns nil when no repo is
// configured. The run supplies its repository to each notification so a
// multi-repo instance resolves and posts through the matching connection.
// Comment-only by deliberate design: the Commenter/UpdateWorkItem seam was
// chosen specifically so escalation never touches the item's status label
// (#63); #20's escalation surfacing is a provider comment on the driving issue,
// not a label change (the goobers:needs-human marker is the curator's output,
// a distinct flow).
func buildEscalationNotifier(cfg *instance.Config, resolver credentials.Resolver, reg runner.SecretRegistrar) *gate.EscalationNotifier {
	if len(cfg.Repos) == 0 {
		return nil
	}
	return &gate.EscalationNotifier{
		Poster: &escalationCommenter{
			resolver: resolver,
			reg:      reg,
		},
	}
}

// buildBlockedHandler wires runner.Config.Blocked (#544/#545/#552): the
// instance-level consequences of a stage reporting status "blocked". Returns
// nil when no repo is configured, mirroring buildEscalationNotifier.
// Every blocked driving issue is parked goobers:needs-human (swap off
// goobers:ready and the provider-visible claim marker) per the #544 ruling /
// #539 convention. This prevents the released claim from making the same item
// immediately eligible again.
//
// When the stage also references blockers through outputs.blockedBy, record
// them in scheduler/blocked.json so #552's selection guard still protects the
// issue if a human re-promotes it before every dependency closes. The runner's
// shared EscalationNotifier owns the explanatory provider comment; this handler
// owns only parking and dependency-record state.
//
// The handler runs before FinalizeTerminal releases the run's claims, so a
// run with no StartInput.Item (scheduled/fan-out implementation runs claim
// their item mid-run) resolves its driving item(s) from the claim ledger by
// run id. Best-effort per item: one item's provider failure doesn't skip the
// rest; the joined error is journaled by the runner (blocked_handling_failed),
// never fatal to the terminal transition.
func buildBlockedHandler(l instance.Layout, cfg *instance.Config, resolver credentials.Resolver, reg runner.SecretRegistrar) runner.BlockedHandler {
	if len(cfg.Repos) == 0 {
		return nil
	}
	poster := &escalationCommenter{
		resolver: resolver,
		reg:      reg,
	}

	return func(ctx context.Context, o runner.BlockedOutcome) error {
		itemIDs := []string{o.ItemID}
		if o.ItemID == "" {
			ids, err := claimedItemIDsForRun(l, o.RunID)
			if err != nil {
				return err
			}
			if len(ids) == 0 {
				// No driving item anywhere (a producer run) — nothing to
				// record or park; the journaled blocked_by_agent cause and the
				// escalated phase are the whole story.
				return nil
			}
			itemIDs = ids
		}

		var errs []error
		repoRef := providers.RepositoryRef{
			Provider: providers.ProviderKind(o.RepoRef.Provider),
			Owner:    o.RepoRef.Owner,
			Name:     o.RepoRef.Name,
		}
		for _, itemID := range itemIDs {
			req := providers.UpdateWorkItemRequest{
				Repository:   repoRef,
				ID:           itemID,
				AddLabels:    []string{providers.LabelNeedsHuman},
				RemoveLabels: []string{providers.LabelReady, providers.LabelClaimed},
			}
			if len(o.Blockers) > 0 {
				if err := updateBlockedRecords(l, func(recs map[string]blockedRecord) bool {
					recs[itemID] = blockedRecord{
						Blockers:   o.Blockers,
						RunID:      o.RunID,
						Stage:      o.Stage,
						Reason:     o.Reason,
						RecordedAt: time.Now().UTC(),
					}
					return true
				}); err != nil {
					errs = append(errs, fmt.Errorf("record block for %s: %w", itemID, err))
				}
			}
			if _, err := poster.UpdateWorkItem(ctx, req); err != nil {
				errs = append(errs, fmt.Errorf("park blocked item %s#%s: %w", repoRef.Name, itemID, err))
			}
		}
		return errors.Join(errs...)
	}
}

// buildFailedHandler wires runner.Config.Failed (#1054): the instance-level
// consequence of a run reaching terminal PhaseFailed. Returns nil when no repo
// is configured, mirroring buildBlockedHandler. Leaves a human-visible trace on
// the driving item — a comment recording the terminal failure cause and the run
// id — so repeated terminal failures on the same item accumulate a countable
// signal instead of the item silently returning to goobers:ready with no
// record. The motivating case (#1054) is a recurring copilot-cli harness
// session timeout in the implement stage: it retries once, times out again, and
// ends the run `failed` (not `escalated`), so today the driving issue returns to
// ready indistinguishable from one never attempted and is re-claimed and
// re-failed forever with nothing accumulating anywhere a human can see.
//
// Deliberately does NOT apply goobers:needs-human: that label is reserved for
// the escalated/park path (buildEscalationNotifier / buildBlockedHandler's
// no-blockers park), keeping a `failed` terminal distinct from an escalation.
// Comment-only, via the same escalationCommenter/UpdateWorkItem seam (which
// normalizes a pr/<n> claim to its bare number).
//
// Like buildBlockedHandler, the handler runs before FinalizeTerminal releases
// the run's claims, so it resolves the driving item(s) from the claim ledger by
// run id — implementation and pr-remediation runs (the two workflows that hit
// this) self-select their item mid-run, so they never carry a StartInput.Item
// snapshot and the ledger is the only source. Best-effort per item: one item's
// provider failure doesn't skip the rest; the joined error is journaled by the
// runner (failed_handling_failed), never fatal to the terminal transition.
func buildFailedHandler(l instance.Layout, cfg *instance.Config, resolver credentials.Resolver, reg runner.SecretRegistrar) runner.FailedHandler {
	if len(cfg.Repos) == 0 {
		return nil
	}
	poster := &escalationCommenter{
		resolver: resolver,
		reg:      reg,
	}

	return func(ctx context.Context, o runner.FailedOutcome) error {
		itemIDs, err := claimedItemIDsForRun(l, o.RunID)
		if err != nil {
			return err
		}
		if len(itemIDs) == 0 {
			// No driving item anywhere (a producer/schedule run, or a run whose
			// claim was already released) — nothing to trace; the journaled
			// run_failed cause and the failed phase are the whole story.
			return nil
		}
		cause := strings.TrimSpace(o.Cause)
		if cause == "" {
			cause = "no cause recorded"
		}
		repoRef := providers.RepositoryRef{
			Provider: providers.ProviderKind(o.RepoRef.Provider),
			Owner:    o.RepoRef.Owner,
			Name:     o.RepoRef.Name,
		}
		var errs []error
		for _, itemID := range itemIDs {
			comment := fmt.Sprintf(
				"Goobers run %s terminated `failed`: %s. The run released its claim and this issue returned to the backlog; this comment records the terminal failure so repeated failures on this item are visible instead of silently recurring. No `%s` applied — a `failed` terminal is distinct from an escalation.",
				o.RunID, cause, providers.LabelNeedsHuman,
			)
			if _, err := poster.UpdateWorkItem(ctx, providers.UpdateWorkItemRequest{
				Repository: repoRef, ID: itemID, Comment: comment,
			}); err != nil {
				errs = append(errs, fmt.Errorf("notify failed on %s#%s: %w", repoRef.Name, itemID, err))
			}
		}
		return errors.Join(errs...)
	}
}

// buildRateLimitedHandler wires runner.Config.RateLimited (#712): records the
// exhausted provider quota into the shared ProviderQuotaState the same
// composition root also hands to the scheduler (via
// localscheduler.WithProviderQuota, schedulerSetup.SchedulerOptions) — the
// Runner and the Scheduler are constructed in different order at the
// composition root, so this pointer, not a Scheduler-owned field, is what
// lets the two agree on one state. pq is never nil (buildSchedulerSetup
// always constructs one); the nil check mirrors the defensive style of this
// file's other optional-dependency handlers.
func buildRateLimitedHandler(pq *localscheduler.ProviderQuotaState) runner.RateLimitedHandler {
	if pq == nil {
		return nil
	}
	return func(_ context.Context, o runner.RateLimitedOutcome) error {
		pq.RecordExhausted(o.ResetAt)
		return nil
	}
}

// claimedItemIDsForRun resolves the backlog item(s) a run currently claims —
// the driving-issue fallback for a run started without an Item snapshot. Read
// under the claim lock like every other ledger access; the blocked handler
// runs before FinalizeTerminal, so the claims are still held here.
func claimedItemIDsForRun(l instance.Layout, runID string) ([]string, error) {
	var ids []string
	err := withClaimLock(filepath.Join(l.SchedulerDir(), claimLockFileName), claimLockOperationRunLookup, func() error {
		ledger, err := localscheduler.OpenClaimLedger(filepath.Join(l.SchedulerDir(), claimLedgerFileName))
		if err != nil {
			return fmt.Errorf("open claim ledger: %w", err)
		}
		for _, entry := range ledger.ForRunAll(runID) {
			ids = append(ids, entry.ItemID)
		}
		return nil
	})
	return ids, err
}

// newOpenPRProvider builds the GitHub client the open-PR lister polls; a package
// var so tests substitute a fake (mirrors newPRPoller / newEscalationPoster).
var newOpenPRProvider = func(token string) localscheduler.OpenPRLister {
	return providers.NewGitHubProvider(token)
}

// resolvingOpenPRLister resolves the org-repo token per poll — honoring
// credentials.Resolver's re-read-on-resolve rotation contract, matching
// buildCIPollExecutor / the escalation notifier — registers it for scrubbing,
// and lists open PR heads through a freshly-authenticated provider. It is the
// OpenPRLister the #353 open-PR-count refresher polls off-tick.
type resolvingOpenPRLister struct {
	ref      string
	resolver credentials.Resolver
	reg      runner.SecretRegistrar
}

func (l *resolvingOpenPRLister) ListOpenPullRequests(ctx context.Context, repo providers.RepositoryRef) ([]providers.OpenPRSummary, error) {
	token, err := l.resolver.Resolve(ctx, l.ref)
	if err != nil {
		return nil, fmt.Errorf("resolve open-pr-list token for %s: %w", l.ref, err)
	}
	l.reg.Register([]byte(token))
	return newOpenPRProvider(token).ListOpenPullRequests(ctx, repo)
}

// buildOpenPRRefresher constructs the #353 open-PR-count refresher only when the
// instance actually needs it — a repo is configured AND some workflow opts into
// the MaxOpenPRs cap — so an instance that doesn't use the cap grows no GitHub
// poller and needs no token for it. Returns nil otherwise. Only the `up` daemon
// starts/wires the returned refresher; a single `goobers run` has no accretion
// to throttle. resolver is a fresh credential resolver over cfg (buildCredentials
// is read-only and idempotent), used only to authenticate the poll.
func buildOpenPRRefresher(cfg *instance.Config, workflows []apiv1.Workflow, reg runner.SecretRegistrar) (*localscheduler.OpenPRRefresher, error) {
	if len(cfg.Repos) == 0 {
		return nil, nil
	}
	capped := false
	for i := range workflows {
		if workflows[i].Spec.Readiness.MaxOpenPRs > 0 {
			capped = true
			break
		}
	}
	if !capped {
		return nil, nil
	}
	resolver, _, err := buildCredentials(cfg)
	if err != nil {
		return nil, fmt.Errorf("build open-pr-list credential resolver: %w", err)
	}
	repo := cfg.Repos[0]
	lister := &resolvingOpenPRLister{ref: repo.Owner + "/" + repo.Name, resolver: resolver, reg: reg}
	repoRef := providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: repo.Owner, Name: repo.Name}
	// Exclude human-parked PRs from the cap (#986): goobers:merge-escalated is
	// the daemon's "parked pending a human" signal on a PR — it cannot be
	// drained autonomously, so counting it against MaxOpenPRs only starves new
	// implementation work. needs-remediation / blocked-on-sibling are
	// deliberately NOT excluded: the daemon can still drain those (remediation,
	// sibling sequencing), and the cap must keep applying backpressure to them.
	return localscheduler.NewOpenPRRefresher(lister, repoRef, localscheduler.DefaultOpenPRRefreshInterval, []string{remediationEscalatedLabel}), nil
}

// backlogCounter adapts a provider + repo + label selector into a
// localscheduler.BacklogCounter (#344) — resolves its token per call (like
// escalationCommenter above), honoring credentials.Resolver's re-read-on-
// resolve rotation contract rather than capturing one at daemon startup.
type backlogCounter struct {
	ref      string
	repo     providers.RepositoryRef
	labels   []string
	resolver credentials.Resolver
	reg      runner.SecretRegistrar
}

func (b *backlogCounter) EligibleCount(ctx context.Context) (int, error) {
	token, err := b.resolver.Resolve(ctx, b.ref)
	if err != nil {
		return 0, fmt.Errorf("resolve backlog-count token for %s: %w", b.ref, err)
	}
	b.reg.Register([]byte(token))
	items, err := newGitHubProvider(token).ListWorkItems(ctx, providers.ListWorkItemsRequest{
		Repository: b.repo, Labels: b.labels, State: "open", Limit: 100,
	})
	if err != nil {
		return 0, err
	}
	return len(items), nil
}

// buildBacklogCounter wires a localscheduler.BacklogCounter for wf's
// declared type=backlog-item trigger, if it has one (#344) — the daemon-
// side fan-out counter, independent of and never dispatched through
// `goobers backlog-query`'s own per-run claim (that stays the actual
// claiming mechanism; this only estimates how many runs a Tick should fan
// out to). Trigger.Selector (already declared in the schema, never
// implemented until now — #342's own survey found it, like Signal, entirely
// unwired) is a flat map; only its KEYS are used as required GitHub labels
// — values are ignored, since GitHub issue labels are plain strings with no
// key=value structure to match against, unlike a true k8s label selector.
// Returns nil (not error) when wf declares no backlog-item trigger, or when
// no repo is configured — mirrors buildCIPollExecutor/buildEscalationNotifier's
// "irrelevant to this workflow" fail-open-to-nil shape, not a real error.
func buildBacklogCounter(cfg *instance.Config, wf *apiv1.Workflow, repoRef apiv1.RepoRef, resolver credentials.Resolver, reg runner.SecretRegistrar) localscheduler.BacklogCounter {
	if len(cfg.Repos) == 0 {
		return nil
	}
	var selector map[string]string
	found := false
	for _, tr := range wf.Spec.Triggers {
		if tr.Type == apiv1.TriggerBacklogItem {
			selector = tr.Selector
			found = true
			break
		}
	}
	if !found {
		return nil
	}
	labels := make([]string, 0, len(selector))
	for k := range selector {
		labels = append(labels, k)
	}
	return &backlogCounter{
		ref:      cfg.Repos[0].Owner + "/" + cfg.Repos[0].Name,
		repo:     providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: repoRef.Owner, Name: repoRef.Name},
		labels:   labels,
		resolver: resolver,
		reg:      reg,
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
func gooberDefinitionDir(configDir string, spec apiv1.GooberSpec, gooberName string) string {
	return filepath.Join(configDir, "gaggles", spec.Gaggle, "goobers", gooberName)
}

func instructionsPath(configDir string, spec apiv1.GooberSpec, gooberName string) string {
	return filepath.Join(gooberDefinitionDir(configDir, spec, gooberName), spec.Instructions)
}

// buildRunnerConfig assembles the runner.Config the daemon (`goobers up`) and
// `goobers run` share: real worktrees, registry-selected harness adapters and
// the shell executor, credentials scoped to instance.yaml's configured repo(s).
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
func buildRunnerConfig(l instance.Layout, cfg *instance.Config, goobers map[string]apiv1.GooberSpec, tel *telemetry.Client, sharedReg *journal.RegistryScrubber, wtMgr *worktree.Manager) (runner.Config, *worktree.Manager, error) {
	if wtMgr == nil {
		var err error
		wtMgr, err = worktree.NewManager(l.WorkcopiesDir())
		if err != nil {
			return runner.Config{}, nil, fmt.Errorf("new worktree manager: %w", err)
		}
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
	adapterRegistry, err := buildHarnessRegistry(envCaps)
	if err != nil {
		return runner.Config{}, nil, err
	}
	instructionsByGoober := make(map[string]string, len(goobers))
	assetsByGoober := make(map[string]*gooberassets.Bundle, len(goobers))
	for name, spec := range goobers {
		instructions, err := os.ReadFile(instructionsPath(l.ConfigDir(), spec, name))
		if err != nil {
			return runner.Config{}, nil, fmt.Errorf("read goober %q instructions: %w", name, err)
		}
		instructionsByGoober[name] = string(instructions)
		assets, err := gooberassets.Load(filepath.Join(gooberDefinitionDir(l.ConfigDir(), spec, name), gooberassets.SourceDir))
		if err != nil {
			return runner.Config{}, nil, fmt.Errorf("load goober %q assets: %w", name, err)
		}
		assetsByGoober[name] = assets
	}

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
			// goobers up --diagnostics: arm the per-stage diagnostics watchdog
			// (process sample/tree/lsof of a long-running stage) and keep stage
			// output un-truncated so a full dump is never clipped.
			if diagnosticsMode {
				shell.Diagnostics = true
				shell.DefaultMaxOutputBytes = diagnosticsMaxOutputBytes
			}

			fallback, err := executor.NewTaskExecutor(shell, nil)
			if err != nil {
				return nil, err
			}
			return buildCIPollExecutor(cfg, injector, fallback, rec)
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
			gooberGrants := buildGooberCredentialGrants(gooberName, spec.Capabilities, grants)
			injector, err := credentials.NewGooberInjector(resolver, gooberName, gooberGrants, teeRegistrar{run: reg, shared: sharedReg})
			if err != nil {
				return nil, err
			}
			harnessName := spec.Harness
			if harnessName == "" {
				harnessName = apiv1.HarnessCopilot
			}
			adapter, err := adapterRegistry.Get(string(harnessName))
			if err != nil {
				return nil, fmt.Errorf("resolve goober %q harness: %w", gooberName, err)
			}
			if err := harness.ValidateConfig(adapter, spec.Model, spec.HarnessOptions); err != nil {
				return nil, fmt.Errorf("validate goober %q harness config: %w", gooberName, err)
			}
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
			opts := []harness.Option{
				harness.WithHarnessConfig(spec.Model, spec.HarnessOptions),
				harness.WithAssetBundle(assetsByGoober[gooberName]),
			}
			// Goober-level default timeout (#1070): raises this goober's built-in
			// 30m harness bound so its bigger tasks aren't cut off, without
			// annotating every stage. A stage's own Task.TimeoutSeconds still
			// wins via env.Limits (invocationTimeout); this only moves the
			// fallback that applies when a stage sets none.
			if spec.TimeoutSeconds > 0 {
				opts = append(opts, harness.WithTimeout(time.Duration(spec.TimeoutSeconds)*time.Second))
			}
			return harness.NewExecutor(
				adapter,
				injector,
				recorder,
				artifacts,
				contextResolver,
				scrubber,
				instructionsByGoober[gooberName],
				opts...,
			)
		},
		Automated:              gate.NewAutomatedEvaluator(),
		Worktrees:              wtMgr,
		ScratchDir:             filepath.Join(l.WorkcopiesDir(), "scratch"),
		RunsDir:                l.RunsDir(),
		RepoCloneURL:           repoCloneURL,
		GateGooberCapabilities: gateGooberCaps,
		// Wire the escalation notifier (#312) so a repass-budget escalation
		// actually comments on the driving issue; nil for a repo-less instance.
		Escalation: buildEscalationNotifier(cfg, resolver, sharedReg),
		// Resolve the driving item(s) from the claim ledger when a run has no
		// Item snapshot (#796): scheduled implementation runs self-select their
		// item mid-run, so notifyTerminalGate would otherwise never comment on an
		// escalation. Mirrors the fallback buildBlockedHandler already uses.
		ClaimedItems: func(runID string) ([]string, error) { return claimedItemIDsForRun(l, runID) },
		// Wire the blocked handler (#544/#552): record/park the driving issue
		// when a stage reports blocked; nil for a repo-less instance.
		Blocked: buildBlockedHandler(l, cfg, resolver, sharedReg),
		// Wire the failed handler (#1054): leave a human-visible trace on the
		// driving item when a run ends terminal `failed`, so a recurring infra
		// fault (e.g. a copilot-cli session timeout) stops silently returning the
		// item to ready with no record; nil for a repo-less instance.
		Failed: buildFailedHandler(l, cfg, resolver, sharedReg),
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
	sort.Strings(names)
	return names
}

// compiledMachines compiles every workflow in set, admission-checked against
// goobers (capabilities, harness, gate-outcome coverage, and known automated
// check names — #124), keyed by gaggle and workflow name. WorkflowVersion is
// registry-assigned (per-name monotonic, WF-016); no registry is wired at the
// instance level yet, so this pins version 1 for every workflow, matching
// run.go's existing limitation until a follow-up introduces one.
func compiledMachines(set *instance.ConfigSet, goobers map[string]apiv1.GooberSpec) (map[localscheduler.WorkflowIdentity]*workflow.Machine, error) {
	const workflowVersion = 1
	knownChecks := knownAutomatedCheckNames()
	adapterRegistry, err := buildHarnessRegistry(nil)
	if err != nil {
		return nil, err
	}
	gooberNames := make([]string, 0, len(goobers))
	for name := range goobers {
		gooberNames = append(gooberNames, name)
	}
	sort.Strings(gooberNames)
	for _, name := range gooberNames {
		spec := goobers[name]
		harnessName := spec.Harness
		if harnessName == "" {
			harnessName = apiv1.HarnessCopilot
		}
		if err := adapterRegistry.ValidateConfig(string(harnessName), spec.Model, spec.HarnessOptions); err != nil {
			return nil, fmt.Errorf("validate goober %q harness config: %w", name, err)
		}
	}
	machines := make(map[localscheduler.WorkflowIdentity]*workflow.Machine, len(set.Workflows))
	for i := range set.Workflows {
		wf := &set.Workflows[i]
		m, err := workflow.Compile(
			workflow.Definition{Name: wf.Name, Version: workflowVersion, Spec: wf.Spec},
			workflow.WithGoobers(goobers),
			workflow.WithKnownChecks(knownChecks),
			workflow.WithKnownHarnesses(adapterRegistry.Names()),
		)
		if err != nil {
			return nil, fmt.Errorf("compile workflow %q: %w", wf.Name, err)
		}
		machines[localscheduler.WorkflowIdentity{Gaggle: wf.Spec.Gaggle, Workflow: wf.Name}] = m
	}
	return machines, nil
}

// repoRefsByWorkflow resolves each workflow's RepoRef via its Gaggle's
// declared project (apiv1.GaggleSpec.Project) — a workflow only names its
// gaggle, not a repo directly.
func repoRefsByWorkflow(set *instance.ConfigSet) (map[localscheduler.WorkflowIdentity]apiv1.RepoRef, error) {
	gagglesByName := make(map[string]apiv1.Gaggle, len(set.Gaggles))
	for _, g := range set.Gaggles {
		gagglesByName[g.Name] = g
	}
	refs := make(map[localscheduler.WorkflowIdentity]apiv1.RepoRef, len(set.Workflows))
	for i := range set.Workflows {
		wf := &set.Workflows[i]
		g, ok := gagglesByName[wf.Spec.Gaggle]
		if !ok {
			return nil, fmt.Errorf("workflow %q references unknown gaggle %q", wf.Name, wf.Spec.Gaggle)
		}
		refs[localscheduler.WorkflowIdentity{Gaggle: wf.Spec.Gaggle, Workflow: wf.Name}] = g.Spec.Project
	}
	return refs, nil
}
