package runner

import (
	"context"
	"encoding/json"
	"fmt"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/gate"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/workflow"
	"github.com/goobers/goobers/internal/worktree"
)

// DefaultMaxSteps bounds the state walk against a runaway machine (carried over
// from the Temporal engine core, ARCHITECTURE §3.1).
const DefaultMaxSteps = 10000

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
}

// Runner advances a compiled workflow.Machine stage-by-stage, durably
// recording every transition to the run journal, and dispatching tasks
// through the pre-existing internal/invoke seam. It is the substrate-neutral
// local runner core (ARCHITECTURE.md §3.1) — deliverable A: seam +
// deterministic walk. Retries, crash-resume, and cancellation (deliverable B)
// build on top of this.
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

// Result is a run's outcome as of the moment Start returns. A human gate
// leaves the run non-terminal (Phase stays journal.PhaseRunning, FinalState is
// the paused gate's name) — resuming past it is deliverable B's job; the
// journal's state.json already checkpoints exactly where to pick up.
type Result struct {
	Phase      journal.RunPhase
	FinalState string
	Steps      int
}

// Start creates a new run journal pinned to the compiled machine's identity,
// snapshots Item as an immutable input, and walks the machine to a terminal
// state (or a human-gate pause). Start is synchronous — it returns once the
// run reaches a terminal state or pauses at a human gate, which for a real
// agentic stage may be minutes. A caller driving many runs (e.g. a scheduler)
// should call Start in its own goroutine per run rather than block its own
// dispatch loop on it.
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

	jr, err := journal.Create(r.cfg.RunsDir, journal.RunIdentity{
		RunID:           in.RunID,
		Workflow:        in.Machine.Def.Name,
		WorkflowVersion: in.Machine.Def.Version,
		WorkflowDigest:  in.Machine.Digest(),
		Gaggle:          in.Gaggle,
		Trigger:         in.Trigger,
	}, inputs)
	if err != nil {
		return Result{}, fmt.Errorf("runner: create journal for run %q: %w", in.RunID, err)
	}
	defer func() { _ = jr.Close() }()

	return r.walk(ctx, jr, in)
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

	det    invoke.Deterministic
	agents map[string]invoke.Goober
}

func newExecutors(cfg Config, rec ArtifactRecorder) *executors {
	return &executors{cfg: cfg, rec: rec, agents: map[string]invoke.Goober{}}
}

func (e *executors) deterministic() (invoke.Deterministic, error) {
	if e.det != nil {
		return e.det, nil
	}
	if e.cfg.NewDeterministic == nil {
		return nil, fmt.Errorf("runner: no NewDeterministic configured for a deterministic task")
	}
	det, err := e.cfg.NewDeterministic(e.rec)
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
	ag, err := e.cfg.NewAgentic(gooberName, e.rec)
	if err != nil {
		return nil, fmt.Errorf("runner: construct agentic executor for goober %q: %w", gooberName, err)
	}
	e.agents[gooberName] = ag
	return ag, nil
}

// walk advances the machine from its start state to a terminal state (or a
// human-gate pause), journaling every stage/gate attempt and every artifact
// produced along the way. Gate dispatch (bounded repass, escalation,
// gate.evaluated journaling) is entirely owned by the gate.Evaluator
// constructed once here — it MUST NOT be shared across runs (its repass
// counters are run-scoped state), so a fresh one is built per walk.
func (r *Runner) walk(ctx context.Context, jr *journal.Run, in StartInput) (Result, error) {
	ex := newExecutors(r.cfg, jr)
	gateEval := &gate.Evaluator{
		Automated:   r.cfg.Automated,
		Journal:     jr,
		MaxRepasses: r.cfg.MaxRepasses,
	}

	state := in.Machine.Def.Spec.Start
	var pointers []apiv1.ContextPointer
	var lastStage string
	var lastResult apiv1.ResultEnvelope
	steps := 0

	for {
		steps++
		if steps > r.maxSteps {
			return Result{}, fmt.Errorf("runner: run %q exceeded max steps (%d): possible loop", in.RunID, r.maxSteps)
		}
		jr.SetMachineState(state)

		if t, ok := in.Machine.Task(state); ok {
			result, produced, err := r.runTask(ctx, jr, in, ex, t, pointers)
			if err != nil {
				return Result{}, err
			}
			pointers = append(pointers, produced...)
			lastStage, lastResult = t.Name, result
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
				// gate (deliverable B: resuming on the operator's decision).
				if err := jr.Checkpoint(); err != nil {
					return Result{}, fmt.Errorf("runner: checkpoint pause at gate %q: %w", g.Name, err)
				}
				return Result{Phase: journal.PhaseRunning, FinalState: g.Name, Steps: steps}, nil
			}

			gr, err := r.evaluateGate(ctx, gateEval, ex, in, g, lastStage, lastResult, pointers)
			if err != nil {
				return Result{}, err
			}
			if gr.Escalated && r.cfg.Escalation != nil && in.Item != nil {
				if err := r.cfg.Escalation.NotifyEscalated(ctx, in.Item.ID, gr, "repass budget exhausted"); err != nil {
					return Result{}, fmt.Errorf("runner: notify escalation for gate %q: %w", g.Name, err)
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

		return Result{}, fmt.Errorf("runner: unknown state %q", state)
	}
}

// finish appends the run's terminal run.finished event and returns its Result.
func (r *Runner) finish(jr *journal.Run, phase journal.RunPhase, finalState string, steps int) (Result, error) {
	if err := jr.Append(journal.Event{Type: journal.EventRunFinished, Status: string(phase)}); err != nil {
		return Result{}, fmt.Errorf("runner: journal run.finished: %w", err)
	}
	return Result{Phase: phase, FinalState: finalState, Steps: steps}, nil
}

// runTask dispatches one task attempt: provisions its worktree, invokes the
// matching invoke.Deterministic/invoke.Goober executor (which resolves its own
// capability-scoped credentials and commits its own artifacts to the run
// journal — see executor.go's ArtifactRecorder doc), and journals the
// attempt. Per the Temporal engine's established semantics, a task always
// flows to Next regardless of its ResultStatus — a downstream gate is what
// branches; only a genuine dispatch error (infra failure, not a business
// "failure" status) aborts the run. Retries (deliverable B) will wrap this
// call.
func (r *Runner) runTask(ctx context.Context, jr *journal.Run, in StartInput, ex *executors, t apiv1.Task, upstream []apiv1.ContextPointer) (apiv1.ResultEnvelope, []apiv1.ContextPointer, error) {
	const attempt = 1
	if err := jr.Append(journal.Event{Type: journal.EventStageStarted, Stage: t.Name, Attempt: attempt}); err != nil {
		return apiv1.ResultEnvelope{}, nil, fmt.Errorf("runner: journal stage.started for %q: %w", t.Name, err)
	}

	env, wt, err := r.buildEnvelope(ctx, in, t.Name, t.Goal, t.Inputs, t.Capabilities, upstream)
	if err != nil {
		return apiv1.ResultEnvelope{}, nil, fmt.Errorf("runner: prepare stage %q: %w", t.Name, err)
	}
	defer func() { _ = wt.Remove(ctx, worktree.RemoveOptions{}) }()

	var result apiv1.ResultEnvelope
	switch t.Type {
	case apiv1.TaskDeterministic:
		if t.Run == nil {
			return apiv1.ResultEnvelope{}, nil, fmt.Errorf("runner: task %q is deterministic but declares no DeterministicRun", t.Name)
		}
		det, derr := ex.deterministic()
		if derr != nil {
			return apiv1.ResultEnvelope{}, nil, derr
		}
		result, err = det.Run(ctx, env, *t.Run)
	case apiv1.TaskAgentic:
		ag, aerr := ex.agentic(t.Goober)
		if aerr != nil {
			return apiv1.ResultEnvelope{}, nil, aerr
		}
		result, err = ag.Invoke(ctx, env)
	default:
		return apiv1.ResultEnvelope{}, nil, fmt.Errorf("runner: task %q has unknown type %q", t.Name, t.Type)
	}
	if err != nil {
		_ = jr.Append(journal.Event{Type: journal.EventError, Stage: t.Name, Error: &journal.ErrorDetail{Code: "executor_error", Message: err.Error()}})
		return apiv1.ResultEnvelope{}, nil, fmt.Errorf("runner: execute stage %q: %w", t.Name, err)
	}

	if err := jr.Append(journal.Event{Type: journal.EventStageFinished, Stage: t.Name, Attempt: attempt, Status: string(result.Status)}); err != nil {
		return apiv1.ResultEnvelope{}, nil, fmt.Errorf("runner: journal stage.finished for %q: %w", t.Name, err)
	}
	return result, contextPointersFor(t.Name, result.Artifacts), nil
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
	env, wt, err := r.buildEnvelope(ctx, in, g.Name, "gate: "+g.Name, nil, nil, upstream)
	if err != nil {
		return gate.Result{}, fmt.Errorf("runner: prepare gate %q: %w", g.Name, err)
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
		gooberName := ""
		if g.Agentic != nil {
			gooberName = g.Agentic.Goober
		}
		ag, aerr := ex.agentic(gooberName)
		if aerr != nil {
			return gate.Result{}, fmt.Errorf("runner: gate %q: %w", g.Name, aerr)
		}
		// Rebound per gate evaluated, not shared across gateEval's lifetime:
		// different agentic gates in the same run may target different
		// reviewer goobers. gate.Evaluator reads this field fresh on every
		// Evaluate call, so mutating it here between calls is safe.
		gateEval.Reviewer = &gate.ReviewerEvaluator{Goober: ag}
	}

	result, err := gateEval.Evaluate(ctx, g, env, subjectStage, subjectResult)
	if err != nil {
		return gate.Result{}, fmt.Errorf("runner: evaluate gate %q: %w", g.Name, err)
	}
	return result, nil
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
	wt, err := r.cfg.Worktrees.Create(ctx, worktree.CreateOptions{
		RepoURL: repoURL,
		RunID:   in.RunID + "-" + stageName,
		BaseRef: baseRef,
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
