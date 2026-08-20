package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/gate"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/runcontrol"
	"github.com/goobers/goobers/internal/workflow"
)

// ErrTerminalGenerationChanged means an intervention was validated against an
// earlier terminal segment than the one currently recorded in the journal.
var ErrTerminalGenerationChanged = errors.New("terminal run generation changed")

// ResumeInput identifies an interrupted run to pick back up. Everything
// recoverable from the journal is read from it (Gaggle, Trigger, the
// snapshotted Item and workflow Definition); RepoRef is not journaled, so the
// caller supplies it again exactly as it did for the original Start.
type ResumeInput struct {
	// RunID selects the run directory under Config.RunsDir.
	RunID string
	// Machine is the compiled workflow (#9) this run was walking. When nil,
	// Resume reconstructs it from the immutable workflow-definition input.
	// When supplied, its digest MUST match the run's pinned WorkflowDigest
	// (WF-016).
	Machine *workflow.Machine
	// GooberDigest is the current resolved execution identity.
	GooberDigest string
	// RepoRef is the target repository every stage worktree branches from —
	// the same value originally passed to Start.
	RepoRef apiv1.RepoRef
	// HumanDecision resolves the latest durable human-gate pause. Nil performs
	// ordinary crash recovery and leaves a paused human gate awaiting input.
	HumanDecision *HumanGateDecision
	// RecoveryReason marks an automatic runner recovery. Empty means this is
	// an operator-driven or otherwise ordinary resume.
	RecoveryReason string
}

// HumanGateDecision is an explicit outcome submitted for one paused human
// gate. PauseSeq binds the decision to the durable gate.paused occurrence so a
// delayed request cannot resolve a later visit to the same gate. Actor is the
// authenticated principal supplying the decision and is required when the gate
// restricts approvers. Decision must exactly match one of that gate's configured
// branch keys.
type HumanGateDecision struct {
	Gate     string
	PauseSeq uint64
	Decision string
	Actor    string
}

// ResumeFromTerminalInput describes an explicit human action that reopens an
// escalated or failed run at a chosen workflow state.
type ResumeFromTerminalInput struct {
	RunID        string
	Machine      *workflow.Machine
	GooberDigest string
	RepoRef      apiv1.RepoRef
	Target       string
	Complete     bool
	Actor        string
	Action       string
	Gate         string
	Decision     string
	Rationale    string
	// ExpectedTerminalSeq binds the action to the run.finished event observed
	// when the intervention was validated.
	ExpectedTerminalSeq uint64
}

// Resume reopens an interrupted run's journal (journal.Recover — replays the
// event log and repairs any torn final write left by a crash mid-append),
// verifies it is still pinned to Machine's exact digest, and continues the
// walk from a checkpointed MachineState. A run already at a terminal phase
// returns that phase immediately without re-walking — Resume is safe to
// call on a run that turns out to have already finished. That terminal
// check, and the MachineState it resumes from, are both event-log-first
// (#242): state.json is read only as a checked hint, never a requirement —
// see rd.Phase() and the MachineState fallback below.
//
// If the checkpointed state names a task, and that task's last attempt has a
// stage.started event with no matching stage.finished, the runner was
// interrupted mid-attempt (a crash, not a graceful drain — a graceful drain
// only ever checkpoints BETWEEN stages, never mid-dispatch). That attempt is
// journaled as a terminal, infra-tagged failure before the next attempt
// dispatches — see walk's resumeContext handling — so a stage is never
// silently re-run as if the interrupted attempt never happened, and the
// crash cannot grant the task extra attempts beyond its own declared policy.
//
// If instead that task's last attempt already finished cleanly before the
// crash (state.json's machineState still names it — see walk's
// SetMachineState timing), Resume does NOT re-dispatch it: re-running a
// side-effecting stage that already completed would duplicate its effects
// (#107). It reconstructs the finished result from the journal and applies
// the exact transition (taskOutcome) a live walk would have taken, so the
// walk actually resumes at the RIGHT next state.
//
// A paused human-gate resume requires a decision bound to the durable
// gate.paused sequence; ordinary crash resume supplies none and remains paused.
// If gate.evaluated was fsynced before a crash, its recorded transition is
// replayed independently of state.json and an exact decision retry is
// idempotent. Any other stale or mismatched decision fails closed.
//
// An automated/agentic gate-state resume evaluates against the REAL subject:
// lastFinishedSubject reconstructs the last finished stage's full result
// (status, outputs, artifacts — journaled on stage.finished for exactly this)
// instead of walk's in-memory-only lastStage/lastResult defaulting to a zero
// value (#108). Its bounded-repass counter (internal/gate.Evaluator.Attempts,
// #89) is restored from gate.evaluated outcomes and gate.started pre-dispatch
// markers. A dangling start consumes its prospective repass slot; once
// repeated interrupted evaluations exceed the budget, Evaluate escalates
// without dispatching the side-effecting evaluator again (#263).
func (r *Runner) Resume(ctx context.Context, in ResumeInput) (Result, error) {
	if in.RunID == "" {
		return Result{}, fmt.Errorf("runner: RunID is required")
	}
	if in.HumanDecision != nil {
		decision := *in.HumanDecision
		decision.Gate = strings.TrimSpace(decision.Gate)
		decision.Decision = strings.TrimSpace(decision.Decision)
		decision.Actor = strings.TrimSpace(decision.Actor)
		if decision.Gate == "" {
			return Result{}, fmt.Errorf("runner: human decision gate is required")
		}
		if decision.PauseSeq == 0 {
			return Result{}, fmt.Errorf("runner: human decision pause sequence is required")
		}
		if decision.Decision == "" {
			return Result{}, fmt.Errorf("runner: human decision is required")
		}
		in.HumanDecision = &decision
	}

	dir := filepath.Join(r.cfg.RunsDir, in.RunID)

	// A fresh registrar/scrubber per resume, exactly like Start — a run's
	// secrets have no business outliving one process's handling of it.
	registrar, scrubber := journal.DefaultScrubber()
	jr, _, err := journal.Recover(dir, journal.WithScrubber(scrubber), journal.WithAppendObserver(r.cfg.JournalAdvanced))
	if err != nil {
		return Result{}, fmt.Errorf("runner: recover run %q: %w", in.RunID, err)
	}
	defer func() { _ = jr.Close() }()

	return r.withActiveRun(ctx, in.RunID, jr, func(ctx context.Context) (Result, error) {
		return r.resumeOwned(ctx, in, jr, registrar, dir)
	})
}

// ResumeFromTerminal durably reopens an escalated or failed run and continues
// it from Target. The action remains pinned to the immutable workflow identity
// in run.yaml (WF-016); a changed definition is refused before run.resumed is
// appended. The event records the human actor, prior terminal phase, target,
// and verified workflow pin so a crash after the action can recover it exactly.
func (r *Runner) ResumeFromTerminal(ctx context.Context, in ResumeFromTerminalInput) (Result, error) {
	if !apiv1.ValidRunID(in.RunID) {
		return Result{}, fmt.Errorf("runner: invalid run id %q", in.RunID)
	}
	if in.Machine == nil {
		return Result{}, fmt.Errorf("runner: Machine is required")
	}
	in.Target = strings.TrimSpace(in.Target)
	if in.Target == "" && !in.Complete {
		return Result{}, fmt.Errorf("runner: terminal resume target is required")
	}
	if in.Target != "" && in.Complete {
		return Result{}, fmt.Errorf("runner: terminal resume cannot target a state and completion together")
	}
	in.Actor = strings.TrimSpace(in.Actor)
	if in.Actor == "" {
		return Result{}, fmt.Errorf("runner: terminal resume actor is required")
	}
	if in.ExpectedTerminalSeq == 0 {
		return Result{}, fmt.Errorf("runner: expected terminal sequence is required")
	}
	in.Action = strings.TrimSpace(in.Action)
	in.Gate = strings.TrimSpace(in.Gate)
	in.Decision = strings.TrimSpace(in.Decision)
	in.Rationale = strings.TrimSpace(in.Rationale)

	dir := filepath.Join(r.cfg.RunsDir, in.RunID)
	registrar, scrubber := journal.DefaultScrubber()
	jr, _, err := journal.Recover(dir, journal.WithScrubber(scrubber), journal.WithAppendObserver(r.cfg.JournalAdvanced))
	if err != nil {
		return Result{}, fmt.Errorf("runner: recover run %q for terminal resume: %w", in.RunID, err)
	}
	defer func() { _ = jr.Close() }()

	return r.withActiveRun(ctx, in.RunID, jr, func(ctx context.Context) (Result, error) {
		rd, err := journal.OpenRead(dir)
		if err != nil {
			return Result{}, fmt.Errorf("runner: open run %q for terminal resume: %w", in.RunID, err)
		}
		id, err := rd.Identity()
		if err != nil {
			return Result{}, fmt.Errorf("runner: read identity for run %q: %w", in.RunID, err)
		}
		phase, err := rd.Phase()
		if err != nil {
			return Result{}, fmt.Errorf("runner: reconstruct phase for run %q: %w", in.RunID, err)
		}
		if phase != journal.PhaseEscalated && phase != journal.PhaseFailed {
			return Result{}, fmt.Errorf("runner: run %q is %s; only escalated or failed runs can be resumed by a human", in.RunID, phase)
		}
		events, err := rd.Events()
		if err != nil {
			return Result{}, fmt.Errorf("runner: read events for run %q terminal resume: %w", in.RunID, err)
		}
		if err := validateTerminalGeneration(in.RunID, events, in.ExpectedTerminalSeq); err != nil {
			return Result{}, err
		}
		if id.WorkflowDigest == "" {
			return Result{}, fmt.Errorf("runner: run %q has no pinned workflow digest, refusing terminal resume (WF-016)", in.RunID)
		}
		if id.Workflow != in.Machine.Def.Name ||
			id.WorkflowVersion != in.Machine.Def.Version ||
			id.WorkflowDigest != in.Machine.Digest() ||
			(id.GooberDigest != "" && id.GooberDigest != in.GooberDigest) {
			return Result{}, fmt.Errorf(
				"runner: run %q is pinned to workflow %q version %d digest %q and goober digest %q, cannot terminal-resume against %q version %d digest %q and goober digest %q (WF-016)",
				in.RunID, id.Workflow, id.WorkflowVersion, id.WorkflowDigest, id.GooberDigest,
				in.Machine.Def.Name, in.Machine.Def.Version, in.Machine.Digest(), in.GooberDigest,
			)
		}
		if !in.Complete {
			if in.Target == workflow.TargetJoin {
				if _, _, ok := interventionParallelContext(events, in.Machine, in.Gate); !ok {
					return Result{}, fmt.Errorf("runner: terminal resume target %q has no parallel branch context", in.Target)
				}
			} else if _, task := in.Machine.Task(in.Target); !task {
				if _, gate := in.Machine.Gate(in.Target); !gate {
					return Result{}, fmt.Errorf("runner: terminal resume target %q is not a workflow state", in.Target)
				}
			}
		}

		resumed := journal.Event{
			Type:            journal.EventRunResumed,
			Status:          string(phase),
			Target:          in.Target,
			Actor:           in.Actor,
			Action:          in.Action,
			Gate:            in.Gate,
			Decision:        in.Decision,
			Rationale:       in.Rationale,
			Complete:        in.Complete,
			WorkflowVersion: id.WorkflowVersion,
			WorkflowDigest:  id.WorkflowDigest,
		}
		if in.Target == workflow.TargetJoin {
			resumed.Parallel, resumed.Branch, _ = interventionParallelContext(events, in.Machine, in.Gate)
		}
		if err := jr.Append(resumed); err != nil {
			return Result{}, fmt.Errorf("runner: journal terminal resume for run %q: %w", in.RunID, err)
		}
		if in.Complete {
			return r.finish(in.RunID, jr, journal.PhaseCompleted, "", 0)
		}

		return r.resumeOwned(ctx, ResumeInput{
			RunID: in.RunID, Machine: in.Machine, GooberDigest: in.GooberDigest, RepoRef: in.RepoRef,
		}, jr, registrar, dir)
	})
}

func validateTerminalGeneration(runID string, events []journal.Event, expected uint64) error {
	for i := len(events) - 1; i >= 0; i-- {
		if events[i].Type != journal.EventRunFinished {
			continue
		}
		if events[i].Seq != expected {
			return fmt.Errorf(
				"runner: run %q terminal sequence changed from %d to %d: %w",
				runID, expected, events[i].Seq, ErrTerminalGenerationChanged,
			)
		}
		return nil
	}
	return fmt.Errorf("runner: run %q has no terminal journal event", runID)
}

func (r *Runner) resumeOwned(ctx context.Context, in ResumeInput, jr *journal.Run, registrar SecretRegistrar, dir string) (result Result, retErr error) {
	rd, err := journal.OpenRead(dir)
	if err != nil {
		return Result{}, fmt.Errorf("runner: open run %q for resume: %w", in.RunID, err)
	}
	id, err := rd.Identity()
	if err != nil {
		return Result{}, fmt.Errorf("runner: read identity for run %q: %w", in.RunID, err)
	}
	// Terminal detection is event-log-first (#242): the on-disk state.json
	// checkpoint can lag a crash-fsynced run.finished event by the write
	// window inside Append (the event's own fsync, then the checkpoint
	// rename that follows it in the same call — a crash between the two
	// leaves state.json still claiming {running, <last stage/gate>}), so
	// trusting it directly for the terminal decision risks re-executing
	// side effects (a re-evaluated gate re-dispatching implement/open-pr)
	// or a duplicate NotifyEscalated call. Phase() reconstructs straight
	// from the event log — the source of truth — so it stays correct
	// regardless of whether state.json ever caught up, and a missing or
	// corrupt checkpoint no longer fails Resume outright.
	//
	// Checked BEFORE the WF-016 digest verification below (#520): a terminal
	// run is returned as-is, never re-walked, so a definition change cannot
	// affect it — and a run a refusal already aborted must short-circuit
	// here on any later Resume rather than re-refuse and journal a second
	// run.finished event onto a finished run.
	phase, err := rd.Phase()
	if err != nil {
		return Result{}, fmt.Errorf("runner: reconstruct phase for run %q: %w", in.RunID, err)
	}
	switch phase {
	case journal.PhaseCompleted, journal.PhaseAborted, journal.PhaseEscalated, journal.PhaseFailed:
		res := Result{Phase: phase}
		if err := r.FinalizeTerminal(in.RunID, phase); err != nil {
			return res, err
		}
		if in.HumanDecision != nil {
			return res, fmt.Errorf("runner: run %q is %s and no longer awaiting a human gate decision", in.RunID, phase)
		}
		return res, nil
	}

	events, err := rd.Events()
	if err != nil {
		return Result{}, fmt.Errorf("runner: read events for run %q: %w", in.RunID, err)
	}

	// Every run Start creates pins WorkflowDigest (run.go's journal.Create
	// call, always from in.Machine.Digest()) — an empty value here means the
	// pin itself is missing (a corrupted or pre-WF-016 run.yaml), which is
	// exactly the "resuming under a changed definition" risk WF-016 exists
	// to catch: refuse rather than silently skip verification (#112). A
	// refusal ends the run at the canonical PhaseFailed terminal (#520,
	// maintainer ruling) — see refuseResume.
	if id.WorkflowDigest == "" {
		return r.refuseResume(jr, in.RunID, "resume_refused_missing_digest",
			fmt.Sprintf("run %q has no pinned workflow digest, refusing to resume (WF-016)", in.RunID))
	}
	if in.Machine == nil {
		in.Machine, err = pinnedWorkflowMachine(rd, id)
		if err != nil {
			return r.refuseResume(jr, in.RunID, "resume_refused_pinned_definition",
				fmt.Sprintf("run %q cannot reconstruct pinned workflow digest %q: %v; refusing to resume (WF-016)", in.RunID, id.WorkflowDigest, err))
		}
	}
	if id.WorkflowDigest != in.Machine.Digest() {
		return r.refuseResume(jr, in.RunID, "resume_refused_digest_mismatch",
			fmt.Sprintf("run %q is pinned to workflow digest %q, cannot resume against %q (WF-016)", in.RunID, id.WorkflowDigest, in.Machine.Digest()))
	}

	if id.GooberDigest != "" && id.GooberDigest != in.GooberDigest {
		return r.refuseResume(jr, in.RunID, "resume_refused_goober_digest_mismatch",
			fmt.Sprintf("run %q is pinned to goober digest %q, cannot resume against %q (WF-016)", in.RunID, id.GooberDigest, in.GooberDigest))
	}
	if override, ok := latestActiveGateOverride(events); ok {
		switch override.Target {
		case journal.TargetComplete, workflow.TerminalComplete:
			return r.finish(in.RunID, jr, journal.PhaseCompleted, override.Gate, 0)
		case workflow.TargetAbort:
			return r.finish(in.RunID, jr, journal.PhaseAborted, override.Gate, 0)
		case workflow.TargetEscalate:
			return r.finish(in.RunID, jr, journal.PhaseEscalated, override.Gate, 0)
		}
	}
	if resumed, ok := latestRunResume(events); ok {
		if resumed.WorkflowVersion != id.WorkflowVersion || resumed.WorkflowDigest != id.WorkflowDigest {
			return r.refuseResume(jr, in.RunID, "resume_refused_intervention_pin_mismatch",
				fmt.Sprintf("run %q terminal-resume pin does not match run.yaml (WF-016)", in.RunID))
		}
		// Runner metadata is accepted only for journals emitted by the
		// pre-normative intervention implementation. New events always use the
		// top-level Complete field above.
		legacyComplete, _ := resumed.Runner["interventionComplete"].(bool)
		if resumed.Complete || legacyComplete {
			return r.finish(in.RunID, jr, journal.PhaseCompleted, "", 0)
		}
	}
	humanProgress := latestHumanGateProgress(events, in.Machine)
	if in.HumanDecision == nil && humanProgress.waiting {
		return Result{Phase: journal.PhaseRunning, FinalState: humanProgress.gate}, nil
	}
	if in.HumanDecision != nil {
		if humanProgress.decided {
			if in.HumanDecision.Gate != humanProgress.gate ||
				in.HumanDecision.PauseSeq != humanProgress.pauseSeq ||
				in.HumanDecision.Decision != humanProgress.decision {
				return Result{}, fmt.Errorf("runner: human gate %q pause %d already recorded decision %q", humanProgress.gate, humanProgress.pauseSeq, humanProgress.decision)
			}
		} else if !humanProgress.waiting {
			return Result{}, fmt.Errorf("runner: run %q is not awaiting a human gate decision", in.RunID)
		} else if in.HumanDecision.Gate != humanProgress.gate || in.HumanDecision.PauseSeq != humanProgress.pauseSeq {
			return Result{}, fmt.Errorf("runner: run %q is awaiting human gate %q pause %d, not %q pause %d",
				in.RunID, humanProgress.gate, humanProgress.pauseSeq, in.HumanDecision.Gate, in.HumanDecision.PauseSeq)
		}
		g, ok := in.Machine.Gate(humanProgress.gate)
		if !ok || g.Evaluator != apiv1.EvaluatorHuman {
			return Result{}, fmt.Errorf("runner: paused gate %q is not a human gate", humanProgress.gate)
		}
		if err := gate.ValidateHumanDecision(g, in.HumanDecision.Decision, in.HumanDecision.Actor); err != nil {
			return Result{}, fmt.Errorf("runner: %w", err)
		}
	}
	rerun, seedEvents, err := pendingRerun(events, in.Machine)
	if err != nil {
		return Result{}, fmt.Errorf("runner: restore pending stage rerun for run %q: %w", in.RunID, err)
	}
	if rerun == nil {
		seedEvents = events
	}

	// Reconstruct the walk-local state a live run only ever holds in memory —
	// pointers accumulated so far (#107) and the last finished stage's result
	// (#108), the subject a resumed gate needs. Both are exactly what a live
	// walk carries forward call-to-call within one process; a crash loses
	// that memory, so Resume rebuilds it from the journal every time.
	activeParallel, parallelStart := pendingParallel(seedEvents, in.Machine)
	parallelTransition := pendingParallelTransition(seedEvents, in.Machine)
	concurrentParallelResume := activeParallel != nil && activeParallel.spec.MaxConcurrentBranches > 1
	pointerEvents := seedEvents
	if activeParallel != nil {
		pointerEvents = seedEvents[:parallelStart]
	}
	ws := newWalkState(jr, StartInput{
		RunID:   in.RunID,
		Machine: in.Machine,
		RepoRef: in.RepoRef,
	}, registrar, "")
	ws.pointers = reconstructPointers(pointerEvents, in.Machine)
	ws.completed = reconstructStageOutputs(seedEvents, in.Machine)
	ws.visitedStages = stageVisitSeed(seedEvents)
	ws.parallel = activeParallel
	ws.fanIn = pendingFanIn(seedEvents, in.Machine)
	if activeParallel != nil {
		ws.parallelRootPointers = append([]apiv1.ContextPointer(nil), ws.pointers...)
		jr.SetBranchCursors(activeParallel.cursors())
		if current := activeParallel.current(); current != nil {
			jr.SetBranch(current.id)
		}
	}
	if humanProgress.waiting {
		ws.humanDecision = in.HumanDecision
	}
	lastStage, lastResult, hasLast := lastFinishedSubject(seedEvents)
	ws.lastStage, ws.lastResult = lastStage, lastResult
	ws.lastResult = discardToleratedFailureOutputs(in.Machine, lastStage, ws.lastResult)
	ws.workspaceBranch = lastWorkspaceBranch(seedEvents, in.Machine, r.branchNamespaceFor(id.Gaggle))
	ws.branchRecorded = hasRunBranchRef(events)
	segment, resumeTarget := currentRunSegment(events)
	segmentLastStage, _, hasSegmentLast := lastFinishedSubject(segment)

	// state.json's MachineState is a checked hint, not a requirement
	// (#242): read it when available, but a missing/corrupt checkpoint no
	// longer fails Resume. The fallback exploits the exact timing
	// state.json itself relies on — SetMachineState is only reassigned to
	// the NEXT state after the post-runTask transition decision (see the
	// SetMachineState-timing note below) — so at the instant of a crash
	// MachineState always still names the stage that just finished, i.e.
	// exactly lastStage. A run interrupted before its first
	// stage.finished (hasLast false) falls back to the machine's own
	// declared start state — the same state Start() itself begins at.
	var startState string
	if rerun != nil {
		startState = rerun.stage
	} else if st, serr := rd.State(); serr == nil {
		startState = st.MachineState
	}
	if startState == "" {
		if hasSegmentLast {
			startState = segmentLastStage
		} else if resumeTarget != "" {
			startState = resumeTarget
		} else if hasLast {
			startState = lastStage
		} else {
			startState = in.Machine.Def.Spec.Start
		}
	}
	if rerun == nil && ws.fanIn != nil {
		// parallel.finished is authoritative over a checkpoint that still names
		// the final branch stage from immediately before the fan-in completed.
		startState = ws.fanIn.spec.Join
	}
	if rerun == nil && parallelTransition != nil {
		startState = parallelTransition.target
	}
	if ws.parallel != nil {
		current := ws.parallel.current()
		switch {
		case current == nil:
			return Result{}, fmt.Errorf("runner: restore active parallel %q: no current branch", ws.parallel.spec.Name)
		case current.settled:
			startState = workflow.TargetJoin
		case startState == ws.parallel.spec.Name || !branchContainsState(in.Machine, current.start, startState):
			startState = current.machine
			if startState == "" {
				startState = current.start
			}
		}
	}
	if humanProgress.waiting || humanProgress.decided {
		// The event log is authoritative when gate.paused or gate.evaluated
		// was fsynced but the corresponding checkpoint was lost or stale.
		startState = humanProgress.gate
	}
	var completedGate *gate.Result
	resumedGateTransition := false
	if rerun == nil && !concurrentParallelResume {
		if retryTarget, pending := pendingRetryTarget(segment, in.Machine, lastStage, lastResult); pending {
			jr.SetMachineState(retryTarget)
			startState = retryTarget
			resumedGateTransition = true
		} else if g, isGate := in.Machine.Gate(startState); isGate {
			if g.Evaluator == apiv1.EvaluatorHuman {
				if evaluated, ok := latestCompletedGateEvaluation(segment, g.Name); ok {
					gr := gateResultFromEvent(evaluated)
					resumedGateTransition = true
					completedGate = &gr
				}
			} else {
				gr, retry, completed, rerr := resumeCompletedRetry(jr, segment, g, lastStage, lastResult)
				if rerr != nil {
					return Result{}, fmt.Errorf("runner: restore completed retry decision for gate %q: %w", g.Name, rerr)
				}
				if completed {
					resumedGateTransition = true
					if retry {
						startState = gr.Target
					} else {
						completedGate = &gr
					}
				}
			}
		}
	}
	if request, ok := stalledRequestFromContext(ctx); ok {
		return r.finishStalled(in.RunID, jr, startState, 0, request)
	}
	// The item snapshot is reconstructed before the finished-task replay below,
	// not after: taskOutcome's blocked arm (#544) hands it to the instance-level
	// Blocked handler, so a resumed run replaying a blocked finish must carry
	// the same item a live walk would have.
	item, err := resumeItem(rd, id)
	if err != nil {
		return Result{}, fmt.Errorf("runner: resume item snapshot for run %q: %w", in.RunID, err)
	}
	ws.in = StartInput{RunID: in.RunID, Machine: in.Machine, RepoRef: in.RepoRef, Item: item}
	if err := runcontrol.ValidatePinned(id.RunControls); err != nil {
		return Result{}, fmt.Errorf("runner: invalid pinned run controls: %w", err)
	}
	runControls, err := r.resolveRunControls(id.RunControls)
	if err != nil {
		return Result{}, fmt.Errorf("runner: resolve pinned run controls: %w", err)
	}
	if completedGate != nil {
		next, res, advance, gerr := r.gateTransition(ctx, ws, *completedGate)
		if gerr != nil {
			return res, gerr
		}
		if !advance {
			return res, nil
		}
		startState = next
	}

	var resume *resumeContext
	if t, isTask := in.Machine.Task(startState); isTask && !concurrentParallelResume {
		if attempt := interruptedAttempt(segment, startState); attempt > 0 {
			resume = &resumeContext{
				stage:                  startState,
				attempt:                attempt,
				class:                  startedAttemptClass(segment, startState, attempt),
				committedWorkOnInfra:   infraFailedAttemptCommittedWork(segment, startState, attempt),
				policyAttempts:         policyAttemptsBefore(segment, startState, attempt),
				infrastructureFailures: infrastructureFailuresBefore(segment, startState, attempt),
			}
		} else if attempt := recordedInterruptedAttempt(segment, startState); resumedGateTransition && attempt > 0 {
			resume = &resumeContext{
				stage:                  startState,
				attempt:                attempt,
				class:                  startedAttemptClass(segment, startState, attempt),
				recorded:               true,
				policyAttempts:         policyAttemptsBefore(segment, startState, attempt),
				infrastructureFailures: infrastructureFailuresBefore(segment, startState, attempt),
			}
		} else if rerun == nil && !resumedGateTransition && hasSegmentLast && segmentLastStage == startState {
			// state.json's machineState still names this task (walk's
			// SetMachineState timing: it's set BEFORE dispatch and not
			// reassigned until the transition decision after runTask
			// returns), but its last attempt already finished cleanly
			// before the crash — interruptedAttempt found nothing in
			// flight. Re-dispatching it now would silently re-run its side
			// effects (#107); instead apply the exact transition a live
			// walk would have taken right after runTask returned.
			replayedBranchOutcome := false
			if ws.parallel != nil {
				switch lastResult.Status {
				case apiv1.ResultFailure:
					if !t.ContinueOnError {
						if _, nextIsGate := in.Machine.Gate(t.Next); !nextIsGate {
							ws.parallel.markCurrentFailed()
							ws.lastResult.Outputs = nil
							startState = workflow.TargetJoin
							replayedBranchOutcome = true
						}
					}
				case apiv1.ResultNoWork:
					ws.parallel.markCurrentNoOutput()
					startState = workflow.TargetJoin
					replayedBranchOutcome = true
				}
			}
			if !replayedBranchOutcome {
				next, res, advance, terr := r.taskOutcome(ctx, ws, taskTransition{t, lastResult})
				if terr != nil {
					return res, terr
				}
				if !advance {
					return res, nil
				}
				startState = next
			}
		}
	}
	startIn := StartInput{
		RunID:        in.RunID,
		Machine:      in.Machine,
		GooberDigest: in.GooberDigest,
		Gaggle:       id.Gaggle,
		Trigger:      id.Trigger,
		RepoRef:      in.RepoRef,
		Item:         item,
		RunControls:  runControls,
		// RequiredCapabilities is intentionally nil on resume: a run only reaches
		// here after it already started (and therefore already cleared the #735
		// toolchain preflight in Start); re-verifying would probe the host again
		// for a decision the original dispatch already made.
	}
	ws.in = startIn
	ws.state = startState
	ws.resume = resume
	ws.rerun = rerun
	if in.RecoveryReason != "" {
		action := journal.RecoveryActionResumed
		detail := map[string]any{
			"kind":   journal.RunnerAnnotationRunRecovery,
			"reason": in.RecoveryReason,
			"action": action,
		}
		if resume != nil {
			action = journal.RecoveryActionRetried
			detail["action"] = action
			detail["stage"] = resume.stage
			detail["attempt"] = resume.attempt
			detail["attemptClass"] = string(journal.AttemptInfra)
		}
		if err := jr.Append(journal.Event{
			Type:   journal.EventRunnerAnnotation,
			Stage:  startState,
			Runner: detail,
		}); err != nil {
			return Result{}, fmt.Errorf("runner: journal automatic recovery for run %q: %w", in.RunID, err)
		}
	}
	_, err = r.acquirePinnedWorkspace(ctx, jr, &startIn)
	if err != nil {
		if interrupted, ok, interruptErr := r.finishStalledRequest(ctx, in.RunID, jr, startState, 0); ok {
			return interrupted, interruptErr
		}
		return Result{}, err
	}
	ws.in = startIn
	defer func() {
		if retErr != nil || result.Phase != "" && result.Phase != journal.PhaseRunning {
			retErr = errors.Join(retErr, r.releasePinnedWorkspace(in.RunID))
		}
	}()
	ctx, span := r.startRunSpan(ctx, startIn)
	defer span.End()
	setStalledAttemptContext(ctx)

	if parallelTransition != nil {
		var result Result
		var transitionErr error
		switch {
		case parallelTransition.task != nil:
			_, result, _, transitionErr = r.taskOutcome(ctx, ws, taskTransition{
				parallelTransition.task.task, parallelTransition.task.result,
			})
		case parallelTransition.gate != nil:
			ws.lastStage = parallelTransition.gate.lastStage
			ws.lastResult = parallelTransition.gate.lastResult
			_, result, _, transitionErr = r.gateTransition(ctx, ws, parallelTransition.gate.result)
		default:
			switch {
			case parallelTransition.aggregate && parallelTransition.target == workflow.TargetAbort:
				result, transitionErr = r.finish(in.RunID, jr, journal.PhaseAborted, parallelTransition.parallel, 0)
			case parallelTransition.aggregate && parallelTransition.target == workflow.TargetEscalate:
				result, transitionErr = r.finish(in.RunID, jr, journal.PhaseEscalated, parallelTransition.parallel, 0)
			case workflow.IsReservedAnyTarget(parallelTransition.target):
				return Result{}, fmt.Errorf("runner: restore parallel terminal %q: source branch outcome is missing", parallelTransition.target)
			}
		}
		aggregateTerminal := parallelTransition.aggregate &&
			(parallelTransition.target == workflow.TargetAbort || parallelTransition.target == workflow.TargetEscalate)
		if parallelTransition.task != nil || parallelTransition.gate != nil || aggregateTerminal {
			if transitionErr != nil {
				span.Fail(transitionErr)
				return result, transitionErr
			}
			completeRunSpan(span, result)
			return result, nil
		}
	}

	gateAttempts, gateDiffDigests := gateRepassSeed(segment), gateDiffSeed(segment)
	gateAttempts = resetRerunGateSeeds(in.Machine, rerun, gateAttempts, gateDiffDigests)
	ws.gateAttempts, ws.repassAttempts, ws.gateDiffDigests = gateAttempts, targetRepassSeed(segment), gateDiffDigests
	ws.infraGateAttempts = gateInfrastructureSeed(segment)
	ws.infraRepassAttempts = infrastructureTargetRepassSeed(segment)
	result, err = r.walk(ctx, ws)
	if err != nil {
		span.Fail(err)
		return result, err
	}
	completeRunSpan(span, result)
	return result, nil
}

type humanGateProgress struct {
	gate     string
	decision string
	pauseSeq uint64
	waiting  bool
	decided  bool
}

func latestHumanGateProgress(events []journal.Event, machine *workflow.Machine) humanGateProgress {
	var decidedGate, decision string
	for i := len(events) - 1; i >= 0; i-- {
		event := events[i]
		switch event.Type {
		case journal.EventGateEvaluated:
			g, ok := machine.Gate(event.Gate)
			if !ok || g.Evaluator != apiv1.EvaluatorHuman {
				return humanGateProgress{}
			}
			decidedGate, decision = g.Name, event.Verdict
		case journal.EventGatePaused:
			g, ok := machine.Gate(event.Gate)
			if !ok || g.Evaluator != apiv1.EvaluatorHuman {
				return humanGateProgress{}
			}
			if decidedGate != "" {
				if decidedGate != g.Name {
					return humanGateProgress{}
				}
				return humanGateProgress{gate: g.Name, decision: decision, pauseSeq: event.Seq, decided: true}
			}
			return humanGateProgress{gate: g.Name, pauseSeq: event.Seq, waiting: true}
		case journal.EventGateStarted,
			journal.EventStageStarted, journal.EventStageFinished,
			journal.EventRunResumed, journal.EventGateOverridden, journal.EventRunFinished:
			return humanGateProgress{}
		}
	}
	return humanGateProgress{}
}

func currentRunSegment(events []journal.Event) ([]journal.Event, string) {
	for i := len(events) - 1; i >= 0; i-- {
		if events[i].Type == journal.EventRunResumed || events[i].Type == journal.EventGateOverridden {
			return events[i+1:], events[i].Target
		}
	}
	return events, ""
}

func latestRunResume(events []journal.Event) (journal.Event, bool) {
	for i := len(events) - 1; i >= 0; i-- {
		if events[i].Type == journal.EventRunResumed || events[i].Type == journal.EventGateOverridden {
			return events[i], true
		}
	}
	return journal.Event{}, false
}

func latestActiveGateOverride(events []journal.Event) (journal.Event, bool) {
	for i := len(events) - 1; i >= 0; i-- {
		switch events[i].Type {
		case journal.EventGateOverridden:
			return events[i], true
		case journal.EventRunFinished:
			return journal.Event{}, false
		}
	}
	return journal.Event{}, false
}

// resumeCompletedRetry closes the crash window between gate.evaluated, the
// retry annotation, and the next machine-state checkpoint. The verdict's
// normative Target is the durable transition; a missing annotation is restored
// once, while an existing one proves the decision was already fully recorded.
func resumeCompletedRetry(jr *journal.Run, events []journal.Event, g apiv1.Gate, subjectStage string, subject apiv1.ResultEnvelope) (gate.Result, bool, bool, error) {
	evaluated, ok := latestCompletedGateEvaluation(events, g.Name)
	if !ok {
		return gate.Result{}, false, false, nil
	}
	result := gateResultFromEvent(evaluated)
	class, _, retryable := retryFailureClassForGateResult(g, subject, result.Outcome)
	if !retryable {
		return gate.Result{}, false, false, nil
	}
	if hasRetryDecisionAfter(events, evaluated) {
		if result.Outcome == gate.OutcomePass || result.Escalated {
			return result, false, true, nil
		}
		switch result.Target {
		case workflow.TargetAbort, workflow.TargetEscalate, workflow.TerminalComplete:
			return result, false, true, nil
		}
		jr.SetMachineState(result.Target)
		return result, true, true, nil
	}
	target, retry, err := routeRetryDecision(jr, result, subjectStage, subject, class, true)
	if err != nil {
		return gate.Result{}, false, false, err
	}
	if retry {
		result.Target = target
	}
	return result, retry, true, nil
}

func gateResultFromEvent(evaluated journal.Event) gate.Result {
	return gate.Result{
		Gate:      evaluated.Gate,
		Outcome:   evaluated.Verdict,
		Target:    evaluated.Target,
		Attempt:   gateRepassAttempt(evaluated),
		Escalated: evaluated.Escalated,
	}
}

func latestCompletedGateEvaluation(events []journal.Event, gateName string) (journal.Event, bool) {
	for i := len(events) - 1; i >= 0; i-- {
		e := events[i]
		if e.Type == journal.EventStageFinished {
			return journal.Event{}, false
		}
		if e.Gate != gateName {
			continue
		}
		switch e.Type {
		case journal.EventGateEvaluated:
			return e, true
		case journal.EventGatePaused, journal.EventGateStarted:
			return journal.Event{}, false
		}
	}
	return journal.Event{}, false
}

// pendingRetryTarget returns a retry annotation that has not yet been followed
// by a completed stage or another gate visit. It is independent of state.json:
// when a gate retries its own subject stage, the checkpoint already names that
// task, so gate-state-only recovery would replay the finished result instead of
// dispatching the recorded retry.
func pendingRetryTarget(events []journal.Event, machine *workflow.Machine, subjectStage string, subject apiv1.ResultEnvelope) (string, bool) {
	for i := len(events) - 1; i >= 0; i-- {
		e := events[i]
		switch e.Type {
		case journal.EventStageFinished:
			if isInterruptedAttemptMarker(e) {
				continue
			}
			return "", false
		case journal.EventGatePaused, journal.EventGateStarted, journal.EventGateEvaluated:
			return "", false
		case journal.EventRunnerAnnotation:
			if e.Runner["kind"] != retryDecisionKind || e.Stage != subjectStage {
				continue
			}
			g, ok := machine.Gate(e.Gate)
			if !ok {
				return "", false
			}
			outcome := ""
			if class, ok := e.Runner[retryFailureClassKey].(string); ok && class == string(journal.AttemptInfra) {
				outcome = gate.OutcomeInfra
			}
			if _, _, retryable := retryFailureClassForGateResult(g, subject, outcome); !retryable {
				return "", false
			}
			target, ok := e.Runner["target"].(string)
			if !ok || target == "" {
				return "", false
			}
			switch target {
			case workflow.TargetAbort, workflow.TargetEscalate, workflow.TerminalComplete:
				return "", false
			}
			return target, true
		}
	}
	return "", false
}

func hasRetryDecisionAfter(events []journal.Event, evaluated journal.Event) bool {
	for i := len(events) - 1; i >= 0 && events[i].Seq > evaluated.Seq; i-- {
		e := events[i]
		if e.Type == journal.EventRunnerAnnotation && e.Gate == evaluated.Gate &&
			e.Runner["kind"] == retryDecisionKind {
			return true
		}
	}
	return false
}

func gateRepassAttempt(e journal.Event) int {
	n, _ := e.Runner["repassAttempt"].(float64)
	return int(n)
}

// pinnedWorkflowMachine reconstructs the historical machine from the trusted,
// content-addressed Definition snapshot and verifies every identity boundary.
func pinnedWorkflowMachine(rd *journal.Reader, id journal.RunIdentity) (*workflow.Machine, error) {
	var definitionRef *journal.InputRef
	for i := range id.Inputs {
		if id.Inputs[i].Name == journal.PinnedWorkflowDefinitionInputName {
			definitionRef = &id.Inputs[i]
			break
		}
	}
	if definitionRef == nil {
		return nil, fmt.Errorf("immutable input %q is missing", journal.PinnedWorkflowDefinitionInputName)
	}
	if definitionRef.Integrity != apiv1.IntegrityTrusted {
		return nil, fmt.Errorf("immutable input %q has integrity %q, want %q",
			journal.PinnedWorkflowDefinitionInputName, definitionRef.Integrity, apiv1.IntegrityTrusted)
	}
	data, err := rd.ArtifactBytes(definitionRef.Ref)
	if err != nil {
		return nil, fmt.Errorf("read immutable input %q: %w", journal.PinnedWorkflowDefinitionInputName, err)
	}
	var def workflow.Definition
	if err := json.Unmarshal(data, &def); err != nil {
		return nil, fmt.Errorf("parse immutable input %q: %w", journal.PinnedWorkflowDefinitionInputName, err)
	}
	if def.Name != id.Workflow || def.Version != id.WorkflowVersion {
		return nil, fmt.Errorf(
			"immutable input %q identifies workflow %q version %d, want %q version %d",
			journal.PinnedWorkflowDefinitionInputName, def.Name, def.Version, id.Workflow, id.WorkflowVersion,
		)
	}
	digest, err := workflow.ComputeDigest(def)
	if err != nil {
		return nil, fmt.Errorf("digest immutable input %q: %w", journal.PinnedWorkflowDefinitionInputName, err)
	}
	if digest != id.WorkflowDigest {
		return nil, fmt.Errorf(
			"immutable input %q digest %q does not match run pin %q",
			journal.PinnedWorkflowDefinitionInputName, digest, id.WorkflowDigest,
		)
	}
	machine, err := workflow.Compile(def, workflow.WithPreviewFeatures(true))
	if err != nil {
		return nil, fmt.Errorf("compile immutable input %q: %w", journal.PinnedWorkflowDefinitionInputName, err)
	}
	if machine.Digest() != id.WorkflowDigest {
		return nil, fmt.Errorf(
			"compiled immutable input %q digest %q does not match run pin %q",
			journal.PinnedWorkflowDefinitionInputName, machine.Digest(), id.WorkflowDigest,
		)
	}
	return machine, nil
}

// refuseResume ends a run whose WF-016 resume verification failed at the
// canonical PhaseFailed terminal and releases its claim.
//
// Per the ruling, the WF-016 text must survive durably in two places: the
// run.finished event's own Error field (never a separate preceding error
// event — one canonical place to look) and, via Run.Append's reason
// tracking (internal/journal/run.go) mirroring that same Error.Message into
// state.json's Reason field, state.json itself. Grepping "WF-016" finds it
// either way.
//
// The success-path return here is deliberately (Result{PhaseFailed}, nil),
// not an error: the refusal has been fully handled, and a nil error is what
// makes the daemon's resume scan record the canonical phase as the run's
// status instead of a raw "error: ..." string.
func (r *Runner) refuseResume(jr *journal.Run, runID, code, msg string) (Result, error) {
	if outcome, takenOver := r.claimOwnerTerminalization(runID); takenOver {
		return outcome.result, outcome.err
	}
	if err := jr.Append(journal.Event{
		Type:   journal.EventRunFinished,
		Status: string(journal.PhaseFailed),
		Error:  &journal.ErrorDetail{Code: code, Message: msg},
	}); err != nil {
		return Result{}, fmt.Errorf("runner: %s (additionally failed to journal terminal refusal: %w)", msg, err)
	}
	// FailureCode/Message (issue #710) let the scheduler/daemon echo surface
	// the WF-016 refusal reason too, not just a bare "failed" — the same fix
	// as taskOutcome's business-failure arm and failTerminal, applied to this
	// third PhaseFailed producer. FailureStage stays empty: a resume-time
	// digest check isn't attributable to one stage.
	res := Result{Phase: journal.PhaseFailed, FailureCode: code, FailureMessage: boundFailureMessage(msg)}
	r.notifyTerminal(runID, journal.PhaseFailed, "")
	if err := r.FinalizeTerminal(runID, journal.PhaseFailed); err != nil {
		return res, fmt.Errorf("runner: %s (additionally failed to finalize terminal refusal: %w)", msg, err)
	}
	return res, nil
}

// lastFinishedSubject reconstructs the (stage, ResultEnvelope) pair a live
// walk's lastStage/lastResult holds at the moment of a crash — the most
// recent REAL stage.finished event in the journal (excluding the synthetic
// interrupted-attempt marker Resume itself writes, which
// is never a genuine subject: it always precedes a fresh attempt of the SAME
// task that finishes for real later, so scanning from the end naturally
// prefers that real finish once it exists). ok is false only for a run that
// has not finished any stage yet (crashed before its first stage.finished).
func lastFinishedSubject(events []journal.Event) (stage string, result apiv1.ResultEnvelope, ok bool) {
	for i := len(events) - 1; i >= 0; i-- {
		e := events[i]
		if e.Type != journal.EventStageFinished || isInterruptedAttemptMarker(e) {
			continue
		}
		var errInfo *apiv1.ErrorInfo
		if e.Error != nil {
			errInfo = &apiv1.ErrorInfo{Code: e.Error.Code, Message: e.Error.Message}
		}
		return e.Stage, apiv1.ResultEnvelope{
			Status:    apiv1.ResultStatus(e.Status),
			Outputs:   e.Outputs,
			Artifacts: artifactPointersFrom(e.Artifacts),
			Error:     errInfo,
		}, true
	}
	return "", apiv1.ResultEnvelope{}, false
}

// reconstructPointers rebuilds walk's pointers slice — the ContextPointers
// every downstream stage receives — from every REAL stage.finished event in
// the journal, mirroring the live path's unconditional `pointers =
// append(pointers, produced...)` right after every runTask call (regardless
// of the stage's business status), PLUS every gate.evaluated event that
// routed onward to a real stage with a journaled verdict artifact —
// mirroring the live path's `pointers = append(pointers,
// apiv1.ContextPointer{...gr.VerdictArtifact})` in walk (issue #412). The
// synthetic interrupted-attempt marker is excluded — it never carries real
// Artifacts (see lastFinishedSubject); a task revisited more than once
// (a gate looping back to it) contributes each visit's artifacts in order,
// exactly as the live path would. Events are walked in their journaled
// (chronological) order so a resumed run's pointers interleave stage
// artifacts and verdict pointers identically to how a live run would have
// accumulated them. machine identifies whether a completed parallel routed to
// its join; branches that routed to onFailure never expose their pointers.
func reconstructPointers(events []journal.Event, machine *workflow.Machine) []apiv1.ContextPointer {
	var out []apiv1.ContextPointer
	var branchNames map[int]string
	var branchPointers map[int][]apiv1.ContextPointer
	discardBranches := func() {
		branchNames = nil
		branchPointers = nil
	}
	flushBranches := func(order []journal.BranchOutcome) {
		for _, branch := range order {
			out = append(out, branchPointers[branch.Branch]...)
		}
		discardBranches()
	}
	record := func(branch int, pointers []apiv1.ContextPointer) {
		if branch <= 0 {
			out = append(out, pointers...)
			return
		}
		if branchPointers == nil {
			branchPointers = map[int][]apiv1.ContextPointer{}
		}
		for i := range pointers {
			pointers[i].Branch = branch
			pointers[i].BranchName = branchNames[branch]
		}
		branchPointers[branch] = append(branchPointers[branch], pointers...)
	}
	for _, e := range events {
		switch e.Type {
		case journal.EventParallelStarted:
			branchNames = map[int]string{}
			branchPointers = map[int][]apiv1.ContextPointer{}
			for _, branch := range e.Completeness {
				branchNames[branch.Branch] = branch.Name
			}
		case journal.EventBranchStarted:
			if branchNames == nil {
				branchNames = map[int]string{}
			}
			branchNames[e.Branch] = e.BranchName
		case journal.EventStageFinished:
			if isInterruptedAttemptMarker(e) {
				continue
			}
			record(e.Branch, contextPointersFor(e.Stage, artifactPointersFrom(e.Artifacts)))
		case journal.EventGateEvaluated:
			if e.Ref == nil {
				continue
			}
			switch e.Target {
			case workflow.TargetAbort, workflow.TargetEscalate, workflow.TerminalComplete:
				continue
			}
			record(e.Branch, []apiv1.ContextPointer{{
				Name:      e.Gate + ".verdict",
				Integrity: e.Ref.Integrity,
				Artifact: &apiv1.ArtifactPointer{
					Path: e.Ref.Path, Digest: e.Ref.Digest, Size: e.Ref.Size,
					MediaType: "application/json", Integrity: e.Ref.Integrity,
				},
			}})
		case journal.EventParallelFinished:
			spec, ok := machine.Parallel(e.Parallel)
			if ok && e.Target == spec.Join {
				flushBranches(e.Completeness)
			} else {
				discardBranches()
			}
		}
	}
	if len(branchPointers) > 0 {
		ids := make([]int, 0, len(branchPointers))
		for branch := range branchPointers {
			ids = append(ids, branch)
		}
		sort.Ints(ids)
		for _, branch := range ids {
			out = append(out, branchPointers[branch]...)
		}
	}
	return out
}

// pendingParallel rebuilds the in-memory execution state for the latest
// parallel.started that has not reached parallel.finished. A run.resumed event
// starts a new run segment, so prior branch state must not cross that boundary;
// run.finished alone does not, because RerunStage can reopen the branch.
func pendingParallel(events []journal.Event, machine *workflow.Machine) (*parallelExec, int) {
	start := -1
	for i, event := range events {
		switch event.Type {
		case journal.EventParallelStarted:
			start = i
		case journal.EventParallelFinished:
			if start >= 0 && event.Parallel == events[start].Parallel {
				start = -1
			}
		case journal.EventRunResumed:
			if event.Target == workflow.TargetJoin {
				if parallel, _, ok := interventionParallelContext(events[:i], machine, event.Gate); ok {
					for candidate := i - 1; candidate >= 0; candidate-- {
						if events[candidate].Type == journal.EventParallelStarted &&
							events[candidate].Parallel == parallel {
							start = candidate
							break
						}
					}
				}
			} else {
				start = -1
			}
		}
	}
	if start < 0 {
		return nil, -1
	}

	spec, ok := machine.Parallel(events[start].Parallel)
	if !ok {
		return nil, -1
	}
	par := newParallelExec(spec)
	branchByID := func(id int) (*branchState, int) {
		for i, branch := range par.branches {
			if branch.id == id {
				return branch, i
			}
		}
		return nil, -1
	}
	record := func(branch *branchState, outputs map[string]any, pointers []apiv1.ContextPointer) {
		if branch == nil {
			return
		}
		branch.pointers = append(branch.pointers, pointers...)
		for _, pointer := range pointers {
			if pointer.Artifact != nil {
				branch.artifacts++
			}
		}
		if len(outputs) > 0 || len(pointers) > 0 {
			branch.produced = true
		}
	}
	lastStage := map[int]journal.Event{}

	for _, event := range events[start+1:] {
		branch, branchIndex := branchByID(event.Branch)
		switch event.Type {
		case journal.EventBranchStarted:
			if event.Parallel != spec.Name || branch == nil {
				continue
			}
			par.active = branchIndex
			branch.machine = event.Stage
			branch.status = ""
			branch.started = true
			branch.settled = false
			// Reconstruct the ORIGINAL start instant so a resumed branch gets
			// its remaining branchTimeoutSeconds budget, not a fresh one.
			branch.startedAt = event.Time
		case journal.EventStageStarted:
			if branch != nil {
				branch.machine = event.Stage
			}
		case journal.EventStageFinished:
			if branch == nil || isInterruptedAttemptMarker(event) {
				continue
			}
			branch.machine = event.Stage
			outputs := event.Outputs
			task, taskKnown := machine.Task(event.Stage)
			if taskKnown && event.Status == string(apiv1.ResultFailure) && task.ContinueOnError {
				outputs = nil
			}
			record(branch, outputs, contextPointersFor(event.Stage, artifactPointersFrom(event.Artifacts)))
			lastStage[event.Branch] = event
			switch event.Status {
			case string(apiv1.ResultFailure):
				if taskKnown && !task.ContinueOnError {
					if _, nextIsGate := machine.Gate(task.Next); !nextIsGate {
						branch.failed = true
					}
				}
			case string(apiv1.ResultNoWork):
				branch.noOutput = true
			}
		case journal.EventGatePaused:
			if branch != nil {
				branch.machine = event.Gate
			}
		case journal.EventGateEvaluated:
			if branch == nil {
				continue
			}
			branch.machine = event.Gate
			switch event.Target {
			case workflow.TargetAbort, workflow.TargetEscalate, workflow.TerminalComplete:
			default:
				if event.Ref != nil {
					record(branch, nil, []apiv1.ContextPointer{{
						Name: event.Gate + ".verdict", Integrity: event.Ref.Integrity,
						Artifact: &apiv1.ArtifactPointer{
							Path: event.Ref.Path, Digest: event.Ref.Digest, Size: event.Ref.Size,
							MediaType: "application/json", Integrity: event.Ref.Integrity,
						},
					}})
				}
			}
			subject, hasSubject := lastStage[event.Branch]
			gateDef, gateKnown := machine.Gate(event.Gate)
			if event.Target == workflow.TargetJoin && hasSubject &&
				subject.Status == string(apiv1.ResultFailure) && gateKnown &&
				!gateClearsFailure(gateResultFromEvent(event), gateDef) {
				branch.failed = true
			}
		case journal.EventBranchFinished:
			if event.Parallel != spec.Name || branch == nil {
				continue
			}
			par.active = branchIndex
			branch.machine = ""
			branch.status = event.BranchStatus
			branch.settled = true
		case journal.EventRunResumed:
			if event.Target != workflow.TargetJoin || branch == nil {
				continue
			}
			par.active = branchIndex
			branch.machine = workflow.TargetJoin
			branch.status = ""
			branch.failed = false
			branch.noOutput = false
			branch.settled = false
			for i := branchIndex + 1; i < len(par.branches); i++ {
				if par.branches[i].status != journal.BranchCancelled {
					continue
				}
				par.branches[i].machine = par.branches[i].start
				par.branches[i].status = ""
				par.branches[i].failed = false
				par.branches[i].noOutput = false
				par.branches[i].started = false
				par.branches[i].settled = false
			}
		}
	}
	return par, start
}

func interventionParallelContext(events []journal.Event, machine *workflow.Machine, gateName string) (string, int, bool) {
	gateIndex := -1
	branch := 0
	for i := len(events) - 1; i >= 0; i-- {
		if events[i].Type == journal.EventGateEvaluated && events[i].Gate == gateName && events[i].Branch > 0 {
			gateIndex = i
			branch = events[i].Branch
			break
		}
	}
	if gateIndex < 0 {
		return "", 0, false
	}
	for i := gateIndex - 1; i >= 0; i-- {
		event := events[i]
		if event.Type != journal.EventParallelStarted {
			continue
		}
		spec, ok := machine.Parallel(event.Parallel)
		if !ok || branch > len(spec.Branches) ||
			!branchContainsState(machine, spec.Branches[branch-1].Start, gateName) {
			continue
		}
		return spec.Name, branch, true
	}
	return "", 0, false
}

func branchContainsState(machine *workflow.Machine, start, state string) bool {
	seen := map[string]bool{}
	stack := []string{start}
	for len(stack) > 0 {
		current := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if current == state {
			return true
		}
		if current == "" || workflow.IsReservedAnyTarget(current) || seen[current] || !machine.Has(current) {
			continue
		}
		seen[current] = true
		stack = append(stack, machine.Outgoing(current)...)
	}
	return false
}

func pendingFanIn(events []journal.Event, machine *workflow.Machine) *parallelExec {
	for i := len(events) - 1; i >= 0; i-- {
		event := events[i]
		if event.Type != journal.EventParallelFinished {
			continue
		}
		spec, ok := machine.Parallel(event.Parallel)
		if !ok || event.Target != spec.Join {
			continue
		}
		for _, later := range events[i+1:] {
			stageFinished := later.Type == journal.EventStageFinished && later.Stage == spec.Join && !isInterruptedAttemptMarker(later)
			gateFinished := later.Type == journal.EventGateEvaluated && later.Gate == spec.Join
			if stageFinished || gateFinished {
				return nil
			}
		}
		return fanInFromFinished(spec, event)
	}
	return nil
}

type parallelResumeTransition struct {
	parallel  string
	target    string
	aggregate bool
	task      *parallelTaskTerminal
	gate      *parallelGateTerminal
}

// pendingParallelTransition restores the root transition after a crash between
// parallel.finished and the next root dispatch or terminal event.
func pendingParallelTransition(events []journal.Event, machine *workflow.Machine) *parallelResumeTransition {
	finished := -1
	for i := len(events) - 1; i >= 0; i-- {
		event := events[i]
		switch event.Type {
		case journal.EventRunFinished, journal.EventRunResumed, journal.EventStageRerunRequested:
			return nil
		case journal.EventStageStarted, journal.EventStageFinished,
			journal.EventGatePaused, journal.EventGateStarted, journal.EventGateEvaluated,
			journal.EventParallelStarted:
			if event.Branch == 0 {
				return nil
			}
		case journal.EventParallelFinished:
			if event.Branch == 0 {
				finished = i
			}
		}
		if finished >= 0 {
			break
		}
	}
	if finished < 0 {
		return nil
	}

	event := events[finished]
	spec, ok := machine.Parallel(event.Parallel)
	if !ok || event.Target == spec.Join {
		return nil
	}
	transition := &parallelResumeTransition{
		parallel: event.Parallel, target: event.Target, aggregate: event.Target == spec.OnFailure,
	}
	if event.Target != workflow.TargetAbort && event.Target != workflow.TargetEscalate {
		return transition
	}

	for branch := 1; branch <= len(spec.Branches); branch++ {
		history := parallelBranchEvents(events[:finished], event.Parallel, branch)
		target, task, gate := parallelBranchTerminal(history, machine)
		if target == event.Target {
			transition.task = task
			transition.gate = gate
			transition.aggregate = false
			break
		}
	}
	return transition
}

// rerunFanIn restores the fan-in associated with an explicitly rerun join;
// earlier completed attempts of that join do not consume its branch state.
func rerunFanIn(events []journal.Event, machine *workflow.Machine, stage string) *parallelExec {
	for i := len(events) - 1; i >= 0; i-- {
		event := events[i]
		if event.Type != journal.EventParallelFinished || event.Target != stage {
			continue
		}
		spec, ok := machine.Parallel(event.Parallel)
		if ok && spec.Join == stage {
			return fanInFromFinished(spec, event)
		}
	}
	return pendingFanIn(events, machine)
}

func fanInFromFinished(spec apiv1.Parallel, event journal.Event) *parallelExec {
	fanIn := newParallelExec(spec)
	for _, outcome := range event.Completeness {
		branch := fanIn.branch(outcome.Name)
		if branch == nil {
			continue
		}
		branch.status = outcome.Status
		branch.artifacts = outcome.Artifacts
		branch.settled = true
	}
	return fanIn
}

// lastWorkspaceBranch rebuilds walk's run-scoped workspace-branch binding
// (#392, WorkspaceBranchOutput) from the journal — the newest real
// stage.finished event that actually emitted the key wins, mirroring the live
// walk's "sticky, last non-empty emission" accumulation. Without this, a crash
// anywhere after the rebinding stage would resume the rest of the chain
// against the run's DEFAULT branch — for pr-remediation, a pristine branch off
// main instead of the PR being remediated, which would silently discard the
// rebase and hand the reviewer somebody else's diff. Returns "" when no stage
// ever rebound (every workflow but pr-remediation today), which is exactly the
// zero value a fresh walk starts from.
//
// machine is consulted to apply the SAME deterministic-producer restriction the
// live path enforces (rebindWorkspaceBranch). A stage.finished event records
// outputs but not the producing task's type, so without the lookup an agentic
// stage's model-authored `workspaceBranch` would be ignored while running and
// then silently honored on resume — the security property would hold only until
// the first crash. An event naming a stage the machine does not have (a
// definition edit is refused upstream by the WF-016 digest pin, so this is
// vestigial) is ignored for the same fail-closed reason.
// nsPrefix is the run's gaggle-resolved run-branch namespace root, applied by
// rebindWorkspaceBranch to reject an out-of-namespace emission exactly as the
// live walk does.
func lastWorkspaceBranch(events []journal.Event, machine *workflow.Machine, nsPrefix string) string {
	for i := len(events) - 1; i >= 0; i-- {
		e := events[i]
		if e.Type != journal.EventStageFinished || isInterruptedAttemptMarker(e) {
			continue
		}
		t, ok := machine.Task(e.Stage)
		if !ok {
			continue
		}
		if e.Status == string(apiv1.ResultFailure) && t.ContinueOnError {
			continue
		}
		if b := rebindWorkspaceBranch(t, apiv1.ResultEnvelope{Outputs: e.Outputs}, nsPrefix); b != "" {
			return b
		}
	}
	return ""
}

// RestoredWorkspaceBranch returns the sticky branch binding Resume reconstructs
// from a run journal. Startup retention uses the same reconstruction so it
// cannot prune a branch before the interrupted run resumes against it.
func RestoredWorkspaceBranch(events []journal.Event, machine *workflow.Machine, nsPrefix string) string {
	return lastWorkspaceBranch(events, machine, nsPrefix)
}

func isInterruptedAttemptMarker(e journal.Event) bool {
	marker, ok := e.Runner[interruptedAttemptMarkerKey].(bool)
	return e.Type == journal.EventStageFinished && ok && marker
}

// gateRepassSeed reconstructs internal/gate.Evaluator.Attempts from the
// journal's event log. gate.started carries the prospective count before
// dispatch, so a dangling marker charges a crash-interrupted evaluation to the
// budget. A following gate.evaluated replaces it with the actual post-outcome
// count (including a pass reset to 0). Thus the last marker or verdict per gate
// is exactly the count at interruption. Returns nil (Evaluator's nil-safe zero
// value) if the run never started a gate evaluation.
func gateRepassSeed(events []journal.Event) map[string]int {
	var seed map[string]int
	for _, e := range events {
		if e.Type != journal.EventGateStarted && e.Type != journal.EventGateEvaluated {
			continue
		}
		if e.Type == journal.EventGateEvaluated && e.Verdict == gate.OutcomeInfra {
			if seed == nil {
				seed = make(map[string]int)
			}
			seed[e.Gate] = 0
			continue
		}
		n, ok := e.Runner["gateAttempt"].(float64)
		if !ok {
			n, ok = e.Runner["repassAttempt"].(float64)
		}
		if !ok {
			continue
		}
		if seed == nil {
			seed = make(map[string]int)
		}
		seed[e.Gate] = int(n)
	}
	return seed
}

func gateInfrastructureSeed(events []journal.Event) map[string]int {
	var seed map[string]int
	for _, e := range events {
		if e.Type != journal.EventGateEvaluated {
			continue
		}
		if seed == nil {
			seed = make(map[string]int)
		}
		if e.Verdict != gate.OutcomeInfra {
			seed[e.Gate] = 0
			continue
		}
		n, ok := e.Runner["gateAttempt"].(float64)
		if !ok {
			n, ok = e.Runner["repassAttempt"].(float64)
		}
		if ok {
			seed[e.Gate] = int(n)
		}
	}
	return seed
}

// targetRepassSeed reconstructs the cumulative repass count for each target
// stage. repassTarget preserves the configured branch when an exhausted
// evaluation was instead routed to escalation.
func targetRepassSeed(events []journal.Event) map[string]int {
	var seed map[string]int
	for _, e := range events {
		if e.Type != journal.EventGateEvaluated || e.Verdict == gate.OutcomeInfra {
			continue
		}
		target, _ := e.Runner["repassTarget"].(string)
		if target == "" && e.Verdict != gate.OutcomePass && !e.Escalated {
			target = e.Target
		}
		n, ok := e.Runner["repassAttempt"].(float64)
		if target == "" || !ok {
			continue
		}
		if seed == nil {
			seed = make(map[string]int)
		}
		if int(n) > seed[target] {
			seed[target] = int(n)
		}
	}
	return seed
}

func infrastructureTargetRepassSeed(events []journal.Event) map[string]int {
	var seed map[string]int
	gateTargets := make(map[string]string)
	for _, e := range events {
		if e.Type != journal.EventGateEvaluated {
			continue
		}
		if e.Verdict != gate.OutcomeInfra {
			if target := gateTargets[e.Gate]; target != "" && seed != nil {
				seed[target] = 0
			}
			continue
		}
		target, _ := e.Runner["repassTarget"].(string)
		if target == "" && !e.Escalated {
			target = e.Target
		}
		n, ok := e.Runner["repassAttempt"].(float64)
		if target == "" || !ok {
			continue
		}
		if seed == nil {
			seed = make(map[string]int)
		}
		gateTargets[e.Gate] = target
		if int(n) > seed[target] {
			seed[target] = int(n)
		}
	}
	return seed
}

func stageVisitSeed(events []journal.Event) map[string]bool {
	seed := make(map[string]bool)
	for _, e := range events {
		if e.Type == journal.EventStageFinished && e.Stage != "" {
			seed[e.Stage] = true
		}
	}
	return seed
}

// gateDiffSeed reconstructs internal/gate.Evaluator.LastDiffDigest from the
// journal's event log (issue #316), the same way gateRepassSeed reconstructs
// Attempts: each gate.evaluated event's Runner["diffDigest"] (recordVerdict,
// internal/gate/journal.go — only present when that attempt carried a
// non-empty diff) is that gate's last-known digest as of the moment it was
// journaled, so the LAST such event per gate name is the digest a resumed
// run must compare its next attempt against. A gate's events that carried no
// diff (automated/human gates, or an agentic gate with no committed change)
// have no "diffDigest" key and leave the prior seed entry untouched, exactly
// mirroring Evaluate's own "" -> no-op behavior on the live path. Returns nil
// (Evaluator's own nil-safe zero value) if the run never evaluated an
// agentic gate with a non-empty diff.
func gateDiffSeed(events []journal.Event) map[string]string {
	var seed map[string]string
	for _, e := range events {
		if e.Type != journal.EventGateEvaluated {
			continue
		}
		digest, ok := e.Runner["diffDigest"].(string)
		if !ok || digest == "" {
			continue
		}
		if seed == nil {
			seed = make(map[string]string)
		}
		seed[e.Gate] = digest
	}
	return seed
}

// interruptedAttempt reports the attempt number when stageName's latest
// attempt-boundary event is stage.started rather than stage.finished. Scanning
// backward, rather than comparing maximum attempt numbers, handles workflow
// loops that revisit a stage and restart numbering at attempt 1.
func interruptedAttempt(events []journal.Event, stageName string) int {
	for i := len(events) - 1; i >= 0; i-- {
		e := events[i]
		if e.Stage != stageName {
			continue
		}
		switch e.Type {
		case journal.EventStageStarted:
			return e.Attempt
		case journal.EventStageFinished:
			return 0
		}
	}
	return 0
}

func recordedInterruptedAttempt(events []journal.Event, stageName string) int {
	for i := len(events) - 1; i >= 0; i-- {
		e := events[i]
		if e.Stage != stageName {
			continue
		}
		if isInterruptedAttemptMarker(e) {
			return e.Attempt
		}
		switch e.Type {
		case journal.EventStageStarted, journal.EventStageFinished:
			return 0
		}
	}
	return 0
}

func startedAttemptClass(events []journal.Event, stageName string, attempt int) journal.AttemptClass {
	for i := len(events) - 1; i >= 0; i-- {
		event := events[i]
		if event.Type == journal.EventStageStarted && event.Stage == stageName && event.Attempt == attempt {
			return event.AttemptClass
		}
	}
	return ""
}

func infraFailedAttemptCommittedWork(events []journal.Event, stageName string, attempt int) bool {
	for i := len(events) - 1; i >= 0; i-- {
		event := events[i]
		if event.Type != journal.EventError || event.Stage != stageName || event.Attempt != attempt {
			continue
		}
		committed, _ := event.Runner[infraCommittedWorkKey].(bool)
		return event.Runner[retryFailureClassKey] == string(journal.AttemptInfra) && committed
	}
	return false
}

func policyAttemptsBefore(events []journal.Event, stageName string, interruptedAttempt int) int32 {
	var attempts int32
	for _, event := range events {
		if event.Type != journal.EventStageStarted ||
			event.Stage != stageName ||
			event.Attempt >= interruptedAttempt ||
			event.AttemptClass == journal.AttemptInfra {
			continue
		}
		attempts++
	}
	return attempts
}

func infrastructureFailuresBefore(events []journal.Event, stageName string, interruptedAttempt int) int32 {
	var failures int32
	for _, event := range events {
		if event.Type != journal.EventError ||
			event.Stage != stageName ||
			event.Attempt >= interruptedAttempt ||
			event.Runner[retryFailureClassKey] != string(journal.AttemptInfra) {
			continue
		}
		failures++
	}
	return failures
}

// resumeItem reconstructs the originating backlog item from its immutable
// input snapshot, if one was taken at Start (nil for a schedule/signal-
// triggered run with no originating item). Reuses Reader.ArtifactBytes for
// the digest-verified read — inputs/ and artifacts/ share the same
// path+digest Ref shape, just different directories.
func resumeItem(rd *journal.Reader, id journal.RunIdentity) (*apiv1.BacklogItem, error) {
	for _, ir := range id.Inputs {
		if ir.Name != "item" {
			continue
		}
		b, err := rd.ArtifactBytes(ir.Ref)
		if err != nil {
			return nil, err
		}
		var item apiv1.BacklogItem
		if err := json.Unmarshal(b, &item); err != nil {
			return nil, fmt.Errorf("unmarshal item snapshot: %w", err)
		}
		return &item, nil
	}
	return nil, nil
}
