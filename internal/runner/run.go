package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/gate"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/telemetry"
	"github.com/goobers/goobers/internal/workflow"
	"github.com/goobers/goobers/internal/worktree"
	"github.com/goobers/goobers/providers"
)

// DefaultMaxSteps bounds the state walk against a runaway machine (carried over
// from the Temporal engine core, ARCHITECTURE §3.1).
const DefaultMaxSteps = 10000

// DefaultMaxInfrastructureAttempts bounds transient infrastructure failures
// independently of a task's policy retry allowance.
const DefaultMaxInfrastructureAttempts int32 = 2

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

// BlockedOutcome describes a run terminating because a stage reported status
// "blocked" (#544/#545) — the value Config.Blocked receives.
type BlockedOutcome struct {
	RunID string
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
// comment on / park the driving issue. Must tolerate an empty ItemID.
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
	// Escalation notifies the driving backlog item's provider when a run
	// escalates (internal/gate.EscalationNotifier). Optional — nil is a no-op.
	Escalation *gate.EscalationNotifier
	// Blocked handles the instance-level consequences of a stage reporting
	// status "blocked" (#544/#545): recording the learned dependency block for
	// selection to skip (#552) and surfacing/parking the driving issue. Called
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
}

// Runner advances a compiled workflow.Machine stage-by-stage, durably
// recording every transition to the run journal, and dispatching tasks
// through the pre-existing internal/invoke seam. It is the substrate-neutral
// local runner core (ARCHITECTURE.md §3.1).
type Runner struct {
	cfg      Config
	maxSteps int
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
	return &Runner{cfg: cfg, maxSteps: maxSteps}, nil
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

	ctx, span := r.startRunSpan(ctx, in)
	defer span.End()

	// A scratch-only workflow has no repository branch. Repo-backed workflows
	// retain the run branch as their primary external ref (#133).
	if machineUsesRepo(in.Machine) {
		if err := jr.Append(journal.Event{
			Type: journal.EventRefTouched,
			ExternalRef: &journal.ExternalRef{
				Provider: string(in.RepoRef.Provider),
				Kind:     "branch",
				ID:       providers.BranchName(in.Machine.Def.Name, in.RunID),
			},
		}); err != nil {
			span.Fail(err)
			return Result{}, fmt.Errorf("runner: journal run branch for %q: %w", in.RunID, err)
		}
	}

	result, err := r.walk(ctx, jr, in, in.Machine.Def.Spec.Start, nil, nil, nil, registrar, walkSeed{})
	if err != nil {
		span.Fail(err)
		return result, err
	}
	// #710: a business-failed run (walk returns a nil error — a Go-level
	// dispatch error is Fail'd above instead) used to call span.Succeed here
	// unconditionally, reporting codes.Ok with the literal message "failed".
	// Anything reading spans (`goobers trace`, rollup span queries) then
	// called a died run "ok" — the exact gap that made #705 a 16-hour mystery
	// despite the real cause sitting one journal line away the whole time.
	span.Complete(string(result.Phase), result.Phase == journal.PhaseFailed)
	return result, nil
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
		RunID:           in.RunID,
		Trigger:         string(in.Trigger.Kind),
	}
	if in.Item != nil {
		attrs.ItemID = in.Item.ID
		attrs.ItemProvider = string(in.Item.Provider)
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

// resumeContext carries the one piece of resume-specific state walk needs: if
// the checkpointed resume point is a task that was still in flight when the
// runner was interrupted (crash or unclean shutdown), the interrupted attempt
// number to journal as a terminal, infra-tagged failure before continuing —
// see resume.go's interruptedAttempt. nil for a fresh Start; consumed (set to
// nil) after its one use, so it never applies to a later dispatch of a
// different stage.
type resumeContext struct {
	stage   string
	attempt int
}

const interruptedAttemptErrorCode = "interrupted"

// walkSeed carries the walk-local state a resumed run must NOT start empty —
// Start's fresh walk always begins with the zero value. pointers is the
// upstream ContextPointers every already-finished stage produced (#107);
// lastStage/lastResult is the subject a resumed gate evaluates against
// (#108) — both reconstructed from the journal by Resume (see
// lastFinishedSubject, reconstructPointers in resume.go), since walk's own
// in-memory accumulation of them is exactly what a crash wipes.
type walkSeed struct {
	pointers   []apiv1.ContextPointer
	lastStage  string
	lastResult apiv1.ResultEnvelope
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
func (r *Runner) walk(ctx context.Context, jr *journal.Run, in StartInput, startState string, resume *resumeContext, gateAttempts map[string]int, gateDiffDigests map[string]string, reg SecretRegistrar, seed walkSeed) (Result, error) {
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
	steps := 0

	for {
		steps++
		if steps > r.maxSteps {
			return r.failTerminal(in.RunID, jr, state, steps, fmt.Errorf("runner: run %q exceeded max steps (%d): possible loop", in.RunID, r.maxSteps))
		}
		jr.SetMachineState(state)

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
			if resume != nil && resume.stage == t.Name {
				// The attempt in flight when the runner was interrupted is
				// terminal now — journal it as a failed, infra-tagged attempt
				// (never silently re-run, §17) before dispatching the next
				// one, which continues the SAME attempt count (so a crash
				// cannot grant a task more attempts than its own policy
				// allows).
				if err := jr.Append(journal.Event{
					Type: journal.EventStageFinished, Stage: t.Name, Attempt: resume.attempt, AttemptClass: journal.AttemptInfra,
					Status: string(apiv1.ResultFailure),
					Error:  &journal.ErrorDetail{Code: interruptedAttemptErrorCode, Message: "attempt was in flight when the runner was interrupted"},
				}); err != nil {
					return Result{}, fmt.Errorf("runner: journal interrupted attempt for %q: %w", t.Name, err)
				}
				startAttempt = int32(resume.attempt) + 1
				resume = nil
			}
			result, produced, err := r.runTask(ctx, jr, in, ex, t, pointers, lastResult, startAttempt)
			if err != nil {
				return r.failTerminal(in.RunID, jr, t.Name, steps, err)
			}
			pointers = append(pointers, produced...)
			lastStage, lastResult = t.Name, result

			next, res, advance, oerr := r.taskOutcome(ctx, in.RunID, jr, in.Machine, in.Item, t, result, steps)
			if oerr != nil {
				return res, oerr
			}
			if !advance {
				return res, nil
			}
			state = next
			continue
		}

		if g, ok := in.Machine.Gate(state); ok {
			if g.Evaluator == apiv1.EvaluatorHuman {
				// A human gate executes nothing (§5): pause here. No stage
				// attempt runs and no event is appended, so checkpoint
				// explicitly — Append's implicit checkpoint never fires on
				// this path, and state.json must still point resume at this
				// gate (resume on the operator's decision).
				if err := jr.Checkpoint(); err != nil {
					return Result{}, fmt.Errorf("runner: checkpoint pause at gate %q: %w", g.Name, err)
				}
				return Result{Phase: journal.PhaseRunning, FinalState: g.Name, Steps: steps}, nil
			}

			gr, err, removeErr := r.evaluateGate(ctx, gateEval, ex, in, g, lastStage, lastResult, pointers)
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
					return r.failTerminal(in.RunID, jr, g.Name, steps, fmt.Errorf("runner: journal worktree removal error for gate %q: %w", g.Name, aerr))
				}
			}
			if err != nil {
				return r.failTerminal(in.RunID, jr, g.Name, steps, err)
			}
			if reason, ok := terminalGateNotificationReason(gr); ok {
				if err := r.notifyTerminalGate(context.WithoutCancel(ctx), jr, in.Item, gr, reason); err != nil {
					return r.failTerminal(in.RunID, jr, g.Name, steps, err)
				}
			}
			switch gr.Target {
			case workflow.TargetAbort:
				return r.finish(in.RunID, jr, journal.PhaseAborted, g.Name, steps)
			case workflow.TargetEscalate:
				return r.finish(in.RunID, jr, journal.PhaseEscalated, g.Name, steps)
			case workflow.TerminalComplete:
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

		return r.failTerminal(in.RunID, jr, state, steps, fmt.Errorf("runner: unknown state %q", state))
	}
}

func terminalGateNotificationReason(gr gate.Result) (string, bool) {
	if gr.Target != workflow.TargetAbort && gr.Target != workflow.TargetEscalate {
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

func (r *Runner) notifyTerminalGate(ctx context.Context, jr *journal.Run, item *apiv1.BacklogItem, gr gate.Result, reason string) error {
	if r.cfg.Escalation == nil || item == nil {
		return nil
	}
	if err := r.cfg.Escalation.NotifyEscalated(ctx, item.ID, gr, reason); err != nil {
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
	return nil
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
func (r *Runner) failTerminal(runID string, jr *journal.Run, finalState string, steps int, origErr error) (Result, error) {
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
	return res, origErr
}

// taskOutcome applies the #110 stage-status ruling to a finished task's
// result: success advances to Next; failure advances only if Next is a gate
// (which branches on it), otherwise ends the run PhaseFailed; blocked ends
// the run PhaseEscalated (#544). advance=true means continue the walk at
// next; advance=false means the walk is over — return res (already appended
// its own terminal event, if any).
//
// Factored out of walk's live dispatch path so Resume (resume.go) can apply
// the IDENTICAL transition when it finds the checkpointed task's last
// attempt already finished, not interrupted — the walk must not re-dispatch
// it (#107), just pick up the same decision a live walk would have made
// right after runTask returned. ctx/item feed only the blocked arm's
// instance-level handler (Config.Blocked); the transition decision itself
// stays pure.
func (r *Runner) taskOutcome(ctx context.Context, runID string, jr *journal.Run, machine *workflow.Machine, item *apiv1.BacklogItem, t apiv1.Task, result apiv1.ResultEnvelope, steps int) (next string, res Result, advance bool, err error) {
	switch result.Status {
	case apiv1.ResultBlocked:
		// #544 ruling: blocked is a schema-valid producer value, so it maps to
		// a canonical terminal phase — escalated, not failed (never punish the
		// producer for using the documented contract), and never the prior
		// immortal PhaseRunning pause (claim held to lease expiry, re-resumed
		// on every restart, run.finished never journaled — #545's 6 live
		// occurrences). The cause is journaled first (#305: finish() alone
		// records only the bare phase), then the instance-level handler runs
		// (recording the learned block for #552's selection skip / parking the
		// issue) while the claim ledger still holds this run's claims, then
		// the ordinary terminal path releases everything via FinalizeTerminal.
		o := BlockedOutcome{
			RunID:    runID,
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
			res, err = r.failTerminal(runID, jr, t.Name, steps, fmt.Errorf("runner: journal blocked cause for %q: %w", t.Name, aerr))
			return "", res, false, err
		}
		// Same drain contract as notifyTerminalGate's call site: a SIGTERM
		// already in progress must not skip the block recording/parking.
		if nerr := r.notifyBlocked(context.WithoutCancel(ctx), jr, o); nerr != nil {
			res, err = r.failTerminal(runID, jr, t.Name, steps, nerr)
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
				if nerr := r.notifyRateLimited(context.WithoutCancel(ctx), jr, o); nerr != nil {
					res, err = r.failTerminal(runID, jr, t.Name, steps, nerr)
					return "", res, false, err
				}
			}
		}
		// #415: a stage that self-identifies a non-retryable business
		// disposition — status:failure with error.retryable==false and a
		// recognized escalate code (ISSUE_OVER_SCOPE / NEEDS_DECOMPOSITION) —
		// routes straight to @escalate after this one attempt, bypassing the
		// Next gate and its repass loop. Without it, an un-scopeable issue the
		// implementer correctly rejected on attempt 1 re-enters the
		// reviewer→implement repass loop and re-derives the identical
		// conclusion until the repass budget exhausts, terminating `aborted`
		// instead of the `escalated` a human / a future decomposition workflow
		// selects on. Placed ahead of the gate-routing branch so the
		// disposition wins over the ordinary "failure into the gate" path.
		if isNonRetryableEscalation(result.Error) {
			res, err = r.finish(runID, jr, journal.PhaseEscalated, t.Name, steps)
			return "", res, false, err
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
		code, message := failureCauseFrom(result.Error)
		journaledMessage := message
		if code != "" {
			// Code-prefixed for the on-disk cause event only, matching #545's
			// blockedReason convention (a code alongside the code-named
			// EventError.Code field would otherwise be lost to a plain grep
			// of run_failed messages) — Result.FailureCode already carries
			// the code on its own for the echo sites, so FailureMessage below
			// stays bare rather than doubling up.
			journaledMessage = code + ": " + message
		}
		if aerr := jr.Append(journal.Event{
			Type: journal.EventError, Stage: t.Name,
			Error: &journal.ErrorDetail{Code: "run_failed", Message: journaledMessage},
		}); aerr != nil {
			res, err = r.failTerminal(runID, jr, t.Name, steps, fmt.Errorf("runner: journal failure cause for %q: %w", t.Name, aerr))
			return "", res, false, err
		}
		res, err = r.finish(runID, jr, journal.PhaseFailed, t.Name, steps)
		res.FailureStage, res.FailureCode, res.FailureMessage = t.Name, code, boundFailureMessage(message)
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

// finish prepares terminal cleanup, appends run.finished, then performs the
// configured post-terminal finalization.
func (r *Runner) finish(runID string, jr *journal.Run, phase journal.RunPhase, finalState string, steps int) (Result, error) {
	if err := r.prepareTerminal(runID, phase, jr); err != nil {
		return Result{}, err
	}
	if err := jr.Append(journal.Event{Type: journal.EventRunFinished, Status: string(phase)}); err != nil {
		return Result{}, fmt.Errorf("runner: journal run.finished: %w", err)
	}
	res := Result{Phase: phase, FinalState: finalState, Steps: steps}
	if err := r.FinalizeTerminal(runID, phase); err != nil {
		return res, err
	}
	return res, nil
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
// Dispatch runs on a context.WithoutCancel of ctx: a run-level cancellation
// (SIGTERM) must let the CURRENT attempt — including any retries already in
// flight for this same stage — finish and journal cleanly; walk only checks
// ctx between stages, never mid-dispatch.
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
func (r *Runner) runTask(ctx context.Context, jr *journal.Run, in StartInput, ex *executors, t apiv1.Task, upstream []apiv1.ContextPointer, upstreamResult apiv1.ResultEnvelope, startAttempt int32) (apiv1.ResultEnvelope, []apiv1.ContextPointer, error) {
	ctx, span := r.startTaskSpan(ctx, in, t)
	defer span.End()

	attemptCtx := context.WithoutCancel(ctx)

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
	if startAttempt > policyMaxAttempts {
		err := fmt.Errorf("runner: task %q has no attempts left after resume (interrupted attempt already exhausted its %d-attempt budget)", t.Name, policyMaxAttempts)
		span.Fail(err)
		return apiv1.ResultEnvelope{}, nil, err
	}

	var lastErr error
	nextRetryClass := journal.AttemptPolicy
	policyAttempts := startAttempt - 1
	var infrastructureFailures int32
	for attempt := startAttempt; attempt <= maxAttempts; attempt++ {
		// The initial attempt carries no class and is always conformance-
		// normative. A retry within THIS dispatch uses the class selected by
		// the prior failure: "infra" for an InfrastructureFailure marker,
		// otherwise "policy". When this call resumes after an interrupted
		// attempt (startAttempt > 1, resume.go), its FIRST iteration is also
		// tagged "infra" — the crash drove it, not Task.Retry, so it must be
		// excluded from the conformance set (§3.3) exactly like the
		// interrupted attempt's own infra-tagged stage.finished marker
		// (walk's resumeContext handling) — otherwise a crashed-and-resumed
		// run's normative event set gains an extra started/finished pair a
		// crash-free run of the identical workflow never produces (#111).
		var class journal.AttemptClass
		switch {
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
		if err := jr.Append(journal.Event{Type: journal.EventStageStarted, Stage: t.Name, Attempt: int(attempt), AttemptClass: class}); err != nil {
			err = fmt.Errorf("runner: journal stage.started for %q: %w", t.Name, err)
			span.Fail(err)
			return apiv1.ResultEnvelope{}, nil, err
		}

		result, mutations, dispatchErr, removeErr := r.dispatchTask(attemptCtx, jr, in, ex, t, upstream, upstreamResult, int(attempt), class)
		for _, m := range mutations {
			// Best-effort, like ClaimLedger's own journal() (issue #228): a
			// provider mutation already happened for real regardless of
			// whether this projection succeeds, so a failed Append here must
			// not fail the stage or mask the mutation's own outcome.
			_ = jr.Append(journal.Event{
				Type: journal.EventRefTouched, Stage: t.Name, Attempt: int(attempt), AttemptClass: class,
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
			if aerr := jr.Append(journal.Event{
				Type: journal.EventError, Stage: t.Name, Attempt: int(attempt), AttemptClass: class,
				Error: &journal.ErrorDetail{Code: "worktree_remove_failed", Message: removeErr.Error()},
			}); aerr != nil {
				return apiv1.ResultEnvelope{}, nil, fmt.Errorf("runner: journal worktree removal error for %q: %w", t.Name, aerr)
			}
		}
		if dispatchErr != nil {
			lastErr = dispatchErr
			retryLimit := policyMaxAttempts
			retryCount := policyAttempts
			shouldRetry := policyAttempts < policyMaxAttempts
			nextRetryClass = journal.AttemptPolicy
			if invoke.IsInfrastructureFailure(dispatchErr) {
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
				Error: &journal.ErrorDetail{Code: "executor_error", Message: dispatchErr.Error()},
			}); aerr != nil {
				return apiv1.ResultEnvelope{}, nil, fmt.Errorf("runner: journal executor error for %q: %w", t.Name, aerr)
			}
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
					}
				}
				continue
			}
			err := fmt.Errorf("runner: execute stage %q: %w (attempt %d/%d)", t.Name, lastErr, retryCount, retryLimit)
			span.Fail(err)
			return apiv1.ResultEnvelope{}, nil, err
		}

		if err := jr.Append(journal.Event{
			Type: journal.EventStageFinished, Stage: t.Name, Attempt: int(attempt), AttemptClass: class,
			Status: string(result.Status), Error: errorDetailFrom(result.Error),
			Outputs: result.Outputs, Artifacts: refsFrom(result.Artifacts),
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
		span.Complete(string(result.Status), result.Status == apiv1.ResultFailure)
		return result, contextPointersFor(t.Name, result.Artifacts), nil
	}
	// Unreachable: maxAttempts >= 1 always executes the loop body at least
	// once, and every path inside either returns or continues.
	err := fmt.Errorf("runner: execute stage %q: exhausted attempts: %w", t.Name, lastErr)
	span.Fail(err)
	return apiv1.ResultEnvelope{}, nil, err
}

// startTaskSpan opens a task span under the run's trace, if telemetry is
// configured. A zero telemetry.Span is safe to use (its methods no-op).
func (r *Runner) startTaskSpan(ctx context.Context, in StartInput, t apiv1.Task) (context.Context, telemetry.Span) {
	if r.cfg.Telemetry == nil {
		return ctx, telemetry.Span{}
	}
	attrs := telemetry.TaskAttributes{
		Gaggle:          in.Gaggle,
		WorkflowID:      in.Machine.Def.Name,
		WorkflowVersion: strconv.Itoa(in.Machine.Def.Version),
		RunID:           in.RunID,
		TaskID:          t.Name,
		TaskType:        string(t.Type),
		GooberID:        t.Goober,
	}
	if in.Item != nil {
		attrs.ItemID = in.Item.ID
		attrs.ItemProvider = string(in.Item.Provider)
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
func (r *Runner) dispatchTask(ctx context.Context, jr *journal.Run, in StartInput, ex *executors, t apiv1.Task, upstream []apiv1.ContextPointer, upstreamResult apiv1.ResultEnvelope, attempt int, class journal.AttemptClass) (result apiv1.ResultEnvelope, mutations []mutationFact, err error, removeErr error) {
	workspaceMode := apiv1.WorkspaceRepo
	if t.Run != nil && t.Run.Workspace != "" {
		workspaceMode = t.Run.Workspace
	}
	env, workspace, err := r.buildEnvelope(ctx, in, t.Name, t.Goal, t.Inputs, t.Capabilities, upstream, workspaceMode)
	if err != nil {
		prepErr := fmt.Errorf("prepare stage %q: %w", t.Name, err)
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
	defer func() { removeErr = workspace.Remove(ctx) }()

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
		if err := recordContextManifest(jr, env, t.Name, attempt, class); err != nil {
			return apiv1.ResultEnvelope{}, nil, fmt.Errorf("task %q: record context manifest: %w", t.Name, err), nil
		}
		result, err = ag.Invoke(ctx, env)
		// #724: a stage that opts into OnTimeout=salvage completes with its
		// already-committed diff instead of discarding a timed-out attempt whose
		// only remaining work was verification. Only a session timeout
		// (invoke.IsTimeout) with a viable committed diff salvages; anything else
		// (and a pre-commit timeout) falls through to the normal retry/fail path.
		if err != nil && t.OnTimeout == apiv1.TaskOnTimeoutSalvage && invoke.IsTimeout(err) {
			if salvaged, ok := r.salvageTimeout(ctx, jr, in, t, workspace, attempt, class, err); ok {
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

// errorDetailFrom converts a stage's business-level error (apiv1.ErrorInfo,
// part of its ResultEnvelope) into the journal's normative ErrorDetail
// (Code+Message) so a failed/blocked stage's reason is actually visible in
// the journal, not just its coarse Status string. nil in, nil out.
func errorDetailFrom(e *apiv1.ErrorInfo) *journal.ErrorDetail {
	if e == nil {
		return nil
	}
	return &journal.ErrorDetail{Code: e.Code, Message: e.Message}
}

// escalateErrorCodes are the recognized non-retryable business dispositions an
// agentic stage can emit to route its run straight to @escalate (#415) rather
// than into the Next gate's repass loop. Each names a conclusion that
// re-running the stage can only re-derive — the item needs a human or a future
// decomposition workflow, not another implement attempt. Kept as a
// runner-owned policy set (the runner owns status→transition routing), not a
// schema enum, so recognizing a new code never reopens the closed envelope
// contract.
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
func (r *Runner) evaluateGate(ctx context.Context, gateEval *gate.Evaluator, ex *executors, in StartInput, g apiv1.Gate, subjectStage string, subjectResult apiv1.ResultEnvelope, upstream []apiv1.ContextPointer) (result gate.Result, err error, removeErr error) {
	// Same drain contract as runTask: a gate attempt already underway when
	// SIGTERM cancels the run-level ctx finishes and journals cleanly.
	ctx = context.WithoutCancel(ctx)

	gooberName := ""
	if g.Evaluator == apiv1.EvaluatorAgentic && g.Agentic != nil {
		gooberName = g.Agentic.Goober
	}
	ctx, span := r.startGateSpan(ctx, in, g, gooberName)
	defer span.End()

	if recovered, ok, recoveryErr := gateEval.RecoverInterrupted(g.Name, ""); recoveryErr != nil {
		err = fmt.Errorf("runner: evaluate gate %q: %w", g.Name, recoveryErr)
		span.Fail(err)
		return gate.Result{}, err, nil
	} else if ok {
		span.Succeed(recovered.Outcome)
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
	if g.Evaluator == apiv1.EvaluatorAutomated {
		env = apiv1.InvocationEnvelope{
			TaskID:     in.RunID + ":" + g.Name,
			WorkflowID: in.Machine.Def.Name,
			RunID:      in.RunID,
			Gaggle:     in.Gaggle,
			Goal:       "gate: " + g.Name,
			RepoRef:    in.RepoRef,
			Item:       in.Item,
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
		var workspace *stageWorkspace
		env, workspace, err = r.buildEnvelope(ctx, in, g.Name, "gate: "+g.Name, nil, gateCaps, upstream, apiv1.WorkspaceRepo)
		if err != nil {
			err = fmt.Errorf("runner: prepare gate %q: %w", g.Name, err)
			span.Fail(err)
			return gate.Result{}, err, nil
		}
		defer func() { removeErr = workspace.Remove(ctx) }()
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
	if raw, ok := subjectResult.Outputs["cachedVerdictJson"].(string); ok && raw != "" {
		var v apiv1.Verdict
		if jerr := json.Unmarshal([]byte(raw), &v); jerr == nil {
			cachedVerdict = &v
		}
	}
	gateEval.CachedVerdict = cachedVerdict

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
			gateEval.Reviewer = &gate.ReviewerEvaluator{Goober: ag}
		}
	}

	result, err = gateEval.Evaluate(ctx, g, env, subjectStage, subjectResult, diffDigest, emptyDiff)
	if err != nil {
		err = fmt.Errorf("runner: evaluate gate %q: %w", g.Name, err)
		span.Fail(err)
		return gate.Result{}, err, nil
	}
	span.Succeed(result.Outcome)
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
		RunID:           in.RunID,
		GateID:          g.Name,
		Evaluator:       string(g.Evaluator),
		GooberID:        gooberName,
	}
	if in.Item != nil {
		attrs.ItemID = in.Item.ID
		attrs.ItemProvider = string(in.Item.Provider)
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

func (w *stageWorkspace) Remove(ctx context.Context) error {
	if w.worktree != nil {
		return w.worktree.Remove(ctx, worktree.RemoveOptions{})
	}
	return os.RemoveAll(w.path)
}

// buildEnvelope provisions an isolated repository worktree or empty scratch
// directory and builds one stage attempt's invocation envelope.
func (r *Runner) buildEnvelope(ctx context.Context, in StartInput, stageName, goal string, taskInputs map[string]string, capabilities []string, upstream []apiv1.ContextPointer, workspaceMode apiv1.WorkspaceMode) (apiv1.InvocationEnvelope, *stageWorkspace, error) {
	workspace, err := r.createStageWorkspace(ctx, in, stageName, workspaceMode)
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
		Goal:            goal,
		Workspace:       workspace.path,
		RepoRef:         in.RepoRef,
		Item:            in.Item,
		ContextPointers: upstream,
		Capabilities:    capabilities,
		Inputs:          inputs,
	}
	return env, workspace, nil
}

func (r *Runner) createStageWorkspace(ctx context.Context, in StartInput, stageName string, mode apiv1.WorkspaceMode) (*stageWorkspace, error) {
	switch mode {
	case apiv1.WorkspaceScratch:
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
		wt, err := r.cfg.Worktrees.Create(ctx, worktree.CreateOptions{
			RepoURL:    repoURL,
			RunID:      in.RunID + "-" + stageName,
			OwnerRunID: in.RunID,
			BaseRef:    baseRef,
			Branch:     providers.BranchName(in.Machine.Def.Name, in.RunID),
		})
		if err != nil {
			return nil, fmt.Errorf("create worktree: %w", err)
		}
		return &stageWorkspace{path: wt.Path, worktree: wt}, nil
	default:
		return nil, fmt.Errorf("unknown workspace mode %q", mode)
	}
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
