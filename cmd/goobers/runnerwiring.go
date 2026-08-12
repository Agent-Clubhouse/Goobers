package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/adoauth"
	"github.com/goobers/goobers/internal/capability"
	"github.com/goobers/goobers/internal/credentials"
	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/internal/externaltelemetry"
	"github.com/goobers/goobers/internal/externaltelemetry/adx"
	"github.com/goobers/goobers/internal/fieldpredicate"
	"github.com/goobers/goobers/internal/gate"
	"github.com/goobers/goobers/internal/githubapp"
	"github.com/goobers/goobers/internal/gooberassets"
	"github.com/goobers/goobers/internal/harness"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/labelpredicate"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/mcpconfig"
	"github.com/goobers/goobers/internal/providersnapshot"
	"github.com/goobers/goobers/internal/readmodel/intake"
	"github.com/goobers/goobers/internal/runner"
	"github.com/goobers/goobers/internal/telemetry"
	"github.com/goobers/goobers/internal/telemetry/rollup"
	"github.com/goobers/goobers/internal/version"
	"github.com/goobers/goobers/internal/workflow"
	"github.com/goobers/goobers/internal/worktree"
	"github.com/goobers/goobers/providers"
	connectorapi "github.com/goobers/goobers/telemetryconnector/v1alpha1"
)

// buildTelemetryClient constructs the OTel client that spans the runner walk
// (run/task/gate) and scheduler decisions, writing completed spans under
// RunsDir via JournalSpanExporter (issue #126) — the same run journal
// layout goobers trace/telemetry read back through the rollup. Shared by
// up.go/run.go exactly like buildRunnerConfig; each caller owns calling
// Shutdown on the returned client once it's done driving runs.
func buildTelemetryClient(
	ctx context.Context,
	l instance.Layout,
	scrubber journal.Scrubber,
	registry *journal.RegistryScrubber,
	otlp instance.OTLPConfig,
	stores credentials.StoreResolver,
) (*telemetry.Client, error) {
	cfg := telemetry.Config{
		ServiceName:    "goobers",
		ServiceVersion: version.Get().Version,
		BuildCommit:    version.Get().Commit,
		SpanExporter:   telemetry.NewPerGaggleJournalSpanExporter(l.Root, scrubber),
		Scrubber:       scrubber,
		Batch:          true,
	}
	if otlp.Enabled() {
		headers, err := resolveOTLPHeaders(ctx, otlp.Headers, registry, stores)
		if err != nil {
			return nil, err
		}
		cfg.Exporter = telemetry.ExporterOTLP
		cfg.OTLPEndpoint = otlp.Endpoint
		cfg.OTLPInsecure = otlp.Insecure
		cfg.OTLPHeaders = headers
	}
	return telemetry.New(ctx, cfg)
}

func resolveOTLPHeaders(
	ctx context.Context,
	headerRefs map[string]instance.TokenRef,
	registry *journal.RegistryScrubber,
	stores credentials.StoreResolver,
) (map[string]string, error) {
	names := make([]string, 0, len(headerRefs))
	for name := range headerRefs {
		names = append(names, name)
	}
	sort.Strings(names)

	refs := make([]credentials.TokenRef, 0, len(names))
	for _, name := range names {
		refs = append(refs, headerRefs[name].CredentialTokenRef("telemetry.otlp.headers."+strings.ToLower(name)))
	}
	resolver, err := credentials.NewResolverWithStores(refs, stores)
	if err != nil {
		return nil, fmt.Errorf("configure telemetry OTLP headers: %w", err)
	}

	headers := make(map[string]string, len(names))
	for i, name := range names {
		value, err := resolver.Resolve(ctx, refs[i].Name)
		if err != nil {
			return nil, fmt.Errorf("resolve telemetry OTLP header %q: %w", name, err)
		}
		registry.Register([]byte(value))
		headers[name] = value
	}
	return headers, nil
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
// FlushLocal MUST run before IngestRun reads spans.jsonl: the local journal
// exporter batches completed spans, but flushing the whole provider would also
// wait for a configured remote collector and delay scheduler-slot release.
func ingestRunTelemetry(tel *telemetry.Client, db *rollup.DB, watermarks *intake.Store, l instance.Layout, runID string, log *journal.InstanceLog) {
	if tel != nil {
		if err := tel.FlushLocal(context.Background()); err != nil {
			logIngestFailure(log, runID, "telemetry_flush_failed", err)
		}
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
	if err := db.IngestRun(context.Background(), filepath.Join(l.RunsDir(), runID)); err != nil {
		logIngestFailure(log, runID, "telemetry_ingest_run_failed", err)
	}
	ingestSchedulerLog(db, l.SchedulerDir(), log, runID)
	recordRunIntake(watermarks, l, runID, log)
}

// recordRunIntake records a source watermark rather than projecting inline
// (#1922/#1923, §6.1).
//
// # The inversion
//
// This used to call ProjectRunDir here, from the writer, in-process, at run
// completion — the same coupling `IngestRun` has above. That dependency does not
// survive separating execution from serving, and it has a defect that shows up
// long before any separation: a run written while the daemon is down is never
// projected at all, and nothing notices.
//
// Now the writer records "this run advanced to sequence N" and forgets about it.
// The projector discovers the marker on its own schedule and applies it. A
// marker written while the daemon is down is still there when it starts, so the
// restart pass picks it up — which is the whole point.
//
// # Why it reads the journal for the sequence
//
// The watermark must carry a REAL sequence, not a placeholder. The
// acknowledgement guard is `source_seq <= projectedSeq`: if every writer
// recorded zero, an append racing a projection would be acknowledged away as
// though it had been applied. One journal read here replaces the much larger
// whole-run ingest that used to happen at this point.
//
// # Failure is degradation, not an error
//
// Best-effort but never silent (#246). A watermark that fails to record means
// that run is discovered by the repair sweep instead — slower, but complete. The
// alternative, failing a run because a read-model hint could not be written,
// would make the read model an availability dependency of execution.
func recordRunIntake(watermarks *intake.Store, l instance.Layout, runID string, log *journal.InstanceLog) {
	if watermarks == nil {
		return
	}
	dir, err := l.FindRunDir(runID)
	if err != nil {
		logIngestFailure(log, runID, "read_model_run_dir_failed", err)
		return
	}
	seq, err := lastJournalSeq(dir)
	if err != nil {
		logIngestFailure(log, runID, "read_model_intake_seq_failed", err)
		return
	}
	if err := watermarks.Observed(context.Background(), runID, seq); err != nil {
		logIngestFailure(log, runID, "read_model_intake_failed", err)
	}
}

func runIntakeObserver(watermarks *intake.Store, log *journal.InstanceLog) func(string, uint64) {
	if watermarks == nil {
		return nil
	}
	return func(runID string, seq uint64) {
		if err := watermarks.Observed(context.Background(), runID, seq); err != nil {
			logIngestFailure(log, runID, "read_model_intake_failed", err)
		}
	}
}

// lastJournalSeq reports the highest sequence in a run's journal.
//
// Takes the maximum rather than the last record's sequence: the live instance's
// journals contain duplicate and regressed sequences (1,394 duplicates and 119
// regressions, from #530), so "the last line" and "the highest sequence" are not
// the same number. The watermark has to be the highest, or the acknowledgement
// guard would let a projection at the true maximum acknowledge a marker that
// still represented unapplied work.
func lastJournalSeq(dir string) (uint64, error) {
	reader, err := journal.OpenRead(dir)
	if err != nil {
		return 0, err
	}
	events, err := reader.Events()
	if err != nil {
		return 0, err
	}
	var highest uint64
	for _, event := range events {
		if event.Seq > highest {
			highest = event.Seq
		}
	}
	return highest, nil
}

func ingestSchedulerTelemetry(ctx context.Context, tel *telemetry.Client, db *rollup.DB, schedulerDir string, log *journal.InstanceLog) {
	if tel != nil {
		if err := tel.Flush(ctx); err != nil {
			logIngestFailure(log, "", "telemetry_flush_failed", err)
		}
	}
	if db == nil {
		return
	}
	ingestSchedulerLog(db, schedulerDir, log, "")
}

func ingestSchedulerLog(db *rollup.DB, schedulerDir string, log *journal.InstanceLog, runID string) {
	if err := db.IngestSchedulerLog(context.Background(), schedulerDir); err != nil {
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

const claudeModelEnv = "ANTHROPIC_API_KEY"

// credentialedCapabilities are the canonical capabilities (internal/capability,
// issue #74) a repo's token can satisfy; telemetry:read needs no credential.
var credentialedCapabilities = []capability.Capability{
	capability.RepoPush, capability.GitHubIssuesRead, capability.GitHubIssuesWrite, capability.GitHubMilestonesWrite, capability.GitHubIssuesApprove, capability.ProviderPRWrite, capability.GitHubPRWrite, capability.GitHubPRReview, capability.GitHubBranchDelete, capability.GitHubPRMerge,
	// ADO PR completion authority is repo-token-backed like the GitHub grants
	// above; only stages that declare ado:pr:complete receive its credential,
	// preserving the decider/executor isolation (merge-review completes; the
	// implementation and remediation lanes never can).
	capability.ADOPRComplete,
}

// daemonIdentityRefName is the resolver ref name a configured DaemonIdentity's
// credential (static token or App-minted) is registered under (#1780),
// namespaced away from repo refs ("owner/name") and explicit credentials:
// refs ("credential:<key>") the same way those two are namespaced from each
// other.
const daemonIdentityRefName = "daemon-identity"

// daemonIdentityCapabilities are the standard daemon-mutation capabilities a
// configured DaemonIdentity backs by default (#1780) — deliberately a subset
// of credentialedCapabilities: GitHubMilestonesWrite (roadmap mutation) and
// GitHubIssuesApprove (the goobers:approved trust decision) are excluded
// because both are explicitly documented (internal/capability) as separate,
// deliberate decisions that must not silently follow whichever identity
// authors ordinary PRs/issues — an instance that wants those on the daemon
// identity too can still say so explicitly via credentials:.
var daemonIdentityCapabilities = []capability.Capability{
	capability.RepoPush, capability.GitHubIssuesWrite, capability.GitHubPRWrite, capability.GitHubPRReview, capability.GitHubBranchDelete, capability.GitHubPRMerge,
}

// buildEnvCapabilities maps each capability the Copilot adapter injects to the
// environment variable that consumes its token. General org-repo capabilities
// use GH_TOKEN (the github tool's var), command-scoped capabilities use their
// dedicated GOOBERS_CRED_* variables, and agent:model uses
// COPILOT_GITHUB_TOKEN (the model backend's var, #288, §3.3).
func buildEnvCapabilities() map[string]string {
	envCaps := make(map[string]string, len(credentialedCapabilities)+1)
	for _, c := range credentialedCapabilities {
		envCaps[string(c)] = credentialGrantEnv
	}
	envCaps[string(capability.GitHubIssuesApprove)] = executor.CredentialEnvVar(string(capability.GitHubIssuesApprove))
	envCaps[string(capability.GitHubMilestonesWrite)] = executor.CredentialEnvVar(string(capability.GitHubMilestonesWrite))
	envCaps[string(capability.AgentModel)] = copilotModelEnv
	return envCaps
}

var copilotModelLister harness.CopilotModelLister

// buildHarnessRegistry is the production harness composition point. Registry
// keys are goober spec.harness values; adapter names remain their diagnostic
// identities, so Copilot continues to report "copilot-cli" in spans and errors.
func buildHarnessRegistry(envCaps map[string]string, envPassthrough []string, harnessCommand map[string][]string, instanceRoot, selfBin string) (*harness.Registry, error) {
	registry := harness.NewRegistry()
	copilotAdapter := &harness.CopilotAdapter{
		Command:         harnessCommandOrDefault(harnessCommand, string(apiv1.HarnessCopilot), []string{"copilot"}),
		AuthCheckArgs:   copilotAuthCheckArgs,
		ModelLister:     copilotModelLister,
		EnvCapabilities: envCaps,
		OptionalCredentialCapabilities: map[string]bool{
			string(capability.AgentModel): true,
		},
		ExtraEnvAllowlist: envPassthrough,
		InstanceRoot:      instanceRoot,
		SelfBin:           selfBin,
	}
	if err := registry.RegisterAs(string(apiv1.HarnessCopilot), copilotAdapter); err != nil {
		return nil, fmt.Errorf("register Copilot harness: %w", err)
	}

	claudeEnvCaps := make(map[string]string, len(envCaps)+1)
	for capability, envVar := range envCaps {
		claudeEnvCaps[capability] = envVar
	}
	claudeEnvCaps[string(capability.AgentModel)] = claudeModelEnv
	claudeAdapter := &harness.ClaudeAdapter{
		Command:         harnessCommandOrDefault(harnessCommand, string(apiv1.HarnessClaudeCode), []string{"claude"}),
		EnvCapabilities: claudeEnvCaps,
		OptionalCredentialCapabilities: map[string]bool{
			string(capability.AgentModel): true,
		},
		ExtraEnvAllowlist: envPassthrough,
		InstanceRoot:      instanceRoot,
		SelfBin:           selfBin,
	}
	if err := registry.RegisterAs(string(apiv1.HarnessClaudeCode), claudeAdapter); err != nil {
		return nil, fmt.Errorf("register Claude Code harness: %w", err)
	}
	return registry, nil
}

// harnessCommandOrDefault returns the adopter's launcher override for the named
// harness (RunnerConfig.HarnessCommand), or def when unset. It defensively
// copies the override so a later mutation of the config map can't reach into
// the registered adapter, and falls back to def on an empty slice (already
// rejected at config load, but belt-and-suspenders — an empty argv would fail
// at exec).
func harnessCommandOrDefault(overrides map[string][]string, name string, def []string) []string {
	if command, ok := overrides[name]; ok && len(command) > 0 {
		return append([]string(nil), command...)
	}
	return def
}

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
	var sources map[string]credentials.ResolveFunc
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
				sources = make(map[string]credentials.ResolveFunc)
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
			mint, err := newDaemonIdentityGitHubAppTokenSource(cfg.DaemonIdentity, gaggleName, registrar, stores)
			if err != nil {
				return nil, nil, fmt.Errorf("build credentials: daemonIdentity: %w", err)
			}
			if sources == nil {
				sources = make(map[string]credentials.ResolveFunc)
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
	resolver, err := credentials.NewResolverWith(refs, stores, sources)
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
var newGitHubAppTokenSource = func(repo instance.RepoRef, registrar credentials.SecretRegistrar, stores credentials.StoreResolver) (credentials.ResolveFunc, error) {
	source, err := githubapp.Source(repo, registrar, stores)
	if err != nil {
		return nil, err
	}
	return source.Token, nil
}

// newDaemonIdentityGitHubAppTokenSource builds the installation-token minting
// source for a github-app-kind DaemonIdentity (#1780/#1779), scoped down to
// this one gaggle's repo exactly like a repo's own github-app auth already
// is (MGV-5 #1012) — a shared App installation must not hand one gaggle's
// stages a token that reaches a sibling gaggle's repo. A package var, like
// newGitHubAppTokenSource, so CLI tests substitute an httptest-backed source.
var newDaemonIdentityGitHubAppTokenSource = func(d *instance.DaemonIdentityConfig, gaggleRepoName string, registrar credentials.SecretRegistrar, stores credentials.StoreResolver) (credentials.ResolveFunc, error) {
	const keyRefName = "daemon-identity-private-key"
	keyResolver, err := credentials.NewResolverWith([]credentials.TokenRef{d.PrivateKey.CredentialTokenRef(keyRefName)}, stores, nil)
	if err != nil {
		return nil, fmt.Errorf("configure daemon identity App key source: %w", err)
	}
	source, err := githubapp.New(githubapp.Config{
		AppID:          string(d.AppID),
		InstallationID: string(d.InstallationID),
		Repositories:   []string{gaggleRepoName},
		Key: func(ctx context.Context) (string, error) {
			return keyResolver.Resolve(ctx, keyRefName)
		},
		Registrar: registrar,
	})
	if err != nil {
		return nil, err
	}
	return source.Token, nil
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

// ciPollKindExecutor admits ci-poll's credential against each invocation's
// declared capabilities. Registering it only for KindCIPoll keeps credential
// materialization out of every other deterministic kind.
type ciPollKindExecutor struct {
	injector *credentials.Injector
	recorder executor.ArtifactRecorder
	// adoRepo is set when this gaggle's repo is Azure DevOps. When set, ci-poll
	// builds its poller from instance config (adoauth.Provider shells out to
	// `az` for the token) rather than materializing a GitHub capability token —
	// mirroring the CLI PR stages' provider resolution.
	adoRepo *instance.RepoRef
	// giteaRepo is set when the gaggle's repo is Gitea; ci-poll then builds a
	// Gitea poller from its baseURL + the materialized capability token instead
	// of defaulting to GitHub.
	giteaRepo *instance.RepoRef
	registrar providers.SecretRegistrar
	quota     providers.QuotaObserver
}

func (e *ciPollKindExecutor) Run(ctx context.Context, env apiv1.InvocationEnvelope, _ apiv1.DeterministicRun) (apiv1.ResultEnvelope, error) {
	required := string(capability.ProviderPRWrite)
	if !slices.Contains(env.Capabilities, required) {
		return apiv1.ResultEnvelope{}, fmt.Errorf("executor: kind=%s requires declared capability %q: %w", executor.KindCIPoll, required, credentials.ErrUndeclaredCapability)
	}
	var poller executor.PRPoller
	switch {
	case e.adoRepo != nil:
		provider, err := adoauth.Provider(*e.adoRepo, nil, e.registrar, nil, e.quota, nil)
		if err != nil {
			return apiv1.ResultEnvelope{}, fmt.Errorf("build ADO ci-poll provider: %w", err)
		}
		poller = provider
	case e.giteaRepo != nil:
		set, err := e.injector.Materialize(ctx, env.Capabilities)
		if err != nil {
			return apiv1.ResultEnvelope{}, fmt.Errorf("resolve ci-poll credentials: %w", err)
		}
		token, err := set.Token(ctx, string(capability.ProviderPRWrite))
		if err != nil {
			return apiv1.ResultEnvelope{}, fmt.Errorf("resolve ci-poll credential: %w", err)
		}
		poller = providers.NewGiteaProvider(e.giteaRepo.BaseURL, token)
	default:
		set, err := e.injector.Materialize(ctx, env.Capabilities)
		if err != nil {
			return apiv1.ResultEnvelope{}, fmt.Errorf("resolve ci-poll credentials: %w", err)
		}
		token, err := set.Token(ctx, string(capability.ProviderPRWrite))
		if err != nil {
			return apiv1.ResultEnvelope{}, fmt.Errorf("resolve ci-poll credential: %w", err)
		}
		if newPRPoller != nil {
			poller = newPRPoller(token)
		} else {
			poller = providers.NewGitHubProvider(token)
		}
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

// buildCIPollExecutor builds the registered ci-poll kind for a repo-backed
// instance. Credential resolution stays lazy until that kind is dispatched.
// When adoRepo is non-nil the gaggle's repo is Azure DevOps, and ci-poll
// resolves its poller from instance config (adoauth.Provider shells out to
// `az` for the token) instead of a GitHub capability token.
func buildCIPollExecutor(cfg *instance.Config, injector *credentials.Injector, recorder executor.ArtifactRecorder, adoRepo *instance.RepoRef, giteaRepo *instance.RepoRef, registrar providers.SecretRegistrar, quota *localscheduler.ProviderQuotaState) (executor.KindExecutor, error) {
	if len(cfg.Repos) == 0 {
		return executor.NewCIPollKindExecutor(nil), nil
	}
	if injector == nil {
		return nil, fmt.Errorf("build ci-poll executor: credential injector is nil")
	}
	if recorder == nil {
		return nil, fmt.Errorf("build ci-poll executor: artifact recorder is nil")
	}
	var quotaObserver providers.QuotaObserver
	if quota != nil {
		quotaObserver = &providerQuotaAccounting{state: quota}
	}
	return &ciPollKindExecutor{injector: injector, recorder: recorder, adoRepo: adoRepo, giteaRepo: giteaRepo, registrar: registrar, quota: quotaObserver}, nil
}

// buildExternalTelemetryExecutor validates every registered plugin
// configuration before a run and constructs the registered query kind.
func buildExternalTelemetryExecutor(
	config externaltelemetry.Configuration,
	recorder executor.ArtifactRecorder,
	registrar externaltelemetry.SecretRegistrar,
) (executor.KindExecutor, error) {
	if recorder == nil {
		return nil, errors.New("build external telemetry executor: artifact recorder is nil")
	}
	registry, err := buildExternalTelemetryRegistry(config, registrar)
	if err != nil {
		return nil, err
	}
	query, err := executor.NewTelemetryQueryExecutor(&externaltelemetry.Host{
		Registry: registry,
	}, recorder)
	if err != nil {
		return nil, err
	}
	return query, nil
}

func buildExternalTelemetryRegistry(
	config externaltelemetry.Configuration,
	registrar externaltelemetry.SecretRegistrar,
) (*externaltelemetry.Registry, error) {
	registry := externaltelemetry.NewRegistry()
	factories := []externaltelemetry.Factory{
		externaltelemetry.FakeFactory{},
		adx.Factory{},
	}
	factories = append(factories, connectorapi.RegisteredFactories()...)
	for _, factory := range factories {
		if err := registry.Register(factory); err != nil {
			return nil, err
		}
	}
	for _, connector := range config.Connectors {
		if err := registry.Configure(connector, nil, registrar); err != nil {
			return nil, err
		}
	}
	return registry, nil
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
//
// On Azure DevOps there is no static repo token to resolve (azure-cli auth
// shells out to `az`), so the ADO branch builds a provider straight from
// instance config (adoauth) and routes the work-item mutation to the backlog
// project the PBI lives in — mirroring the provider-chain stages. Without this
// every ADO run's failure/park/escalation handler no-ops (token ref not found),
// leaking the goobers/status:claimed marker and never applying needs-human.
type escalationCommenter struct {
	resolver           credentials.Resolver
	reg                runner.SecretRegistrar
	layout             instance.Layout
	needsHumanAssignee string
}

func (c *escalationCommenter) UpdateWorkItem(ctx context.Context, req providers.UpdateWorkItemRequest) (providers.WorkItem, error) {
	// PR remediation uses pr/<number> as its internal claim key; provider work
	// item endpoints use the shared bare issue/PR number.
	req.ID = blockedLookupID(req.ID)
	req = withNeedsHumanAssignee(req, c.needsHumanAssignee)
	if req.Repository.Provider == providers.ProviderADO {
		provider, err := newADOProviderForStage(c.layout.Root, req.Repository)
		if err != nil {
			return providers.WorkItem{}, fmt.Errorf("build ADO escalation provider for %s/%s: %w", req.Repository.Owner, req.Repository.Name, err)
		}
		req.Repository = backlogRepoRefForGaggle(c.layout, req.Repository)
		return provider.UpdateWorkItem(ctx, req)
	}
	if req.Repository.Provider == providers.ProviderGitea {
		// Gitea authenticates with a static token like GitHub (resolved per call
		// through the rotation-aware resolver), but the mutation must reach the
		// self-hosted forge — newGiteaProviderForStage resolves its BaseURL from
		// instance config. The claim marker is the plain LabelClaimed (as GitHub),
		// so no ADO status-label rewrite is needed, and backlogRepoRefForGaggle is
		// a no-op for gitea (code repo and backlog coincide).
		ref := req.Repository.Owner + "/" + req.Repository.Name
		token, err := c.resolver.Resolve(ctx, ref)
		if err != nil {
			return providers.WorkItem{}, fmt.Errorf("resolve escalation-comment token for %s: %w", ref, err)
		}
		c.reg.Register([]byte(token))
		provider, err := newGiteaProviderForStage(c.layout.Root, req.Repository, token)
		if err != nil {
			return providers.WorkItem{}, fmt.Errorf("build gitea escalation provider for %s: %w", ref, err)
		}
		return provider.UpdateWorkItem(ctx, req)
	}
	ref := req.Repository.Owner + "/" + req.Repository.Name
	token, err := c.resolver.Resolve(ctx, ref)
	if err != nil {
		return providers.WorkItem{}, fmt.Errorf("resolve escalation-comment token for %s: %w", ref, err)
	}
	c.reg.Register([]byte(token))
	return newEscalationPoster(token).UpdateWorkItem(ctx, req)
}

func (c *escalationCommenter) ListComments(ctx context.Context, repository providers.RepositoryRef, itemID string) ([]providers.Comment, error) {
	itemID = blockedLookupID(itemID)
	if repository.Provider == providers.ProviderADO {
		provider, err := newADOProviderForStage(c.layout.Root, repository)
		if err != nil {
			return nil, fmt.Errorf("build ADO escalation provider for %s/%s: %w", repository.Owner, repository.Name, err)
		}
		return provider.ListComments(ctx, backlogRepoRefForGaggle(c.layout, repository), itemID)
	}
	ref := repository.Owner + "/" + repository.Name
	token, err := c.resolver.Resolve(ctx, ref)
	if err != nil {
		return nil, fmt.Errorf("resolve escalation-comment token for %s: %w", ref, err)
	}
	c.reg.Register([]byte(token))
	if repository.Provider == providers.ProviderGitea {
		provider, err := newGiteaProviderForStage(c.layout.Root, repository, token)
		if err != nil {
			return nil, fmt.Errorf("build gitea escalation provider for %s: %w", ref, err)
		}
		return provider.ListComments(ctx, repository, itemID)
	}
	return newEscalationPoster(token).ListComments(ctx, repository, itemID)
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
func buildEscalationNotifier(l instance.Layout, cfg *instance.Config, resolver credentials.Resolver, reg runner.SecretRegistrar) *gate.EscalationNotifier {
	if len(cfg.Repos) == 0 {
		return nil
	}
	return &gate.EscalationNotifier{
		Poster: &escalationCommenter{
			resolver:           resolver,
			reg:                reg,
			layout:             l,
			needsHumanAssignee: cfg.NeedsHumanAssignee,
		},
	}
}

// buildBlockedHandler wires runner.Config.Blocked (#544/#545/#552): the
// instance-level consequences of a stage reporting status "blocked". Returns
// nil when no repo is configured, mirroring buildEscalationNotifier.
// Every blocked driving issue is parked (swap off goobers:ready and the
// provider-visible claim marker) per the #544 ruling / #539 convention. This
// prevents the released claim from making the same item immediately eligible
// again.
//
// The park label depends on whether the stage named a blocker (#2028): a
// named, non-cyclic blocker is goobers:blocked-on-sibling — a self-healing
// dependency park, not a decision only a human can make; the record below is
// what actually self-heals it (filterBlockedEligibility, blockedrecords.go),
// the label just needs to say so. An unattributed block (no blocker named) or
// a detected circular dependency is goobers:needs-human — the runner can't
// resolve either on its own, so it genuinely is a human decision.
//
// When the stage also references blockers through outputs.blockedBy, record
// them in scheduler/blocked.json so #552's selection guard still protects the
// issue if a human re-promotes it before every dependency closes. If a new
// record closes a cycle, every issue in that cycle is parked goobers:needs-human
// and receives a cycle-specific comment for human resolution. The runner's
// shared EscalationNotifier owns the normal explanatory provider comment.
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
		resolver:           resolver,
		reg:                reg,
		layout:             l,
		needsHumanAssignee: cfg.NeedsHumanAssignee,
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
		if blockedRepositoryEmpty(repoRef) {
			return fmt.Errorf("blocked outcome for run %s has no repository", o.RunID)
		}
		// Scope blocked records to the backlog project, not the code repo.
		// Work items live in the gaggle's backlog project (e.g. "Example Backlog"), which
		// is a different ADO project than the code repo ("Example Service").
		// The selection guard (filterBlockedEligibility) evaluates records
		// against the backlog repo, so records must be keyed/stored under the
		// backlog repo or a parked parent is never skipped and gets re-claimed.
		// Idempotent for GitHub (backlog == code repo) and re-applied safely by
		// escalationCommenter before the work-item call.
		repoRef = backlogRepoRefForGaggle(l, repoRef)
		for _, itemID := range itemIDs {
			// #2028: a named blocker is a self-healing dependency park
			// (blocked-on-sibling), not a human decision; only an
			// unattributed block stays needs-human. A detected cycle
			// overrides this below with its own needs-human cycleReq.
			label := providers.LabelNeedsHuman
			if len(o.Blockers) > 0 {
				label = blockedOnSiblingLabel
			}
			req := providers.UpdateWorkItemRequest{
				Repository:   repoRef,
				ID:           itemID,
				AddLabels:    []string{label},
				RemoveLabels: []string{providers.LabelReady, providers.LabelClaimed},
			}
			if len(o.Blockers) > 0 {
				var cycle blockedCycleResult
				if err := updateBlockedRecords(l, func(recs map[string]blockedRecord) bool {
					recordKey := blockedRecordKey(repoRef, itemID)
					recs[recordKey] = blockedRecord{
						Repository: repoRef,
						ItemID:     itemID,
						Blockers:   o.Blockers,
						RunID:      o.RunID,
						Stage:      o.Stage,
						Reason:     o.Reason,
						RecordedAt: time.Now().UTC(),
					}
					cycle = findBlockedCycle(recs, recordKey)
					return true
				}); err != nil {
					errs = append(errs, fmt.Errorf("record block for %s: %w", itemID, err))
				}
				if len(cycle.Affected) > 0 {
					comments := blockedCycleComments(cycle)
					for _, cycleItem := range cycle.Affected {
						for _, comment := range comments {
							cycleReq := providers.UpdateWorkItemRequest{
								Repository:   cycleItem.Repository,
								ID:           cycleItem.ItemID,
								Comment:      comment,
								AddLabels:    []string{providers.LabelNeedsHuman},
								RemoveLabels: []string{providers.LabelReady, providers.LabelClaimed},
							}
							if _, err := poster.UpdateWorkItem(ctx, cycleReq); err != nil {
								errs = append(errs, fmt.Errorf("escalate circular dependency on %s#%s: %w", cycleItem.Repository.Name, cycleItem.ItemID, err))
							}
						}
					}
					continue
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
// the driving item — a comment recording a stable failure code and the run id —
// so repeated terminal failures on the same item accumulate a countable signal
// instead of the item silently returning to goobers:ready with no record.
// Detailed causes remain in the local run trace because execution errors can
// contain harness argv, prompts, credentials, environment values, or context.
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
		resolver:           resolver,
		reg:                reg,
		layout:             l,
		needsHumanAssignee: cfg.NeedsHumanAssignee,
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
		repoRef := providers.RepositoryRef{
			Provider: providers.ProviderKind(o.RepoRef.Provider),
			Owner:    o.RepoRef.Owner,
			Name:     o.RepoRef.Name,
		}
		var errs []error
		for _, itemID := range itemIDs {
			comment := fmt.Sprintf(
				"Goobers run %s terminated `failed` (`RUN_FAILED`). Failure details are available only in the local run trace. The run released its claim and this issue returned to the backlog; this comment records the terminal failure so repeated failures on this item are visible instead of silently recurring. No `%s` applied — a `failed` terminal is distinct from an escalation.",
				o.RunID, providers.LabelNeedsHuman,
			)
			if err := gate.PostRunComment(ctx, poster, repoRef, itemID, o.RunID, o.Seq, comment); err != nil {
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

// issueRefList renders issue numbers as "#441, #442" for provider comments.
func issueRefList(numbers []string) string {
	out := make([]byte, 0, len(numbers)*6)
	for i, n := range numbers {
		if i > 0 {
			out = append(out, ", "...)
		}
		out = append(out, '#')
		out = append(out, n...)
	}
	return string(out)
}

const cyclePathSeparator = " -> "

func issueCyclePath(numbers []string) string {
	var out strings.Builder
	for i, n := range numbers {
		if i > 0 {
			out.WriteString(cyclePathSeparator)
		}
		out.WriteByte('#')
		out.WriteString(n)
	}
	return out.String()
}

func issueCyclePathLength(numbers []string, maxLength int) (int, bool) {
	length := 0
	for i, number := range numbers {
		addition := 1 + len(number)
		if i > 0 {
			addition += len(cyclePathSeparator)
		}
		if addition > maxLength-length {
			return 0, false
		}
		length += addition
	}
	return length, true
}

func boundedIssueCyclePath(numbers []string, maxLength int) (string, bool) {
	if _, fits := issueCyclePathLength(numbers, maxLength); fits {
		return issueCyclePath(numbers), false
	}
	return truncatedIssueCyclePath(numbers, maxLength), true
}

func truncatedIssueCyclePath(numbers []string, maxLength int) string {
	if len(numbers) == 0 || maxLength <= 0 {
		return ""
	}

	bestHead, bestIdentified := 0, -1
	bestTail := false
	prefixLength := 0
	for head := 0; head < len(numbers); head++ {
		consider := func(includeTail bool) {
			omitted := len(numbers) - head
			identified := head
			if includeTail {
				omitted--
				identified++
			}
			if omitted <= 0 {
				return
			}

			length := prefixLength
			if head > 0 {
				length += len(cyclePathSeparator)
			}
			length += len(cycleMembersOmitted(omitted))
			if includeTail {
				length += len(cyclePathSeparator) + 1 + len(numbers[len(numbers)-1])
			}
			if length <= maxLength &&
				(identified > bestIdentified || identified == bestIdentified && head > bestHead) {
				bestHead = head
				bestTail = includeTail
				bestIdentified = identified
			}
		}

		consider(false)
		consider(head < len(numbers)-1)

		addition := 1 + len(numbers[head])
		if head > 0 {
			addition += len(cyclePathSeparator)
		}
		prefixLength += addition
		if prefixLength > maxLength {
			break
		}
	}
	if bestIdentified < 0 {
		return ""
	}

	omitted := len(numbers) - bestHead
	if bestTail {
		omitted--
	}
	parts := make([]string, 0, bestHead+2)
	for _, number := range numbers[:bestHead] {
		parts = append(parts, "#"+number)
	}
	parts = append(parts, cycleMembersOmitted(omitted))
	if bestTail {
		parts = append(parts, "#"+numbers[len(numbers)-1])
	}
	return strings.Join(parts, cyclePathSeparator)
}

func cycleMembersOmitted(count int) string {
	return fmt.Sprintf("[%d cycle members omitted]", count)
}

const maxBlockedCycleCommentLength = 2000

func blockedCycleComment(paths [][]string, morePaths bool) string {
	const prefix = "Goobers detected circular issue dependencies. Representative cycles: "
	const additionalPathsOmitted = "additional cycle paths omitted"
	suffix := fmt.Sprintf(
		". Every issue in the cycle has been marked `%s` and removed from `%s` for human resolution.",
		providers.LabelNeedsHuman, providers.LabelReady,
	)
	available := maxBlockedCycleCommentLength - len(prefix) - len(suffix)
	if summaries, ok := completeCycleSummaries(paths, morePaths, available, additionalPathsOmitted); ok {
		return prefix + summaries + suffix
	}

	var summaries strings.Builder
	included := 0
	for i, path := range paths {
		separatorLength := 0
		if summaries.Len() > 0 {
			separatorLength = 2
		}

		reservedNoticeLength := 0
		if morePaths || i < len(paths)-1 {
			reservedNoticeLength = 2 + len(additionalPathsOmitted)
		}
		pathBudget := available - summaries.Len() - separatorLength - reservedNoticeLength
		summary, truncated := boundedIssueCyclePath(path, pathBudget)
		if summary == "" {
			break
		}
		if separatorLength > 0 {
			summaries.WriteString("; ")
		}
		summaries.WriteString(summary)
		included++
		if truncated {
			break
		}
	}

	if morePaths || included < len(paths) {
		if summaries.Len() > 0 {
			summaries.WriteString("; ")
		}
		summaries.WriteString(additionalPathsOmitted)
	}
	return prefix + summaries.String() + suffix
}

func blockedCycleComments(cycle blockedCycleResult) []string {
	report := blockedCycleComment(cycle.Paths, cycle.MorePaths)
	itemIDs := make([]string, len(cycle.Affected))
	for i, item := range cycle.Affected {
		itemIDs[i] = item.ItemID
	}

	memberList := " Affected issues: " + issueRefList(itemIDs) + "."
	if len(report)+len(memberList) <= maxBlockedCycleCommentLength {
		return []string{report + memberList}
	}

	comments := []string{report}
	const prefix = "Affected issues in this dependency cycle: "
	var current strings.Builder
	current.WriteString(prefix)
	for _, itemID := range itemIDs {
		separator := ""
		if current.Len() > len(prefix) {
			separator = ", "
		}
		reference := "#" + itemID
		if current.Len()+len(separator)+len(reference)+1 > maxBlockedCycleCommentLength {
			current.WriteByte('.')
			comments = append(comments, current.String())
			current.Reset()
			current.WriteString(prefix)
			separator = ""
		}
		current.WriteString(separator)
		current.WriteString(reference)
	}
	if current.Len() > len(prefix) {
		current.WriteByte('.')
		comments = append(comments, current.String())
	}
	return comments
}

func completeCycleSummaries(paths [][]string, morePaths bool, maxLength int, additionalPathsOmitted string) (string, bool) {
	total := 0
	for i, path := range paths {
		separatorLength := 0
		if i > 0 {
			separatorLength = 2
		}
		pathLength, fits := issueCyclePathLength(path, maxLength-total-separatorLength)
		if !fits {
			return "", false
		}
		total += separatorLength + pathLength
	}
	if morePaths {
		separatorLength := 0
		if len(paths) > 0 {
			separatorLength = 2
		}
		if len(additionalPathsOmitted) > maxLength-total-separatorLength {
			return "", false
		}
		total += separatorLength + len(additionalPathsOmitted)
	}

	var summaries strings.Builder
	summaries.Grow(total)
	for i, path := range paths {
		if i > 0 {
			summaries.WriteString("; ")
		}
		summaries.WriteString(issueCyclePath(path))
	}
	if morePaths {
		if summaries.Len() > 0 {
			summaries.WriteString("; ")
		}
		summaries.WriteString(additionalPathsOmitted)
	}
	return summaries.String(), true
}

// newOpenPRProvider builds the GitHub client the open-PR lister polls; a package
// var so tests substitute a fake (mirrors newPRPoller / newEscalationPoster).
var newOpenPRProvider = func(token string, opts ...func(*providers.GitHubProvider)) localscheduler.OpenPRLister {
	return providers.NewGitHubProvider(token, opts...)
}

// resolvingOpenPRLister resolves the org-repo token per poll — honoring
// credentials.Resolver's re-read-on-resolve rotation contract, matching
// buildCIPollExecutor / the escalation notifier — registers it for scrubbing,
// and lists open PR heads through a freshly-authenticated provider. It is the
// OpenPRLister the #353 open-PR-count refresher polls off-tick.
type resolvingOpenPRLister struct {
	ref          string
	resolver     credentials.Resolver
	reg          runner.SecretRegistrar
	schedulerDir string
}

func (l *resolvingOpenPRLister) ListOpenPullRequests(ctx context.Context, repo providers.RepositoryRef) ([]providers.OpenPRSummary, error) {
	token, err := l.resolver.Resolve(ctx, l.ref)
	if err != nil {
		return nil, fmt.Errorf("resolve open-pr-list token for %s: %w", l.ref, err)
	}
	l.reg.Register([]byte(token))
	return newOpenPRProvider(token, apiReadCacheOptionForSnapshot(l.schedulerDir, "")).ListOpenPullRequests(ctx, repo)
}

// buildOpenPRRefresher constructs the #353 open-PR-count refreshers only when
// the instance actually needs them — a repo is configured AND some workflow
// opts into the MaxOpenPRs cap — so an instance that doesn't use the cap grows
// no GitHub poller and needs no token for it. Returns nil otherwise. One
// refresher is built per distinct repo among the capped workflows' gaggle
// projects (#2692), each listing through that repo's OWN owner/name token ref
// (the same binding credentials.RunnerGrants scopes the run path by): a
// gaggle's cap must bind on its own repo's PR count, never the first repo's.
// A gaggle whose project is zero or has no configured binding falls back to
// the first repo — byte-identical to RunnerGrants' first-binding default for
// such gaggles. Only the `up` daemon starts/wires the returned set; a single
// `goobers run` has no accretion to throttle. resolver is a fresh credential
// resolver over cfg (buildCredentials is read-only and idempotent), used only
// to authenticate the polls.
func buildOpenPRRefresher(cfg *instance.Config, workflows []apiv1.Workflow, gaggleProjects map[string]apiv1.RepoRef, reg runner.SecretRegistrar, branchNamespaces map[string]string, schedulerDir string, stores credentials.StoreResolver) (*localscheduler.OpenPRRefresherSet, error) {
	if len(cfg.Repos) == 0 {
		return nil, nil
	}
	cappedGaggles := make(map[string]bool)
	for i := range workflows {
		if workflows[i].Spec.Readiness.MaxOpenPRs > 0 {
			cappedGaggles[workflows[i].Spec.Gaggle] = true
		}
	}
	if len(cappedGaggles) == 0 {
		return nil, nil
	}
	// The refresher polls through providers.GitHubProvider's
	// ListOpenPullRequests, which no other backend implements. Rather than
	// silently building a GitHub client for a non-GitHub repo — which polls
	// api.github.com with that forge's token and fails 401 on every refresh,
	// leaving the cap reading a stale/zero count that quietly mis-admits work —
	// refuse at wiring time so the operator sees the unsupported combination.
	if repoProvider := cfg.Repos[0].Provider; repoProvider != "" && repoProvider != string(providers.ProviderGitHub) {
		return nil, fmt.Errorf("workflow readiness.maxOpenPRs is only supported on github repositories, not %q", repoProvider)
	}
	resolver, _, err := buildCredentials(cfg, stores, "", "", nil, reg)
	if err != nil {
		return nil, fmt.Errorf("build open-pr-list credential resolver: %w", err)
	}
	byRepo := make(map[string]*localscheduler.OpenPRRefresher)
	byGaggle := make(map[string]*localscheduler.OpenPRRefresher, len(cappedGaggles))
	for gaggle := range cappedGaggles {
		repo := cfg.Repos[0]
		if configured, ok := configuredRepoForProject(cfg, gaggleProjects[gaggle]); ok {
			repo = configured
		} else if project := gaggleProjects[gaggle]; project.Owner != "" && project.Name != "" {
			// A project with no configured binding is polled under its own
			// owner/name ref; token resolution fails per-poll and the count
			// stays "unknown" (Admit fails open) instead of silently reading
			// the first repo's PRs.
			repo = instance.RepoRef{Owner: project.Owner, Name: project.Name, Provider: string(project.Provider)}
		}
		if repo.Provider == "ado" {
			// The cap counts GitHub PR heads; an ADO-projected gaggle has no
			// list to poll, so its count stays "unknown" (Admit fails open).
			continue
		}
		key := repo.Owner + "/" + repo.Name
		refresher := byRepo[key]
		if refresher == nil {
			lister := &resolvingOpenPRLister{ref: key, resolver: resolver, reg: reg, schedulerDir: schedulerDir}
			repoRef := providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: repo.Owner, Name: repo.Name}
			// Exclude human-parked PRs from the cap (#986): goobers:merge-escalated is
			// the daemon's "parked pending a human" signal on a PR — it cannot be
			// drained autonomously, so counting it against MaxOpenPRs only starves new
			// implementation work. needs-remediation / blocked-on-sibling are
			// deliberately NOT excluded: the daemon can still drain those (remediation,
			// sibling sequencing), and the cap must keep applying backpressure to them.
			refresher = localscheduler.NewOpenPRRefresher(lister, repoRef, localscheduler.DefaultOpenPRRefreshInterval, []string{remediationEscalatedLabel}, branchNamespaces)
			byRepo[key] = refresher
		}
		byGaggle[gaggle] = refresher
	}
	if len(byGaggle) == 0 {
		return nil, nil
	}
	return localscheduler.NewOpenPRRefresherSet(byGaggle), nil
}

// backlogCounter adapts a provider + repo + label selector into a
// localscheduler.BacklogCounter (#344) — resolves its token per call (like
// escalationCommenter above), honoring credentials.Resolver's re-read-on-
// resolve rotation contract rather than capturing one at daemon startup.
type backlogCounter struct {
	mu             sync.Mutex
	ref            string
	repo           providers.RepositoryRef
	labels         []string
	labelPredicate *labelpredicate.Predicate
	fieldPredicate *fieldpredicate.Predicate
	resolver       credentials.Resolver
	reg            runner.SecretRegistrar
	schedulerDir   string
	// root is the instance root the Gitea arm resolves its forge BaseURL from.
	// The counter polls the repo's declared provider, not GitHub unconditionally:
	// a Gitea instance with a type=backlog-item trigger otherwise counted its
	// backlog against api.github.com and every tick failed 401, permanently
	// wedging that workflow's fan-out at zero eligible items.
	root   string
	quota  *localscheduler.ProviderQuotaState
	cursor string
}

// backlogCountProvider is the single read the counter needs. Both backends
// implement it, so the counter stays provider-neutral once resolved.
type backlogCountProvider interface {
	ListWorkItems(ctx context.Context, req providers.ListWorkItemsRequest) ([]providers.WorkItem, error)
}

// newCounterProvider dispatches on the counted repo's own provider kind. The
// GitHub arm keeps the conditional-GET snapshot read cache and the scheduler's
// quota accounting (both GitHub HTTPClient decorators); the Gitea arm stays
// uncached and unmetered, matching every other Gitea arm in the tree, and
// refunds any prepaid poll reservation immediately since it consumes no GitHub
// quota.
func (b *backlogCounter) newCounterProvider(ctx context.Context) (backlogCountProvider, func(), error) {
	if b.repo.Provider == providers.ProviderGitea {
		token, err := b.resolver.Resolve(ctx, b.ref)
		if err != nil {
			return nil, func() {}, err
		}
		b.reg.Register([]byte(token))
		provider, err := newGiteaProviderForStage(b.root, b.repo, token)
		if err != nil {
			return nil, func() {}, err
		}
		return provider, func() {}, nil
	}
	return newCounterGitHubProvider(ctx, b.ref, b.schedulerDir, b.resolver, b.reg, b.quota)
}

func (b *backlogCounter) EligibleCount(ctx context.Context) (int, error) {
	provider, cleanup, err := b.newCounterProvider(ctx)
	if err != nil {
		return 0, fmt.Errorf("resolve backlog-count token for %s: %w", b.ref, err)
	}
	defer cleanup()

	b.mu.Lock()
	cursor := b.cursor
	b.mu.Unlock()

	const pageSize = 100
	pageInfo := &providers.ListWorkItemsPageInfo{}
	items, err := provider.ListWorkItems(ctx, providers.ListWorkItemsRequest{
		Repository: b.repo, Labels: b.labels, State: "open", Limit: pageSize,
		Cursor: cursor, PageInfo: pageInfo, OldestFirst: true,
	})
	if err != nil {
		return 0, err
	}
	b.mu.Lock()
	if pageInfo.HasNext {
		b.cursor = pageInfo.NextCursor
	} else {
		b.cursor = ""
	}
	b.mu.Unlock()
	count := 0
	for _, item := range items {
		matched, err := b.labelPredicate.Matches(item.Labels)
		if err != nil {
			return 0, fmt.Errorf("evaluate backlog label predicate: %w", err)
		}
		if matched {
			matched, err = b.fieldPredicate.Matches(item.Fields)
			if err != nil {
				return 0, fmt.Errorf("evaluate backlog field predicate: %w", err)
			}
			if matched {
				count++
			}
		}
	}
	return count, nil
}

func newCounterGitHubProvider(
	ctx context.Context,
	ref string,
	schedulerDir string,
	resolver credentials.Resolver,
	reg runner.SecretRegistrar,
	quota *localscheduler.ProviderQuotaState,
) (*providers.GitHubProvider, func(), error) {
	var accounting *providerQuotaAccounting
	if quota != nil {
		accounting = &providerQuotaAccounting{state: quota}
		if reservation, ok := localscheduler.ProviderPollReservationFromContext(ctx); ok {
			accounting.prepaid = &reservation
		}
	}
	cleanup := func() {}
	if accounting != nil {
		cleanup = accounting.RefundUnused
	}

	token, err := resolver.Resolve(ctx, ref)
	if err != nil {
		cleanup()
		return nil, func() {}, err
	}
	reg.Register([]byte(token))
	opts := []func(*providers.GitHubProvider){
		apiReadCacheOptionForSnapshot(schedulerDir, providersnapshot.ID(ctx)),
	}
	if accounting != nil {
		opts = append(opts,
			providers.WithQuotaObserver(accounting),
			providers.WithQuotaRequestGate(accounting),
		)
	}
	// Fail fast on rate limits so polling waits for the scheduler's next
	// reset-aware admission. Transport and 5xx retries remain enabled and each
	// attempt is reserved through the quota gate above.
	opts = append(opts, providers.WithMaxRateLimitRetries(0))
	return newGitHubProvider(token, opts...), cleanup, nil
}

func (b *backlogCounter) ProviderQuotaGuarded() bool {
	return b.quota != nil
}

// buildBacklogCounter wires the daemon-side fan-out counter for a workflow's
// declared type=backlog-item trigger (#344). It counts work items carrying
// every selector key as a GitHub label. The per-run backlog-query stage remains
// the actual claiming mechanism; this only estimates how many runs a Tick
// should fan out to.
// Returns nil (not error) when wf declares no backlog-item trigger, or when
// no repo is configured — mirrors buildCIPollExecutor/buildEscalationNotifier's
// "irrelevant to this workflow" fail-open-to-nil shape, not a real error.
// backlogCounterRepoRef resolves the counted repository, carrying the repo's
// OWN declared provider kind rather than an unconditional GitHub. The kind is
// what newCounterProvider dispatches on, so hard-coding it here sent a Gitea
// instance's backlog count to api.github.com.
func backlogCounterRepoRef(cfg *instance.Config, repoRef apiv1.RepoRef) providers.RepositoryRef {
	provider := providers.ProviderGitHub
	if len(cfg.Repos) > 0 && cfg.Repos[0].Provider != "" {
		provider = providers.ProviderKind(cfg.Repos[0].Provider)
	}
	return providers.RepositoryRef{Provider: provider, Owner: repoRef.Owner, Name: repoRef.Name}
}

func buildBacklogCounter(cfg *instance.Config, gaggle apiv1.Gaggle, wf *apiv1.Workflow, repoRef apiv1.RepoRef, resolver credentials.Resolver, reg runner.SecretRegistrar, schedulerDir string, quota *localscheduler.ProviderQuotaState, root string) (localscheduler.BacklogCounter, error) {
	if len(cfg.Repos) == 0 {
		return nil, nil
	}
	var selector map[string]string
	var expression string
	var fieldExpression string
	found := false
	for _, tr := range wf.Spec.Triggers {
		if tr.Type == apiv1.TriggerBacklogItem {
			selector = tr.Selector
			expression = tr.LabelPredicate
			fieldExpression = tr.FieldPredicate
			found = true
			break
		}
	}
	if !found {
		return nil, nil
	}
	labels := make([]string, 0, len(selector))
	for k := range selector {
		labels = append(labels, k)
	}
	sort.Strings(labels)
	predicate, err := labelpredicate.Compile(expression, labels, nil)
	if err != nil {
		return nil, fmt.Errorf("workflow %q backlog label predicate: %w", wf.Name, err)
	}
	fieldPredicate, err := fieldpredicate.CompileConjunction(gaggle.Spec.Backlog.FieldPredicate, fieldExpression)
	if err != nil {
		return nil, fmt.Errorf("workflow %q backlog field predicate: %w", wf.Name, err)
	}
	counter := &backlogCounter{
		// ref must follow the workflow's own repo (#2692 sibling): the query
		// below targets repoRef, so its token must resolve from the same
		// owner/name binding — matching buildScheduleDemandCounter.
		ref:            repoRef.Owner + "/" + repoRef.Name,
		repo:           backlogCounterRepoRef(cfg, repoRef),
		labels:         labels,
		labelPredicate: predicate,
		fieldPredicate: fieldPredicate,
		resolver:       resolver,
		reg:            reg,
		schedulerDir:   schedulerDir,
		root:           root,
	}
	if quota != nil {
		counter.quota = quota
	}
	return counter, nil
}

// buildScheduleDemandCounter recognizes the built-in update-behind-pr selector
// and sizes each due schedule tick to its unclaimed eligible PR set.
func buildScheduleDemandCounter(
	cfg *instance.Config,
	wf *apiv1.Workflow,
	repoRef apiv1.RepoRef,
	resolver credentials.Resolver,
	reg runner.SecretRegistrar,
	schedulerDir, branchNamespace string,
	quota *localscheduler.ProviderQuotaState,
) localscheduler.BacklogCounter {
	if len(cfg.Repos) == 0 {
		return nil
	}
	hasSchedule := false
	for _, trigger := range wf.Spec.Triggers {
		if trigger.Type == apiv1.TriggerSchedule {
			hasSchedule = true
			break
		}
	}
	base, headPrefix, ok := remediationCounterScope(wf, branchNamespace)
	if !hasSchedule || !ok {
		return nil
	}
	return &remediationDemandCounter{
		ref:          repoRef.Owner + "/" + repoRef.Name,
		repo:         providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: repoRef.Owner, Name: repoRef.Name},
		base:         base,
		headPrefix:   headPrefix,
		gaggle:       wf.Spec.Gaggle,
		resolver:     resolver,
		reg:          reg,
		schedulerDir: schedulerDir,
		quota:        quota,
	}
}

func remediationCounterScope(wf *apiv1.Workflow, branchNamespace string) (base, headPrefix string, ok bool) {
	for _, task := range wf.Spec.Tasks {
		if task.Name != wf.Spec.Start || task.Run == nil ||
			len(task.Run.Command) != 2 ||
			task.Run.Command[0] != "goobers" ||
			task.Run.Command[1] != "update-behind-pr" {
			continue
		}
		base = task.Inputs["base"]
		if base == "" {
			base = "main"
		}
		headPrefix = task.Inputs["headPrefix"]
		if headPrefix == "" {
			headPrefix = providers.NormalizeBranchNamespace(branchNamespace)
		}
		return base, headPrefix, true
	}
	return "", "", false
}

type providerQuotaAccounting struct {
	mu          sync.Mutex
	state       *localscheduler.ProviderQuotaState
	prepaid     *localscheduler.ProviderPollReservation
	outstanding []localscheduler.ProviderPollReservation
}

func (a *providerQuotaAccounting) AcquireQuotaRequest(_ context.Context, provider providers.ProviderKind) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.prepaid != nil {
		a.outstanding = append(a.outstanding, *a.prepaid)
		a.prepaid = nil
		return nil
	}
	decision := a.state.ReserveCurrentPolls(apiv1.Provider(provider), 1)
	if decision.Allowed == 0 {
		return &localscheduler.ProviderPollBudgetError{
			Provider:  decision.Provider,
			Remaining: decision.RemainingBefore,
			Requested: 1,
			ResetAt:   decision.ResetAt,
		}
	}
	reservation, _ := decision.Reservation()
	a.outstanding = append(a.outstanding, reservation)
	return nil
}

func (a *providerQuotaAccounting) ObserveQuota(_ context.Context, observation providers.QuotaObservation) {
	a.mu.Lock()
	var reservation localscheduler.ProviderPollReservation
	if len(a.outstanding) > 0 {
		reservation = a.outstanding[0]
		a.outstanding = a.outstanding[1:]
	}
	a.mu.Unlock()

	provider := apiv1.Provider(observation.Provider)
	if observation.Cached {
		a.state.RefundReservation(reservation)
		return
	}
	if observation.Known {
		a.state.Record(provider, observation.Remaining, observation.Reset)
	}
}

func (a *providerQuotaAccounting) RefundUnused() {
	a.mu.Lock()
	if a.prepaid == nil {
		a.mu.Unlock()
		return
	}
	reservation := *a.prepaid
	a.prepaid = nil
	a.mu.Unlock()
	a.state.RefundReservation(reservation)
}

// instructionsPath resolves a goober's Instructions field to an absolute
// file path. Instructions is documented as "relative to the goober
// definition directory" (api/v1alpha1.GooberSpec), which config-as-code
// objects don't retain after instance.LoadConfigDir flattens them into a
// ConfigSet — but every shipped config (internal/instance/starter,
// config-examples/, reference-workflows/) lays goobers out at the same fixed path, so
// that layout convention is reproduced here rather than widening ConfigSet's
// shape for this one field.
func gooberDefinitionDir(configDir string, spec apiv1.GooberSpec, gooberName string) string {
	return filepath.Join(configDir, "gaggles", spec.Gaggle, "goobers", gooberName)
}

func instructionsPath(configDir string, spec apiv1.GooberSpec, gooberName string) string {
	return filepath.Join(gooberDefinitionDir(configDir, spec, gooberName), spec.Instructions)
}

func adoRemoteGitQuotaGate(state *localscheduler.ProviderQuotaState) func(context.Context, string) error {
	if state == nil {
		return nil
	}
	return func(_ context.Context, repoURL string) error {
		if !isADORemote(repoURL) {
			return nil
		}
		decision := state.ReservePolls(apiv1.ProviderADO, time.Now(), 1)
		if decision.Allowed != 0 {
			return nil
		}
		return &localscheduler.ProviderPollBudgetError{
			Provider:  decision.Provider,
			Remaining: decision.RemainingBefore,
			Requested: 1,
			ResetAt:   decision.ResetAt,
		}
	}
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
func buildRunnerConfig(l instance.Layout, cfg *instance.Config, goobers map[string]apiv1.GooberSpec, instructionsByGoober map[string]string, tel *telemetry.Client, sharedReg *journal.RegistryScrubber, wtMgr *worktree.Manager, branchNamespaces map[string]string, gaggleProject apiv1.RepoRef, additionalRepos []apiv1.RepoRef, harnessInfo harnessPreflightInfo, stores credentials.StoreResolver, sandboxPosture instance.SandboxPosture, providerQuota *localscheduler.ProviderQuotaState) (runner.Config, *worktree.Manager, error) {
	// Per-gaggle credential scoping (MGV-5, #1012): this runner serves one
	// gaggle, so its stages are granted that gaggle's own project-repo token —
	// not an instance-wide default. gaggleProject is zero for a single-gaggle /
	// legacy instance, which falls back to the first repo's token unchanged.
	// Computed before the worktree Manager so its per-repo git-auth resolver can
	// back each read-only reference-repo clone with that repo's contents:read
	// token (MGV-10/#1285, consumed by MGV-11/#1286).
	gaggleOwner := gaggleProject.Owner
	if gaggleProject.Provider == apiv1.ProviderADO && gaggleProject.Project != "" {
		gaggleOwner += "/" + gaggleProject.Project
	}
	resolver, grants, err := buildCredentials(cfg, stores, gaggleOwner, gaggleProject.Name, additionalRepos, sharedReg)
	if err != nil {
		return runner.Config{}, nil, err
	}
	deterministicGrants := deterministicCredentialGrants(grants)
	// The clone-URL derivation the runner will use (the test seam when set, else
	// the runner default) — the worktree auth resolver must key on the identical
	// URLs the runner hands WorkingCopy.
	cloneURLFn := repoCloneURL
	if cloneURLFn == nil {
		cloneURLFn = runner.DefaultRepoCloneURL
	}
	pathLimits, pathLimitsErr := pathLengthManagerLimits(cfg, cloneURLFn, runtime.GOOS)
	if pathLimitsErr != nil {
		return runner.Config{}, nil, pathLimitsErr
	}
	configuredProject, projectConfigured := configuredRepoForProject(cfg, gaggleProject)
	pinned := projectConfigured && configuredProject.Pinned()
	if pinned && len(additionalRepos) > 0 {
		return runner.Config{}, nil, fmt.Errorf("VER: pinned workspace for %s/%s cannot be combined with additional repository worktrees", gaggleProject.Owner, gaggleProject.Name)
	}
	workcopiesRoot := l.WorkcopiesDir()
	if pinned {
		workcopiesRoot = l.WorkcopiesBaseDir()
	}
	absoluteWorkcopiesRoot, err := filepath.Abs(workcopiesRoot)
	if err != nil {
		return runner.Config{}, nil, fmt.Errorf("resolve workcopies root: %w", err)
	}
	if wtMgr != nil && wtMgr.Root != absoluteWorkcopiesRoot {
		// A config reload may switch this repo into or out of pinned mode; do
		// not retain a manager rooted in the opposite lifecycle namespace.
		wtMgr = nil
	}
	if wtMgr == nil {
		var err error
		// This layout is gaggle-scoped (l.ForGaggle) in the daemon; its Manager
		// serves only this gaggle's runs, so its mirror-fetch exclusion is
		// seeded with just this gaggle's run-branch namespace. A missing/empty
		// entry leaves the default "goobers/" in place (WithRunBranchNamespaces
		// drops empties), so a single-gaggle default instance is unchanged.
		managerOptions := []worktree.ManagerOption{
			worktree.WithRunBranchNamespaces(branchNamespaces[l.Gaggle()]),
			worktree.WithPinnedRoot(l.WorkcopiesBaseDir()),
		}
		for repoURL, limit := range pathLimits {
			managerOptions = append(managerOptions, worktree.WithPathLengthLimit(repoURL, limit))
		}
		if gitQuotaGate := adoRemoteGitQuotaGate(providerQuota); gitQuotaGate != nil {
			managerOptions = append(managerOptions, worktree.WithRemoteGitGate(gitQuotaGate))
		}
		if cfg.PartialCloneEnabled() {
			managerOptions = append(managerOptions, worktree.WithPartialClone())
		}
		if cfg.ObjectCacheEnabled() {
			managerOptions = append(managerOptions, worktree.WithObjectCache())
		}
		gitEnv, gitEnvErr := buildWorktreeGitEnv(cfg, absoluteWorkcopiesRoot, gaggleProject, additionalRepos, resolver, grants, cloneURLFn, sharedReg, stores)
		if gitEnvErr != nil {
			return runner.Config{}, nil, gitEnvErr
		}
		if gitEnv != nil {
			managerOptions = append(managerOptions, worktree.WithGitEnvironment(gitEnv))
		}
		if tel != nil {
			managerOptions = append(managerOptions, worktree.WithUsageObserver(l.Gaggle(), tel.RecordWorkcopyUsage))
		}
		wtMgr, err = worktree.NewManager(absoluteWorkcopiesRoot, managerOptions...)
		if err != nil {
			return runner.Config{}, nil, fmt.Errorf("new worktree manager: %w", err)
		}
	}
	if _, err := buildExternalTelemetryRegistry(cfg.ExternalTelemetry, sharedReg); err != nil {
		return runner.Config{}, nil, fmt.Errorf("preflight external telemetry connectors: %w", err)
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
	adapterRegistry, err := buildHarnessRegistry(envCaps, cfg.Runner.EnvPassthrough, cfg.Runner.HarnessCommand, instanceRoot, selfBin)
	if err != nil {
		return runner.Config{}, nil, err
	}
	assetsByGoober := make(map[string]*gooberassets.Bundle, len(goobers))
	for name, spec := range goobers {
		if _, ok := instructionsByGoober[name]; !ok {
			return runner.Config{}, nil, fmt.Errorf("goober %q has no resolved instructions", name)
		}
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
	agentProvenance := make(map[string]runner.AgentProvenance, len(goobers))
	for name, spec := range goobers {
		if len(spec.Capabilities) > 0 {
			gateGooberCaps[name] = append([]string(nil), spec.Capabilities...)
		}
		harnessName := spec.Harness
		if harnessName == "" {
			harnessName = apiv1.HarnessCopilot
		}
		agentProvenance[name] = runner.AgentProvenance{
			Model:          spec.Model,
			HarnessVersion: harnessInfo[harnessName].Version,
		}
	}

	rc := runner.Config{
		RunControls: cfg.RunConditions.RunControls(),
		NewDeterministic: func(rec runner.ArtifactRecorder, reg runner.SecretRegistrar) (invoke.Deterministic, error) {
			// Register resolved secrets into the run's own registrar AND the
			// instance-global shared registry, so they are scrubbed from the run
			// journal (via reg) and from the span exporter / instance log (via
			// sharedReg) alike (#117 Piece B).
			reg = teeRegistrar{run: reg, shared: sharedReg}
			injector, err := credentials.NewInjector(resolver, deterministicGrants, reg)
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
			// Additional ambient env vars this instance opts into passing through
			// to every deterministic stage, on top of the built-in procenv
			// allowlist (#736) — the executor twin of the harness adapter's
			// ExtraEnvAllowlist, from the same cfg value so the two never drift.
			shell.ExtraEnvAllowlist = cfg.Runner.EnvPassthrough
			if projectConfigured && configuredProject.LargeRepo {
				shell.DefaultEnv = map[string]string{"MSBUILDDISABLENODEREUSE": "1"}
			}
			// Baseline deadline for a stage that declares no timeoutSeconds
			// (#1969). Zero leaves executor.DefaultTimeout in force, so an
			// instance that configures nothing is unchanged.
			defaultStageTimeoutSetting := cfg.Runner.DefaultStageTimeout
			if projectConfigured {
				defaultStageTimeoutSetting = configuredProject.EffectiveDefaultStageTimeout(defaultStageTimeoutSetting)
			}
			defaultStageTimeout, err := (instance.RunnerConfig{DefaultStageTimeout: defaultStageTimeoutSetting}).DefaultStageTimeoutDuration()
			if err != nil {
				return nil, err
			}
			shell.DefaultTimeout = defaultStageTimeout
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

			kinds := executor.NewKindRegistry()
			if err := kinds.Register(executor.KindShell, shell); err != nil {
				return nil, err
			}
			var adoRepo *instance.RepoRef
			if r, ok := adoRepoForGaggle(cfg, gaggleProject); ok {
				adoRepo = &r
			}
			var giteaRepo *instance.RepoRef
			if r, ok := giteaRepoForGaggle(cfg, gaggleProject); ok {
				giteaRepo = &r
			}
			ciPoll, err := buildCIPollExecutor(cfg, injector, rec, adoRepo, giteaRepo, reg, providerQuota)
			if err != nil {
				return nil, err
			}
			if err := kinds.Register(executor.KindCIPoll, ciPoll); err != nil {
				return nil, err
			}
			telemetryQuery, err := buildExternalTelemetryExecutor(cfg.ExternalTelemetry, rec, reg)
			if err != nil {
				return nil, err
			}
			if err := kinds.Register(executor.KindExternalTelemetry, telemetryQuery); err != nil {
				return nil, err
			}
			return executor.NewTaskExecutor(kinds)
		},
		NewAgentic: func(gooberName string, rec runner.ArtifactRecorder, reg runner.SecretRegistrar) (invoke.Goober, error) {
			spec, ok := goobers[gooberName]
			if !ok {
				return nil, fmt.Errorf("goober %q not found in config", gooberName)
			}
			harnessName := spec.Harness
			if harnessName == "" {
				harnessName = apiv1.HarnessCopilot
			}
			if err := mcpconfig.ValidateForHarness(harnessName, spec.MCPServers, spec.Capabilities, spec.Tools); err != nil {
				return nil, fmt.Errorf("validate goober %q MCP config: %w", gooberName, err)
			}
			// The injector registers resolved secrets into the run's registrar AND
			// the shared instance registry (#117 Piece B). reg (not the tee) is
			// kept below for the journal.Scrubber assertion — it still accumulates
			// every secret, since the tee forwards to it.
			credentialKeys := append([]string(nil), spec.Capabilities...)
			credentialKeys = append(credentialKeys, mcpconfig.BYOCredentialKeys(spec.MCPServers)...)
			gooberGrants := buildGooberCredentialGrants(gooberName, credentialKeys, grants)
			injector, err := credentials.NewGooberInjectorWithCredentialKeys(
				resolver,
				gooberName,
				gooberGrants,
				mcpconfig.BYOCredentialKeys(spec.MCPServers),
				teeRegistrar{run: reg, shared: sharedReg},
			)
			if err != nil {
				return nil, err
			}
			adapter, err := adapterRegistry.Get(string(harnessName))
			if err != nil {
				return nil, fmt.Errorf("resolve goober %q harness: %w", gooberName, err)
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
				harness.WithHarnessVersion(harnessInfo[harnessName].Version),
				harness.WithAssetBundle(assetsByGoober[gooberName]),
				harness.WithMCPServers(spec.MCPServers),
				harness.WithTools(spec.Tools),
			}
			// Goober-level default timeout (#1070): raises this goober's built-in
			// 30m harness bound so its bigger tasks aren't cut off, without
			// annotating every stage. A stage's own Task.TimeoutSeconds still
			// wins via env.Limits (invocationTimeout); this only moves the
			// fallback that applies when a stage sets none.
			if spec.TimeoutSeconds > 0 {
				opts = append(opts, harness.WithTimeout(time.Duration(spec.TimeoutSeconds)*time.Second))
			}
			// Opt-in agentic sandbox enforcement (S3/#166, #1305): this
			// gaggle's effective posture, resolved once at the composition
			// root (instance.EffectiveAgenticSandbox). The default posture,
			// disabled, adds no option — the executor and adapter behave
			// byte-identically to an instance with no sandbox config at all.
			if sandboxPosture == instance.SandboxEnforced {
				opts = append(opts, harness.WithSandboxEnforcement())
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
		Automated:         gate.NewAutomatedEvaluator(),
		Worktrees:         wtMgr,
		PinnedWorkspace:   pinned,
		PinnedCleanPolicy: configuredProject.WorkspaceCleanPolicy(),
		// Resolve each run's branch namespace from its gaggle (StartInput.Gaggle),
		// so the run branch, the mirror-fetch exclusion above, and the stage
		// env's GOOBERS_BRANCH_NAMESPACE all agree (#965/#1010). Absent/empty
		// entries fall back to providers.DefaultBranchNamespace in the runner.
		BranchNamespaces: branchNamespaces,
		ScratchDir:       filepath.Join(l.WorkcopiesDir(), "scratch"),
		RunsDir:          l.RunsDir(),
		RepoCloneURL:     repoCloneURL,
		// The gaggle's read-only reference repos (MGV-11 #1286): the runner
		// provisions a read-only checkout of each alongside a repo-workspace
		// stage's primary worktree. Empty for a single-repo gaggle (unchanged).
		AdditionalRepos:        additionalRepos,
		GateGooberCapabilities: gateGooberCaps,
		AgentProvenance:        agentProvenance,
		// Wire the escalation notifier (#312) so a repass-budget escalation
		// actually comments on the driving issue; nil for a repo-less instance.
		Escalation: buildEscalationNotifier(l, cfg, resolver, sharedReg),
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
		// PATH-preflight the local-ci stage's configured ciCommand (#1380) for
		// a real daemon run. Left nil in every runner-package test and any
		// embedder that doesn't want it (Config.LookPathFunc's doc comment) —
		// this is the one place that actually wants a host PATH check.
		LookPathFunc: exec.LookPath,
	}
	if tel != nil {
		rc.Telemetry = tel
	}
	wtMgr.SetPathLengthLimits(pathLimits)
	return rc, wtMgr, nil
}

func pathLengthManagerLimits(cfg *instance.Config, cloneURL func(apiv1.RepoRef) (string, error), goos string) (map[string]worktree.PathLengthLimit, error) {
	limits := make(map[string]worktree.PathLengthLimit)
	for i, repo := range cfg.Repos {
		if repo.PathLength != nil && repo.PathLength.Disabled {
			continue
		}
		if repo.PathLength == nil && goos != "windows" {
			continue
		}
		url, err := cloneURL(apiv1.RepoRef{
			Provider: apiv1.Provider(repo.Provider),
			BaseURL:  repo.BaseURL,
			Owner:    repo.Owner,
			Project:  repo.Project,
			Name:     repo.Name,
		})
		if err != nil {
			return nil, fmt.Errorf("repos[%d] (%s/%s): resolve clone URL for path-length preflight: %w", i, repo.Owner, repo.Name, err)
		}
		limit := worktree.PathLengthLimit{MaxPathLength: worktree.DefaultMaxPathLength}
		if repo.PathLength != nil {
			if repo.PathLength.MaxPathLength != 0 {
				limit.MaxPathLength = repo.PathLength.MaxPathLength
			}
			limit.BuildOutputAllowance = repo.PathLength.BuildOutputAllowance
		}
		limits[url] = limit
	}
	return limits, nil
}

func configuredRepoForProject(cfg *instance.Config, project apiv1.RepoRef) (instance.RepoRef, bool) {
	if cfg == nil {
		return instance.RepoRef{}, false
	}
	if project.Provider == apiv1.ProviderADO {
		if repo, ok := adoRepoForGaggle(cfg, project); ok {
			return repo, true
		}
	}
	for _, repo := range cfg.Repos {
		if repo.Provider == string(project.Provider) && repo.Owner == project.Owner &&
			repo.Project == project.Project && repo.Name == project.Name {
			return repo, true
		}
	}
	if len(cfg.Repos) == 1 && project.Owner == "" && project.Name == "" {
		return cfg.Repos[0], true
	}
	return instance.RepoRef{}, false
}

func adoRepoForGaggle(cfg *instance.Config, project apiv1.RepoRef) (instance.RepoRef, bool) {
	if cfg == nil {
		return instance.RepoRef{}, false
	}
	if project.Provider == "" && len(cfg.Repos) == 1 && cfg.Repos[0].Provider == "ado" {
		return cfg.Repos[0], true
	}
	if project.Provider != apiv1.ProviderADO {
		return instance.RepoRef{}, false
	}
	organization := project.Owner
	projectName := project.Project
	if projectName == "" {
		organization, projectName, _ = strings.Cut(project.Owner, "/")
	}
	for _, repo := range cfg.Repos {
		if repo.Provider == "ado" && repo.Owner == organization && repo.Project == projectName && repo.Name == project.Name {
			return repo, true
		}
	}
	return instance.RepoRef{}, false
}

// githubRepoForGaggle is adoRepoForGaggle's GitHub counterpart: the instance
// repo backing this gaggle's project, resolved so its configured token can
// authenticate mirror clone/fetch (#667).
func githubRepoForGaggle(cfg *instance.Config, project apiv1.RepoRef) (instance.RepoRef, bool) {
	if cfg == nil {
		return instance.RepoRef{}, false
	}
	if project.Provider == "" && len(cfg.Repos) == 1 && cfg.Repos[0].Provider == "github" {
		return cfg.Repos[0], true
	}
	if project.Provider != apiv1.ProviderGitHub {
		return instance.RepoRef{}, false
	}
	for _, repo := range cfg.Repos {
		if repo.Provider == "github" && repo.Owner == project.Owner && repo.Name == project.Name {
			return repo, true
		}
	}
	return instance.RepoRef{}, false
}

// githubWorktreeGitEnvironment builds the worktree.WithGitEnvironment resolver
// that authenticates mirror clone/fetch of a GitHub repo with its configured
// credential (#667), via the secret-free askpass helper — the token only ever
// exists in the git child process's environment, never on disk or argv.
//
// A repo with no credential returns a nil resolver and writes nothing: a
// public-repo instance keeps today's unauthenticated child environment, byte
// for byte. With a token ref configured the resolver re-resolves it on every
// clone/fetch (rotation without restart, matching the env/file resolver's
// contract); a github-app repo (#686) mints per operation instead, so a
// refreshed installation token flows into the next fetch with no worktree
// changes. A store-backed token ref (#683) resolves through stores like
// env/file refs. All three fail closed — an unresolvable ref or failed mint
// aborts provisioning rather than falling back to an anonymous fetch, and
// GIT_TERMINAL_PROMPT=0 turns a rejected credential into an immediate error
// instead of an interactive hang. The token is scoped to the configured repo:
// any other remote URL the manager is ever pointed at gets the ambient
// (unauthenticated) environment.
func githubWorktreeGitEnvironment(workcopiesDir string, repo instance.RepoRef, registrar credentials.SecretRegistrar, stores credentials.StoreResolver) (func(context.Context, string) ([]string, error), error) {
	var resolve credentials.ResolveFunc
	switch {
	case repo.GitHubAppAuth():
		// The minting source registers tokens with registrar itself, at mint
		// time — before any consumer (including this one) sees the value.
		mint, err := newGitHubAppTokenSource(repo, registrar, stores)
		if err != nil {
			return nil, err
		}
		resolve = mint
	case repo.Token.Configured():
		// A static token ref (env|file|store) resolves through stores; a
		// store-backed ref can never fall into the unauthenticated arm because
		// Configured() counts it as a source and resolver construction fails
		// closed without store support.
		refName := repo.Owner + "/" + repo.Name
		resolver, err := credentials.NewResolverWithStores([]credentials.TokenRef{repo.Token.CredentialTokenRef(refName)}, stores)
		if err != nil {
			return nil, err
		}
		resolve = func(ctx context.Context) (string, error) {
			return resolver.Resolve(ctx, refName)
		}
	default:
		return nil, nil
	}
	askpass, err := credentials.WriteAskpassScript(filepath.Join(workcopiesDir, "auth"))
	if err != nil {
		return nil, err
	}
	cloneURL := fmt.Sprintf("https://github.com/%s/%s.git", repo.Owner, repo.Name)
	return func(ctx context.Context, repoURL string) ([]string, error) {
		if !sameGitRemote(repoURL, cloneURL) {
			return nil, nil
		}
		token, err := resolve(ctx)
		if err != nil {
			return nil, err
		}
		if registrar != nil {
			registrar.Register([]byte(token))
		}
		return credentials.GitAuthEnvironment(askpass, token), nil
	}, nil
}

// giteaRepoForGaggle returns the instance Gitea repo config backing a gaggle's
// project repo, mirroring githubRepoForGaggle. A single-repo instance with an
// unspecified project provider resolves to its sole Gitea repo.
func giteaRepoForGaggle(cfg *instance.Config, project apiv1.RepoRef) (instance.RepoRef, bool) {
	if cfg == nil {
		return instance.RepoRef{}, false
	}
	if project.Provider == "" && len(cfg.Repos) == 1 && cfg.Repos[0].Provider == "gitea" {
		return cfg.Repos[0], true
	}
	if project.Provider != apiv1.ProviderGitea {
		return instance.RepoRef{}, false
	}
	for _, repo := range cfg.Repos {
		if repo.Provider == "gitea" && repo.Owner == project.Owner && repo.Name == project.Name {
			return repo, true
		}
	}
	return instance.RepoRef{}, false
}

// giteaWorktreeGitEnvironment builds the worktree.WithGitEnvironment resolver
// that authenticates mirror clone/fetch and run-branch push of a Gitea repo
// with its configured token, scoped to the repo's smart-HTTP clone URL
// (<baseURL>/<owner>/<name>.git — the same URL defaultRepoCloneURL derives).
// Gitea has no app-install auth, so only a static/store-backed token applies;
// returns (nil, nil) when the repo carries no configured token (public read,
// ambient). The token is resolved per call and never persisted.
func giteaWorktreeGitEnvironment(repo instance.RepoRef, registrar providers.SecretRegistrar, stores credentials.StoreResolver) (func(context.Context, string) ([]string, error), error) {
	if !repo.Token.Configured() {
		return nil, nil
	}
	base := strings.TrimSuffix(strings.TrimSpace(repo.BaseURL), "/")
	if base == "" {
		return nil, fmt.Errorf("gitea repo %s/%s requires baseUrl for worktree authentication", repo.Owner, repo.Name)
	}
	refName := repo.Owner + "/" + repo.Name
	resolver, err := credentials.NewResolverWithStores([]credentials.TokenRef{repo.Token.CredentialTokenRef(refName)}, stores)
	if err != nil {
		return nil, err
	}
	cloneURL := fmt.Sprintf("%s/%s/%s.git", base, repo.Owner, repo.Name)
	return func(ctx context.Context, repoURL string) ([]string, error) {
		if !sameGitRemote(repoURL, cloneURL) {
			return nil, nil
		}
		token, err := resolver.Resolve(ctx, refName)
		if err != nil {
			return nil, err
		}
		return providers.GiteaGitAuthEnvironment(token, repoURL, registrar), nil
	}, nil
}

// sameGitRemote reports whether two https remote URLs name the same repo,
// tolerating the cosmetic variance git remotes carry: an optional .git
// suffix, a trailing slash, and case (GitHub owner/name are case-insensitive).
func sameGitRemote(a, b string) bool {
	normalize := func(u string) string {
		u = strings.TrimRight(strings.TrimSpace(u), "/")
		u = strings.TrimSuffix(u, ".git")
		return strings.ToLower(u)
	}
	return normalize(a) == normalize(b)
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

type gooberHarnessConfigError struct {
	Goober string
	Err    error
}

func (e *gooberHarnessConfigError) Error() string {
	return fmt.Sprintf("validate goober %q harness config: %v", e.Goober, e.Err)
}

func (e *gooberHarnessConfigError) Unwrap() error {
	return e.Err
}

type gooberHarnessWarning struct {
	Goober  string
	Warning harness.ConfigWarning
}

type workflowCompileError struct {
	Gaggle   string
	Workflow string
	Err      error
}

func (e *workflowCompileError) Error() string {
	return fmt.Sprintf("compile workflow %q: %v", e.Workflow, e.Err)
}

func (e *workflowCompileError) Unwrap() error {
	return e.Err
}

// compiledMachinesWithWarnings compiles every workflow in set,
// admission-checked against goobers (capabilities, harness, gate-outcome
// coverage, and known automated check names — #124), keyed by gaggle and
// workflow name. WorkflowVersion is registry-assigned (per-name monotonic,
// WF-016); no registry is wired at the instance level yet, so this pins
// version 1 for every workflow, matching run.go's existing limitation until a
// follow-up introduces one.
func compiledMachinesWithWarnings(set *instance.ConfigSet, goobers map[string]apiv1.GooberSpec, envPassthrough []string, harnessCommand map[string][]string) (map[localscheduler.WorkflowIdentity]*workflow.Machine, map[string]apiv1.GooberSpec, []gooberHarnessWarning, error) {
	const workflowVersion = 1
	knownChecks := knownAutomatedCheckNames()
	allowPreview := set.Manifest != nil && workflow.PreviewFeaturesEnabled(set.Manifest.Annotations)
	// The admission registry resolves harness config (model/options), and model
	// resolution spawns the configured launcher for model discovery whenever a
	// goober declares spec.Model — so the launcher override must apply here too,
	// or admission probes the wrong runtime (bare copilot on a wrapper-only
	// host, or a divergent bare install beside the wrapper).
	adapterRegistry, err := buildHarnessRegistry(nil, envPassthrough, harnessCommand, "", "")
	if err != nil {
		return nil, nil, nil, err
	}
	resolvedGoobers, warnings, err := admitGooberHarnessConfigs(adapterRegistry, goobers)
	if err != nil {
		return nil, nil, nil, err
	}
	machines := make(map[localscheduler.WorkflowIdentity]*workflow.Machine, len(set.Workflows))
	for i := range set.Workflows {
		wf := &set.Workflows[i]
		m, err := workflow.Compile(
			workflow.Definition{
				Name: wf.Name, Version: workflowVersion, DSLVersion: wf.DSLVersion, Spec: wf.Spec,
			},
			workflow.WithGoobers(goobers),
			workflow.WithKnownChecks(knownChecks),
			workflow.WithKnownHarnesses(adapterRegistry.Names()),
			workflow.WithPreviewFeatures(allowPreview),
		)
		if err != nil {
			return nil, nil, nil, &workflowCompileError{Gaggle: wf.Spec.Gaggle, Workflow: wf.Name, Err: err}
		}
		machines[localscheduler.WorkflowIdentity{Gaggle: wf.Spec.Gaggle, Workflow: wf.Name}] = m
	}
	return machines, resolvedGoobers, warnings, nil
}

func admitGooberHarnessConfigs(adapterRegistry *harness.Registry, goobers map[string]apiv1.GooberSpec) (map[string]apiv1.GooberSpec, []gooberHarnessWarning, error) {
	gooberNames := make([]string, 0, len(goobers))
	for name := range goobers {
		gooberNames = append(gooberNames, name)
	}
	sort.Strings(gooberNames)
	resolvedGoobers := make(map[string]apiv1.GooberSpec, len(goobers))
	var warnings []gooberHarnessWarning
	for _, name := range gooberNames {
		spec := goobers[name]
		harnessName := spec.Harness
		if harnessName == "" {
			harnessName = apiv1.HarnessCopilot
		}
		resolution, err := adapterRegistry.ResolveConfig(string(harnessName), spec.Model, spec.HarnessOptions)
		if err != nil {
			return nil, nil, &gooberHarnessConfigError{Goober: name, Err: err}
		}
		spec.Model = resolution.Model
		spec.HarnessOptions = resolution.HarnessOptions
		resolvedGoobers[name] = spec
		for _, warning := range resolution.Warnings {
			warnings = append(warnings, gooberHarnessWarning{Goober: name, Warning: warning})
		}
		if err := mcpconfig.ValidateForHarness(harnessName, spec.MCPServers, spec.Capabilities, spec.Tools); err != nil {
			return nil, nil, fmt.Errorf("validate goober %q MCP config: %w", name, err)
		}
	}
	return resolvedGoobers, warnings, nil
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

// sandboxPosturesByGaggle resolves each configured gaggle's effective agentic
// isolation posture (#1305): the gaggle's own sandbox override when declared,
// else the instance-wide sandbox.agentic posture, else disabled. Resolved once
// here, at the composition root, so the per-gaggle runner wiring and anything
// else that needs the posture agree on one resolution (the same shape as
// branchNamespacesByGaggle above).
func sandboxPosturesByGaggle(cfg *instance.Config, set *instance.ConfigSet) map[string]instance.SandboxPosture {
	out := make(map[string]instance.SandboxPosture, len(set.Gaggles))
	for i := range set.Gaggles {
		g := &set.Gaggles[i]
		out[g.Name] = instance.EffectiveAgenticSandbox(cfg, g)
	}
	return out
}

// branchNamespacesByGaggle maps each configured gaggle to its run-branch
// namespace root (GaggleSpec.BranchNamespace), normalized to a single trailing
// "/" and defaulted to providers.DefaultBranchNamespace when unset. It is the
// one place the gaggle-configured namespace is read for the runtime: the
// per-gaggle worktree Manager's mirror-fetch exclusion (WithRunBranchNamespaces)
// and every run's Runner.Config.BranchNamespaces both derive from it, so the
// branch a run pushes, the exclusion that preserves it, and the PR-selector
// headPrefix all move together instead of drifting off independent literals
// (#965/#1010).
func branchNamespacesByGaggle(set *instance.ConfigSet) map[string]string {
	out := make(map[string]string, len(set.Gaggles))
	for i := range set.Gaggles {
		g := &set.Gaggles[i]
		out[g.Name] = providers.NormalizeBranchNamespace(g.Spec.BranchNamespace)
	}
	return out
}

func selfIdentitiesByGaggle(cfg *instance.Config, set *instance.ConfigSet) map[string]string {
	out := make(map[string]string, len(set.Gaggles))
	for i := range set.Gaggles {
		g := &set.Gaggles[i]
		out[g.Name] = instance.EffectiveSelfIdentity(cfg, g)
	}
	return out
}

// requireLabelsByGaggle maps each configured gaggle to its comma-joined
// GaggleSpec.RequireLabels default (MIRC-2, #1901) — the same
// gaggle-default shape branchNamespacesByGaggle/selfIdentitiesByGaggle
// resolve, feeding Runner.Config.BacklogQueryRequireLabels so a gaggle
// omitting RequireLabels behaves exactly as before (empty string, a no-op
// in defaultBacklogQueryRequireLabels).
func requireLabelsByGaggle(set *instance.ConfigSet) map[string]string {
	out := make(map[string]string, len(set.Gaggles))
	for i := range set.Gaggles {
		g := &set.Gaggles[i]
		out[g.Name] = strings.Join(g.Spec.RequireLabels, ",")
	}
	return out
}
