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
	// under a changed definition is refused, not silently reinterpreted. A
	// refusal is itself terminal (#520): the run is journaled PhaseFailed
	// (with the WF-016 refusal text as the run.finished event's own error
	// detail) and finalized like any other terminal run, releasing its
	// claims — see refuseResume.
	Machine *workflow.Machine
	// RepoRef is the target repository every stage worktree branches from —
	// the same value originally passed to Start.
	RepoRef apiv1.RepoRef
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
// A gate-state resume evaluates against the REAL subject:
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
		return res, nil
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
	if id.WorkflowDigest != in.Machine.Digest() {
		return r.refuseResume(jr, in.RunID, "resume_refused_digest_mismatch",
			fmt.Sprintf("run %q is pinned to workflow digest %q, cannot resume against %q (WF-016)", in.RunID, id.WorkflowDigest, in.Machine.Digest()))
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
	seed.workspaceBranch = lastWorkspaceBranch(events, in.Machine)

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
	if st, serr := rd.State(); serr == nil {
		startState = st.MachineState
	}
	if startState == "" {
		if hasLast {
			startState = lastStage
		} else {
			startState = in.Machine.Def.Spec.Start
		}
	}
	// The item snapshot is reconstructed before the finished-task replay below,
	// not after: taskOutcome's blocked arm (#544) hands it to the instance-level
	// Blocked handler, so a resumed run replaying a blocked finish must carry
	// the same item a live walk would have.
	item, err := resumeItem(rd, id)
	if err != nil {
		return Result{}, fmt.Errorf("runner: resume item snapshot for run %q: %w", in.RunID, err)
	}

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
			next, res, advance, terr := r.taskOutcome(ctx, in.RunID, jr, in.Machine, in.RepoRef, item, t, lastResult, 0)
			if terr != nil {
				return res, terr
			}
			if !advance {
				return res, nil
			}
			startState = next
		}
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

	result, err := r.walk(ctx, jr, startIn, startState, resume, gateRepassSeed(events), gateDiffSeed(events), registrar, seed)
	if err != nil {
		span.Fail(err)
		return result, err
	}
	completeRunSpan(span, result)
	return result, nil
}

// refuseResume ends a run whose WF-016 resume verification failed at the
// canonical PhaseFailed terminal (maintainer ruling on #520, issue comment
// 2026-07-16T08:45:22Z): the run cannot proceed through no operator's or
// scheduler's live decision, so the aborted/cancelled family — which implies
// someone CHOSE to stop it — is the wrong phase; this is a failure of the
// run, even though the refusal itself is correct behavior WF-016 must keep.
// Landing on PhaseFailed also gets this path #498/#526's claim-release
// coverage with zero special-casing, instead of leaving the journal's phase
// "running" forever the way the pre-#520 bare-error return did — the live
// failure mode was a config-only daemon restart leaking one held claim per
// in-flight run.
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
// accumulated them.
func reconstructPointers(events []journal.Event) []apiv1.ContextPointer {
	var out []apiv1.ContextPointer
	for _, e := range events {
		switch e.Type {
		case journal.EventStageFinished:
			if isInterruptedAttemptMarker(e) {
				continue
			}
			out = append(out, contextPointersFor(e.Stage, artifactPointersFrom(e.Artifacts))...)
		case journal.EventGateEvaluated:
			if e.Ref == nil {
				continue
			}
			switch e.Target {
			case workflow.TargetAbort, workflow.TargetEscalate, workflow.TerminalComplete:
				continue
			}
			out = append(out, apiv1.ContextPointer{
				Name:     e.Gate + ".verdict",
				Artifact: &apiv1.ArtifactPointer{Path: e.Ref.Path, Digest: e.Ref.Digest, Size: e.Ref.Size, MediaType: "application/json"},
			})
		}
	}
	return out
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
func lastWorkspaceBranch(events []journal.Event, machine *workflow.Machine) string {
	for i := len(events) - 1; i >= 0; i-- {
		e := events[i]
		if e.Type != journal.EventStageFinished || isInterruptedAttemptMarker(e) {
			continue
		}
		t, ok := machine.Task(e.Stage)
		if !ok {
			continue
		}
		if b := rebindWorkspaceBranch(t, apiv1.ResultEnvelope{Outputs: e.Outputs}); b != "" {
			return b
		}
	}
	return ""
}

func isInterruptedAttemptMarker(e journal.Event) bool {
	return e.Type == journal.EventStageFinished &&
		e.AttemptClass == journal.AttemptInfra &&
		e.Error != nil &&
		e.Error.Code == interruptedAttemptErrorCode
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
