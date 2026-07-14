package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/workflow"
)

// ResumeInput identifies an interrupted run to pick back up. Everything
// recoverable from the journal is read from it (Gaggle, Trigger, the
// snapshotted Item); RepoRef and Machine are NOT journaled — RunIdentity
// pins the workflow's name/version/digest for verification, but not the
// target repo or the compiled Machine object itself — so the caller (the
// daemon, which already holds this per-gaggle/workflow config) supplies them
// again, exactly as it did for the original Start.
type ResumeInput struct {
	// RunID selects the run directory under Config.RunsDir.
	RunID string
	// Machine is the compiled workflow (#9) this run was walking. Its digest
	// MUST match the run's pinned WorkflowDigest (WF-016) — resuming a run
	// under a changed definition is refused, not silently reinterpreted.
	Machine *workflow.Machine
	// RepoRef is the target repository every stage worktree branches from —
	// the same value originally passed to Start.
	RepoRef apiv1.RepoRef
}

// Resume reopens an interrupted run's journal (journal.Recover — replays the
// event log and repairs any torn final write left by a crash mid-append),
// verifies it is still pinned to Machine's exact digest, and continues the
// walk from state.json's checkpointed MachineState. A run already at a
// terminal phase returns that phase immediately without re-walking — Resume
// is safe to call on a run that turns out to have already finished.
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
// A gate-state resume has no equivalent in-flight signal to detect (a gate
// evaluation journals only its terminal gate.evaluated event, never a
// started/finished pair) — it always just re-evaluates fresh, but now
// against the REAL subject: lastFinishedSubject reconstructs the last
// finished stage's full result (status, outputs, artifacts — journaled on
// stage.finished for exactly this) instead of walk's in-memory-only
// lastStage/lastResult defaulting to a zero value (#108). Its bounded-repass
// counter (internal/gate.Evaluator.Attempts, #89) IS restored too:
// gateRepassSeed reconstructs it from each gate's last gate.evaluated event
// (Runner["repassAttempt"], recordVerdict in internal/gate/journal.go) — the
// same event log state.json itself is always reconstructable from — so a
// crash mid repass-loop cannot grant a gate extra passes beyond its budget.
func (r *Runner) Resume(ctx context.Context, in ResumeInput) (Result, error) {
	if in.RunID == "" {
		return Result{}, fmt.Errorf("runner: RunID is required")
	}
	if in.Machine == nil {
		return Result{}, fmt.Errorf("runner: Machine is required")
	}

	dir := filepath.Join(r.cfg.RunsDir, in.RunID)

	// A fresh registrar/scrubber per resume, exactly like Start — a run's
	// secrets have no business outliving one process's handling of it.
	registrar, scrubber := journal.DefaultScrubber()
	jr, _, err := journal.Recover(dir, journal.WithScrubber(scrubber))
	if err != nil {
		return Result{}, fmt.Errorf("runner: recover run %q: %w", in.RunID, err)
	}
	defer func() { _ = jr.Close() }()

	rd, err := journal.OpenRead(dir)
	if err != nil {
		return Result{}, fmt.Errorf("runner: open run %q for resume: %w", in.RunID, err)
	}
	id, err := rd.Identity()
	if err != nil {
		return Result{}, fmt.Errorf("runner: read identity for run %q: %w", in.RunID, err)
	}
	if id.WorkflowDigest != "" && id.WorkflowDigest != in.Machine.Digest() {
		return Result{}, fmt.Errorf("runner: run %q is pinned to workflow digest %q, cannot resume against %q (WF-016)", in.RunID, id.WorkflowDigest, in.Machine.Digest())
	}

	st, err := rd.State()
	if err != nil {
		return Result{}, fmt.Errorf("runner: read state.json for run %q: %w", in.RunID, err)
	}
	switch st.Phase {
	case journal.PhaseCompleted, journal.PhaseAborted, journal.PhaseEscalated, journal.PhaseFailed:
		return Result{Phase: st.Phase}, nil
	}
	if st.MachineState == "" {
		return Result{}, fmt.Errorf("runner: run %q has no checkpointed machine state to resume from", in.RunID)
	}

	events, err := rd.Events()
	if err != nil {
		return Result{}, fmt.Errorf("runner: read events for run %q: %w", in.RunID, err)
	}

	// Reconstruct the walk-local state a live run only ever holds in memory —
	// pointers accumulated so far (#107) and the last finished stage's result
	// (#108), the subject a resumed gate needs. Both are exactly what a live
	// walk carries forward call-to-call within one process; a crash loses
	// that memory, so Resume rebuilds it from the journal every time.
	seed := walkSeed{pointers: reconstructPointers(events)}
	lastStage, lastResult, hasLast := lastFinishedSubject(events)
	seed.lastStage, seed.lastResult = lastStage, lastResult

	startState := st.MachineState
	var resume *resumeContext
	if t, isTask := in.Machine.Task(startState); isTask {
		if attempt := interruptedAttempt(events, startState); attempt > 0 {
			resume = &resumeContext{stage: startState, attempt: attempt}
		} else if hasLast && lastStage == startState {
			// state.json's machineState still names this task (walk's
			// SetMachineState timing: it's set BEFORE dispatch and not
			// reassigned until the transition decision after runTask
			// returns), but its last attempt already finished cleanly
			// before the crash — interruptedAttempt found nothing in
			// flight. Re-dispatching it now would silently re-run its side
			// effects (#107); instead apply the exact transition a live
			// walk would have taken right after runTask returned.
			next, res, advance, terr := r.taskOutcome(jr, in.Machine, t, lastResult, 0)
			if terr != nil {
				return Result{}, terr
			}
			if !advance {
				return res, nil
			}
			startState = next
		}
	}

	item, err := resumeItem(rd, id)
	if err != nil {
		return Result{}, fmt.Errorf("runner: resume item snapshot for run %q: %w", in.RunID, err)
	}
	startIn := StartInput{
		RunID:   in.RunID,
		Machine: in.Machine,
		Gaggle:  id.Gaggle,
		Trigger: id.Trigger,
		RepoRef: in.RepoRef,
		Item:    item,
	}
	ctx, span := r.startRunSpan(ctx, startIn)
	defer span.End()

	result, err := r.walk(ctx, jr, startIn, startState, resume, gateRepassSeed(events), registrar, seed)
	if err != nil {
		span.Fail(err)
		return result, err
	}
	span.Succeed(string(result.Phase))
	return result, nil
}

// lastFinishedSubject reconstructs the (stage, ResultEnvelope) pair a live
// walk's lastStage/lastResult holds at the moment of a crash — the most
// recent REAL stage.finished event in the journal (excluding the
// infra-tagged interrupted-attempt marker Resume itself synthesizes, which
// is never a genuine subject: it always precedes a fresh attempt of the SAME
// task that finishes for real later, so scanning from the end naturally
// prefers that real finish once it exists). ok is false only for a run that
// has not finished any stage yet (crashed before its first stage.finished).
func lastFinishedSubject(events []journal.Event) (stage string, result apiv1.ResultEnvelope, ok bool) {
	for i := len(events) - 1; i >= 0; i-- {
		e := events[i]
		if e.Type != journal.EventStageFinished || e.AttemptClass == journal.AttemptInfra {
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
// of the stage's business status). The infra-tagged interrupted-attempt
// marker is excluded — it never carries real Artifacts (see
// lastFinishedSubject); a task revisited more than once (a gate looping back
// to it) contributes each visit's artifacts in order, exactly as the live
// path would.
func reconstructPointers(events []journal.Event) []apiv1.ContextPointer {
	var out []apiv1.ContextPointer
	for _, e := range events {
		if e.Type != journal.EventStageFinished || e.AttemptClass == journal.AttemptInfra {
			continue
		}
		out = append(out, contextPointersFor(e.Stage, artifactPointersFrom(e.Artifacts))...)
	}
	return out
}

// gateRepassSeed reconstructs internal/gate.Evaluator.Attempts from the
// journal's event log: each gate.evaluated event's Runner["repassAttempt"]
// (recordVerdict, internal/gate/journal.go) is exactly the count Attempts
// held for that gate right after the event was journaled, so the LAST such
// event per gate name is that gate's count as of the moment of interruption
// — a later "pass" event's repassAttempt is already 0, so no separate reset
// tracking is needed here. Returns nil (Evaluator's own nil-safe zero value)
// if the run never evaluated a gate.
func gateRepassSeed(events []journal.Event) map[string]int {
	var seed map[string]int
	for _, e := range events {
		if e.Type != journal.EventGateEvaluated {
			continue
		}
		n, ok := e.Runner["repassAttempt"].(float64)
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

// interruptedAttempt reports the attempt number of stageName's most recent
// stage.started event that has no matching stage.finished among events — the
// signature of a crash mid-attempt, since a graceful path (success, business
// failure/blocked, or a retry loop giving up) always journals a matching
// stage.finished before returning control to walk. Returns 0 if stageName's
// last attempt completed normally (or the stage was never started at all).
func interruptedAttempt(events []journal.Event, stageName string) int {
	started, finished := 0, 0
	for _, e := range events {
		if e.Stage != stageName {
			continue
		}
		switch e.Type {
		case journal.EventStageStarted:
			if e.Attempt > started {
				started = e.Attempt
			}
		case journal.EventStageFinished:
			if e.Attempt > finished {
				finished = e.Attempt
			}
		}
	}
	if started > finished {
		return started
	}
	return 0
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
