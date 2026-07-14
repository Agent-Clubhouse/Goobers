package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
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
	// Worktrees provisions the fresh, isolated, disposable working copy each
	// stage attempt runs in (§5).
	Worktrees *worktree.Manager
	// RunsDir is the journal's run directory (<instance-root>/runs).
	RunsDir string
	// MaxSteps overrides DefaultMaxSteps when > 0.
	MaxSteps int
	// RepoCloneURL derives the git remote URL worktree.Manager clones from a
	// RepoRef. Defaults to defaultRepoCloneURL (github.com over HTTPS). Tests
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

	// Record the run's branch up front (providers.BranchName): every stage's
	// worktree checks it out and the implementer pushes it, so it is the run's
	// primary external ref for traceability (#133). Deterministic from
	// (workflow, run id), so it is conformance-stable across runners.
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

	result, err := r.walk(ctx, jr, in, in.Machine.Def.Spec.Start, nil, nil, registrar)
	if err != nil {
		span.Fail(err)
		return result, err
	}
	span.Succeed(string(result.Phase))
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

// walk advances the machine from startState to a terminal state (or a
// human-gate/drain pause), journaling every stage/gate attempt and every
// artifact produced along the way. Gate dispatch (bounded repass, escalation,
// gate.evaluated journaling) is entirely owned by the gate.Evaluator
// constructed once here — it MUST NOT be shared across runs (its repass
// counters are run-scoped state), so a fresh one is built per walk. Start
// always begins at the machine's declared start state with resume=nil and
// gateAttempts=nil; Resume (resume.go) begins at the journal's checkpointed
// state, optionally with a resumeContext for an interrupted task attempt and
// gateAttempts seeded from each gate's last gate.evaluated event, so a
// resumed run's repass budget continues rather than resetting (#89). reg is
// the run's SecretRegistrar (see Start), threaded to every executor
// constructed here.
func (r *Runner) walk(ctx context.Context, jr *journal.Run, in StartInput, startState string, resume *resumeContext, gateAttempts map[string]int, reg SecretRegistrar) (Result, error) {
	ex := newExecutors(r.cfg, jr, reg)
	gateEval := &gate.Evaluator{
		Automated:   r.cfg.Automated,
		Journal:     jr,
		MaxRepasses: r.cfg.MaxRepasses,
		Attempts:    gateAttempts,
	}

	state := startState
	var pointers []apiv1.ContextPointer
	var lastStage string
	var lastResult apiv1.ResultEnvelope
	steps := 0

	for {
		steps++
		if steps > r.maxSteps {
			return r.failTerminal(jr, state, steps, fmt.Errorf("runner: run %q exceeded max steps (%d): possible loop", in.RunID, r.maxSteps))
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
					Error:  &journal.ErrorDetail{Code: "interrupted", Message: "attempt was in flight when the runner was interrupted"},
				}); err != nil {
					return Result{}, fmt.Errorf("runner: journal interrupted attempt for %q: %w", t.Name, err)
				}
				startAttempt = int32(resume.attempt) + 1
				resume = nil
			}
			result, produced, err := r.runTask(ctx, jr, in, ex, t, pointers, startAttempt)
			if err != nil {
				return r.failTerminal(jr, t.Name, steps, err)
			}
			pointers = append(pointers, produced...)
			lastStage, lastResult = t.Name, result

			switch result.Status {
			case apiv1.ResultBlocked:
				// Halt pending external intervention, at any position — a
				// resumable pause like a human gate's (ruling #110). The
				// stage.finished event's Append already checkpointed
				// state.json at t.Name (SetMachineState above ran before
				// dispatch and hasn't changed since), so no extra Checkpoint
				// call is needed here.
				return Result{Phase: journal.PhaseRunning, FinalState: t.Name, Steps: steps}, nil
			case apiv1.ResultFailure:
				if _, isGate := in.Machine.Gate(t.Next); t.Next != "" && isGate {
					// A downstream gate branches on the failure — the
					// shipped reviewer-gate pattern (docs/stage-contract.md).
					// Advance; do not terminalize here.
					state = t.Next
					continue
				}
				// Next is empty, a task, or a reserved terminal target: no
				// gate to branch on the failure. Never dispatch downstream
				// stages on a failed result, never silently complete
				// (ruling #110).
				return r.finish(jr, journal.PhaseFailed, t.Name, steps)
			}

			if t.Next == "" {
				return r.finish(jr, journal.PhaseCompleted, t.Name, steps)
			}
			state = t.Next
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

			gr, err := r.evaluateGate(ctx, gateEval, ex, in, g, lastStage, lastResult, pointers)
			if err != nil {
				return r.failTerminal(jr, g.Name, steps, err)
			}
			if gr.Escalated && r.cfg.Escalation != nil && in.Item != nil {
				if err := r.cfg.Escalation.NotifyEscalated(ctx, in.Item.ID, gr, "repass budget exhausted"); err != nil {
					return r.failTerminal(jr, g.Name, steps, fmt.Errorf("runner: notify escalation for gate %q: %w", g.Name, err))
				}
			}
			switch gr.Target {
			case workflow.TargetAbort:
				return r.finish(jr, journal.PhaseAborted, g.Name, steps)
			case workflow.TargetEscalate:
				return r.finish(jr, journal.PhaseEscalated, g.Name, steps)
			case workflow.TerminalComplete:
				return r.finish(jr, journal.PhaseCompleted, g.Name, steps)
			}
			state = gr.Target
			continue
		}

		return r.failTerminal(jr, state, steps, fmt.Errorf("runner: unknown state %q", state))
	}
}

// failTerminal journals the run's terminal run.finished(PhaseFailed) event
// before surfacing origErr, so a walk-level error never leaves phase=running
// forever — the daemon auto-resumes every PhaseRunning run on restart
// (cmd/goobers/daemon.go), and an unterminated failed run would be resumed
// (and fail identically) on every restart. Per §2.6's fail-closed journaling,
// the journal must record the failure, not pretend the run is still live
// (ruling #110). If the terminal append itself fails, both errors are
// reported rather than one silently swallowing the other.
func (r *Runner) failTerminal(jr *journal.Run, finalState string, steps int, origErr error) (Result, error) {
	if _, ferr := r.finish(jr, journal.PhaseFailed, finalState, steps); ferr != nil {
		return Result{}, fmt.Errorf("%w (additionally failed to journal terminal failure: %w)", origErr, ferr)
	}
	return Result{Phase: journal.PhaseFailed, FinalState: finalState, Steps: steps}, origErr
}

// finish appends the run's terminal run.finished event and returns its Result.
func (r *Runner) finish(jr *journal.Run, phase journal.RunPhase, finalState string, steps int) (Result, error) {
	if err := jr.Append(journal.Event{Type: journal.EventRunFinished, Status: string(phase)}); err != nil {
		return Result{}, fmt.Errorf("runner: journal run.finished: %w", err)
	}
	return Result{Phase: phase, FinalState: finalState, Steps: steps}, nil
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
// retry, and only Task.Retry's declared policy governs it.
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
func (r *Runner) runTask(ctx context.Context, jr *journal.Run, in StartInput, ex *executors, t apiv1.Task, upstream []apiv1.ContextPointer, startAttempt int32) (apiv1.ResultEnvelope, []apiv1.ContextPointer, error) {
	ctx, span := r.startTaskSpan(ctx, in, t)
	defer span.End()

	attemptCtx := context.WithoutCancel(ctx)

	maxAttempts := int32(1)
	var backoff time.Duration
	if t.Retry != nil {
		if t.Retry.MaxAttempts > 0 {
			maxAttempts = t.Retry.MaxAttempts
		}
		backoff = time.Duration(t.Retry.BackoffSeconds) * time.Second
	}
	if startAttempt > maxAttempts {
		err := fmt.Errorf("runner: task %q has no attempts left after resume (interrupted attempt already exhausted its %d-attempt budget)", t.Name, maxAttempts)
		span.Fail(err)
		return apiv1.ResultEnvelope{}, nil, err
	}

	var lastErr error
	for attempt := startAttempt; attempt <= maxAttempts; attempt++ {
		// The initial attempt carries no class and is always conformance-
		// normative; a retry is tagged "policy" since Task.Retry drove it
		// (journal §3.3 — "infra" is reserved for a crash-triggered resume
		// retry, not this stage-declared policy loop).
		var class journal.AttemptClass
		if attempt > 1 {
			class = journal.AttemptPolicy
		}
		if err := jr.Append(journal.Event{Type: journal.EventStageStarted, Stage: t.Name, Attempt: int(attempt), AttemptClass: class}); err != nil {
			err = fmt.Errorf("runner: journal stage.started for %q: %w", t.Name, err)
			span.Fail(err)
			return apiv1.ResultEnvelope{}, nil, err
		}

		result, dispatchErr := r.dispatchTask(attemptCtx, in, ex, t, upstream)
		if dispatchErr != nil {
			lastErr = dispatchErr
			// A journal that cannot be written stops the run (§2.6): this
			// write failing means the run's own record of what happened is
			// now unreliable, so it is fatal, not best-effort.
			if aerr := jr.Append(journal.Event{
				Type: journal.EventError, Stage: t.Name, Attempt: int(attempt), AttemptClass: class,
				Error: &journal.ErrorDetail{Code: "executor_error", Message: dispatchErr.Error()},
			}); aerr != nil {
				return apiv1.ResultEnvelope{}, nil, fmt.Errorf("runner: journal executor error for %q: %w", t.Name, aerr)
			}
			if attempt < maxAttempts {
				if backoff > 0 {
					time.Sleep(backoff)
				}
				continue
			}
			err := fmt.Errorf("runner: execute stage %q: %w (attempt %d/%d)", t.Name, lastErr, attempt, maxAttempts)
			span.Fail(err)
			return apiv1.ResultEnvelope{}, nil, err
		}

		if err := jr.Append(journal.Event{
			Type: journal.EventStageFinished, Stage: t.Name, Attempt: int(attempt), AttemptClass: class,
			Status: string(result.Status), Error: errorDetailFrom(result.Error),
		}); err != nil {
			err = fmt.Errorf("runner: journal stage.finished for %q: %w", t.Name, err)
			span.Fail(err)
			return apiv1.ResultEnvelope{}, nil, err
		}
		span.Succeed(string(result.Status))
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

// dispatchTask provisions one attempt's worktree and invokes the task's
// executor. It never journals — runTask owns attempt/retry journaling so a
// retried attempt is never mistaken for the run's overall outcome.
func (r *Runner) dispatchTask(ctx context.Context, in StartInput, ex *executors, t apiv1.Task, upstream []apiv1.ContextPointer) (apiv1.ResultEnvelope, error) {
	env, wt, err := r.buildEnvelope(ctx, in, t.Name, t.Goal, t.Inputs, t.Capabilities, upstream)
	if err != nil {
		return apiv1.ResultEnvelope{}, fmt.Errorf("prepare stage %q: %w", t.Name, err)
	}
	defer func() { _ = wt.Remove(ctx, worktree.RemoveOptions{}) }()

	switch t.Type {
	case apiv1.TaskDeterministic:
		if t.Run == nil {
			return apiv1.ResultEnvelope{}, fmt.Errorf("task %q is deterministic but declares no DeterministicRun", t.Name)
		}
		det, err := ex.deterministic()
		if err != nil {
			return apiv1.ResultEnvelope{}, err
		}
		return det.Run(ctx, env, *t.Run)
	case apiv1.TaskAgentic:
		ag, err := ex.agentic(t.Goober)
		if err != nil {
			return apiv1.ResultEnvelope{}, err
		}
		return ag.Invoke(ctx, env)
	default:
		return apiv1.ResultEnvelope{}, fmt.Errorf("task %q has unknown type %q", t.Name, t.Type)
	}
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
func (r *Runner) evaluateGate(ctx context.Context, gateEval *gate.Evaluator, ex *executors, in StartInput, g apiv1.Gate, subjectStage string, subjectResult apiv1.ResultEnvelope, upstream []apiv1.ContextPointer) (gate.Result, error) {
	// Same drain contract as runTask: a gate attempt already underway when
	// SIGTERM cancels the run-level ctx finishes and journals cleanly.
	ctx = context.WithoutCancel(ctx)

	gooberName := ""
	if g.Evaluator == apiv1.EvaluatorAgentic && g.Agentic != nil {
		gooberName = g.Agentic.Goober
	}
	ctx, span := r.startGateSpan(ctx, in, g, gooberName)
	defer span.End()

	env, wt, err := r.buildEnvelope(ctx, in, g.Name, "gate: "+g.Name, nil, nil, upstream)
	if err != nil {
		err = fmt.Errorf("runner: prepare gate %q: %w", g.Name, err)
		span.Fail(err)
		return gate.Result{}, err
	}
	defer func() { _ = wt.Remove(ctx, worktree.RemoveOptions{}) }()

	switch g.Evaluator {
	case apiv1.EvaluatorAutomated:
		if env.Inputs == nil {
			env.Inputs = make(map[string]interface{}, 1+len(subjectResult.Outputs))
		}
		env.Inputs[gate.InputKeyStatus] = string(subjectResult.Status)
		for k, v := range subjectResult.Outputs {
			env.Inputs[k] = v
		}
	case apiv1.EvaluatorAgentic:
		ag, aerr := ex.agentic(gooberName)
		if aerr != nil {
			err := fmt.Errorf("runner: gate %q: %w", g.Name, aerr)
			span.Fail(err)
			return gate.Result{}, err
		}
		// Rebound per gate evaluated, not shared across gateEval's lifetime:
		// different agentic gates in the same run may target different
		// reviewer goobers. gate.Evaluator reads this field fresh on every
		// Evaluate call, so mutating it here between calls is safe.
		gateEval.Reviewer = &gate.ReviewerEvaluator{Goober: ag}
	}

	result, err := gateEval.Evaluate(ctx, g, env, subjectStage, subjectResult)
	if err != nil {
		err = fmt.Errorf("runner: evaluate gate %q: %w", g.Name, err)
		span.Fail(err)
		return gate.Result{}, err
	}
	span.Succeed(result.Outcome)
	return result, nil
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

// buildEnvelope provisions the isolated worktree and builds the invocation
// envelope for one stage attempt. The caller owns removing the returned
// worktree. Credentials are NOT resolved here — each invoke.Deterministic/
// invoke.Goober executor resolves its own capability-scoped credentials from
// env.Capabilities (see executor.go).
func (r *Runner) buildEnvelope(ctx context.Context, in StartInput, stageName, goal string, taskInputs map[string]string, capabilities []string, upstream []apiv1.ContextPointer) (apiv1.InvocationEnvelope, *worktree.Worktree, error) {
	repoURL, err := r.cfg.RepoCloneURL(in.RepoRef)
	if err != nil {
		return apiv1.InvocationEnvelope{}, nil, err
	}
	baseRef := in.RepoRef.Branch
	if baseRef == "" {
		baseRef = "main"
	}
	// Every stage of a run checks out the run's shared branch in its own fresh
	// worktree: the first mutating stage creates it off baseRef, later stages
	// inherit the prior stages' commits. Without this, each stage's worktree
	// detached at baseRef and local-ci/reviewer gates evaluated a pristine tree
	// instead of the run's actual diff (#133). Branch is keyed on the run id,
	// not the per-stage worktree key, so all stages share the one branch.
	wt, err := r.cfg.Worktrees.Create(ctx, worktree.CreateOptions{
		RepoURL: repoURL,
		RunID:   in.RunID + "-" + stageName,
		BaseRef: baseRef,
		Branch:  providers.BranchName(in.Machine.Def.Name, in.RunID),
	})
	if err != nil {
		return apiv1.InvocationEnvelope{}, nil, fmt.Errorf("create worktree: %w", err)
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
		Workspace:       wt.Path,
		RepoRef:         in.RepoRef,
		Item:            in.Item,
		ContextPointers: upstream,
		Capabilities:    capabilities,
		Inputs:          inputs,
	}
	return env, wt, nil
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

// defaultRepoCloneURL derives the git remote URL worktree.Manager clones from
// a RepoRef. ADO support is a placeholder pending its provider's clone-URL
// convention (V0 ships GitHub first, ARCHITECTURE §12).
func defaultRepoCloneURL(ref apiv1.RepoRef) (string, error) {
	switch ref.Provider {
	case apiv1.ProviderGitHub:
		return fmt.Sprintf("https://github.com/%s/%s.git", ref.Owner, ref.Name), nil
	default:
		return "", fmt.Errorf("runner: unsupported repo provider %q", ref.Provider)
	}
}
