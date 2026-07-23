package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/gate"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/telemetry"
	"github.com/goobers/goobers/internal/toolchain"
	"github.com/goobers/goobers/internal/workflow"
	"github.com/goobers/goobers/internal/worktree"
	"github.com/goobers/goobers/providers"
)

// DefaultMaxSteps bounds the state walk against a runaway machine (carried over
// from the Temporal engine core, ARCHITECTURE §3.1).
const DefaultMaxSteps = 10000

// toolchainPreflightState is the synthetic failing-state name recorded when a
// run fails the #735 toolchain preflight before any real stage executes, so a
// `failed` run's FailureStage reads as the preflight rather than mis-attributing
// the missing toolchain to the workflow's first stage.
const toolchainPreflightState = "runtime-preflight"

// DefaultMaxInfrastructureAttempts bounds transient infrastructure failures
// independently of a task's policy retry allowance.
const DefaultMaxInfrastructureAttempts int32 = 2

// StageHeartbeatInterval coalesces observable executor progress into at most
// one compact liveness event per minute.
const StageHeartbeatInterval = time.Minute

// StalledCancellationGrace bounds how long the watchdog waits for a live owner
// to honor cancellation before taking over terminalization.
const StalledCancellationGrace = 5 * time.Second

// StalledTerminalizationGrace bounds how long a sweep waits after another
// goroutine has already begun terminal cleanup.
const StalledTerminalizationGrace = 30 * time.Second

type heartbeatTicker interface {
	Ticks() <-chan time.Time
	Stop()
}

type wallHeartbeatTicker struct {
	ticker *time.Ticker
}

func (t wallHeartbeatTicker) Ticks() <-chan time.Time { return t.ticker.C }
func (t wallHeartbeatTicker) Stop()                   { t.ticker.Stop() }

type journalAppender interface {
	Append(journal.Event) error
	ObserveActivity()
}

type stageHeartbeat struct {
	stop chan struct{}
	done <-chan error
}

func (h stageHeartbeat) Stop() error {
	close(h.stop)
	return <-h.done
}

// SpanStarter is the slice of the telemetry client the runner needs to open
// run/task/gate spans (issue #126). *telemetry.Client satisfies it
// structurally, mirroring internal/scheduler.SpanStarter's narrow-interface
// pattern for the same reason: no import cycle, and the runner never depends
// on telemetry's full surface.
type SpanStarter interface {
	StartRun(ctx context.Context, attrs telemetry.RunAttributes) (context.Context, telemetry.Span, error)
	StartTask(ctx context.Context, attrs telemetry.TaskAttributes) (context.Context, telemetry.Span, error)
	StartGate(ctx context.Context, attrs telemetry.GateAttributes) (context.Context, telemetry.Span, error)
}

// TerminalPreparer performs best-effort external cleanup whose outcome must be
// journaled immediately before run.finished closes the event sequence.
type TerminalPreparer func(runID string, phase journal.RunPhase, jr *journal.Run) error

// TerminalFinalizer performs instance-level cleanup after a run's terminal
// event is durably journaled. It must not append to the closed run journal and
// may be invoked again when startup observes an already-terminal run, so
// implementations must be idempotent.
type TerminalFinalizer func(runID string, phase journal.RunPhase) error

// TerminalNotifier performs a best-effort side effect for a newly-terminal
// live run. Errors are deliberately ignored so notification delivery can never
// affect run processing.
type TerminalNotifier func(runID string, phase journal.RunPhase, finalState string) error

// BlockedOutcome describes a run terminating because a stage reported status
// "blocked" (#544/#545) — the value Config.Blocked receives.
type BlockedOutcome struct {
	RunID string
	// RepoRef is the target repository containing the driving backlog item.
	RepoRef apiv1.RepoRef
	// Stage is the stage that reported blocked.
	Stage string
	// ItemID is the driving backlog item's id when the run was started with
	// one (StartInput.Item). Empty for a run that claims its item mid-run
	// (schedule/backlog-item-triggered implementation runs) — the handler
	// resolves those from the claim ledger by RunID instead.
	ItemID string
	// Reason is the agent's stated reason for the block (its error detail,
	// falling back to its summary).
	Reason string
	// Blockers are the blocking issue numbers the stage referenced via the
	// documented outputs.blockedBy convention (comma-separated numbers in a
	// scalar string — see docs/stage-contract.md). Empty when the stage named
	// none in machine-readable form.
	Blockers []string
}

// BlockedHandler is Config.Blocked's shape. Implementations are instance-level
// (composition-root) policy: record the block for selection to skip (#552),
// and park the driving issue. Must tolerate an empty ItemID.
type BlockedHandler func(ctx context.Context, o BlockedOutcome) error

// RateLimitedOutcome describes a stage failing with the typed
// providers.ErrorCodeRateLimited (#614) — the value Config.RateLimited
// receives. Unlike BlockedOutcome this never ends the run itself (the
// ordinary failure-routing below still decides that); it's a side-channel
// notification so the scheduler can learn the reset time before its next
// tick (#712).
type RateLimitedOutcome struct {
	RunID string
	// Stage is the stage that reported the rate-limited failure.
	Stage string
	// ResetAt is when the provider says its quota window rolls over, parsed
	// from the stage's declared result file (the rateLimitReset key
	// failProviderStage writes, cmd/goobers/providercmd.go). Never zero when
	// the handler is called — a rate-limited failure with no parseable reset
	// carries nothing actionable, so taskOutcome skips the call entirely.
	ResetAt time.Time
}

// RateLimitedHandler is Config.RateLimited's shape. Implementations are
// instance-level (composition-root) policy: record the exhausted provider
// quota so the scheduler's dispatch loop can stop starting doomed runs
// before the reset (#712's ProviderQuotaState.RecordExhausted is the
// reference implementation).
type RateLimitedHandler func(ctx context.Context, o RateLimitedOutcome) error

// FailedOutcome describes a run terminating at PhaseFailed (#1054) — the value
// Config.Failed receives. Distinct from the escalated/blocked terminals (which
// park the driving issue goobers:needs-human): a `failed` terminal must leave a
// human-visible trace WITHOUT that label, so repeated failures on the same item
// — a recurring copilot-cli harness session timeout is the motivating case —
// accumulate a countable signal instead of the item silently returning to
// goobers:ready with no record.
type FailedOutcome struct {
	RunID string
	// RepoRef is the target repository containing the driving backlog item —
	// the handler resolves its per-repo credential and posts the trace comment
	// to this repo, mirroring BlockedOutcome.
	RepoRef apiv1.RepoRef
	// Stage is the failing stage/gate name when the failure is attributable to
	// one (a stage-reported business failure, a gate-eval error). Empty for a
	// state-less walk-level failure (max-steps, an unknown state) — the
	// harness-timeout case (a dispatch-level runTask error) carries the stage
	// that was executing.
	Stage string
	// Cause is the run's terminal error message — the same text journaled as the
	// run_failed cause event, which the handler surfaces on the driving item.
	Cause string
}

// FailedHandler is Config.Failed's shape. Implementations are instance-level
// (composition-root) policy: leave a human-visible trace on the driving item
// when a run ends PhaseFailed (#1054). Must tolerate a run with no driving
// item.
type FailedHandler func(ctx context.Context, o FailedOutcome) error

// AgentProvenance identifies the configured model and preflighted harness
// version for spans emitted before an agent executor is resolved or invoked.
type AgentProvenance struct {
	Model          string
	HarnessVersion string
}

// Config wires a Runner's dependencies: the daemon-wide singletons (worktree
// provisioning, gate evaluation) and the per-run executor factories
// (executor.go) — the substrate the local runner drives directly (worktrees
// #16, the journal's runs directory #8). One Runner serves every workflow
// definition a daemon knows about; the compiled Machine for a specific run is
// supplied per call in StartInput, not fixed here.
type Config struct {
	// NewDeterministic constructs this run's deterministic-task executor
	// (invoke.Deterministic). Required if any workflow run through this
	// Runner has a deterministic task.
	NewDeterministic NewDeterministicFunc
	// NewAgentic constructs a named goober's executor (invoke.Goober) for this
	// run. Required if any workflow has an agentic task or agentic gate.
	NewAgentic NewAgenticFunc
	// Automated evaluates automated gates (internal/gate, #20) — stateless,
	// shared across every run. Required if any workflow has an
	// evaluator=automated gate. gate.NewAutomatedEvaluator() (the default
	// check registry) is a ready-made implementation.
	Automated invoke.Automated
	// MaxRepasses bounds gate repass loops before escalating
	// (gate.DefaultMaxRepasses if 0). See internal/gate.Evaluator.
	MaxRepasses int
	// Escalation notifies the driving backlog item's provider when a gate or
	// stage escalates the run (internal/gate.EscalationNotifier). Optional —
	// nil is a no-op.
	Escalation *gate.EscalationNotifier
	// ClaimedItems resolves the backlog item id(s) a run currently claims, for
	// runs started without an Item snapshot — scheduled/fan-out implementation
	// runs claim their item mid-run, so in.Item is nil (#796). Used as the
	// driving-item fallback in notifyTerminalGate, mirroring buildBlockedHandler's
	// claim-ledger fallback (the blocked path already resolves this way, the
	// escalation path silently did not). Called before FinalizeTerminal releases
	// the run's claims, so the ledger still sees them. Optional — nil disables the
	// fallback (a run with no Item snapshot then posts nothing, the prior behavior).
	ClaimedItems func(runID string) ([]string, error)
	// Blocked handles the instance-level consequences of a stage reporting
	// status "blocked" (#544/#545): recording the learned dependency block for
	// selection to skip (#552) and parking the driving issue. Called
	// after the blocked cause is journaled and before the run's terminal
	// run.finished event, so a claim-ledger lookup inside the handler still
	// sees the run's claims (FinalizeTerminal releases them only after).
	// Optional — nil is a no-op; a handler error is journaled, never fatal to
	// reaching the terminal phase.
	Blocked BlockedHandler
	// RateLimited handles the instance-level consequence of a stage failing
	// with the typed providers.ErrorCodeRateLimited (#614): recording the
	// exhausted provider quota so the scheduler's dispatch loop stops
	// starting doomed runs before the reset (#712). Called from
	// taskOutcome's ResultFailure case, before the ordinary failure-routing
	// decision (repass-gate vs. terminal-fail) — a side notification, not a
	// control-flow branch, so it never changes what the run itself does
	// next. Optional — nil is a no-op; a handler error is journaled, never
	// fatal to reaching whatever terminal/repass state the failure would
	// have reached anyway.
	RateLimited RateLimitedHandler
	// ToolchainVerifier preflights a run's declared runner-capability toolchains
	// on the host before any stage executes (#735). Optional — nil defaults to
	// toolchain.DefaultVerifier() (real host probing). A run declaring no
	// RequiredCapabilities never invokes it, so the default is inert until a
	// gaggle/stage opts in.
	ToolchainVerifier ToolchainVerifier
	// Failed handles the instance-level consequence of a run reaching terminal
	// PhaseFailed (#1054): leaving a human-visible trace (a comment carrying the
	// terminal cause + run id) on the driving item, so a systematic infra fault
	// — a recurring copilot-cli session timeout ends a run `failed`, not
	// `escalated` — accumulates a countable signal instead of the item silently
	// returning to goobers:ready with no record. Deliberately distinct from
	// Escalation/Blocked's needs-human park: that label stays reserved for the
	// escalated/park path, so a `failed` terminal reads as separate. Called
	// after the run's failure cause is journaled and before the run's terminal
	// run.finished event, so a claim-ledger lookup inside the handler still sees
	// the run's claims (FinalizeTerminal releases them only after) — the same
	// ordering notifyBlocked relies on. Fires ONLY on genuine terminal `failed`,
	// never on completed/escalated/aborted. Optional — nil is a no-op; a handler
	// error is journaled (failed_handling_failed), never fatal to reaching the
	// terminal phase.
	Failed FailedHandler
	// GateGooberCapabilities resolves an agentic gate's reviewer goober name to
	// the capabilities its definition declares. An agentic GATE has no
	// stage-level capabilities of its own (apiv1.AgenticGate is just a Goober
	// reference), yet its reviewer runs a real goober subprocess that needs its
	// capability-scoped credentials — e.g. agent:model for Copilot model auth
	// (#294). Consulted ONLY for evaluator=agentic gates; automated and human
	// gates run no goober and must never receive credentials. A goober absent
	// here (or a nil map) sources no capabilities — exactly the prior behavior,
	// so a gate is never silently over-credentialed. Sourcing a goober's OWN
	// declared grants can never exceed them, so this needs no separate admission
	// check (the compiler already validated the goober's grants, #74).
	GateGooberCapabilities map[string][]string
	// AgentProvenance resolves a goober name to the model and preflighted harness
	// version attached when its task or reviewer-gate span is created.
	AgentProvenance map[string]AgentProvenance
	// Worktrees provisions the fresh, isolated, disposable working copy each
	// stage attempt runs in (§5).
	Worktrees *worktree.Manager
	// ScratchDir contains disposable workspaces for deterministic commands that
	// declare run.workspace=scratch. Required only when such a task executes.
	ScratchDir string
	// RunsDir is the journal's run directory (<instance-root>/runs).
	RunsDir string
	// PrepareTerminal records external cleanup immediately before run.finished.
	// Optional; errors are surfaced before the terminal transition.
	PrepareTerminal TerminalPreparer
	// FinalizeTerminal performs instance-level cleanup for every terminal run,
	// after run.finished is durable. Optional; errors are surfaced to the caller.
	FinalizeTerminal TerminalFinalizer
	// NotifyTerminal reports a run newly made terminal by this Runner after
	// run.finished is durable and before instance-level cleanup. Optional and
	// best-effort: errors never affect the run. Recovery of a run that was
	// already terminal does not invoke it.
	NotifyTerminal TerminalNotifier
	// MaxSteps overrides DefaultMaxSteps when > 0.
	MaxSteps int
	// RepoCloneURL derives the git remote URL worktree.Manager clones from a
	// RepoRef. Defaults to defaultRepoCloneURL. Tests
	// override this to point at a local fixture repo without network access.
	RepoCloneURL func(apiv1.RepoRef) (string, error)
	// Telemetry optionally spans the run/task/gate walk (issue #126). Nil
	// disables span emission — every telemetry.Span zero-value method no-ops,
	// so call sites below need no nil checks beyond the one guard in each
	// start*Span helper.
	Telemetry SpanStarter
	// BranchNamespaces maps a gaggle name to its configured run-branch
	// namespace root (GaggleSpec.BranchNamespace). A run's branch name
	// (providers.BranchNameIn), its stage env's GOOBERS_BRANCH_NAMESPACE, and
	// the workspace-rebind guard all resolve through it keyed by
	// StartInput.Gaggle, so a gaggle that retunes its namespace stays
	// consistent with the mirror-fetch exclusion the worktree Manager is built
	// with (#965/#1010). A gaggle absent from the map — or an empty value —
	// falls back to providers.DefaultBranchNamespace, so an instance running
	// only default-prefix gaggles needs no entries and behaves exactly as
	// before. See Runner.branchNamespaceFor.
	BranchNamespaces map[string]string
}

// Runner advances a compiled workflow.Machine stage-by-stage, durably
// recording every transition to the run journal, and dispatching tasks
// through the pre-existing internal/invoke seam. It is the substrate-neutral
// local runner core (ARCHITECTURE.md §3.1).
type Runner struct {
	cfg                  Config
	maxSteps             int
	heartbeatInterval    time.Duration
	newHeartbeatTicker   func(time.Duration) heartbeatTicker
	stalledCancelGrace   time.Duration
	stalledTerminalGrace time.Duration
	active               activeRunSet
	toolchains           ToolchainVerifier
}

// New validates cfg and returns a ready Runner.
func New(cfg Config) (*Runner, error) {
	if cfg.Worktrees == nil {
		return nil, fmt.Errorf("runner: Worktrees is required")
	}
	if cfg.RunsDir == "" {
		return nil, fmt.Errorf("runner: RunsDir is required")
	}
	maxSteps := cfg.MaxSteps
	if maxSteps <= 0 {
		maxSteps = DefaultMaxSteps
	}
	if cfg.RepoCloneURL == nil {
		cfg.RepoCloneURL = defaultRepoCloneURL
	}
	toolchains := cfg.ToolchainVerifier
	if toolchains == nil {
		toolchains = toolchain.DefaultVerifier()
	}
	return &Runner{
		cfg:               cfg,
		maxSteps:          maxSteps,
		toolchains:        toolchains,
		heartbeatInterval: StageHeartbeatInterval,
		newHeartbeatTicker: func(interval time.Duration) heartbeatTicker {
			return wallHeartbeatTicker{ticker: time.NewTicker(interval)}
		},
		stalledCancelGrace:   StalledCancellationGrace,
		stalledTerminalGrace: StalledTerminalizationGrace,
	}, nil
}

// StartInput is what triggers one run.
type StartInput struct {
	// RunID uniquely identifies this run (the OpenTelemetry trace id). Caller-
	// supplied — typically the scheduler, which needs this same id for its
	// claim ledger before dispatch, so claim identity and run identity are one
	// value throughout with no reconciliation step.
	RunID string
	// Machine is the compiled workflow (#9) this run walks. Runs of the same
	// workflow definition all pass the same *workflow.Machine; different
	// workflows pass different ones — this Runner is not bound to one.
	Machine *workflow.Machine
	// Gaggle is the gaggle this run belongs to.
	Gaggle string
	// Trigger is what started the run (manual/schedule/signal/item).
	Trigger journal.Trigger
	// RepoRef is the target repository every stage worktree branches from.
	RepoRef apiv1.RepoRef
	// Item is the originating backlog item, snapshotted immutably into the
	// journal at run start. Nil for a schedule/signal-triggered producer run.
	Item *apiv1.BacklogItem
	// RequiredCapabilities are the runner (toolchain/platform) capabilities this
	// run declares (its gaggle's + its stages', RRQ-1/#1101). They are
	// preflight-verified on the host before any stage executes (#735): a token
	// naming a probeable toolchain (`dotnet@8`, `go@1.26`, `os=linux`) that the
	// host does not actually satisfy fails the run closed with a clear
	// diagnostic, converting a runner's false capability claim from an opaque
	// mid-run error into an actionable one. Empty imposes no preflight, so a run
	// that declares no requirement behaves exactly as before. A resumed run
	// leaves this nil — it already passed preflight at its original Start.
	RequiredCapabilities []string
}

// ToolchainVerifier verifies, on the executing host, that a run's declared
// runner-capability tokens are satisfied by an installed toolchain (#735). It
// is the Config seam the runner preflights through; internal/toolchain provides
// the production implementation and tests inject a fake.
type ToolchainVerifier interface {
	Verify(ctx context.Context, required []string) error
}

// Result is a run's outcome as of the moment Start/Resume returns. A human
// gate or a drained cancellation both leave the run non-terminal (Phase stays
// journal.PhaseRunning, FinalState is where it paused) — the journal's
// state.json already checkpoints exactly where to pick up next.
type Result struct {
	Phase      journal.RunPhase
	FinalState string
	Steps      int
	// FailureStage/FailureCode/FailureMessage carry a PhaseFailed run's actual
	// cause (issue #710) — populated by taskOutcome's business-failure arm
	// (FailureCode/Message from the stage's own ErrorInfo, bounded), by
	// failTerminal (FailureCode "run_failed", FailureStage the failing
	// stage/gate name, FailureMessage the walk-level error, bounded), and by
	// refuseResume (FailureCode the WF-016 refusal code, FailureStage empty —
	// a resume-time digest check, not stage-scoped). Every caller reading
	// Phase == PhaseFailed downstream (the scheduler's and daemon's run-
	// finished echo, cmd/goobers/daemon.go) threads these onto the echoed
	// event so the instance journal actually says WHY a run failed, instead
	// of a bare status:"failed" — #705's root cause was recorded in every
	// failing run's own journal the whole time; this is what was missing to
	// see it one level up, at the scheduler/daemon-log level. Empty for every
	// non-failed phase and for a failed run with no attributable stage (a
	// bare walk-level error before any task started, e.g. an unknown start
	// state).
	FailureStage   string
	FailureCode    string
	FailureMessage string
}

// maxFailureMessageLen bounds FailureMessage so a verbose provider/error
// string (a real GitHub 403 body has run well past this) never bloats the
// scheduler/instance-log echo it feeds (issue #710's design: "a bounded
// message"). The full, untruncated message already lives in the run's own
// journal (the stage.finished/error event this is derived from) — this is
// purely a cap on the COPY threaded up to the coarser echo sites.
const maxFailureMessageLen = 500

// boundFailureMessage truncates s to maxFailureMessageLen runes, appending a
// marker so a truncated echo is never mistaken for the complete message.
func boundFailureMessage(s string) string {
	r := []rune(s)
	if len(r) <= maxFailureMessageLen {
		return s
	}
	return string(r[:maxFailureMessageLen]) + "...(truncated)"
}

// Start creates a new run journal pinned to the compiled machine's identity,
// snapshots Item as an immutable input, and walks the machine to a terminal
// state (or a human-gate/drain pause). Start is synchronous — it returns once
// the run reaches a terminal state or pauses, which for a real agentic stage
// may be minutes. A caller driving many runs (e.g. a scheduler) should call
// Start in its own goroutine per run rather than block its own dispatch loop
// on it.
func (r *Runner) Start(ctx context.Context, in StartInput) (Result, error) {
	if in.RunID == "" {
		return Result{}, fmt.Errorf("runner: RunID is required")
	}
	if in.Machine == nil {
		return Result{}, fmt.Errorf("runner: Machine is required")
	}

	inputs := map[string][]byte{}
	graph, err := json.Marshal(in.Machine.Graph())
	if err != nil {
		return Result{}, fmt.Errorf("runner: marshal pinned workflow graph: %w", err)
	}
	inputs[journal.PinnedWorkflowGraphInputName] = graph
	if in.Item != nil {
		b, err := json.Marshal(in.Item)
		if err != nil {
			return Result{}, fmt.Errorf("runner: marshal item snapshot: %w", err)
		}
		inputs["item"] = b
	}

	// registrar/scrubber are fresh per run (never shared — a run's secrets
	// have no business outliving it in an in-memory registry). Chaining the
	// registry ahead of the pattern net (#66) means any secret this run's
	// executors resolve (registered via registrar, threaded to
	// NewDeterministic/NewAgentic below) is redacted from this run's journal
	// by exact value — not just pattern-matched — even for events the runner
	// itself authors (e.g. an executor_error message), not only the
	// artifacts an executor scrubs and commits itself.
	registrar, scrubber := journal.DefaultScrubber()
	jr, err := journal.Create(r.cfg.RunsDir, journal.RunIdentity{
		RunID:           in.RunID,
		Workflow:        in.Machine.Def.Name,
		WorkflowVersion: in.Machine.Def.Version,
		WorkflowDigest:  in.Machine.Digest(),
		Gaggle:          in.Gaggle,
		Trigger:         in.Trigger,
	}, inputs, journal.WithScrubber(scrubber))
	if err != nil {
		return Result{}, fmt.Errorf("runner: create journal for run %q: %w", in.RunID, err)
	}
	defer func() { _ = jr.Close() }()

	return r.withActiveRun(ctx, in.RunID, jr, func(ctx context.Context) (Result, error) {
		ctx, span := r.startRunSpan(ctx, in)
		defer span.End()
		setStalledAttemptContext(ctx)

		// #735: verify the run's declared runtime toolchains are actually present
		// on this host before executing any stage. #1101 refuses to schedule a
		// run onto a runner that does not *claim* a required capability, but
		// accepts that a runner which *falsely* claims one degrades to an opaque
		// mid-run error; this turns that into a fail-closed preflight naming the
		// unsatisfied toolchain. A run with no declared requirement skips the
		// preflight entirely (no behavior change), and a resumed run passes nil
		// (it already cleared preflight at its original Start).
		if len(in.RequiredCapabilities) > 0 {
			if err := r.toolchains.Verify(ctx, in.RequiredCapabilities); err != nil {
				span.Fail(err)
				return r.failTerminal(ctx, in.RunID, jr, in.RepoRef, toolchainPreflightState, 0, err)
			}
		}

		// A scratch-only workflow has no repository branch. Repo-backed workflows
		// retain the run branch as their primary external ref (#133).
		if machineUsesRepo(in.Machine) {
			if err := jr.Append(journal.Event{
				Type: journal.EventRefTouched,
				ExternalRef: &journal.ExternalRef{
					Provider: string(in.RepoRef.Provider),
					Kind:     "branch",
					ID:       providers.BranchNameIn(r.branchNamespaceFor(in.Gaggle), in.Machine.Def.Name, in.RunID),
				},
			}); err != nil {
				span.Fail(err)
				return Result{}, fmt.Errorf("runner: journal run branch for %q: %w", in.RunID, err)
			}
		}

		result, err := r.walk(ctx, jr, in, in.Machine.Def.Spec.Start, nil, nil, nil, nil, registrar, walkSeed{})
		if err != nil {
			span.Fail(err)
			return result, err
		}
		completeRunSpan(span, result)
		return result, nil
	})
}

func completeRunSpan(span telemetry.Span, result Result) {
	outcome, isFailure := runSpanOutcome(result.Phase)
	span.CompleteWithError(outcome, result.FailureCode, isFailure)
}

func runSpanOutcome(phase journal.RunPhase) (string, bool) {
	switch phase {
	case journal.PhaseCompleted:
		return telemetry.OutcomeSuccess, false
	case journal.PhaseRunning, journal.PhaseEscalated:
		return telemetry.OutcomeBlocked, false
	case journal.PhaseFailed, journal.PhaseAborted:
		return telemetry.OutcomeFailure, true
	default:
		return telemetry.OutcomeFailure, true
	}
}

// startRunSpan opens the run's root span, if telemetry is configured. A zero
// telemetry.Span is safe to use (its methods no-op), so callers need no nil
// checks — mirrors internal/scheduler.Scheduler.startSpan. The returned ctx
// carries the run's trace id (RunID, per telemetry.Client.StartRun) so every
// task/gate span opened while walking this run joins the same trace.
func (r *Runner) startRunSpan(ctx context.Context, in StartInput) (context.Context, telemetry.Span) {
	if r.cfg.Telemetry == nil {
		return ctx, telemetry.Span{}
	}
	attrs := telemetry.RunAttributes{
		Gaggle:          in.Gaggle,
		WorkflowID:      in.Machine.Def.Name,
		WorkflowVersion: strconv.Itoa(in.Machine.Def.Version),
		WorkflowDigest:  in.Machine.Digest(),
		RunID:           in.RunID,
	}
	if in.Item != nil {
		attrs.ItemID = in.Item.ID
		attrs.ItemURL = in.Item.URL
	}
	ctx, span, err := r.cfg.Telemetry.StartRun(ctx, attrs)
	if err != nil {
		return ctx, telemetry.Span{}
	}
	return ctx, span
}

// executors holds the per-run invoke.* instances, constructed lazily on first
// use and reused for the rest of the run. A deterministic executor is bound
// once (§18 has no per-goober concept); an agentic executor is bound per
// goober name, since one run can target more than one distinct goober (e.g.
// "coder" for a task, "reviewer" for its gate) and each needs its own
// instructions/model — but the SAME instance serves both a task's Invoke and
// a paired gate's Review.
type executors struct {
	cfg Config
	rec ArtifactRecorder
	reg SecretRegistrar

	det    invoke.Deterministic
	agents map[string]invoke.Goober
}

func newExecutors(cfg Config, rec ArtifactRecorder, reg SecretRegistrar) *executors {
	return &executors{cfg: cfg, rec: rec, reg: reg, agents: map[string]invoke.Goober{}}
}

func (e *executors) deterministic() (invoke.Deterministic, error) {
	if e.det != nil {
		return e.det, nil
	}
	if e.cfg.NewDeterministic == nil {
		return nil, fmt.Errorf("runner: no NewDeterministic configured for a deterministic task")
	}
	det, err := e.cfg.NewDeterministic(e.rec, e.reg)
	if err != nil {
		return nil, fmt.Errorf("runner: construct deterministic executor: %w", err)
	}
	e.det = det
	return det, nil
}

func (e *executors) agentic(gooberName string) (invoke.Goober, error) {
	if ag, ok := e.agents[gooberName]; ok {
		return ag, nil
	}
	if e.cfg.NewAgentic == nil {
		return nil, fmt.Errorf("runner: no NewAgentic configured for goober %q", gooberName)
	}
	ag, err := e.cfg.NewAgentic(gooberName, e.rec, e.reg)
	if err != nil {
		return nil, fmt.Errorf("runner: construct agentic executor for goober %q: %w", gooberName, err)
	}
	e.agents[gooberName] = ag
	return ag, nil
}

// resumeContext identifies a task attempt that was in flight when the runner
// stopped. Its original class is retained so a human rerun's start/finish audit
// pair remains matched; ordinary crash recovery still closes it as infra.
type resumeContext struct {
	stage   string
	attempt int
	class   journal.AttemptClass
}

const (
	interruptedAttemptErrorCode = "interrupted"
	interruptedAttemptMarkerKey = "interruptedAttempt"
	retryFailureClassKey        = "retryFailureClass"
	retryDecisionKind           = "stage.retry.decision"
	toleratedFailureErrorCode   = "stage_failure_tolerated"
	baseSyncConflictErrorCode   = "base_sync_conflict"
)

type baseSyncConflictArtifact struct {
	Code             string   `json:"code"`
	Message          string   `json:"message"`
	Branch           string   `json:"branch"`
	BaseRef          string   `json:"baseRef"`
	ConflictingFiles []string `json:"conflictingFiles"`
}

// walkSeed carries the walk-local state a resumed run must NOT start empty —
// Start's fresh walk always begins with the zero value. pointers is the
// upstream ContextPointers every already-finished stage produced (#107);
// lastStage/lastResult is the subject a resumed gate evaluates against
// (#108) — both reconstructed from the journal by Resume (see
// lastFinishedSubject, reconstructPointers in resume.go), since walk's own
// in-memory accumulation of them is exactly what a crash wipes.
// workspaceBranch is the same for the run-scoped branch rebinding below
// (lastWorkspaceBranch in resume.go).
type walkSeed struct {
	pointers        []apiv1.ContextPointer
	lastStage       string
	lastResult      apiv1.ResultEnvelope
	workspaceBranch string
}

// WorkspaceBranchOutput is the well-known stage output that REBINDS the branch
// every subsequent stage's worktree checks out, for the rest of the run
// (issue #392).
//
// By default a run's stages share one branch — providers.BranchName,
// "goobers/<workflow>/<run-id>" — created off RepoRef.Branch by the first
// stage and checked out as-is, carrying prior stages' commits, by every stage
// after it (internal/worktree.Manager.Create's own doc comment, #133). That
// shared branch, not a shared directory, is what makes local-ci and the
// reviewer gate evaluate the run's actual diff.
//
// A workflow that re-enters on work that ALREADY has a branch needs that same
// continuity mechanism pointed somewhere else. pr-remediation is the case this
// exists for (docs/design/v0/pr-lifecycle-loop.md §5): it rebases and reworks
// an EXISTING PR, so its implement/review/local-ci stages must operate on the
// PR's own head branch, not a fresh one cut from main. Its gather-pr-context
// entrypoint emits the selected PR's head branch under this key, and every
// stage from there on — including agentic stages and gate evaluators, which
// have no way to re-checkout anything for themselves — is provisioned against
// it with no per-stage cooperation at all.
//
// The rebinding is sticky for the remainder of the run and survives a crash:
// stage outputs are journaled on stage.finished, so Resume recovers the most
// recent binding (lastWorkspaceBranch) into walkSeed rather than silently
// reverting a resumed run to the default branch mid-chain.
//
// A rebound branch must already exist (worktree.CreateOptions.RequireExistingBranch
// below turns a missing one into a loud failure rather than a silent empty branch
// cut from base), and must live in the run-branch namespace the worktree manager
// protects from its prune-fetch (internal/worktree/manager.go's
// runBranchNamespace) — otherwise the next stage's WorkingCopy refresh would
// delete it. Emitting an empty value is a no-op, not a reset.
//
// ONLY A DETERMINISTIC STAGE MAY REBIND. An agentic stage's Outputs are
// authored by the model itself (internal/harness's result-shape hint invites a
// free-form `"outputs": {...}` map and internal/harness.Executor passes it
// through verbatim), so honoring this key from one would let any goober, in any
// workflow, silently move every subsequent stage — including `push-branch` —
// onto a branch of its choosing. `implementation` needs no diff at all to be
// affected by that; it just needs an implementer that decides to report a
// branch name. Rebinding is a runner control-plane decision, so it is sourced
// only from stages whose output the runner itself produced.
const WorkspaceBranchOutput = "workspaceBranch"

// branchNamespaceFor resolves the run-branch namespace root for gaggle, from
// Config.BranchNamespaces, falling back to providers.DefaultBranchNamespace so
// a gaggle with no configured override (the common case) gets the historical
// "goobers/" prefix. The result is normalized to a single trailing "/", so
// callers can concatenate or prefix-match against it uniformly.
func (r *Runner) branchNamespaceFor(gaggle string) string {
	return providers.NormalizeBranchNamespace(r.cfg.BranchNamespaces[gaggle])
}

// rebindWorkspaceBranch reports the branch a finished task's result rebinds the
// run's workspace to, or "" if it rebinds nothing. Fails closed on every
// doubtful case: a non-deterministic producer, a non-string value, or a branch
// outside the run-branch namespace (nsPrefix, the run's gaggle-resolved branch
// namespace root) are all ignored rather than honored.
func rebindWorkspaceBranch(t apiv1.Task, result apiv1.ResultEnvelope, nsPrefix string) string {
	if t.Type != apiv1.TaskDeterministic {
		return ""
	}
	return workspaceBranchFrom(result.Outputs, nsPrefix)
}

// workspaceBranchFrom reads WorkspaceBranchOutput out of a stage's scalar
// Outputs. Shared with resume.go, whose journaled outputs are the same values
// under map[string]any rather than map[string]interface{} (identical types,
// different spelling). nsPrefix is the run's gaggle-resolved run-branch
// namespace root (Runner.branchNamespaceFor); a value outside it is rejected.
//
// Deliberately does NOT stringify non-strings the way internal/gate's
// stringField does: a branch name is not a value to coerce, and
// `"workspaceBranch": false` becoming a branch literally named "false" is a
// worse outcome than ignoring a malformed emission. Values outside the
// run-branch namespace are ignored for the same reason — "main" is the
// dangerous case, and nothing legitimate needs it.
func workspaceBranchFrom(outputs map[string]interface{}, nsPrefix string) string {
	v, ok := outputs[WorkspaceBranchOutput]
	if !ok {
		return ""
	}
	s, ok := v.(string)
	if !ok {
		return ""
	}
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, nsPrefix) {
		return ""
	}
	return s
}

// walk advances the machine from startState to a terminal state (or a
// human-gate/drain pause), journaling every stage/gate attempt and every
// artifact produced along the way. Gate dispatch (bounded repass, escalation,
// gate.evaluated journaling) is entirely owned by the gate.Evaluator
// constructed once here — it MUST NOT be shared across runs (its repass
// counters are run-scoped state), so a fresh one is built per walk. Start
// always begins at the machine's declared start state with resume=nil,
// gateAttempts=nil, gateDiffDigests=nil, and a zero-value seed; Resume
// (resume.go) begins at the journal's checkpointed state, optionally with a
// resumeContext for an interrupted task attempt, gateAttempts seeded from
// each gate's last gate.started/gate.evaluated event so a resumed run's repass
// budget continues rather than resetting (#89/#263), gateDiffDigests likewise
// seeded (gateDiffSeed) so a resumed run's non-convergence detection continues
// too (#316), and seed reconstructed from the journal (#107/#108). reg is the
// run's SecretRegistrar (see Start), threaded to every executor constructed
// here.
func (r *Runner) walk(ctx context.Context, jr *journal.Run, in StartInput, startState string, resume *resumeContext, rerun *rerunContext, gateAttempts map[string]int, gateDiffDigests map[string]string, reg SecretRegistrar, seed walkSeed) (Result, error) {
	ex := newExecutors(r.cfg, jr, reg)
	gateEval := &gate.Evaluator{
		Automated:      r.cfg.Automated,
		Journal:        jr,
		MaxRepasses:    r.cfg.MaxRepasses,
		Attempts:       gateAttempts,
		LastDiffDigest: gateDiffDigests,
	}

	state := startState
	pointers := append([]apiv1.ContextPointer(nil), seed.pointers...)
	lastStage := seed.lastStage
	lastResult := seed.lastResult
	// The branch every stage's worktree is provisioned against, rebindable
	// mid-run by a stage output (#392, WorkspaceBranchOutput). Empty means
	// "the run's own branch", resolved per stage in createStageWorkspace.
	workspaceBranch := seed.workspaceBranch
	steps := 0

	for {
		steps++
		if steps > r.maxSteps {
			return r.failTerminal(ctx, in.RunID, jr, in.RepoRef, state, steps, fmt.Errorf("runner: run %q exceeded max steps (%d): possible loop", in.RunID, r.maxSteps))
		}
		jr.SetMachineState(state)

		if stalledResult, stalled, stalledErr := r.finishStalledRequest(ctx, in.RunID, jr, state, steps); stalled {
			return stalledResult, stalledErr
		}

		// Drain, don't abort, on cancellation (SIGTERM: internal/signals
		// cancels this same ctx). Checked only BETWEEN stages — an in-flight
		// dispatch below runs on context.WithoutCancel(ctx), so a signal never
		// interrupts an attempt already underway; it only stops the NEXT one
		// from starting. Checkpointing here (state.json already points at
		// `state`, the next stage to run) is what makes this a resumable
		// pause, not a failure: journal.Recover replays straight back to it.
		if err := ctx.Err(); err != nil {
			if cerr := jr.Checkpoint(); cerr != nil {
				return Result{}, fmt.Errorf("runner: checkpoint drain at %q: %w", state, cerr)
			}
			return Result{Phase: journal.PhaseRunning, FinalState: state, Steps: steps}, nil
		}

		if t, ok := in.Machine.Task(state); ok {
			startAttempt := int32(1)
			var firstClass journal.AttemptClass
			var instructionAddendum string
			var taskRerun *rerunContext
			if rerun != nil && rerun.stage == t.Name {
				taskRerun = rerun
				startAttempt = int32(rerun.attempt)
				firstClass = journal.AttemptHuman
				instructionAddendum = rerun.instructionAddendum
			}
			if resume != nil && resume.stage == t.Name {
				interruptedClass := journal.AttemptInfra
				if rerun != nil && rerun.stage == t.Name {
					interruptedClass = resume.class
				}
				// The attempt in flight when the runner was interrupted is
				// terminal now. Preserve a human rerun's class for an auditable
				// matched start/finish; ordinary crash recovery remains infra-
				// tagged. The next dispatch advances the attempt count.
				if err := jr.Append(journal.Event{
					Type: journal.EventStageFinished, Stage: t.Name, Attempt: resume.attempt, AttemptClass: interruptedClass,
					Status: string(apiv1.ResultFailure),
					Error:  &journal.ErrorDetail{Code: interruptedAttemptErrorCode, Message: "attempt was in flight when the runner was interrupted"},
					Runner: map[string]any{interruptedAttemptMarkerKey: true},
				}); err != nil {
					return Result{}, fmt.Errorf("runner: journal interrupted attempt for %q: %w", t.Name, err)
				}
				startAttempt = int32(resume.attempt) + 1
				if rerun != nil && rerun.stage == t.Name {
					firstClass = journal.AttemptHuman
				} else {
					firstClass = journal.AttemptInfra
				}
				resume = nil
			}
			result, produced, err := r.runTask(ctx, jr, in, ex, t, pointers, lastResult, startAttempt, firstClass, instructionAddendum, workspaceBranch, taskRerun)
			if rerun != nil && rerun.stage == t.Name {
				rerun = nil
			}
			if stalledResult, stalled, stalledErr := r.finishStalledRequest(ctx, in.RunID, jr, t.Name, steps); stalled {
				return stalledResult, stalledErr
			}
			if err != nil {
				return r.failTerminal(ctx, in.RunID, jr, in.RepoRef, t.Name, steps, err)
			}
			pointers = append(pointers, produced...)
			lastStage, lastResult = t.Name, result
			// Sticky for the rest of the run, and only ever set by a stage
			// that actually emitted the key — see WorkspaceBranchOutput. This
			// runs AFTER the stage that emits it, so that stage itself still
			// gets the previous binding (gather-pr-context is provisioned on
			// the run's own branch and checks the PR's branch out for itself;
			// every stage after it is provisioned on the PR's branch directly).
			if result.Status != apiv1.ResultFailure || !t.ContinueOnError {
				if b := rebindWorkspaceBranch(t, result, r.branchNamespaceFor(in.Gaggle)); b != "" {
					workspaceBranch = b
				}
			}

			next, res, advance, oerr := r.taskOutcome(ctx, in.RunID, jr, in.Machine, in.RepoRef, in.Item, t, result, steps)
			if res.Phase == "" {
				if stalledResult, stalled, stalledErr := r.finishStalledRequest(ctx, in.RunID, jr, t.Name, steps); stalled {
					return stalledResult, stalledErr
				}
			}
			if oerr != nil {
				return res, oerr
			}
			if result.Status == apiv1.ResultFailure && t.ContinueOnError {
				lastResult.Outputs = nil
			}
			if !advance {
				return res, nil
			}
			state = next
			continue
		}

		if g, ok := in.Machine.Gate(state); ok {
			// The machine remains at this gate until its evaluator records a
			// verdict. Persist that wait before dispatch so observers can
			// distinguish it from an active stage.
			if err := jr.Append(journal.Event{Type: journal.EventGatePaused, Gate: g.Name}); err != nil {
				return Result{}, fmt.Errorf("runner: journal pause at gate %q: %w", g.Name, err)
			}
			if g.Evaluator == apiv1.EvaluatorHuman {
				return Result{Phase: journal.PhaseRunning, FinalState: g.Name, Steps: steps}, nil
			}

			var instructionAddendum string
			if rerun != nil && rerun.stage == g.Name {
				instructionAddendum = rerun.instructionAddendum
				rerun = nil
			}
			gr, err, removeErr := r.evaluateGate(ctx, jr, gateEval, ex, in, g, lastStage, lastResult, pointers, instructionAddendum, workspaceBranch)
			if stalledResult, stalled, stalledErr := r.finishStalledRequest(ctx, in.RunID, jr, g.Name, steps); stalled {
				return stalledResult, stalledErr
			}
			if removeErr != nil {
				// Non-fatal (issue #136), same rationale as runTask's own
				// worktree_remove_failed journaling — the teardown failure
				// itself doesn't change this gate's outcome, but the append
				// recording it must not itself be silently discarded
				// (#243): a journal that cannot be written is fatal (§2.6),
				// and a gate's own outcome may `continue` the walk without
				// any further append until the next stage dispatches, so
				// this can be the only append for an arbitrarily long
				// stretch — there is no guaranteed nearby append to also
				// catch the same failure.
				if aerr := jr.Append(journal.Event{
					Type: journal.EventError, Gate: g.Name,
					Error: &journal.ErrorDetail{Code: "worktree_remove_failed", Message: removeErr.Error()},
				}); aerr != nil {
					return r.failTerminal(ctx, in.RunID, jr, in.RepoRef, g.Name, steps, fmt.Errorf("runner: journal worktree removal error for gate %q: %w", g.Name, aerr))
				}
			}
			if err != nil {
				return r.failTerminal(ctx, in.RunID, jr, in.RepoRef, g.Name, steps, err)
			}
			if err := journalRetryDecision(jr, gr, lastStage, lastResult); err != nil {
				return r.failTerminal(ctx, in.RunID, jr, in.RepoRef, g.Name, steps, fmt.Errorf("runner: journal retry decision for gate %q: %w", g.Name, err))
			}
			if reason, ok := terminalGateNotificationReason(gr); ok {
				notifyErr := r.notifyTerminalGate(stalledAttemptContext(ctx), jr, in.RunID, in.RepoRef, in.Item, gr, reason)
				if stalledResult, stalled, stalledErr := r.finishStalledRequest(ctx, in.RunID, jr, g.Name, steps); stalled {
					return stalledResult, stalledErr
				}
				if notifyErr != nil {
					return r.failTerminal(ctx, in.RunID, jr, in.RepoRef, g.Name, steps, notifyErr)
				}
			}
			switch gr.Target {
			case workflow.TargetAbort:
				return r.finish(in.RunID, jr, journal.PhaseAborted, g.Name, steps)
			case workflow.TargetEscalate:
				return r.finish(in.RunID, jr, journal.PhaseEscalated, g.Name, steps)
			case workflow.TerminalComplete:
				// #849: a gate can route a stage that reported ResultFailure
				// into a terminal-complete branch, and two very different
				// things look alike here:
				//   - merge-review's merge-gate: merge-pr ERRORED, its
				//     land-outcome check found no landOutcome → "fail" → ""
				//     (complete). The failure dead-ended unresolved, yet the
				//     run reported `completed` — the 23/23 merge-pr masking
				//     that hid the merge blocker for four rounds.
				//   - the implement→review loop: implement reported failure,
				//     but the reviewer evaluated the actual diff and PASSED it
				//     → "pass" → complete. The gate affirmatively cleared the
				//     failure; the run legitimately completed.
				// The discriminator is the gate's own outcome: a non-pass
				// outcome reaching complete while still carrying a ResultFailure
				// is unresolved → fail the run with the stage's own cause; a
				// pass outcome resolved it → complete. A designed negative
				// outcome (merge-pr's exit-0 `reasons` refusal, ci-poll's
				// status output) is a ResultSuccess and never trips this.
				subject, _ := in.Machine.Task(lastStage)
				if lastResult.Status == apiv1.ResultFailure && !subject.ContinueOnError && gr.Outcome != gate.OutcomePass {
					return r.finishStageFailure(ctx, in.RunID, jr, in.RepoRef, lastStage, steps, lastResult.Error)
				}
				return r.finish(in.RunID, jr, journal.PhaseCompleted, g.Name, steps)
			}
			if gr.VerdictArtifact != nil {
				// #412: the next dispatch — a repass back to the stage that
				// produced the subject this gate just evaluated, most
				// commonly — must actually receive the reviewer's verdict,
				// not just infer "something needs to change" from git. The
				// implementer's own instructions already promise it'll read
				// "the reviewer rationale … attached as context"; before
				// this, that promise was never kept, so a repass regenerated
				// the same diff and tripped the #316 identical-diff guard
				// for lack of anything new to act on.
				pointers = append(pointers, apiv1.ContextPointer{Name: g.Name + ".verdict", Artifact: gr.VerdictArtifact})
			}
			state = gr.Target
			continue
		}

		return r.failTerminal(ctx, in.RunID, jr, in.RepoRef, state, steps, fmt.Errorf("runner: unknown state %q", state))
	}
}

func terminalGateNotificationReason(gr gate.Result) (string, bool) {
	// An escalation still notifies the driving issue when the gate's escalate
	// control branch routes disposition work (a parking stage) before the
	// terminal, rather than naming @escalate directly: the repass-attempt count
	// and gate attribution are the point of the notification, and keying only
	// on the control-branch targets would silently drop them.
	if gr.Target != workflow.TargetAbort && gr.Target != workflow.TargetEscalate && !gr.Escalated {
		return "", false
	}
	if gr.Escalated {
		if gr.DuplicateDiff {
			return "repass produced a diff identical to the immediately prior attempt", true
		}
		return "repass budget exhausted", true
	}

	reason := fmt.Sprintf("gate %s resolved %s -> %s", gr.Gate, gr.Outcome, gr.Target)
	if gr.Verdict != nil {
		rationale := strings.TrimSpace(gr.Verdict.Rationale)
		if rationale == "" {
			rationale = strings.TrimSpace(gr.Verdict.Summary)
		}
		if rationale != "" {
			reason += ": " + rationale
		}
	}
	return reason, true
}

func (r *Runner) notifyTerminalGate(ctx context.Context, jr *journal.Run, runID string, repoRef apiv1.RepoRef, item *apiv1.BacklogItem, gr gate.Result, reason string) error {
	if r.cfg.Escalation == nil {
		return nil
	}
	itemIDs, err := r.terminalGateItemIDs(runID, item)
	if err != nil {
		// The claim-ledger fallback failed — journal and swallow, mirroring the
		// NotifyEscalated best-effort contract below. Surfacing the escalation is
		// best-effort; the run must still reach its terminal phase.
		if aerr := jr.Append(journal.Event{
			Type: journal.EventError,
			Gate: gr.Gate,
			Error: &journal.ErrorDetail{
				Code:    "gate_terminal_item_resolution_failed",
				Message: err.Error(),
			},
		}); aerr != nil {
			return fmt.Errorf("runner: journal terminal item resolution failure for gate %q: %w", gr.Gate, aerr)
		}
		return nil
	}
	for _, itemID := range itemIDs {
		if err := r.cfg.Escalation.NotifyEscalated(ctx, providerRepositoryRef(repoRef), itemID, gr, reason); err != nil {
			if aerr := jr.Append(journal.Event{
				Type: journal.EventError,
				Gate: gr.Gate,
				Error: &journal.ErrorDetail{
					Code:    "gate_terminal_notification_failed",
					Message: err.Error(),
				},
			}); aerr != nil {
				return fmt.Errorf("runner: journal terminal notification failure for gate %q: %w", gr.Gate, aerr)
			}
		}
	}
	return nil
}

func providerRepositoryRef(repo apiv1.RepoRef) providers.RepositoryRef {
	return providers.RepositoryRef{
		Provider: providers.ProviderKind(repo.Provider),
		Owner:    repo.Owner,
		Name:     repo.Name,
	}
}

// terminalGateItemIDs resolves the driving backlog item(s) an escalation
// comment should post to. A run started with an Item snapshot (in.Item, e.g. an
// item-triggered dispatch) uses it directly. A run started without one —
// scheduled/fan-out implementation runs, which self-select their item mid-run so
// in.Item is always nil (#796) — falls back to the claim ledger via the
// configured ClaimedItems resolver, exactly as buildBlockedHandler does for the
// blocked path. Nil resolver or no claims yields no ids: nothing to comment on,
// the prior behavior for a producer run with no driving issue.
func (r *Runner) terminalGateItemIDs(runID string, item *apiv1.BacklogItem) ([]string, error) {
	if item != nil && item.ID != "" {
		return []string{item.ID}, nil
	}
	if r.cfg.ClaimedItems == nil {
		return nil, nil
	}
	return r.cfg.ClaimedItems(runID)
}

// notifyBlocked invokes the configured Blocked handler (#544/#545/#552).
// A handler error is journaled (blocked_handling_failed) and swallowed — the
// run must still reach its terminal phase; the recording/parking is
// best-effort, mirroring notifyTerminalGate. Only a journal-write failure is
// returned (a journal that cannot be written is fatal, §2.6).
func (r *Runner) notifyBlocked(ctx context.Context, jr *journal.Run, o BlockedOutcome) error {
	if r.cfg.Blocked == nil {
		return nil
	}
	if err := r.cfg.Blocked(ctx, o); err != nil {
		if aerr := jr.Append(journal.Event{
			Type: journal.EventError, Stage: o.Stage,
			Error: &journal.ErrorDetail{Code: "blocked_handling_failed", Message: err.Error()},
		}); aerr != nil {
			return fmt.Errorf("runner: journal blocked-handling failure for %q: %w", o.Stage, aerr)
		}
	}
	return nil
}

// notifyBlockedEscalation surfaces a blocked stage through the same escalation
// notifier used by terminal gates. Provider and claim-resolution failures are
// journaled and swallowed so notification cannot prevent terminal cleanup.
func (r *Runner) notifyBlockedEscalation(ctx context.Context, jr *journal.Run, runID string, item *apiv1.BacklogItem, o BlockedOutcome) error {
	if r.cfg.Escalation == nil {
		return nil
	}
	itemIDs, err := r.terminalGateItemIDs(runID, item)
	if err != nil {
		if aerr := jr.Append(journal.Event{
			Type:  journal.EventError,
			Stage: o.Stage,
			Error: &journal.ErrorDetail{
				Code:    "stage_terminal_item_resolution_failed",
				Message: err.Error(),
			},
		}); aerr != nil {
			return fmt.Errorf("runner: journal terminal item resolution failure for stage %q: %w", o.Stage, aerr)
		}
		return nil
	}
	for _, itemID := range itemIDs {
		if err := r.cfg.Escalation.NotifyStageEscalated(ctx, providerRepositoryRef(o.RepoRef), itemID, o.Stage, o.Reason); err != nil {
			if aerr := jr.Append(journal.Event{
				Type:  journal.EventError,
				Stage: o.Stage,
				Error: &journal.ErrorDetail{
					Code:    "stage_terminal_notification_failed",
					Message: err.Error(),
				},
			}); aerr != nil {
				return fmt.Errorf("runner: journal terminal notification failure for stage %q: %w", o.Stage, aerr)
			}
		}
	}
	return nil
}

// notifyRateLimited invokes the configured RateLimited handler (#712). A
// handler error is journaled (rate_limited_handling_failed) and swallowed —
// unlike notifyBlocked, this never gates the run's own terminal phase, so
// only a journal-write failure is returned.
func (r *Runner) notifyRateLimited(ctx context.Context, jr *journal.Run, o RateLimitedOutcome) error {
	if r.cfg.RateLimited == nil {
		return nil
	}
	if err := r.cfg.RateLimited(ctx, o); err != nil {
		if aerr := jr.Append(journal.Event{
			Type: journal.EventError, Stage: o.Stage,
			Error: &journal.ErrorDetail{Code: "rate_limited_handling_failed", Message: err.Error()},
		}); aerr != nil {
			return fmt.Errorf("runner: journal rate-limited-handling failure for %q: %w", o.Stage, aerr)
		}
	}
	return nil
}

// notifyFailed invokes the configured Failed handler (#1054) at a terminal
// PhaseFailed transition — after the run_failed cause is journaled and before
// the run's terminal run.finished, mirroring notifyBlocked's ordering so the
// claim ledger still holds this run's claims (FinalizeTerminal releases them
// only after). A handler error is journaled (failed_handling_failed) and
// swallowed — the run must still reach its terminal phase; leaving the trace is
// best-effort. Only a journal-write failure is returned (a journal that cannot
// be written is fatal, §2.6).
func (r *Runner) notifyFailed(ctx context.Context, jr *journal.Run, o FailedOutcome) error {
	if r.cfg.Failed == nil {
		return nil
	}
	if err := r.cfg.Failed(ctx, o); err != nil {
		if aerr := jr.Append(journal.Event{
			Type: journal.EventError, Stage: o.Stage,
			Error: &journal.ErrorDetail{Code: "failed_handling_failed", Message: err.Error()},
		}); aerr != nil {
			return fmt.Errorf("runner: journal failed-handling failure for run %q: %w", o.RunID, aerr)
		}
	}
	return nil
}

// outputRateLimitReset parses the rateLimitReset RFC3339 timestamp a stage
// writes into its declared result file on a github_rate_limited failure
// (failProviderStage, cmd/goobers/providercmd.go, #614). Returns zero/false
// when the key is absent, not a string, or not parseable — a rate-limited
// failure whose reset couldn't be recovered simply skips notifyRateLimited
// rather than surfacing a second, unrelated parse error.
func outputRateLimitReset(outputs map[string]interface{}) (time.Time, bool) {
	v, ok := outputs["rateLimitReset"]
	if !ok {
		return time.Time{}, false
	}
	s, ok := v.(string)
	if !ok {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

// blockedReason condenses a blocked result's own explanation for the journal's
// blocked_by_agent cause event: the structured error detail when present
// (code-prefixed, so an agent's DEPENDENCY_NOT_MET survives into the run-level
// record), else the summary, else a fixed marker so the event is never empty.
func blockedReason(result apiv1.ResultEnvelope) string {
	if result.Error != nil && result.Error.Message != "" {
		if result.Error.Code != "" {
			return result.Error.Code + ": " + result.Error.Message
		}
		return result.Error.Message
	}
	if s := strings.TrimSpace(result.Summary); s != "" {
		return s
	}
	return "stage reported blocked with no error detail"
}

// failureCauseFrom extracts the bare (code, message) pair a business
// ResultFailure carries (issue #710) — code feeds Result.FailureCode directly
// (the scheduler/daemon echo's own "(stage: CODE)" suffix already shows the
// code, so folding it into message too would be redundant there); message is
// the bare stage-reported text, code-prefixed separately by the caller only
// where that reads better (the run_failed journal event, matching #545's
// blockedReason convention). Falls back to a fixed marker so neither is ever
// empty — docs/stage-contract.md requires "error" on every failure result,
// but a stage that violates that (a bug in the executor, not the contract)
// must still produce a describable cause rather than an empty one.
func failureCauseFrom(e *apiv1.ErrorInfo) (code, message string) {
	if e == nil || e.Message == "" {
		return "", "stage reported failure with no error detail"
	}
	return e.Code, e.Message
}

// parseBlockedBy extracts blocking issue numbers from the documented
// outputs.blockedBy convention (docs/stage-contract.md): a scalar string of
// comma-separated issue numbers — the envelope schema admits only scalars in
// outputs, which is exactly why live blocked results that tried a structured
// array got schema-rejected and burned an attempt (#545). Lenient on input
// shape (tolerates "#" prefixes, whitespace, a bare JSON number), strict on
// content: only all-digit tokens survive, deduplicated in first-seen order.
// Nil when the key is absent or nothing parseable remains.
func parseBlockedBy(outputs map[string]interface{}) []string {
	v, ok := outputs[OutputBlockedBy]
	if !ok || v == nil {
		return nil
	}
	var raw string
	switch n := v.(type) {
	case string:
		raw = n
	case float64:
		raw = strconv.FormatInt(int64(n), 10)
	case int:
		raw = strconv.Itoa(n)
	default:
		return nil
	}
	var out []string
	seen := map[string]bool{}
	for _, tok := range strings.FieldsFunc(raw, func(r rune) bool { return r == ',' || r == ';' }) {
		tok = strings.TrimPrefix(strings.TrimSpace(tok), "#")
		if tok == "" || seen[tok] {
			continue
		}
		allDigits := true
		for _, r := range tok {
			if r < '0' || r > '9' {
				allDigits = false
				break
			}
		}
		if !allDigits {
			continue
		}
		seen[tok] = true
		out = append(out, tok)
	}
	return out
}

// OutputBlockedBy is the documented ResultEnvelope output key a blocked stage
// references its blocking issue numbers through — comma-separated numbers in
// a single scalar string (outputs are scalar-only by schema). Shared by the
// runner's parse (above) and the instructions/docs that teach producers the
// convention.
const OutputBlockedBy = "blockedBy"

// failTerminal journals the run's terminal run.finished(PhaseFailed) event
// before surfacing origErr, so a walk-level error never leaves phase=running
// forever — the daemon auto-resumes every PhaseRunning run on restart
// (cmd/goobers/daemon.go), and an unterminated failed run would be resumed
// (and fail identically) on every restart. Per §2.6's fail-closed journaling,
// the journal must record the failure, not pretend the run is still live
// (ruling #110). If the terminal append itself fails, both errors are
// reported rather than one silently swallowing the other.
func (r *Runner) failTerminal(ctx context.Context, runID string, jr *journal.Run, repoRef apiv1.RepoRef, finalState string, steps int, origErr error) (Result, error) {
	// Record the actual cause as an error event before the bare terminal marker
	// (#305). finish() below journals only run.finished{PhaseFailed}; origErr was
	// otherwise merely returned up the Go call stack — which `goobers run` never
	// surfaces (it polls the journal for the terminal phase) and `goobers trace`
	// couldn't show either. So a walk-level failure (a gate-eval error, an
	// escalation-notify failure, max-steps, an unknown state) died with zero
	// recorded explanation anywhere an operator can reach. This is a run-level
	// failure, so the JOURNALED event carries no stage/gate attribution;
	// origErr's message already names the failing state. Best-effort: the
	// terminal marker must still be written even if this diagnostic append
	// fails (#110), and a journal write failure of either is reported
	// alongside origErr, never swallowing it.
	message := boundFailureMessage(origErr.Error())
	appendErr := jr.Append(journal.Event{
		Type:  journal.EventError,
		Error: &journal.ErrorDetail{Code: "run_failed", Message: origErr.Error()},
	})
	// #1054: leave a human-visible trace on the driving item before finish()'s
	// FinalizeTerminal releases the run's claims — this walk-level path is the
	// harness-timeout terminal (a dispatch-level runTask error routed here from
	// walk), the exact case that was silently returning the issue to ready.
	// SIGTERM must not skip the trace, but a stalled-run watchdog can interrupt
	// a hung provider call. The full origErr is what the item's comment records.
	nerr := r.notifyFailed(stalledAttemptContext(ctx), jr, FailedOutcome{RunID: runID, RepoRef: repoRef, Stage: finalState, Cause: origErr.Error()})
	if stalledResult, stalled, stalledErr := r.finishStalledRequest(ctx, runID, jr, finalState, steps); stalled {
		return stalledResult, stalledErr
	}
	res, ferr := r.finish(runID, jr, journal.PhaseFailed, finalState, steps)
	// FailureStage/Code/Message (issue #710) are populated on the RETURNED
	// Result regardless of the append's own outcome — even a best-effort
	// diagnostic-append failure must not silently drop the cause the caller
	// (the scheduler/daemon echo) needs; finalState is the failing stage/gate
	// name where available (a gate-eval error, e.g.), empty for a genuinely
	// state-less failure (max-steps, unknown state).
	res.FailureStage, res.FailureCode, res.FailureMessage = finalState, "run_failed", message
	if ferr != nil {
		return res, fmt.Errorf("%w (additionally failed to finalize terminal failure: %w)", origErr, ferr)
	}
	if appendErr != nil {
		return res, fmt.Errorf("%w (additionally failed to journal the failure cause: %w)", origErr, appendErr)
	}
	if nerr != nil {
		return res, fmt.Errorf("%w (additionally failed to journal the failed-trace failure: %w)", origErr, nerr)
	}
	return res, origErr
}

// finishStageFailure terminates the run as PhaseFailed because a STAGE reported
// ResultFailure, journaling a run_failed cause event WITH stage attribution (a
// stage-level failure, unlike a walk-level error, always has one — #710/#305)
// and threading the stage's own code/message onto the Result so the
// scheduler/daemon echo sites can surface "failed (merge-pr: …)" instead of a
// bare "failed". Shared by the two places a failed stage ends a run: its own
// Next terminating the run (taskOutcome), and a gate absorbing it into a
// terminal-complete branch (walk's gate handling, #849).
func (r *Runner) finishStageFailure(ctx context.Context, runID string, jr *journal.Run, repoRef apiv1.RepoRef, stage string, steps int, cause *apiv1.ErrorInfo) (Result, error) {
	code, message := failureCauseFrom(cause)
	journaledMessage := message
	if code != "" {
		// Code-prefixed for the on-disk cause event only (matching #545's
		// blockedReason convention — a code alongside the code-named
		// EventError.Code field would otherwise be lost to a plain grep of
		// run_failed messages); Result.FailureCode carries the code on its own
		// for the echo sites, so FailureMessage below stays bare.
		journaledMessage = code + ": " + message
	}
	if aerr := jr.Append(journal.Event{
		Type: journal.EventError, Stage: stage,
		Error: &journal.ErrorDetail{Code: "run_failed", Message: journaledMessage},
	}); aerr != nil {
		// This degenerate journal-write failure routes through failTerminal,
		// which fires notifyFailed itself — so the trace is left exactly once,
		// never doubly.
		return r.failTerminal(ctx, runID, jr, repoRef, stage, steps, fmt.Errorf("runner: journal failure cause for %q: %w", stage, aerr))
	}
	// #1054: leave a human-visible trace on the driving item for a stage-reported
	// terminal failure too, before finish()'s FinalizeTerminal releases claims.
	// The code-prefixed journaledMessage is the run's terminal cause.
	nerr := r.notifyFailed(stalledAttemptContext(ctx), jr, FailedOutcome{RunID: runID, RepoRef: repoRef, Stage: stage, Cause: journaledMessage})
	if stalledResult, stalled, stalledErr := r.finishStalledRequest(ctx, runID, jr, stage, steps); stalled {
		return stalledResult, stalledErr
	}
	res, err := r.finish(runID, jr, journal.PhaseFailed, stage, steps)
	res.FailureStage, res.FailureCode, res.FailureMessage = stage, code, boundFailureMessage(message)
	if err == nil && nerr != nil {
		err = nerr
	}
	return res, err
}

// taskOutcome applies the #110 stage-status ruling to a finished task's
// result: success advances to Next; failure advances when ContinueOnError is
// set or Next is a gate (which branches on the honest failed status), otherwise
// ends the run PhaseFailed; blocked ends the run PhaseEscalated (#544).
// advance=true means continue the walk at next; advance=false means the walk is
// over — return res (already appended its own terminal event, if any).
//
// Factored out of walk's live dispatch path so Resume (resume.go) can apply
// the IDENTICAL transition when it finds the checkpointed task's last
// attempt already finished, not interrupted — the walk must not re-dispatch
// it (#107), just pick up the same decision a live walk would have made
// right after runTask returned. ctx/repoRef/item feed only the blocked arm's
// instance-level handler (Config.Blocked); the transition decision itself
// stays pure.
func (r *Runner) taskOutcome(ctx context.Context, runID string, jr *journal.Run, machine *workflow.Machine, repoRef apiv1.RepoRef, item *apiv1.BacklogItem, t apiv1.Task, result apiv1.ResultEnvelope, steps int) (next string, res Result, advance bool, err error) {
	if stalledResult, stalled, stalledErr := r.finishStalledRequest(ctx, runID, jr, t.Name, steps); stalled {
		return "", stalledResult, false, stalledErr
	}
	switch result.Status {
	case apiv1.ResultBlocked:
		// #544 ruling: blocked is a schema-valid producer value, so it maps to
		// a canonical terminal phase — escalated, not failed (never punish the
		// producer for using the documented contract), and never the prior
		// immortal PhaseRunning pause (claim held to lease expiry, re-resumed
		// on every restart, run.finished never journaled — #545's 6 live
		// occurrences). The cause is journaled first (#305: finish() alone
		// records only the bare phase), then the escalation notifier and
		// instance-level parking handler run while the claim ledger still holds
		// this run's claims, then the ordinary terminal path releases everything
		// via FinalizeTerminal.
		o := BlockedOutcome{
			RunID:    runID,
			RepoRef:  repoRef,
			Stage:    t.Name,
			Reason:   blockedReason(result),
			Blockers: parseBlockedBy(result.Outputs),
		}
		if item != nil {
			o.ItemID = item.ID
		}
		if aerr := jr.Append(journal.Event{
			Type: journal.EventError, Stage: t.Name,
			Error: &journal.ErrorDetail{Code: "blocked_by_agent", Message: o.Reason},
		}); aerr != nil {
			res, err = r.failTerminal(ctx, runID, jr, repoRef, t.Name, steps, fmt.Errorf("runner: journal blocked cause for %q: %w", t.Name, aerr))
			return "", res, false, err
		}
		nerr := r.notifyBlockedEscalation(stalledAttemptContext(ctx), jr, runID, item, o)
		if stalledResult, stalled, stalledErr := r.finishStalledRequest(ctx, runID, jr, t.Name, steps); stalled {
			return "", stalledResult, false, stalledErr
		}
		if nerr != nil {
			res, err = r.failTerminal(ctx, runID, jr, repoRef, t.Name, steps, nerr)
			return "", res, false, err
		}
		// Same drain contract as notifyTerminalGate's call site: a SIGTERM
		// already in progress must not skip the block recording/parking.
		nerr = r.notifyBlocked(stalledAttemptContext(ctx), jr, o)
		if stalledResult, stalled, stalledErr := r.finishStalledRequest(ctx, runID, jr, t.Name, steps); stalled {
			return "", stalledResult, false, stalledErr
		}
		if nerr != nil {
			res, err = r.failTerminal(ctx, runID, jr, repoRef, t.Name, steps, nerr)
			return "", res, false, err
		}
		res, err = r.finish(runID, jr, journal.PhaseEscalated, t.Name, steps)
		return "", res, false, err
	case apiv1.ResultFailure:
		// #712: notify before any routing decision below — a rate-limited
		// stage failure means "the scheduler should stop dispatching more
		// provider-dependent runs until reset", which is true regardless of
		// whether THIS run repasses into a gate, escalates, or ends failed.
		// A side notification, not a control-flow branch: it never changes
		// what happens next, only journals a handler failure if the handler
		// itself can't be recorded (fail-closed, mirrors notifyBlocked).
		if result.Error != nil && result.Error.Code == providers.ErrorCodeRateLimited {
			if resetAt, ok := outputRateLimitReset(result.Outputs); ok {
				o := RateLimitedOutcome{RunID: runID, Stage: t.Name, ResetAt: resetAt}
				nerr := r.notifyRateLimited(stalledAttemptContext(ctx), jr, o)
				if stalledResult, stalled, stalledErr := r.finishStalledRequest(ctx, runID, jr, t.Name, steps); stalled {
					return "", stalledResult, false, stalledErr
				}
				if nerr != nil {
					res, err = r.failTerminal(ctx, runID, jr, repoRef, t.Name, steps, nerr)
					return "", res, false, err
				}
			}
		}
		if t.ContinueOnError {
			if aerr := journalToleratedFailure(jr, t.Name); aerr != nil {
				res, err = r.failTerminal(ctx, runID, jr, repoRef, t.Name, steps, fmt.Errorf("runner: journal tolerated failure for %q: %w", t.Name, aerr))
				return "", res, false, err
			}
			break
		}
		// #415: a stage that self-identifies a non-retryable business
		// disposition — status:failure with error.retryable==false and a
		// recognized escalate code (ISSUE_OVER_SCOPE / NEEDS_DECOMPOSITION) —
		// bypasses the Next gate evaluator and its repass loop after this one
		// attempt. Its escalation control branch may route disposition work
		// before termination; without one the run ends at @escalate. Otherwise
		// an un-scopeable issue the implementer correctly rejected on attempt 1
		// re-enters the reviewer→implement loop and re-derives the identical
		// conclusion until the budget exhausts.
		if result.Status == apiv1.ResultFailure && isNonRetryableEscalation(result.Error) {
			target := taskEscalationTarget(machine, t)
			switch target {
			case workflow.TargetAbort:
				res, err = r.finish(runID, jr, journal.PhaseAborted, t.Name, steps)
				return "", res, false, err
			case workflow.TargetEscalate:
				res, err = r.finish(runID, jr, journal.PhaseEscalated, t.Name, steps)
				return "", res, false, err
			case workflow.TerminalComplete:
				res, err = r.finish(runID, jr, journal.PhaseEscalated, t.Name, steps)
				return "", res, false, err
			default:
				return target, Result{}, true, nil
			}
		}
		if _, isGate := machine.Gate(t.Next); t.Next != "" && isGate {
			return t.Next, Result{}, true, nil
		}
		// #710: the stage's own business error (e.g. a deterministic
		// pr-select surfacing errorCode:"github_rate_limited") was already
		// journaled on stage.finished a moment ago (runTask), but the run's
		// OWN terminal event carried nothing beyond a bare status:"failed" —
		// #705's root cause was sitting one journal line above the
		// terminal marker the entire 16 hours it went unseen. Append a
		// run_failed cause event mirroring failTerminal's own pattern (#305),
		// this time WITH stage attribution since a business failure, unlike a
		// walk-level error, always has one, then thread the stage's own code/
		// message onto the returned Result so the scheduler/daemon echo sites
		// can surface "failed (pr-select: github_rate_limited)" instead of a
		// bare "failed".
		res, err = r.finishStageFailure(ctx, runID, jr, repoRef, t.Name, steps, result.Error)
		return "", res, false, err
	case apiv1.ResultNoWork:
		// Short-circuits straight to PhaseCompleted, unconditionally — never
		// t.Next, regardless of what it names (issue #233): a query stage
		// that genuinely found nothing to do must not hand off to a
		// downstream agentic stage with no subject to act on. Distinct from
		// the reserved-Next-target switch below, which only fires for a
		// state the WORKFLOW DEFINITION declares as terminal — this fires
		// on the STAGE'S OWN reported outcome, so an ordinary
		// query-backlog -> curate/implement wiring (Next names a real,
		// non-reserved state) still terminates cleanly on an empty tick
		// without the workflow author having to special-case it in the DSL.
		res, err = r.finish(runID, jr, journal.PhaseCompleted, t.Name, steps)
		return "", res, false, err
	}
	// A successful task's Next may be a plain state name or one of the
	// compiler's reserved terminal targets (@abort/@escalate, #123) — the
	// same three-way switch the gate branch below already uses. Before this
	// fix, only "" was handled here; a compiled definition with a reserved
	// task-next fell through to being treated as a state name, then
	// "unknown state" in walk(), even though workflow.Compile admits it
	// identically to a gate branch (ARCHITECTURE.md §3.3: a compile-admitted
	// surface must not crash one runner while completing on another).
	switch t.Next {
	case workflow.TargetAbort:
		res, err = r.finish(runID, jr, journal.PhaseAborted, t.Name, steps)
		return "", res, false, err
	case workflow.TargetEscalate:
		res, err = r.finish(runID, jr, journal.PhaseEscalated, t.Name, steps)
		return "", res, false, err
	case workflow.TerminalComplete:
		res, err = r.finish(runID, jr, journal.PhaseCompleted, t.Name, steps)
		return "", res, false, err
	}
	return t.Next, Result{}, true, nil
}

func journalToleratedFailure(jr *journal.Run, stage string) error {
	rd, err := journal.OpenRead(jr.Dir())
	if err != nil {
		return err
	}
	events, err := rd.Events()
	if err != nil {
		return err
	}
	var attempt int
	var attemptClass journal.AttemptClass
	for i := len(events) - 1; i >= 0; i-- {
		event := events[i]
		if event.Stage != stage {
			continue
		}
		if event.Type == journal.EventError && event.Error != nil && event.Error.Code == toleratedFailureErrorCode {
			return nil
		}
		if event.Type == journal.EventStageFinished {
			attempt = event.Attempt
			attemptClass = event.AttemptClass
			break
		}
	}
	return jr.Append(journal.Event{
		Type:         journal.EventError,
		Stage:        stage,
		Attempt:      attempt,
		AttemptClass: attemptClass,
		Error: &journal.ErrorDetail{
			Code:    toleratedFailureErrorCode,
			Message: fmt.Sprintf("stage %q failure tolerated by continueOnError", stage),
		},
	})
}

// finish claims terminalization from the watchdog before preparing cleanup.
func (r *Runner) finish(runID string, jr *journal.Run, phase journal.RunPhase, finalState string, steps int) (Result, error) {
	if outcome, takenOver := r.claimOwnerTerminalization(runID); takenOver {
		return outcome.result, outcome.err
	}
	return r.finishTakeover(runID, jr, phase, finalState, steps)
}

// finishTakeover performs terminal cleanup for an already-claimed watchdog
// takeover, or for a recovered run with no live owner.
func (r *Runner) finishTakeover(runID string, jr *journal.Run, phase journal.RunPhase, finalState string, steps int) (Result, error) {
	if err := r.prepareTerminal(runID, phase, jr); err != nil {
		return Result{}, err
	}
	if err := jr.Append(journal.Event{Type: journal.EventRunFinished, Status: string(phase)}); err != nil {
		return Result{}, fmt.Errorf("runner: journal run.finished: %w", err)
	}
	res := Result{Phase: phase, FinalState: finalState, Steps: steps}
	r.notifyTerminal(runID, phase, finalState)
	if err := r.FinalizeTerminal(runID, phase); err != nil {
		return res, err
	}
	return res, nil
}

func (r *Runner) notifyTerminal(runID string, phase journal.RunPhase, finalState string) {
	if r.cfg.NotifyTerminal != nil {
		_ = r.cfg.NotifyTerminal(runID, phase, finalState)
	}
}

func (r *Runner) prepareTerminal(runID string, phase journal.RunPhase, jr *journal.Run) error {
	if r.cfg.PrepareTerminal == nil {
		return nil
	}
	if err := r.cfg.PrepareTerminal(runID, phase, jr); err != nil {
		return fmt.Errorf("runner: prepare terminal run %q (%s): %w", runID, phase, err)
	}
	return nil
}

// FinalizeTerminal runs the configured idempotent instance-level finalizer.
// Startup recovery uses the same entrypoint after discovering a terminal run.
func (r *Runner) FinalizeTerminal(runID string, phase journal.RunPhase) error {
	if r.cfg.FinalizeTerminal == nil {
		return nil
	}
	if err := r.cfg.FinalizeTerminal(runID, phase); err != nil {
		return fmt.Errorf("runner: finalize terminal run %q (%s): %w", runID, phase, err)
	}
	return nil
}

// runTask dispatches one task, retrying dispatch-level failures up to its
// declared policy: provisions a fresh worktree per attempt, invokes the
// matching invoke.Deterministic/invoke.Goober executor (which resolves its own
// capability-scoped credentials and commits its own artifacts to the run
// journal — see executor.go's ArtifactRecorder doc), and journals every
// attempt distinctly, never overwriting history (§5). Per the Temporal
// engine's established semantics, a SUCCESSFUL attempt's task always flows to
// Next regardless of its business ResultStatus — a downstream gate is what
// branches on that; only a genuine dispatch error (the executor returning a
// Go error — infra failure, not a business "failure" status) triggers a
// retry. Dispatch errors marked by invoke.InfrastructureFailure use the
// runner's bounded infrastructure allowance and journal their retry as
// infrastructure; other dispatch retries use Task.Retry and are policy
// attempts.
//
// Dispatch ignores ordinary run-level cancellation (SIGTERM), so the current
// attempt can finish and journal cleanly; only a stalled-run watchdog request
// cancels it mid-dispatch. walk checks ordinary cancellation between stages.
//
// startAttempt is normally 1; a resume past an interrupted attempt (resume.go)
// passes the next attempt number instead, so the attempts a crash already
// consumed still count against the task's own MaxAttempts budget — a crash
// must never grant a task more attempts than its declared policy allows.
//
// upstreamResult is the immediately preceding stage's ResultEnvelope (the
// zero value for the run's first task) — dispatchTask threads its Outputs
// into this task's Inputs per t.InputsFrom (#132), the task-to-task analog
// of evaluateGate's unconditional Outputs flatten.
func (r *Runner) startStageHeartbeat(ctx context.Context, jr journalAppender, stage string, attempt int, class journal.AttemptClass) (context.Context, stageHeartbeat) {
	interval := r.heartbeatInterval
	if interval <= 0 {
		interval = StageHeartbeatInterval
	}
	newTicker := r.newHeartbeatTicker
	if newTicker == nil {
		newTicker = func(interval time.Duration) heartbeatTicker {
			return wallHeartbeatTicker{ticker: time.NewTicker(interval)}
		}
	}
	ticker := newTicker(interval)
	stop := make(chan struct{})
	done := make(chan error, 1)
	var progressed atomic.Bool
	ctx = invoke.WithProgressReporter(ctx, func() {
		jr.ObserveActivity()
		progressed.Store(true)
	})
	go func() {
		var heartbeatErr error
		defer func() {
			ticker.Stop()
			done <- heartbeatErr
		}()
		for {
			select {
			case <-stop:
				return
			case <-ctx.Done():
				return
			case <-ticker.Ticks():
				if !progressed.Swap(false) {
					continue
				}
				if err := jr.Append(journal.Event{
					Type:         journal.EventStageHeartbeat,
					Stage:        stage,
					Attempt:      attempt,
					AttemptClass: class,
				}); err != nil {
					heartbeatErr = fmt.Errorf("runner: journal stage.heartbeat for %q: %w", stage, err)
					return
				}
			}
		}
	}()
	return ctx, stageHeartbeat{stop: stop, done: done}
}

type gateHeartbeatGoober struct {
	goober  invoke.Goober
	runner  *Runner
	journal gateHeartbeatJournal
	stage   string
	attempt int
}

type gateHeartbeatJournal interface {
	journalAppender
	RepairAppendBoundary() error
}

func (g gateHeartbeatGoober) Invoke(ctx context.Context, env apiv1.InvocationEnvelope) (apiv1.ResultEnvelope, error) {
	return g.goober.Invoke(ctx, env)
}

func (g gateHeartbeatGoober) Review(ctx context.Context, env apiv1.InvocationEnvelope) (apiv1.Verdict, error) {
	ctx, heartbeat := g.runner.startStageHeartbeat(ctx, g.journal, g.stage, g.attempt, journal.AttemptPolicy)
	verdict, reviewErr := g.goober.Review(ctx, env)
	heartbeatErr := heartbeat.Stop()
	if heartbeatErr != nil {
		if repairErr := g.journal.RepairAppendBoundary(); repairErr != nil {
			heartbeatErr = fmt.Errorf("runner: repair journal after gate heartbeat failure for %q: %w", g.stage, errors.Join(heartbeatErr, repairErr))
		}
	}
	return verdict, errors.Join(reviewErr, heartbeatErr)
}

func finishTaskDispatch(jr *journal.Run, heartbeat stageHeartbeat, stage string, attempt int, class journal.AttemptClass, mutations []mutationFact, removeErr error) error {
	heartbeatErr := heartbeat.Stop()
	if heartbeatErr != nil {
		if err := jr.RepairAppendBoundary(); err != nil {
			return fmt.Errorf("runner: repair journal after heartbeat failure for %q: %w", stage, errors.Join(heartbeatErr, err))
		}
	}
	for _, m := range mutations {
		// Best-effort, like ClaimLedger's own journal() (issue #228): a
		// provider mutation already happened for real regardless of
		// whether this projection succeeds, so a failed Append here must
		// not fail the stage or mask the mutation's own outcome.
		_ = jr.Append(journal.Event{
			Type: journal.EventRefTouched, Stage: stage, Attempt: attempt, AttemptClass: class,
			ExternalRef: &journal.ExternalRef{Provider: m.Provider, Kind: m.Kind, ID: m.ID, URL: m.URL},
			Runner:      map[string]any{"operation": m.Operation},
		})
	}
	if removeErr != nil {
		// Non-fatal (issue #136): a failed worktree teardown doesn't
		// change this attempt's own outcome, and worktree.Create's own
		// adopt-and-reset means it no longer blocks the next attempt
		// either — but the append recording it must not itself be
		// silently discarded (#243): a journal that cannot be written
		// is fatal (§2.6), consistent with the executor_error and
		// stage.finished appends just below treating their own write
		// failures the same way.
		if err := jr.Append(journal.Event{
			Type: journal.EventError, Stage: stage, Attempt: attempt, AttemptClass: class,
			Error: &journal.ErrorDetail{Code: "worktree_remove_failed", Message: removeErr.Error()},
		}); err != nil {
			return fmt.Errorf("runner: journal worktree removal error for %q: %w", stage, err)
		}
	}
	return heartbeatErr
}

func (r *Runner) runTask(ctx context.Context, jr *journal.Run, in StartInput, ex *executors, t apiv1.Task, upstream []apiv1.ContextPointer, upstreamResult apiv1.ResultEnvelope, startAttempt int32, firstClass journal.AttemptClass, instructionAddendum, workspaceBranch string, rerun *rerunContext) (apiv1.ResultEnvelope, []apiv1.ContextPointer, error) {
	policyMaxAttempts := int32(1)
	var backoff time.Duration
	if t.Retry != nil {
		if t.Retry.MaxAttempts > 0 {
			policyMaxAttempts = t.Retry.MaxAttempts
		}
		backoff = time.Duration(t.Retry.BackoffSeconds) * time.Second
	}
	// The infrastructure budget includes its triggering failure, so it can add
	// at most MaxInfrastructureAttempts-1 dispatches to the policy budget.
	maxAttempts := policyMaxAttempts + DefaultMaxInfrastructureAttempts - 1
	policyAttempts := startAttempt - 1
	var infrastructureFailures int32
	if rerun != nil {
		maxAttempts = int32(rerun.requestAttempt) + policyMaxAttempts + DefaultMaxInfrastructureAttempts - 2
		policyAttempts = rerun.policyAttempts
		infrastructureFailures = rerun.infrastructureFailures
		if policyAttempts >= policyMaxAttempts {
			err := fmt.Errorf("runner: task %q has no attempts left after resuming human rerun (interrupted attempts already exhausted its %d-attempt policy budget)", t.Name, policyMaxAttempts)
			return apiv1.ResultEnvelope{}, nil, err
		}
		if infrastructureFailures >= DefaultMaxInfrastructureAttempts {
			err := fmt.Errorf("runner: task %q has no attempts left after resuming human rerun (infrastructure failures already exhausted its %d-attempt infrastructure budget)", t.Name, DefaultMaxInfrastructureAttempts)
			return apiv1.ResultEnvelope{}, nil, err
		}
		if startAttempt > maxAttempts {
			err := fmt.Errorf("runner: task %q has no attempts left after resuming human rerun (interrupted attempts exhausted the combined retry budget)", t.Name)
			return apiv1.ResultEnvelope{}, nil, err
		}
	} else if startAttempt > policyMaxAttempts {
		err := fmt.Errorf("runner: task %q has no attempts left after resume (interrupted attempt already exhausted its %d-attempt budget)", t.Name, policyMaxAttempts)
		return apiv1.ResultEnvelope{}, nil, err
	}

	var lastErr error
	nextRetryClass := journal.AttemptPolicy
	for attempt := startAttempt; attempt <= maxAttempts; attempt++ {
		if _, ok := stalledRequestFromContext(ctx); ok {
			return apiv1.ResultEnvelope{}, nil, errStalledRun
		}
		// An ordinary initial attempt carries no class. An operator rerun starts
		// with "human"; a retry within this dispatch uses the class selected by
		// the prior failure ("infra" or "policy"). A crash-driven continuation
		// starts "infra" so it stays excluded from conformance (§3.3).
		var class journal.AttemptClass
		switch {
		case attempt == startAttempt && firstClass != "":
			class = firstClass
		case attempt == startAttempt && startAttempt > 1:
			class = journal.AttemptInfra
		case attempt > startAttempt:
			class = nextRetryClass
		}
		// A crash-driven continuation is infra-tagged for conformance but still
		// consumes policy budget; provider infrastructure retries do not.
		if class != journal.AttemptInfra || attempt == startAttempt {
			policyAttempts++
		}
		attemptCtx, span := r.startTaskSpan(stalledAttemptContext(ctx), in, t, int(attempt), string(class))
		if err := jr.Append(journal.Event{Type: journal.EventStageStarted, Stage: t.Name, Attempt: int(attempt), AttemptClass: class}); err != nil {
			err = fmt.Errorf("runner: journal stage.started for %q: %w", t.Name, err)
			span.Fail(err)
			return apiv1.ResultEnvelope{}, nil, err
		}

		attemptCtx, heartbeat := r.startStageHeartbeat(attemptCtx, jr, t.Name, int(attempt), class)
		var attemptAddendum string
		if class == journal.AttemptHuman {
			attemptAddendum = instructionAddendum
		}
		result, mutations, dispatchErr, removeErr := r.dispatchTask(attemptCtx, jr, in, ex, t, upstream, upstreamResult, int(attempt), class, attemptAddendum, span, workspaceBranch)
		if err := finishTaskDispatch(jr, heartbeat, t.Name, int(attempt), class, mutations, removeErr); err != nil {
			span.Fail(err)
			return apiv1.ResultEnvelope{}, nil, err
		}
		if _, ok := stalledRequestFromContext(ctx); ok {
			return apiv1.ResultEnvelope{}, nil, errStalledRun
		}
		if dispatchErr != nil {
			lastErr = dispatchErr
			retryLimit := policyMaxAttempts
			retryCount := policyAttempts
			shouldRetry := policyAttempts < policyMaxAttempts
			nextRetryClass = journal.AttemptPolicy
			failureClass, _ := retryFailureClass(dispatchErr, result)
			if failureClass == journal.AttemptInfra {
				infrastructureFailures++
				retryLimit = DefaultMaxInfrastructureAttempts
				retryCount = infrastructureFailures
				shouldRetry = infrastructureFailures < DefaultMaxInfrastructureAttempts
				nextRetryClass = journal.AttemptInfra
			}
			// A journal that cannot be written stops the run (§2.6): this
			// write failing means the run's own record of what happened is
			// now unreliable, so it is fatal, not best-effort.
			if aerr := jr.Append(journal.Event{
				Type: journal.EventError, Stage: t.Name, Attempt: int(attempt), AttemptClass: class,
				Error:  &journal.ErrorDetail{Code: "executor_error", Message: dispatchErr.Error()},
				Runner: map[string]any{retryFailureClassKey: string(failureClass)},
			}); aerr != nil {
				err := fmt.Errorf("runner: journal executor error for %q: %w", t.Name, aerr)
				span.Fail(err)
				return apiv1.ResultEnvelope{}, nil, err
			}
			span.FailWithCode(dispatchErr, "executor_error")
			if shouldRetry {
				if backoff > 0 {
					// Wait on the run-level ctx (not attemptCtx, which never
					// cancels — the drain contract for an in-flight
					// dispatch), so a SIGTERM already in progress doesn't
					// block this idle wait for the full backoff on every
					// remaining retry of a transient-failure storm (#112).
					// Dispatch itself, and the number of attempts a task may
					// use, are unaffected — this only shortens how long a
					// graceful shutdown waits between attempts; the next
					// attempt still proceeds exactly as before (no new
					// checkpoint/pause point — a graceful drain only ever
					// pauses BETWEEN stages, per resume.go's interruptedAttempt
					// doc).
					select {
					case <-time.After(backoff):
					case <-ctx.Done():
					case <-attemptCtx.Done():
					}
				}
				if _, ok := stalledRequestFromContext(ctx); ok {
					return apiv1.ResultEnvelope{}, nil, errStalledRun
				}
				continue
			}
			err := fmt.Errorf("runner: execute stage %q: %w (attempt %d/%d)", t.Name, lastErr, retryCount, retryLimit)
			return apiv1.ResultEnvelope{}, nil, err
		}

		outputs := result.Outputs
		if result.Status == apiv1.ResultFailure && t.ContinueOnError {
			outputs = nil
		}
		if err := jr.Append(journal.Event{
			Type: journal.EventStageFinished, Stage: t.Name, Attempt: int(attempt), AttemptClass: class,
			Status: string(result.Status), Error: errorDetailFrom(result),
			Outputs: outputs, Artifacts: refsFrom(result.Artifacts),
		}); err != nil {
			err = fmt.Errorf("runner: journal stage.finished for %q: %w", t.Name, err)
			span.Fail(err)
			return apiv1.ResultEnvelope{}, nil, err
		}
		// #710: a stage returning a business "failure" ResultEnvelope is a
		// dispatch SUCCESS (the executor ran to completion and reported a
		// clean business outcome) — Fail'd above is reserved for a genuine Go
		// dispatch error, never this. But span.Succeed unconditionally here
		// meant a failed stage's own span reported codes.Ok with the literal
		// message "failure", same defect as the run's root span above.
		errorCode := ""
		if result.Error != nil {
			errorCode = result.Error.Code
		}
		span.CompleteWithError(string(result.Status), errorCode, result.Status == apiv1.ResultFailure)
		return result, contextPointersFor(t.Name, result.Artifacts), nil
	}
	// Unreachable: maxAttempts >= 1 always executes the loop body at least
	// once, and every path inside either returns or continues.
	err := fmt.Errorf("runner: execute stage %q: exhausted attempts: %w", t.Name, lastErr)
	return apiv1.ResultEnvelope{}, nil, err
}

// retryFailureClass covers both dispatch retries and business failures that a
// downstream gate sends through its bounded repass loop.
func retryFailureClass(dispatchErr error, result apiv1.ResultEnvelope) (journal.AttemptClass, bool) {
	if dispatchErr != nil {
		if invoke.IsInfrastructureFailure(dispatchErr) {
			return journal.AttemptInfra, true
		}
		return journal.AttemptPolicy, true
	}
	if result.Status != apiv1.ResultFailure || result.Error == nil {
		return "", false
	}
	switch result.Error.Code {
	case "nonzero_exit", baseSyncConflictErrorCode:
		return journal.AttemptPolicy, true
	default:
		return "", false
	}
}

func journalRetryDecision(jr *journal.Run, result gate.Result, stage string, subject apiv1.ResultEnvelope) error {
	class, ok := retryFailureClass(nil, subject)
	if !ok || result.Outcome == gate.OutcomePass || result.Escalated {
		return nil
	}
	switch result.Target {
	case workflow.TargetAbort, workflow.TargetEscalate, workflow.TerminalComplete:
		return nil
	}
	return jr.Append(journal.Event{
		Type:  journal.EventRunnerAnnotation,
		Stage: stage,
		Gate:  result.Gate,
		Runner: map[string]any{
			"kind":               retryDecisionKind,
			retryFailureClassKey: string(class),
			"failureCode":        subject.Error.Code,
			"repassAttempt":      result.Attempt,
			"target":             result.Target,
		},
	})
}

// startTaskSpan opens one task-attempt span under the run's trace, if telemetry is
// configured. A zero telemetry.Span is safe to use (its methods no-op).
func (r *Runner) startTaskSpan(ctx context.Context, in StartInput, t apiv1.Task, attempt int, attemptKind string) (context.Context, telemetry.Span) {
	if r.cfg.Telemetry == nil {
		return ctx, telemetry.Span{}
	}
	attrs := telemetry.TaskAttributes{
		Gaggle:          in.Gaggle,
		WorkflowID:      in.Machine.Def.Name,
		WorkflowVersion: strconv.Itoa(in.Machine.Def.Version),
		WorkflowDigest:  in.Machine.Digest(),
		RunID:           in.RunID,
		TaskID:          t.Name,
		TaskType:        string(t.Type),
		GooberID:        t.Goober,
		Attempt:         attempt,
		AttemptKind:     attemptKind,
	}
	if t.Type == apiv1.TaskAgentic {
		provenance := r.cfg.AgentProvenance[t.Goober]
		attrs.Model = provenance.Model
		attrs.HarnessVersion = provenance.HarnessVersion
	}
	if in.Item != nil {
		attrs.ItemID = in.Item.ID
		attrs.ItemURL = in.Item.URL
	}
	ctx, span, err := r.cfg.Telemetry.StartTask(ctx, attrs)
	if err != nil {
		return ctx, telemetry.Span{}
	}
	return ctx, span
}

// dispatchTask provisions one attempt's workspace and invokes the task's
// executor. It never journals its own result/err — runTask owns attempt/
// retry journaling so a retried attempt is never mistaken for the run's
// overall outcome. removeErr is separate and additive: a failed workspace
// teardown (issue #136 — worktree failures were previously silently discarded,
// letting a failed
// Remove turn every subsequent retry of this stage into a guaranteed
// "already has a worktree" error) never overrides the stage's own
// result/err, but runTask still surfaces it as a journaled warning so it's
// visible rather than silently dropped. Adopt-and-reset (worktree.Create,
// issue #136) means a failed Remove no longer blocks the next attempt
// either way — this is purely about not hiding the failure.
//
// mutations is the same kind of additive, non-overriding signal (issue
// #228): a deterministic stage's underlying subcommand (backlog-query/
// open-pr/issue-close-out) runs as a separate short-lived process with no
// legal journal access, so it records its provider-mutation facts to a
// sidecar file in the workspace instead; dispatchTask reads that sidecar
// (before cleanup destroys the workspace, since runTask can't read
// it after the fact) and returns the parsed facts for runTask to project
// into ref.touched events. Read on the deterministic success path only —
// mutations only ever come from a deterministic provider-chain subcommand,
// never an agentic stage.
//
// After buildEnvelope, t.InputsFrom is applied on top of the static declared
// Inputs: for each inputKey/outputKey pair, upstreamResult.Outputs[outputKey]
// overlays env.Inputs[inputKey] (#132) — a declared outputKey missing from
// upstreamResult.Outputs fails the stage closed, since InputsFrom is a
// contract, not a hint (unlike evaluateGate's unconditional Outputs flatten,
// which is safe precisely because a gate never mutates run state on a wide-
// open read).
func (r *Runner) dispatchTask(ctx context.Context, jr *journal.Run, in StartInput, ex *executors, t apiv1.Task, upstream []apiv1.ContextPointer, upstreamResult apiv1.ResultEnvelope, attempt int, class journal.AttemptClass, instructionAddendum string, span telemetry.Span, workspaceBranch string) (result apiv1.ResultEnvelope, mutations []mutationFact, err error, removeErr error) {
	workspaceMode := apiv1.WorkspaceRepo
	if t.Run != nil && t.Run.Workspace != "" {
		workspaceMode = t.Run.Workspace
	}
	taskInputs := workflow.TaskInvocationInputs(in.Machine, t)
	syncBase := t.Run != nil && t.Run.SyncBase
	env, workspace, err := r.buildEnvelope(ctx, in, t.Name, t.Goal, taskInputs, t.Capabilities, workflow.TaskLimits(t), upstream, workspaceMode, syncBase, workspaceBranch)
	if err != nil {
		prepErr := fmt.Errorf("prepare stage %q: %w", t.Name, err)
		var conflict *worktree.BaseSyncConflictError
		if errors.As(err, &conflict) {
			data, marshalErr := json.Marshal(baseSyncConflictArtifact{
				Code:             baseSyncConflictErrorCode,
				Message:          prepErr.Error(),
				Branch:           conflict.Branch,
				BaseRef:          conflict.BaseRef,
				ConflictingFiles: conflict.ConflictingFiles,
			})
			if marshalErr != nil {
				return apiv1.ResultEnvelope{}, nil, fmt.Errorf("marshal base synchronization conflict for stage %q: %w", t.Name, marshalErr), nil
			}
			ref, recordErr := jr.RecordStageArtifact(t.Name, attempt, class, t.Name+"/base-sync-conflict.json", data)
			if recordErr != nil {
				return apiv1.ResultEnvelope{}, nil, fmt.Errorf("record base synchronization conflict for stage %q: %w", t.Name, recordErr), nil
			}
			return apiv1.ResultEnvelope{
				Status:  apiv1.ResultFailure,
				Summary: "base synchronization conflicted; the implementation branch was preserved for remediation",
				Artifacts: []apiv1.ArtifactPointer{{
					Path: ref.Path, Digest: ref.Digest, Size: ref.Size, MediaType: "application/json",
				}},
				Error: &apiv1.ErrorInfo{
					Code:      baseSyncConflictErrorCode,
					Message:   prepErr.Error(),
					Retryable: true,
				},
			}, nil, nil, nil
		}
		// #572: a transient network/remote failure provisioning the stage's
		// worktree (clone/fetch/worktree-add) is retryable infrastructure,
		// same as #613's transient built-in provider failures — classified
		// here and marked via the identical invoke.InfrastructureFailure
		// seam, so it flows through runTask's existing bounded
		// infrastructure retry budget with zero changes to that loop, resume
		// reconstruction, or journaling. Auth/missing-ref/other deterministic
		// worktree failures are unmarked and fail the run immediately, same
		// as before this check existed.
		if worktree.IsTransientProvisionError(err) {
			return apiv1.ResultEnvelope{}, nil, invoke.InfrastructureFailure(prepErr), nil
		}
		return apiv1.ResultEnvelope{}, nil, prepErr, nil
	}
	env.InstructionAddendum = instructionAddendum
	telemetryDir := telemetry.ResetStageTelemetryDir(env.Workspace)
	var agentInvocation *gooberInvocation
	defer func() {
		telemetry.IngestStageEmissions(telemetryDir, &result, span)
		telemetry.CleanupStageTelemetryDir(telemetryDir)
		if agentInvocation != nil && agentInvocation.materializedAssets() {
			if validationErr := workspace.ValidateReservedPaths(context.WithoutCancel(ctx)); validationErr != nil {
				err = errors.Join(err, fmt.Errorf("stage %q: %w", t.Name, validationErr))
			}
		}
		removeErr = workspace.Remove(ctx)
	}()

	// Surface any non-fatal worktree-provisioning warnings (today: symlinks a
	// symlink-less platform flattened to plain files, #643) into the run journal
	// as a runner.annotation — non-normative, excluded from conformance, but
	// operator-visible — rather than letting the degradation pass silently.
	if ev, ok := worktreeWarningEvent(t.Name, workspace.worktree); ok {
		if err := jr.Append(ev); err != nil {
			return apiv1.ResultEnvelope{}, nil, fmt.Errorf("task %q: journal worktree warnings: %w", t.Name, err), nil
		}
	}

	for inputKey, outputKey := range t.InputsFrom {
		v, ok := upstreamResult.Outputs[outputKey]
		if !ok {
			return apiv1.ResultEnvelope{}, nil, fmt.Errorf("task %q: inputsFrom %q: upstream output %q not found", t.Name, inputKey, outputKey), nil
		}
		env.Inputs[inputKey] = v
	}

	switch t.Type {
	case apiv1.TaskDeterministic:
		if t.Run == nil {
			return apiv1.ResultEnvelope{}, nil, fmt.Errorf("task %q is deterministic but declares no DeterministicRun", t.Name), nil
		}
		det, err := ex.deterministic()
		if err != nil {
			return apiv1.ResultEnvelope{}, nil, err, nil
		}
		if err := recordContextManifest(jr, env, t.Name, attempt, class); err != nil {
			return apiv1.ResultEnvelope{}, nil, fmt.Errorf("task %q: record context manifest: %w", t.Name, err), nil
		}
		result, err = det.Run(ctx, env, *t.Run)
		if err == nil {
			mutations = readMutationSidecar(env.Workspace)
		}
		return result, mutations, err, nil
	case apiv1.TaskAgentic:
		ag, err := ex.agentic(t.Goober)
		if err != nil {
			return apiv1.ResultEnvelope{}, nil, err, nil
		}
		agentInvocation = &gooberInvocation{
			Goober:                 ag,
			activateAssetPathGuard: workspace.ActivateAssetPathGuard,
		}
		if err := recordContextManifest(jr, env, t.Name, attempt, class); err != nil {
			return apiv1.ResultEnvelope{}, nil, fmt.Errorf("task %q: record context manifest: %w", t.Name, err), nil
		}
		result, err = agentInvocation.Invoke(ctx, env)
		// #724: a stage that opts into OnTimeout=salvage completes with its
		// already-committed diff instead of discarding a timed-out attempt whose
		// only remaining work was verification. Only a session timeout
		// (invoke.IsTimeout) with a viable committed diff salvages; anything else
		// (and a pre-commit timeout) falls through to the normal retry/fail path.
		if err != nil && t.OnTimeout == apiv1.TaskOnTimeoutSalvage && invoke.IsTimeout(err) {
			if salvaged, ok := r.salvageTimeout(ctx, jr, in, t, workspace, attempt, class, err); ok {
				salvaged.Transcript = result.Transcript
				return salvaged, nil, nil, nil
			}
		}
		return result, nil, err, nil
	default:
		return apiv1.ResultEnvelope{}, nil, fmt.Errorf("task %q has unknown type %q", t.Name, t.Type), nil
	}
}

// salvageTimeout implements Task.OnTimeout == salvage (#724): when an agentic
// stage's session hits its wall-clock timeout but has already committed a
// non-empty diff to the run branch, complete the stage with that committed diff
// instead of discarding the attempt. The workflow then advances to the stage's
// Next state (normally the reviewer gate, then the deterministic local-ci stage
// that owns `make ci`) rather than burning the retry budget re-running work
// whose only unfinished step was verification. The committed diff survives the
// per-attempt worktree teardown because a run's stages share one branch, so
// there is no need to persist anything here beyond a provenance marker — the
// reviewer gate recomputes and digests the diff downstream (recordReviewerDiff).
//
// Returns ok=false when there is nothing viable to salvage — no worktree, a
// diff error, or an empty diff (a pre-commit timeout) — in which case the
// caller falls back to the normal timeout path (retry per Task.Retry, then
// fail).
func (r *Runner) salvageTimeout(ctx context.Context, jr *journal.Run, in StartInput, t apiv1.Task, workspace *stageWorkspace, attempt int, class journal.AttemptClass, cause error) (apiv1.ResultEnvelope, bool) {
	if workspace == nil || workspace.worktree == nil {
		return apiv1.ResultEnvelope{}, false
	}
	baseRef := in.RepoRef.Branch
	if baseRef == "" {
		baseRef = "main"
	}
	diff, err := workspace.worktree.Diff(ctx, baseRef)
	if err != nil || len(diff) == 0 {
		return apiv1.ResultEnvelope{}, false
	}
	// Provenance only (the diff bytes are the reviewer gate's to record and
	// digest): a small marker so a salvaged completion is distinguishable in the
	// journal from an ordinary one. Best-effort — a recording failure must not
	// turn a salvageable timeout back into a total loss.
	if marker, mErr := json.Marshal(map[string]interface{}{
		"salvagedOnTimeout": true,
		"diffBytes":         len(diff),
		"cause":             cause.Error(),
	}); mErr == nil {
		_, _ = jr.RecordStageArtifact(t.Name, attempt, class, t.Name+"/salvage-on-timeout.json", marker)
	}
	return apiv1.ResultEnvelope{
		Status:  apiv1.ResultSuccess,
		Summary: "salvaged committed diff after agentic session timeout (#724); local-ci verifies it authoritatively",
		Outputs: map[string]interface{}{"salvagedOnTimeout": true},
	}, true
}

type contextManifest struct {
	ContextPointers []apiv1.ContextPointer `json:"contextPointers"`
}

func recordContextManifest(jr *journal.Run, env apiv1.InvocationEnvelope, stage string, attempt int, class journal.AttemptClass) error {
	pointers := make([]apiv1.ContextPointer, len(env.ContextPointers))
	copy(pointers, env.ContextPointers)
	data, err := json.Marshal(contextManifest{ContextPointers: pointers})
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	name := journal.ContextManifestArtifactName(stage, attempt)
	if _, err := jr.RecordStageArtifact(stage, attempt, class, name, data); err != nil {
		return fmt.Errorf("record artifact: %w", err)
	}
	return nil
}

// mutationsSidecarFile is the well-known, worktree-relative file a
// provider-chain subcommand records its mutation facts to (issue #228);
// mirrors cmd/goobers's own mutationsSidecarFile constant (kept independent
// rather than imported — cmd depends on internal, never the reverse).
const mutationsSidecarFile = "mutations.jsonl"

// mutationFact mirrors one line of cmd/goobers's sidecar record shape
// without importing cmd/goobers (same decoupling convention
// internal/telemetry/rollup/mirror.go uses for the journal's own event
// shape) — just enough to build a journal.ExternalRef plus an operation
// annotation.
type mutationFact struct {
	Provider  string `json:"provider"`
	Kind      string `json:"kind"`
	ID        string `json:"id"`
	URL       string `json:"url,omitempty"`
	Operation string `json:"operation,omitempty"`
}

// readMutationSidecar reads and parses mutationsSidecarFile from workspace,
// if present. Absence is the overwhelmingly common case (most deterministic
// stages run no provider-mutating subcommand at all) and is not an error;
// neither is a symlink/path escape or a malformed line — this is a
// provenance signal, not the stage's deliverable, and the provider mutation
// it describes already happened for real regardless of whether this sidecar
// can be trusted, so failing the stage over it would be disproportionate
// (and a way for a compromised subcommand to sabotage an otherwise-successful
// stage). All failure modes simply yield no facts; ResolveContainedPath
// still applies the #120 path/symlink-escape containment check others use
// for declared-output files, so a malicious sidecar path is never followed.
func readMutationSidecar(workspace string) []mutationFact {
	full, err := apiv1.ResolveContainedPath(workspace, mutationsSidecarFile)
	if err != nil {
		return nil
	}
	data, err := os.ReadFile(full)
	if err != nil {
		return nil
	}
	var facts []mutationFact
	for _, line := range bytes.Split(data, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		var f mutationFact
		if err := json.Unmarshal(line, &f); err != nil {
			continue
		}
		facts = append(facts, f)
	}
	return facts
}

// errorDetailFrom converts a stage's business-level error into the journal's
// normative ErrorDetail. A recognized non-retryable disposition retains the
// result summary as its human-facing reason so a downstream parking stage can
// surface the implementer's explanation without re-running a reviewer.
func errorDetailFrom(result apiv1.ResultEnvelope) *journal.ErrorDetail {
	if result.Error == nil {
		return nil
	}
	message := result.Error.Message
	if result.Status == apiv1.ResultFailure && isNonRetryableEscalation(result.Error) {
		if summary := strings.TrimSpace(result.Summary); summary != "" {
			message = summary
		}
	}
	return &journal.ErrorDetail{Code: result.Error.Code, Message: message}
}

// escalateErrorCodes are the recognized non-retryable business dispositions an
// agentic stage can emit to bypass the Next gate's repass loop (#415). Each
// names a conclusion that re-running the stage can only re-derive — the item
// needs a human or a future decomposition workflow, not another implement
// attempt. Kept as a runner-owned policy set (the runner owns status→transition
// routing), not a schema enum, so recognizing a new code never reopens the
// closed envelope contract.
var escalateErrorCodes = map[string]bool{
	"ISSUE_OVER_SCOPE":    true,
	"NEEDS_DECOMPOSITION": true,
}

// isNonRetryableEscalation reports whether a stage failure is a non-retryable
// business disposition the runner escalates on sight (#415): error.retryable
// is false AND error.code is a recognized escalate code. A failure that is
// retryable, or that carries an absent/unrecognized code, is not escalated
// here — it follows the ordinary failure route (into the Next gate, else
// PhaseFailed). nil in → false (no error detail, nothing to route on).
func isNonRetryableEscalation(e *apiv1.ErrorInfo) bool {
	return e != nil && !e.Retryable && escalateErrorCodes[e.Code]
}

// taskEscalationTarget lets a workflow intercept a non-retryable task
// disposition through the escalation control branch on its Next gate. The gate
// evaluator is deliberately bypassed; absent that control branch, the existing
// direct @escalate behavior remains unchanged.
func taskEscalationTarget(machine *workflow.Machine, task apiv1.Task) string {
	if nextGate, ok := machine.Gate(task.Next); ok {
		if target, ok := workflow.BranchTarget(nextGate, workflow.BranchEscalate); ok {
			return target
		}
	}
	return workflow.TargetEscalate
}

// evaluateGate dispatches one gate attempt through gateEval (internal/gate,
// #20), which owns branch resolution, bounded repass, escalation override, and
// gate.evaluated journaling — this method does none of that itself.
//
// Per the runner-contract convention internal/gate documents (automated.go):
// a gate never receives the subject stage's ResultEnvelope over the wire
// envelope (§2.4) — so before dispatch, the subject's Status and small
// Outputs are flattened into the gate's own env.Inputs. subjectResult itself
// is still passed to gateEval.Evaluate as a plain in-process value (not
// serialized, not journaled as such) purely so an agentic reviewer gate can
// attach its artifacts as evidence ContextPointers (internal/gate/reviewer.go)
// — that is a same-process function argument, not a stage-boundary crossing.
// removeErr mirrors dispatchTask's own contract (issue #136): additive, never
// overriding the gate's own result/err, but never silently discarded either.
//
// An automated gate never gets a worktree (#112): its checks are pure
// functions over env.Inputs alone (internal/gate/automated.go's DefaultChecks
// "keeps the checker registry pure — no journal/filesystem access"), so
// unlike an agentic reviewer gate it reads and writes no workspace at all.
// Provisioning one anyway wasted a git clone/checkout on every automated-gate
// evaluation and turned a worktree-provisioning failure (disk, git) into a
// failure of a gate that touches no filesystem whatsoever.
func (r *Runner) evaluateGate(ctx context.Context, jr *journal.Run, gateEval *gate.Evaluator, ex *executors, in StartInput, g apiv1.Gate, subjectStage string, subjectResult apiv1.ResultEnvelope, upstream []apiv1.ContextPointer, instructionAddendum, workspaceBranch string) (result gate.Result, err error, removeErr error) {
	// Same drain contract as runTask: SIGTERM does not interrupt an active gate,
	// but a stalled-run watchdog request does.
	ctx = stalledAttemptContext(ctx)

	gooberName := ""
	if g.Evaluator == apiv1.EvaluatorAgentic && g.Agentic != nil {
		gooberName = g.Agentic.Goober
	}
	ctx, span := r.startGateSpan(ctx, in, g, gooberName)
	defer span.End()

	if recovered, ok, recoveryErr := gateEval.RecoverInterrupted(g, ""); recoveryErr != nil {
		err = fmt.Errorf("runner: evaluate gate %q: %w", g.Name, recoveryErr)
		span.Fail(err)
		return gate.Result{}, err, nil
	} else if ok {
		span.SetGateResult(recovered.Outcome, recovered.Attempt)
		span.Complete(telemetry.OutcomeSuccess, false)
		return recovered, nil, nil
	}

	// diffDigest (issue #316) is only ever set below for an agentic gate
	// whose branch carries a non-empty diff — an automated/human gate, or an
	// agentic gate with no committed change, passes "" through to Evaluate,
	// which treats that as "no digest to compare" and never short-circuits.
	var diffDigest string
	// emptyDiff (#415) is set below only for an agentic gate whose AGENTIC
	// subject stage committed no change — recordReviewerDiff returns a nil
	// pointer for a zero-length diff. Passed to Evaluate so the reviewer gate
	// fast-fails that empty diff on review-1 instead of looping repasses over
	// it. Scoped to an agentic subject so a deterministic subject that is not
	// expected to commit (e.g. merge-review) still gets a real reviewer pass.
	var emptyDiff bool
	var env apiv1.InvocationEnvelope
	var gateTelemetryDir string
	var agentInvocation *gooberInvocation
	var workspace *stageWorkspace
	if g.Evaluator == apiv1.EvaluatorAutomated {
		env = apiv1.InvocationEnvelope{
			TaskID:          in.RunID + ":" + g.Name,
			WorkflowID:      in.Machine.Def.Name,
			RunID:           in.RunID,
			Gaggle:          in.Gaggle,
			BranchNamespace: r.branchNamespaceFor(in.Gaggle),
			Goal:            "gate: " + g.Name,
			RepoRef:         in.RepoRef,
			Item:            in.Item,
			Limits:          workflow.GateLimits(g),
		}
	} else {
		var wt *worktree.Worktree
		// An agentic gate's reviewer runs a real goober subprocess, so — unlike
		// an automated/human gate — it needs its capability-scoped credentials
		// (agent:model for Copilot model auth, #294). AgenticGate carries no
		// stage-level capabilities, so source them from the reviewer goober's
		// own definition; automated/human gates stay uncredentialed (nil caps).
		var gateCaps []string
		if g.Evaluator == apiv1.EvaluatorAgentic {
			gateCaps = r.cfg.GateGooberCapabilities[gooberName]
		}
		env, workspace, err = r.buildEnvelope(ctx, in, g.Name, "gate: "+g.Name, nil, gateCaps, workflow.GateLimits(g), upstream, apiv1.WorkspaceRepo, false, workspaceBranch)
		if err != nil {
			err = fmt.Errorf("runner: prepare gate %q: %w", g.Name, err)
			span.Fail(err)
			return gate.Result{}, err, nil
		}
		env.InstructionAddendum = instructionAddendum
		if g.Evaluator == apiv1.EvaluatorAgentic {
			gateTelemetryDir = telemetry.ResetStageTelemetryDir(env.Workspace)
		}
		defer func() {
			telemetry.CleanupStageTelemetryDir(gateTelemetryDir)
			if agentInvocation != nil && agentInvocation.materializedAssets() {
				if validationErr := workspace.ValidateReservedPaths(context.WithoutCancel(ctx)); validationErr != nil {
					err = errors.Join(err, fmt.Errorf("gate %q: %w", g.Name, validationErr))
				}
			}
			removeErr = workspace.Remove(ctx)
		}()
		wt = workspace.worktree

		// #301: give an agentic reviewer gate a runner-produced, digested diff
		// of the run branch (git diff base...HEAD) as evidence, so it judges the
		// actual committed change rather than a model-self-reported artifact —
		// the implementer's model cannot correctly report digested ArtifactPointers,
		// and its true deliverable is the committed branch. Attached via the #20
		// evidence-pointer mechanism (env.ContextPointers), resolved into the
		// reviewer's workspace like any other evidence pointer.
		if g.Evaluator == apiv1.EvaluatorAgentic {
			ptr, derr := r.recordReviewerDiff(ctx, ex, in, g.Name, wt)
			if derr != nil {
				err = fmt.Errorf("runner: gate %q: reviewer diff evidence: %w", g.Name, derr)
				span.Fail(err)
				return gate.Result{}, err, nil
			}
			if ptr != nil {
				env.ContextPointers = append(env.ContextPointers, *ptr)
				if ptr.Artifact != nil {
					diffDigest = ptr.Artifact.Digest
				}
			} else if subjectTask, ok := in.Machine.Task(subjectStage); ok && subjectTask.Type == apiv1.TaskAgentic {
				// A nil pointer (no error — that returned early above) means the
				// run branch has a zero-length diff. Fast-fail it (#415) only
				// when the subject stage is AGENTIC: an agent whose deliverable
				// is its committed work produced nothing to review, so a repass
				// can only re-observe the same emptiness. A deterministic
				// subject (e.g. merge-review's gather-sibling-context, whose
				// reviewer judges PRs from its outputs, not a run-branch commit)
				// is never expected to commit — its empty diff is normal, and
				// the reviewer must still run against its actual evidence.
				emptyDiff = true
			}
		}
	}

	// cachedVerdict (issue #523): a deterministic subject stage (merge-
	// review's gather-sibling-context) may have already found a digest-
	// matched prior verdict for this exact evaluation and handed it back as
	// a scalar JSON string output — subjectResult.Outputs is a generic
	// map[string]interface{} no gate-package code should need to know the
	// shape of, so the decode (and the "is this even a merge-review-style
	// cache hit" question) lives entirely here, at the one call site that
	// already owns subjectResult. A decode failure is silently ignored, not
	// fatal: an absent or malformed cachedVerdictJson is exactly the normal
	// "no cache hit" case for every gate that never produces this key at
	// all (every gate but merge-review's review gate). Rebound on every
	// call — possibly to nil — mirroring Reviewer's own rebind contract
	// just below, so a hit for one gate can never leak into the next.
	var cachedVerdict *apiv1.Verdict
	if raw, ok := subjectResult.Outputs["cachedVerdictJson"].(string); instructionAddendum == "" && ok && raw != "" {
		var v apiv1.Verdict
		if jerr := json.Unmarshal([]byte(raw), &v); jerr == nil {
			cachedVerdict = &v
		}
	}
	gateEval.CachedVerdict = cachedVerdict
	if instructionAddendum != "" {
		// An explicit operator rerun must invoke the reviewer it targets, even
		// when ordinary automation would reuse a cached verdict or fast-fail an
		// empty diff without calling the agent.
		emptyDiff = false
	}

	switch g.Evaluator {
	case apiv1.EvaluatorAutomated:
		env.Inputs = make(map[string]interface{}, 1+len(subjectResult.Outputs))
		env.Inputs[gate.InputKeyStatus] = string(subjectResult.Status)
		for k, v := range subjectResult.Outputs {
			env.Inputs[k] = v
		}
	case apiv1.EvaluatorAgentic:
		// A cache hit means Evaluate below will never call the reviewer at
		// all, so there is nothing for a goober executor to do — skip
		// constructing one entirely rather than resolving credentials and
		// wiring a Goober that Evaluate is guaranteed not to invoke.
		if cachedVerdict == nil {
			ag, aerr := ex.agentic(gooberName)
			if aerr != nil {
				err := fmt.Errorf("runner: gate %q: %w", g.Name, aerr)
				span.Fail(err)
				return gate.Result{}, err, nil
			}
			// Rebound per gate evaluated, not shared across gateEval's lifetime:
			// different agentic gates in the same run may target different
			// reviewer goobers. gate.Evaluator reads this field fresh on every
			// Evaluate call, so mutating it here between calls is safe.
			agentInvocation = &gooberInvocation{
				Goober:                 ag,
				activateAssetPathGuard: workspace.ActivateAssetPathGuard,
			}
			gateEval.Reviewer = &gate.ReviewerEvaluator{Goober: gateHeartbeatGoober{
				goober:  agentInvocation,
				runner:  r,
				journal: jr,
				stage:   g.Name,
				attempt: gateEval.Attempts[g.Name] + 1,
			}}
		}
	}

	result, err = gateEval.Evaluate(ctx, g, env, subjectStage, subjectResult, diffDigest, emptyDiff)
	telemetry.IngestStageEmissions(gateTelemetryDir, nil, span)
	if err != nil {
		err = fmt.Errorf("runner: evaluate gate %q: %w", g.Name, err)
		span.Fail(err)
		return gate.Result{}, err, nil
	}
	span.SetGateResult(result.Outcome, result.Attempt)
	span.Complete(telemetry.OutcomeSuccess, false)
	return result, nil, nil
}

// recordReviewerDiff produces an agentic reviewer gate's evidence (#301): the
// unified diff of the run branch against its base, recorded as a digested
// artifact and returned as a context pointer for the reviewer's envelope. The
// diff is computed by the runner from the actual commits — never self-reported
// by the implementer's model — so the reviewer judges the real change with the
// same content-addressed integrity as any other artifact. Returns (nil, nil)
// when the branch carries no change vs. base (nothing to attach).
func (r *Runner) recordReviewerDiff(ctx context.Context, ex *executors, in StartInput, gateName string, wt *worktree.Worktree) (*apiv1.ContextPointer, error) {
	baseRef := in.RepoRef.Branch
	if baseRef == "" {
		baseRef = "main"
	}
	diff, err := wt.Diff(ctx, baseRef)
	if err != nil {
		return nil, err
	}
	if len(diff) == 0 {
		return nil, nil
	}
	// Defense-in-depth: scrub any registered secret a stage's commit might have
	// captured before the diff lands in the journal, mirroring the harness's own
	// artifact scrubbing (internal/harness.Executor.liftArtifactFile). The run's
	// SecretRegistrar is the RegistryScrubber that also implements journal.Scrubber.
	if s, ok := ex.reg.(journal.Scrubber); ok {
		diff = s.Scrub(diff)
	}
	ref, err := ex.rec.RecordArtifact(in.RunID+":"+gateName+"/reviewer-diff.patch", diff)
	if err != nil {
		return nil, fmt.Errorf("record reviewer diff artifact: %w", err)
	}
	ptr := apiv1.ArtifactPointer{Path: ref.Path, Digest: ref.Digest, Size: ref.Size, MediaType: "text/x-diff"}
	return &apiv1.ContextPointer{Name: gateName + ".diff", Artifact: &ptr}, nil
}

// startGateSpan opens a gate span under the run's trace, if telemetry is
// configured. A zero telemetry.Span is safe to use (its methods no-op).
func (r *Runner) startGateSpan(ctx context.Context, in StartInput, g apiv1.Gate, gooberName string) (context.Context, telemetry.Span) {
	if r.cfg.Telemetry == nil {
		return ctx, telemetry.Span{}
	}
	attrs := telemetry.GateAttributes{
		Gaggle:          in.Gaggle,
		WorkflowID:      in.Machine.Def.Name,
		WorkflowVersion: strconv.Itoa(in.Machine.Def.Version),
		WorkflowDigest:  in.Machine.Digest(),
		RunID:           in.RunID,
		GateID:          g.Name,
		GooberID:        gooberName,
		Agentic:         g.Evaluator == apiv1.EvaluatorAgentic,
	}
	if attrs.Agentic {
		provenance := r.cfg.AgentProvenance[gooberName]
		attrs.Model = provenance.Model
		attrs.HarnessVersion = provenance.HarnessVersion
	}
	if in.Item != nil {
		attrs.ItemID = in.Item.ID
		attrs.ItemURL = in.Item.URL
	}
	ctx, span, err := r.cfg.Telemetry.StartGate(ctx, attrs)
	if err != nil {
		return ctx, telemetry.Span{}
	}
	return ctx, span
}

type stageWorkspace struct {
	path     string
	worktree *worktree.Worktree
}

func (w *stageWorkspace) ActivateAssetPathGuard() error {
	if w.worktree == nil {
		return nil
	}
	return w.worktree.ActivateAssetPathGuard()
}

func (w *stageWorkspace) ValidateReservedPaths(ctx context.Context) error {
	if w.worktree == nil {
		return nil
	}
	return w.worktree.ValidateReservedPaths(ctx)
}

func (w *stageWorkspace) Remove(ctx context.Context) error {
	if w.worktree != nil {
		return w.worktree.Remove(ctx, worktree.RemoveOptions{})
	}
	return os.RemoveAll(w.path)
}

// buildEnvelope provisions an isolated repository worktree or empty scratch
// directory and builds one stage attempt's invocation envelope.
func (r *Runner) buildEnvelope(ctx context.Context, in StartInput, stageName, goal string, taskInputs map[string]string, capabilities []string, limits apiv1.Limits, upstream []apiv1.ContextPointer, workspaceMode apiv1.WorkspaceMode, syncBase bool, workspaceBranch string) (apiv1.InvocationEnvelope, *stageWorkspace, error) {
	workspace, err := r.createStageWorkspace(ctx, in, stageName, workspaceMode, syncBase, workspaceBranch)
	if err != nil {
		return apiv1.InvocationEnvelope{}, nil, err
	}

	inputs := make(map[string]interface{}, len(taskInputs))
	for k, v := range taskInputs {
		inputs[k] = v
	}
	env := apiv1.InvocationEnvelope{
		TaskID:          in.RunID + ":" + stageName,
		WorkflowID:      in.Machine.Def.Name,
		RunID:           in.RunID,
		Gaggle:          in.Gaggle,
		BranchNamespace: r.branchNamespaceFor(in.Gaggle),
		Goal:            goal,
		Workspace:       workspace.path,
		RepoRef:         in.RepoRef,
		Item:            in.Item,
		ContextPointers: upstream,
		Capabilities:    capabilities,
		Limits:          limits,
		Inputs:          inputs,
	}
	return env, workspace, nil
}

// createStageWorkspace provisions this stage attempt's workspace. workspaceBranch
// is the run-scoped branch rebinding (WorkspaceBranchOutput, #392): empty — the
// normal case — means the run's own branch, providers.BranchName.
func (r *Runner) createStageWorkspace(ctx context.Context, in StartInput, stageName string, mode apiv1.WorkspaceMode, syncBase bool, workspaceBranch string) (*stageWorkspace, error) {
	switch mode {
	case apiv1.WorkspaceScratch:
		if syncBase {
			return nil, fmt.Errorf("create scratch workspace: syncBase requires a repo workspace")
		}
		if r.cfg.ScratchDir == "" {
			return nil, fmt.Errorf("create scratch workspace: runner ScratchDir is required")
		}
		if err := os.MkdirAll(r.cfg.ScratchDir, 0o700); err != nil {
			return nil, fmt.Errorf("create scratch workspace root: %w", err)
		}
		path, err := os.MkdirTemp(r.cfg.ScratchDir, scratchWorkspacePrefix+"*")
		if err != nil {
			return nil, fmt.Errorf("create scratch workspace: %w", err)
		}
		return &stageWorkspace{path: path}, nil
	case "", apiv1.WorkspaceRepo:
		repoURL, err := r.cfg.RepoCloneURL(in.RepoRef)
		if err != nil {
			return nil, err
		}
		baseRef := in.RepoRef.Branch
		if baseRef == "" {
			baseRef = "main"
		}
		branch := providers.BranchNameIn(r.branchNamespaceFor(in.Gaggle), in.Machine.Def.Name, in.RunID)
		if workspaceBranch != "" {
			branch = workspaceBranch
		}
		wt, err := r.cfg.Worktrees.Create(ctx, worktree.CreateOptions{
			RepoURL:    repoURL,
			RunID:      in.RunID + "-" + stageName,
			OwnerRunID: in.RunID,
			BaseRef:    baseRef,
			Branch:     branch,
			SyncBase:   syncBase,
			// A rebound branch names work that already exists; creating it
			// from base instead would hand the stage a pristine checkout
			// wearing the PR's branch name. Fail loudly instead.
			RequireExistingBranch: workspaceBranch != "",
		})
		if err != nil {
			return nil, fmt.Errorf("create worktree: %w", err)
		}
		return &stageWorkspace{path: wt.Path, worktree: wt}, nil
	default:
		return nil, fmt.Errorf("unknown workspace mode %q", mode)
	}
}

// worktreeWarningEvent builds the runner.annotation journal event that surfaces
// a provisioned worktree's non-fatal warnings (#643 symlink flattening today),
// returning ok=false when there is nothing to report. The payload lives under
// Runner, which is excluded from conformance, so recording it never perturbs a
// run's normative event stream — it is purely an operator-visible note. Split
// out from dispatchTask so the event shape is unit-testable without a live
// worktree provision.
func worktreeWarningEvent(stage string, wt *worktree.Worktree) (journal.Event, bool) {
	if wt == nil || len(wt.Warnings) == 0 {
		return journal.Event{}, false
	}
	return journal.Event{
		Type:   journal.EventRunnerAnnotation,
		Stage:  stage,
		Runner: map[string]any{"kind": "worktree.warnings", "warnings": wt.Warnings},
	}, true
}

func machineUsesRepo(machine *workflow.Machine) bool {
	for _, task := range machine.Def.Spec.Tasks {
		if task.Type == apiv1.TaskAgentic || task.Run == nil || task.Run.Workspace != apiv1.WorkspaceScratch {
			return true
		}
	}
	for _, g := range machine.Def.Spec.Gates {
		if g.Evaluator != apiv1.EvaluatorAutomated {
			return true
		}
	}
	return false
}

// contextPointersFor turns stageName's already-committed artifacts into the
// ContextPointers downstream stages receive, named "<stageName>.artifact[i]"
// (matching internal/gate/reviewer.go's evidencePointers naming). V0 has no
// explicit data-flow declaration in the DSL yet, so every downstream stage
// sees every artifact produced so far in the run — pointer only, never the
// producing stage's full result (§2.4) — and picks out what it needs by
// index/media type.
func contextPointersFor(stageName string, artifacts []apiv1.ArtifactPointer) []apiv1.ContextPointer {
	out := make([]apiv1.ContextPointer, 0, len(artifacts))
	for i := range artifacts {
		a := artifacts[i]
		out = append(out, apiv1.ContextPointer{Name: fmt.Sprintf("%s.artifact[%d]", stageName, i), Artifact: &a})
	}
	return out
}

// refsFrom converts a ResultEnvelope's wire ArtifactPointers into their
// journal.Ref production form (journal.Ref's doc comment: same fields, 1:1),
// for journaling on stage.finished (#107/#108's subject/pointer
// reconstruction).
func refsFrom(artifacts []apiv1.ArtifactPointer) []journal.Ref {
	if len(artifacts) == 0 {
		return nil
	}
	out := make([]journal.Ref, len(artifacts))
	for i, a := range artifacts {
		out[i] = journal.Ref{Path: a.Path, Digest: a.Digest, Size: a.Size, MediaType: a.MediaType}
	}
	return out
}

// artifactPointersFrom is refsFrom's inverse — converting a journaled
// stage.finished event's Artifacts back into wire ArtifactPointers, e.g. to
// rebuild a resumed run's ContextPointers (contextPointersFor) or a resumed
// gate's subject ResultEnvelope from the journal.
func artifactPointersFrom(refs []journal.Ref) []apiv1.ArtifactPointer {
	if len(refs) == 0 {
		return nil
	}
	out := make([]apiv1.ArtifactPointer, len(refs))
	for i, ref := range refs {
		out[i] = apiv1.ArtifactPointer{Path: ref.Path, Digest: ref.Digest, Size: ref.Size, MediaType: ref.MediaType}
	}
	return out
}

// defaultRepoCloneURL derives the git remote URL worktree.Manager clones from
// a RepoRef.
func defaultRepoCloneURL(ref apiv1.RepoRef) (string, error) {
	switch ref.Provider {
	case apiv1.ProviderGitHub:
		return fmt.Sprintf("https://github.com/%s/%s.git", ref.Owner, ref.Name), nil
	case apiv1.ProviderADO:
		organization, project, _ := strings.Cut(ref.Owner, "/")
		return fmt.Sprintf("https://dev.azure.com/%s/%s/_git/%s",
			url.PathEscape(organization), url.PathEscape(project), url.PathEscape(ref.Name)), nil
	default:
		return "", fmt.Errorf("runner: unsupported repo provider %q", ref.Provider)
	}
}
