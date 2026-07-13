package runner

import (
	"context"
	"encoding/json"
	"fmt"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/credentials"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/workflow"
	"github.com/goobers/goobers/internal/worktree"
)

// DefaultMaxSteps bounds the state walk against a runaway machine (carried over
// from the Temporal engine core, ARCHITECTURE §3.1).
const DefaultMaxSteps = 10000

// Config wires a Runner's dependencies: the compiled machine to walk, the
// executor/gate seam implementations (executor.go), and the substrate the
// local runner drives directly — worktrees (#16), credentials (#14), and the
// journal's runs directory (#8).
type Config struct {
	// Machine is the compiled workflow (#9) this Runner walks.
	Machine *workflow.Machine
	// Executors dispatches deterministic/agentic stage attempts.
	Executors Executors
	// Gates dispatches automated/agentic gate attempts. Human gates are never
	// looked up here (see GateEvaluator doc in executor.go).
	Gates GateEvaluators
	// Worktrees provisions the fresh, isolated, disposable working copy each
	// stage attempt runs in (§5).
	Worktrees *worktree.Manager
	// Credentials materializes capability-scoped credentials per attempt.
	Credentials *credentials.Injector
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
// recording every transition to the run journal, and dispatching stages
// through the executor seam. It is the substrate-neutral local runner core
// (ARCHITECTURE.md §3.1) — deliverable A: seam + deterministic walk. Retries,
// crash-resume, and cancellation (deliverable B) build on top of this.
type Runner struct {
	cfg      Config
	maxSteps int
}

// New validates cfg and returns a ready Runner.
func New(cfg Config) (*Runner, error) {
	if cfg.Machine == nil {
		return nil, fmt.Errorf("runner: Machine is required")
	}
	if cfg.Executors == nil {
		return nil, fmt.Errorf("runner: Executors is required")
	}
	if cfg.Gates == nil {
		return nil, fmt.Errorf("runner: Gates is required")
	}
	if cfg.Worktrees == nil {
		return nil, fmt.Errorf("runner: Worktrees is required")
	}
	if cfg.Credentials == nil {
		return nil, fmt.Errorf("runner: Credentials is required")
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
	// RunID uniquely identifies this run (the OpenTelemetry trace id).
	RunID string
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
// state (or a human-gate pause).
func (r *Runner) Start(ctx context.Context, in StartInput) (Result, error) {
	if in.RunID == "" {
		return Result{}, fmt.Errorf("runner: RunID is required")
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
		Workflow:        r.cfg.Machine.Def.Name,
		WorkflowVersion: r.cfg.Machine.Def.Version,
		WorkflowDigest:  r.cfg.Machine.Digest(),
		Gaggle:          in.Gaggle,
		Trigger:         in.Trigger,
	}, inputs)
	if err != nil {
		return Result{}, fmt.Errorf("runner: create journal for run %q: %w", in.RunID, err)
	}
	defer func() { _ = jr.Close() }()

	return r.walk(ctx, jr, in)
}

// walk advances the machine from its start state to a terminal state (or a
// human-gate pause), journaling every stage/gate attempt and every artifact
// produced along the way.
func (r *Runner) walk(ctx context.Context, jr *journal.Run, in StartInput) (Result, error) {
	state := r.cfg.Machine.Def.Spec.Start
	var pointers []apiv1.ContextPointer
	steps := 0

	for {
		steps++
		if steps > r.maxSteps {
			return Result{}, fmt.Errorf("runner: run %q exceeded max steps (%d): possible loop", in.RunID, r.maxSteps)
		}
		jr.SetMachineState(state)

		if t, ok := r.cfg.Machine.Task(state); ok {
			_, produced, err := r.runTask(ctx, jr, in, t, pointers)
			if err != nil {
				return Result{}, err
			}
			pointers = append(pointers, produced...)
			if t.Next == "" {
				return r.finish(jr, journal.PhaseCompleted, t.Name, steps)
			}
			state = t.Next
			continue
		}

		if g, ok := r.cfg.Machine.Gate(state); ok {
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

			outcome, err := r.evaluateGate(ctx, jr, in, g, pointers)
			if err != nil {
				return Result{}, err
			}
			target, ok := workflow.BranchTarget(g, outcome)
			if !ok {
				return Result{}, fmt.Errorf("runner: gate %q produced outcome %q with no defined branch (never a silent pass)", g.Name, outcome)
			}
			switch target {
			case workflow.TargetAbort:
				return r.finish(jr, journal.PhaseAborted, g.Name, steps)
			case workflow.TargetEscalate:
				return r.finish(jr, journal.PhaseEscalated, g.Name, steps)
			case workflow.TerminalComplete:
				return r.finish(jr, journal.PhaseCompleted, g.Name, steps)
			}
			state = target
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

// runTask dispatches one task attempt: provisions its worktree, materializes
// its declared capabilities' credentials, invokes the matching StageExecutor,
// commits any produced artifacts to the journal, and journals the attempt.
// Per the Temporal engine's established semantics, a task always flows to
// Next regardless of its ResultStatus — a downstream gate is what branches;
// only a genuine dispatch error (infra failure, not a business "failure"
// status) aborts the run. Retries (deliverable B) will wrap this call.
func (r *Runner) runTask(ctx context.Context, jr *journal.Run, in StartInput, t apiv1.Task, upstream []apiv1.ContextPointer) (apiv1.ResultEnvelope, []apiv1.ContextPointer, error) {
	const attempt = 1
	if err := jr.Append(journal.Event{Type: journal.EventStageStarted, Stage: t.Name, Attempt: attempt}); err != nil {
		return apiv1.ResultEnvelope{}, nil, fmt.Errorf("runner: journal stage.started for %q: %w", t.Name, err)
	}

	executor, ok := r.cfg.Executors[t.Type]
	if !ok {
		return apiv1.ResultEnvelope{}, nil, fmt.Errorf("runner: no executor registered for task type %q (task %q)", t.Type, t.Name)
	}

	req, wt, err := r.prepareStage(ctx, in, t.Name, t.Goal, t.Inputs, t.Capabilities, upstream)
	if err != nil {
		return apiv1.ResultEnvelope{}, nil, fmt.Errorf("runner: prepare stage %q: %w", t.Name, err)
	}
	defer func() { _ = wt.Remove(ctx, worktree.RemoveOptions{}) }()

	out, err := executor.Execute(ctx, req)
	if err != nil {
		_ = jr.Append(journal.Event{Type: journal.EventError, Stage: t.Name, Error: &journal.ErrorDetail{Code: "executor_error", Message: err.Error()}})
		return apiv1.ResultEnvelope{}, nil, fmt.Errorf("runner: execute stage %q: %w", t.Name, err)
	}

	result, produced, err := r.commitArtifacts(jr, t.Name, out)
	if err != nil {
		return apiv1.ResultEnvelope{}, nil, err
	}

	if err := jr.Append(journal.Event{Type: journal.EventStageFinished, Stage: t.Name, Attempt: attempt, Status: string(result.Status)}); err != nil {
		return apiv1.ResultEnvelope{}, nil, fmt.Errorf("runner: journal stage.finished for %q: %w", t.Name, err)
	}
	return result, produced, nil
}

// evaluateGate dispatches one automated or agentic gate attempt and returns
// the outcome string the machine branches on.
func (r *Runner) evaluateGate(ctx context.Context, jr *journal.Run, in StartInput, g apiv1.Gate, upstream []apiv1.ContextPointer) (string, error) {
	evaluator, ok := r.cfg.Gates[g.Evaluator]
	if !ok {
		return "", fmt.Errorf("runner: no gate evaluator registered for evaluator kind %q (gate %q)", g.Evaluator, g.Name)
	}

	req, wt, err := r.prepareStage(ctx, in, g.Name, "gate: "+g.Name, nil, nil, upstream)
	if err != nil {
		return "", fmt.Errorf("runner: prepare gate %q: %w", g.Name, err)
	}
	defer func() { _ = wt.Remove(ctx, worktree.RemoveOptions{}) }()

	verdict, err := evaluator.Evaluate(ctx, req)
	if err != nil {
		_ = jr.Append(journal.Event{Type: journal.EventError, Gate: g.Name, Error: &journal.ErrorDetail{Code: "gate_evaluator_error", Message: err.Error()}})
		return "", fmt.Errorf("runner: evaluate gate %q: %w", g.Name, err)
	}
	outcome := string(verdict.Decision)
	if err := jr.Append(journal.Event{Type: journal.EventGateEvaluated, Gate: g.Name, Verdict: outcome}); err != nil {
		return "", fmt.Errorf("runner: journal gate.evaluated for %q: %w", g.Name, err)
	}
	return outcome, nil
}

// prepareStage provisions the isolated worktree, materializes capability-
// scoped credentials, and builds the invocation envelope + StageRequest for
// one stage attempt. The caller owns removing the returned worktree.
func (r *Runner) prepareStage(ctx context.Context, in StartInput, stageName, goal string, taskInputs map[string]string, capabilities []string, upstream []apiv1.ContextPointer) (StageRequest, *worktree.Worktree, error) {
	repoURL, err := r.cfg.RepoCloneURL(in.RepoRef)
	if err != nil {
		return StageRequest{}, nil, err
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
		return StageRequest{}, nil, fmt.Errorf("create worktree: %w", err)
	}

	creds, err := r.cfg.Credentials.Materialize(ctx, capabilities)
	if err != nil {
		_ = wt.Remove(ctx, worktree.RemoveOptions{})
		return StageRequest{}, nil, fmt.Errorf("materialize credentials: %w", err)
	}

	inputs := make(map[string]interface{}, len(taskInputs))
	for k, v := range taskInputs {
		inputs[k] = v
	}
	env := apiv1.InvocationEnvelope{
		TaskID:          in.RunID + ":" + stageName,
		WorkflowID:      r.cfg.Machine.Def.Name,
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
	return StageRequest{Envelope: env, Credentials: creds}, wt, nil
}

// commitArtifacts writes each of out.Produced into the journal by content
// digest, folds the resulting ArtifactPointers into out.Result.Artifacts, and
// builds the ContextPointers downstream stages will receive for them — named
// "<stageName>/<artifact name>". This is the one place an executor's raw bytes
// become the wire-visible ArtifactPointer type (#10); see executor.go's
// StageOutput doc for why executors don't construct pointers themselves.
//
// V0 has no explicit data-flow declaration in the DSL yet, so every downstream
// stage sees every artifact produced so far in the run — pointer only, never
// the producing stage's full result (§2.4) — and picks out what it needs by
// name.
func (r *Runner) commitArtifacts(jr *journal.Run, stageName string, out StageOutput) (apiv1.ResultEnvelope, []apiv1.ContextPointer, error) {
	result := out.Result
	pointers := make([]apiv1.ContextPointer, 0, len(out.Produced))
	for _, p := range out.Produced {
		ref, err := jr.RecordArtifact(p.Name, p.Data)
		if err != nil {
			return apiv1.ResultEnvelope{}, nil, fmt.Errorf("runner: commit artifact %q for stage %q: %w", p.Name, stageName, err)
		}
		pointer := apiv1.ArtifactPointer{
			Path:      ref.Path,
			Digest:    ref.Digest,
			Size:      ref.Size,
			MediaType: p.MediaType,
		}
		result.Artifacts = append(result.Artifacts, pointer)
		pointers = append(pointers, apiv1.ContextPointer{Name: stageName + "/" + p.Name, Artifact: &pointer})
	}
	return result, pointers, nil
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
