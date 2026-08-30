package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	gateevaluator "github.com/goobers/goobers/internal/gate"
	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/runner"
	"github.com/goobers/goobers/internal/workflow"
)

type runInterventionService struct {
	layout         instance.Layout
	definitions    *interventionDefinitionRegistry
	runnerRegistry *daemonRunnerRegistry
	instanceLog    *journal.InstanceLog
	errorLog       *log.Logger
	// hitl delivers operator intents to engine-driven runs (#3883). When it
	// is nil — a daemon with no engine client — engine-driven runs keep the
	// #3847 refusal exactly as they had it.
	hitl      atomic.Pointer[hitlDeliverer]
	scheduler atomic.Pointer[localscheduler.Scheduler]
	wg        *sync.WaitGroup
	activeMu  sync.Mutex
	active    map[string]struct{}
}

type interventionDefinitionSet struct {
	runners       map[string]*runner.Runner
	legacyRunner  *runner.Runner
	machines      map[localscheduler.WorkflowIdentity]*workflow.Machine
	gooberDigests map[localscheduler.WorkflowIdentity]string
	repoRefs      map[localscheduler.WorkflowIdentity]apiv1.RepoRef
}

type interventionDefinitionRegistry struct {
	current atomic.Pointer[interventionDefinitionSet]
}

func newInterventionDefinitionRegistry(definitions interventionDefinitionSet) *interventionDefinitionRegistry {
	registry := &interventionDefinitionRegistry{}
	registry.Replace(definitions)
	return registry
}

func (r *interventionDefinitionRegistry) Replace(definitions interventionDefinitionSet) {
	if r == nil {
		return
	}
	r.current.Store(&definitions)
}

func (r *interventionDefinitionRegistry) Snapshot() interventionDefinitionSet {
	if r == nil {
		return interventionDefinitionSet{}
	}
	definitions := r.current.Load()
	if definitions == nil {
		return interventionDefinitionSet{}
	}
	return *definitions
}

func interventionDefinitions(definitions *schedulerDefinitions, legacyRunner *runner.Runner) interventionDefinitionSet {
	return interventionDefinitionSet{
		runners:       definitions.Runners,
		legacyRunner:  legacyRunner,
		machines:      definitions.Machines,
		gooberDigests: definitions.GooberDigests,
		repoRefs:      definitions.RepoRefs,
	}
}

type resolvedInterventionRun struct {
	runID        string
	runner       *runner.Runner
	machine      *workflow.Machine
	gooberDigest string
	repoRef      apiv1.RepoRef
	runDir       string
	gaggle       string
	workflow     string
	phase        journal.RunPhase
	events       []journal.Event
	terminalSeq  uint64
	// engineDriven marks a run the Temporal engine owns. Its runner and
	// machine fields are deliberately nil: nothing on the runner path is
	// legitimate for it, and leaving them nil makes that unmistakable.
	engineDriven bool
	// generation is the engine's compare-and-set token — the number of
	// terminals the run has produced. It is the ExpectedTerminalGeneration an
	// operator intent must quote, and plays the role terminalSeq plays for
	// runner-driven runs.
	generation uint64
}

func newRunInterventionService(layout instance.Layout, setup *schedulerSetup, wg *sync.WaitGroup, errorLog *log.Logger) *runInterventionService {
	return &runInterventionService{
		layout:         layout,
		definitions:    setup.Interventions,
		runnerRegistry: setup.RunnerRegistry,
		instanceLog:    setup.InstanceLog,
		errorLog:       errorLog,
		wg:             wg,
	}
}

// AttachHITLDeliverer hands the service the engine-run delivery seam. It is
// attached after boot, once the engine client and its workflow-id resolver
// exist, for the same reason the scheduler is: the intervention service is
// constructed before either.
func (s *runInterventionService) AttachHITLDeliverer(deliverer hitlDeliverer) {
	if s == nil || deliverer == nil {
		return
	}
	s.hitl.Store(&deliverer)
}

// hitlDelivery returns the attached deliverer, or nil when this daemon has no
// engine client and engine-driven runs must keep the #3847 refusal.
func (s *runInterventionService) hitlDelivery() hitlDeliverer {
	if s == nil {
		return nil
	}
	deliverer := s.hitl.Load()
	if deliverer == nil {
		return nil
	}
	return *deliverer
}

func (s *runInterventionService) AttachScheduler(scheduler *localscheduler.Scheduler) {
	if s != nil {
		s.scheduler.Store(scheduler)
	}
}

func (s *runInterventionService) Approve(ctx context.Context, input httpapi.InterventionRequest) (httpapi.InterventionResult, error) {
	return s.approve(ctx, ctx, input, false)
}

func (s *runInterventionService) AcceptApprove(admission, execution context.Context, input httpapi.InterventionRequest) (httpapi.InterventionResult, error) {
	return s.approve(admission, execution, input, true)
}

func (s *runInterventionService) approve(admission, execution context.Context, input httpapi.InterventionRequest, background bool) (httpapi.InterventionResult, error) {
	resolved, err := s.resolve(input.RunID)
	if err != nil {
		return httpapi.InterventionResult{}, err
	}
	// An engine-driven run is answered by its workflow, not by the runner
	// machinery below. The branch is taken before replayIntervention because
	// deduplication for engine runs is the protocol's, keyed on the intent's
	// request id and settled durably inside the workflow — scanning this
	// daemon's journal snapshot for a runner-written marker would be both
	// wrong and racy against the workflow's own writer.
	if resolved.engineDriven {
		return s.deliverHITL(execution, hitlActionApprove, resolved, input)
	}
	if result, replayed, err := replayIntervention(resolved, "approve", input); replayed || err != nil {
		return result, err
	}
	decision := strings.TrimSpace(input.Decision)
	if decision == "" {
		decision = "pass"
	}
	gate, target, err := interventionBranch(resolved.machine, resolved.events, input.Stage, decision)
	if err != nil {
		return httpapi.InterventionResult{}, err
	}
	if gate.Evaluator == apiv1.EvaluatorHuman {
		if err := gateevaluator.ValidateHumanDecision(gate, decision, input.Actor); err != nil {
			return httpapi.InterventionResult{}, interventionForbidden("approval_forbidden", err.Error())
		}
	}

	switch resolved.phase {
	case journal.PhaseRunning:
		if gate.Evaluator != apiv1.EvaluatorHuman {
			return httpapi.InterventionResult{}, interventionConflict(
				"gate_not_approvable",
				fmt.Sprintf("gate %q is not awaiting human approval", input.Stage),
			)
		}
		pauseSeq, ok := unresolvedGatePause(resolved.events, input.Stage)
		if !ok {
			return httpapi.InterventionResult{}, interventionConflict(
				"gate_not_paused",
				fmt.Sprintf("run %q is not paused at gate %q", input.RunID, input.Stage),
			)
		}
		_, err = s.execute(admission, execution, background, resolved, true, "approve", input, func(ctx context.Context) (runner.Result, error) {
			return resolved.runner.Resume(ctx, runner.ResumeInput{
				RunID: input.RunID, Machine: resolved.machine, GooberDigest: resolved.gooberDigest, RepoRef: resolved.repoRef,
				HumanDecision: &runner.HumanGateDecision{
					Gate: input.Stage, PauseSeq: pauseSeq, Decision: decision, Actor: input.Actor,
				},
			})
		})
	case journal.PhaseEscalated, journal.PhaseFailed:
		if gate.Evaluator != apiv1.EvaluatorHuman && gate.Evaluator != apiv1.EvaluatorAgentic {
			return httpapi.InterventionResult{}, interventionConflict(
				"gate_not_approvable",
				fmt.Sprintf("gate %q is deterministic and cannot be approved by an operator", input.Stage),
			)
		}
		if !gateEvaluatedInCurrentSegment(resolved.events, input.Stage) {
			return httpapi.InterventionResult{}, interventionConflict(
				"gate_not_evaluated",
				fmt.Sprintf("gate %q was not evaluated in the current run segment", input.Stage),
			)
		}
		target, complete := interventionResumeTarget(target)
		_, err = s.execute(admission, execution, background, resolved, true, "approve", input, func(ctx context.Context) (runner.Result, error) {
			return resolved.runner.ResumeFromTerminal(ctx, runner.ResumeFromTerminalInput{
				RunID: input.RunID, Machine: resolved.machine, GooberDigest: resolved.gooberDigest, RepoRef: resolved.repoRef,
				Target: target, Complete: complete,
				Actor: input.Actor, Action: "approve", Gate: input.Stage, Decision: decision,
				ExpectedTerminalSeq: resolved.terminalSeq,
			})
		})
	default:
		return httpapi.InterventionResult{}, interventionConflict(
			"run_not_intervenable",
			fmt.Sprintf("run %q is %s and cannot be approved", input.RunID, resolved.phase),
		)
	}
	if err != nil {
		return httpapi.InterventionResult{}, interventionExecutionError("approve", err)
	}
	return interventionResult(resolved)
}

func (s *runInterventionService) Override(ctx context.Context, input httpapi.InterventionRequest) (httpapi.InterventionResult, error) {
	return s.override(ctx, ctx, input, false)
}

func (s *runInterventionService) AcceptOverride(admission, execution context.Context, input httpapi.InterventionRequest) (httpapi.InterventionResult, error) {
	return s.override(admission, execution, input, true)
}

func (s *runInterventionService) override(admission, execution context.Context, input httpapi.InterventionRequest, background bool) (httpapi.InterventionResult, error) {
	rationale := strings.TrimSpace(input.Rationale)
	if rationale == "" {
		return httpapi.InterventionResult{}, interventionBadRequest("rationale_required", "override rationale is required")
	}
	decision := strings.TrimSpace(input.Decision)
	if decision == "" {
		decision = "pass"
	}
	resolved, err := s.resolve(input.RunID)
	if err != nil {
		return httpapi.InterventionResult{}, err
	}
	if resolved.engineDriven {
		return s.deliverHITL(execution, hitlActionOverride, resolved, input)
	}
	if result, replayed, err := replayIntervention(resolved, "override", input); replayed || err != nil {
		return result, err
	}
	if resolved.phase != journal.PhaseEscalated && resolved.phase != journal.PhaseFailed {
		return httpapi.InterventionResult{}, interventionConflict(
			"run_not_escalated",
			fmt.Sprintf("run %q is %s; only escalated or failed runs can be overridden", input.RunID, resolved.phase),
		)
	}
	gate, target, err := interventionBranch(resolved.machine, resolved.events, input.Stage, decision)
	if err != nil {
		return httpapi.InterventionResult{}, err
	}
	if gate.Evaluator == apiv1.EvaluatorAutomated {
		return httpapi.InterventionResult{}, interventionConflict(
			"gate_not_overridable",
			fmt.Sprintf("gate %q is deterministic and cannot be overridden", input.Stage),
		)
	}
	if !gateEvaluatedInCurrentSegment(resolved.events, input.Stage) {
		return httpapi.InterventionResult{}, interventionConflict(
			"gate_not_evaluated",
			fmt.Sprintf("gate %q was not evaluated in the current run segment", input.Stage),
		)
	}
	target, complete := interventionResumeTarget(target)
	_, err = s.execute(admission, execution, background, resolved, true, "override", input, func(ctx context.Context) (runner.Result, error) {
		return resolved.runner.ResumeFromTerminal(ctx, runner.ResumeFromTerminalInput{
			RunID: input.RunID, Machine: resolved.machine, GooberDigest: resolved.gooberDigest, RepoRef: resolved.repoRef,
			Target: target, Complete: complete,
			Actor: input.Actor, Action: "override", Gate: input.Stage, Decision: decision, Rationale: rationale,
			ExpectedTerminalSeq: resolved.terminalSeq,
		})
	})
	if err != nil {
		return httpapi.InterventionResult{}, interventionExecutionError("override", err)
	}
	return interventionResult(resolved)
}

func (s *runInterventionService) RerunStage(ctx context.Context, input httpapi.InterventionRequest) (httpapi.InterventionResult, error) {
	return s.rerunStage(ctx, ctx, input, false)
}

func (s *runInterventionService) AcceptRerunStage(admission, execution context.Context, input httpapi.InterventionRequest) (httpapi.InterventionResult, error) {
	return s.rerunStage(admission, execution, input, true)
}

func (s *runInterventionService) rerunStage(admission, execution context.Context, input httpapi.InterventionRequest, background bool) (httpapi.InterventionResult, error) {
	addendum := strings.TrimSpace(input.InstructionAddendum)
	if addendum == "" {
		return httpapi.InterventionResult{}, interventionBadRequest("addendum_required", "instruction addendum is required")
	}
	resolved, err := s.resolve(input.RunID)
	if err != nil {
		return httpapi.InterventionResult{}, err
	}
	if resolved.engineDriven {
		return s.deliverHITL(execution, hitlActionRerun, resolved, input)
	}
	if result, replayed, err := replayIntervention(resolved, "rerun", input); replayed || err != nil {
		return result, err
	}
	if resolved.phase != journal.PhaseEscalated {
		return httpapi.InterventionResult{}, interventionConflict(
			"run_not_escalated",
			fmt.Sprintf("run %q is %s; only escalated runs can rerun a stage", input.RunID, resolved.phase),
		)
	}
	_, err = s.execute(admission, execution, background, resolved, true, "rerun", input, func(ctx context.Context) (runner.Result, error) {
		return resolved.runner.RerunStage(ctx, runner.RerunStageInput{
			RunID: input.RunID, Machine: resolved.machine, GooberDigest: resolved.gooberDigest, RepoRef: resolved.repoRef,
			Stage: input.Stage, Actor: input.Actor, InstructionAddendum: addendum,
			ExpectedTerminalSeq: resolved.terminalSeq,
		})
	})
	if err != nil {
		return httpapi.InterventionResult{}, interventionExecutionError("rerun stage", err)
	}
	return interventionResult(resolved)
}

// escalationResolutionMarker tags the HITL plane's deny resolution event: an
// escalated run resolved as "stays denied" keeps its terminal phase, so the
// resolution exists only as this journal record, appended by the run's own
// journal writer (journal.Recover — the same writer recordInterventionMarker
// uses). approve/redirect resolutions journal through the resume machinery
// instead and need no marker of their own.
const escalationResolutionMarker = "escalation.resolution"

// scanEscalationResolution looks for an escalation.resolution marker under
// key. replayed reports a marker whose payload fingerprint matches; a reused
// key with a different payload is refused.
func scanEscalationResolution(events []journal.Event, key, fingerprint string) (replayed bool, err error) {
	for _, event := range events {
		if event.Type != journal.EventRunnerAnnotation || event.Runner["kind"] != escalationResolutionMarker {
			continue
		}
		recordedKey, _ := event.Runner["idempotencyKey"].(string)
		if recordedKey != key {
			continue
		}
		recorded, _ := event.Runner["fingerprint"].(string)
		if recorded != fingerprint {
			return false, interventionConflict(
				"idempotency_key_reused",
				"Idempotency-Key was already used for a different resolution",
			)
		}
		return true, nil
	}
	return false, nil
}

// AcceptDenyEscalation resolves an escalated (or failed) run as denied: the
// escalation was reviewed and the run deliberately stays terminal. The
// resolution event — actor, rationale, idempotency key — is journaled; a
// replay of the same Idempotency-Key returns the current result without a
// second event, and a reused key with a different payload is refused.
func (s *runInterventionService) AcceptDenyEscalation(admission, execution context.Context, input httpapi.InterventionRequest) (httpapi.InterventionResult, error) {
	if err := admission.Err(); err != nil {
		return httpapi.InterventionResult{}, httpapi.NewInterventionError(
			http.StatusServiceUnavailable, "request_budget_exceeded", "the resolution was not accepted within the request budget", err,
		)
	}
	if err := execution.Err(); err != nil {
		return httpapi.InterventionResult{}, httpapi.NewInterventionError(
			http.StatusServiceUnavailable, "daemon_stopping", "the daemon is stopping", err,
		)
	}
	if input.IdempotencyKey == "" {
		return httpapi.InterventionResult{}, interventionBadRequest("idempotency_key_required", "Idempotency-Key is required")
	}
	rationale := strings.TrimSpace(input.Rationale)
	if rationale == "" {
		return httpapi.InterventionResult{}, interventionBadRequest("rationale_required", "deny rationale is required")
	}
	resolved, err := s.resolve(input.RunID)
	if err != nil {
		return httpapi.InterventionResult{}, err
	}
	if resolved.engineDriven {
		return s.deliverHITL(execution, hitlActionDeny, resolved, input)
	}
	return s.denyEscalation(resolved, input)
}

// denyEscalation is AcceptDenyEscalation after run resolution: scan for a
// replay, then journal the resolution under the run's active-intervention
// slot. Split out so the replay/append race is testable with a genuinely
// stale resolved snapshot.
func (s *runInterventionService) denyEscalation(resolved resolvedInterventionRun, input httpapi.InterventionRequest) (httpapi.InterventionResult, error) {
	rationale := strings.TrimSpace(input.Rationale)
	fingerprint, err := interventionFingerprint("deny", input)
	if err != nil {
		return httpapi.InterventionResult{}, fmt.Errorf("fingerprint escalation resolution: %w", err)
	}
	replayed, err := scanEscalationResolution(resolved.events, input.IdempotencyKey, fingerprint)
	if err != nil {
		return httpapi.InterventionResult{}, err
	}
	if replayed {
		return currentInterventionResult(resolved)
	}
	if resolved.phase != journal.PhaseEscalated && resolved.phase != journal.PhaseFailed {
		return httpapi.InterventionResult{}, interventionConflict(
			"run_not_escalated",
			fmt.Sprintf("run %q is %s; only escalated or failed runs can be denied", input.RunID, resolved.phase),
		)
	}
	releaseActive, exclusive := s.trackActiveIntervention(resolved.runID)
	if !exclusive {
		return httpapi.InterventionResult{}, interventionConflict("intervention_in_progress", "another intervention is already active for this run")
	}
	defer releaseActive()

	// Re-scan now that the slot is held (the recheck-under-writer pattern
	// ClaimNotificationDelivery demonstrates): the scan above ran on a journal
	// snapshot taken before the slot was acquired, so a concurrent same-key
	// deny may have appended the resolution in between — approve/override are
	// backstopped by ResumeFromTerminal's ExpectedTerminalSeq CAS, but deny's
	// only writer-side guard is this recheck.
	current, err := journal.OpenRead(resolved.runDir)
	if err == nil {
		var events []journal.Event
		if events, err = current.Events(); err == nil {
			replayed, err = scanEscalationResolution(events, input.IdempotencyKey, fingerprint)
		}
	}
	if err != nil {
		var interventionErr *httpapi.InterventionError
		if errors.As(err, &interventionErr) {
			return httpapi.InterventionResult{}, err
		}
		return httpapi.InterventionResult{}, httpapi.NewInterventionError(
			http.StatusInternalServerError, "escalation_failed", "escalation resolution could not be journaled",
			fmt.Errorf("re-scan run before journaling escalation resolution: %w", err),
		)
	}
	if replayed {
		return currentInterventionResult(resolved)
	}

	_, scrubber := journal.DefaultScrubber()
	run, _, err := journal.Recover(resolved.runDir, journal.WithScrubber(scrubber))
	if err != nil {
		return httpapi.InterventionResult{}, httpapi.NewInterventionError(
			http.StatusInternalServerError, "escalation_failed", "escalation resolution could not be journaled",
			fmt.Errorf("recover run to journal escalation resolution: %w", err),
		)
	}
	appendErr := run.Append(journal.Event{
		Type: journal.EventRunnerAnnotation,
		Runner: map[string]any{
			"kind":           escalationResolutionMarker,
			"resolution":     "deny",
			"idempotencyKey": input.IdempotencyKey,
			"fingerprint":    fingerprint,
			"actor":          input.Actor,
			"rationale":      rationale,
		},
	})
	closeErr := run.Close()
	if err := errors.Join(appendErr, closeErr); err != nil {
		return httpapi.InterventionResult{}, httpapi.NewInterventionError(
			http.StatusInternalServerError, "escalation_failed", "escalation resolution could not be journaled",
			fmt.Errorf("journal escalation resolution: %w", err),
		)
	}
	return currentInterventionResult(resolved)
}

func (s *runInterventionService) resolve(runID string) (resolvedInterventionRun, error) {
	if !apiv1.ValidRunID(runID) {
		return resolvedInterventionRun{}, interventionBadRequest("invalid_run_id", "run ID is invalid")
	}
	type candidate struct {
		gaggle string
		dir    string
		runner *runner.Runner
	}
	definitions := s.definitions.Snapshot()
	gaggles := make([]string, 0, len(definitions.runners))
	for gaggle := range definitions.runners {
		gaggles = append(gaggles, gaggle)
	}
	sort.Strings(gaggles)
	candidates := make([]candidate, 0, len(gaggles)+1)
	for _, gaggle := range gaggles {
		candidates = append(candidates, candidate{
			gaggle: gaggle,
			dir:    filepath.Join(s.layout.ForGaggle(gaggle).RunsDir(), runID),
			runner: definitions.runners[gaggle],
		})
	}
	if definitions.legacyRunner != nil {
		candidates = append(candidates, candidate{dir: filepath.Join(s.layout.RunsDir(), runID), runner: definitions.legacyRunner})
	}

	var found *candidate
	for i := range candidates {
		if _, err := os.Stat(filepath.Join(candidates[i].dir, "run.yaml")); err == nil {
			if found != nil {
				return resolvedInterventionRun{}, interventionConflict("ambiguous_run_id", "run ID exists in more than one gaggle")
			}
			found = &candidates[i]
		} else if !os.IsNotExist(err) {
			return resolvedInterventionRun{}, httpapi.NewInterventionError(
				http.StatusInternalServerError, "run_lookup_failed", "run could not be inspected", err,
			)
		}
	}
	if found == nil {
		return resolvedInterventionRun{}, httpapi.NewInterventionError(http.StatusNotFound, "run_not_found", "run was not found", nil)
	}
	reader, err := journal.OpenRead(found.dir)
	if err != nil {
		return resolvedInterventionRun{}, httpapi.NewInterventionError(
			http.StatusInternalServerError, "run_read_failed", "run journal could not be read", err,
		)
	}
	identity, err := reader.Identity()
	if err != nil {
		return resolvedInterventionRun{}, httpapi.NewInterventionError(
			http.StatusInternalServerError, "run_read_failed", "run identity could not be read", err,
		)
	}
	if found.gaggle != "" && identity.Gaggle != found.gaggle {
		return resolvedInterventionRun{}, httpapi.NewInterventionError(
			http.StatusInternalServerError, "run_identity_mismatch", "run identity does not match its runtime scope", nil,
		)
	}
	// Every intervention this service performs — approve, override, rerun,
	// deny — either calls Runner.Resume/ResumeFromTerminal or appends to the
	// run's journal directly. All four are wrong for a run the engine drives:
	// the runner resolved below has never executed a stage of it, and its
	// journal has a live writer on the other side of a Temporal workflow.
	//
	// #3883 gives those four verbs a second destination. An engine-driven run
	// resolves HERE and returns early, carrying only the facts an operator
	// intent needs — the run's identity and the terminal generation it is
	// being issued against. It deliberately does NOT resolve a runner, a
	// machine, or a goober digest: none of them may be touched for this run,
	// and a nil field is a louder guarantee of that than a comment.
	//
	// A daemon with no deliverer attached (no engine client configured) keeps
	// the #3847 refusal verbatim, so its behaviour is unchanged.
	if identity.EngineDriven() {
		if s.hitlDelivery() == nil {
			return resolvedInterventionRun{}, interventionConflict(
				"run_engine_driven",
				engineDrivenRefusal(identity.RunID, "an operator intervention").Error(),
			)
		}
		return s.resolveEngineDriven(runID, found.dir, identity.Gaggle, identity.Workflow, reader)
	}
	key := localscheduler.WorkflowIdentity{Gaggle: identity.Gaggle, Workflow: identity.Workflow}
	machine := definitions.machines[key]
	if machine == nil {
		return resolvedInterventionRun{}, interventionConflict(
			"workflow_unavailable",
			fmt.Sprintf("workflow %q for run %q is no longer available", identity.Workflow, runID),
		)
	}
	// Never reinterpret a historical run under the current workflow merely
	// because the name still matches (#3376, same rule as the daemon resume
	// scan's interruptedRunMachine): when the config drifted after this run
	// started, an intervention must act on the definition the run is pinned
	// to — otherwise a routine workflow edit turns an operator's approve into
	// a terminal WF-016 refusal that destroys the paused run. The pinned
	// snapshot is trusted and content-addressed, so this cannot resurrect a
	// tampered definition; if it is missing or invalid the current machine is
	// kept and the runner's WF-016 verification refuses exactly as before.
	if identity.WorkflowDigest != "" && machine.Digest() != identity.WorkflowDigest {
		if pinned, pinErr := runner.PinnedWorkflowMachine(reader, identity); pinErr == nil {
			machine = pinned
		}
	}
	runRunner, _ := s.runnerRegistry.Resolve(runID, identity.Gaggle, found.runner)
	if runRunner == nil {
		return resolvedInterventionRun{}, httpapi.NewInterventionError(
			http.StatusInternalServerError, "runner_unavailable", "run owner is unavailable", nil,
		)
	}
	phase, err := reader.Phase()
	if err != nil {
		return resolvedInterventionRun{}, httpapi.NewInterventionError(
			http.StatusInternalServerError, "run_read_failed", "run phase could not be read", err,
		)
	}
	events, err := reader.Events()
	if err != nil {
		return resolvedInterventionRun{}, httpapi.NewInterventionError(
			http.StatusInternalServerError, "run_read_failed", "run events could not be read", err,
		)
	}
	terminalSeq := uint64(0)
	if phase == journal.PhaseEscalated || phase == journal.PhaseFailed {
		terminalSeq = latestTerminalSequence(events)
		if terminalSeq == 0 {
			return resolvedInterventionRun{}, httpapi.NewInterventionError(
				http.StatusInternalServerError, "run_read_failed", "terminal run has no run.finished event", nil,
			)
		}
	}
	return resolvedInterventionRun{
		runID:        runID,
		runner:       runRunner,
		machine:      machine,
		gooberDigest: definitions.gooberDigests[key],
		repoRef:      definitions.repoRefs[key],
		runDir:       found.dir,
		gaggle:       identity.Gaggle,
		workflow:     identity.Workflow,
		phase:        phase,
		events:       events,
		terminalSeq:  terminalSeq,
	}, nil
}

// resolveEngineDriven builds the resolved run an operator intent is issued
// against. It reads phase and events for the SAME reason the runner path does
// — to report the run back to the operator and to compute the compare-and-set
// token — and for no other: every decision about whether the intent may land
// is the workflow's to make.
func (s *runInterventionService) resolveEngineDriven(runID, runDir, gaggle, workflowName string, reader *journal.Reader) (resolvedInterventionRun, error) {
	phase, err := reader.Phase()
	if err != nil {
		return resolvedInterventionRun{}, httpapi.NewInterventionError(
			http.StatusInternalServerError, "run_read_failed", "run phase could not be read", err,
		)
	}
	events, err := reader.Events()
	if err != nil {
		return resolvedInterventionRun{}, httpapi.NewInterventionError(
			http.StatusInternalServerError, "run_read_failed", "run events could not be read", err,
		)
	}
	return resolvedInterventionRun{
		runID:        runID,
		runDir:       runDir,
		gaggle:       gaggle,
		workflow:     workflowName,
		phase:        phase,
		events:       events,
		engineDriven: true,
		generation:   terminalGeneration(events),
	}, nil
}

func latestTerminalSequence(events []journal.Event) uint64 {
	for i := len(events) - 1; i >= 0; i-- {
		if events[i].Type == journal.EventRunFinished {
			return events[i].Seq
		}
	}
	return 0
}

type interventionExecutionLease struct {
	service          *runInterventionService
	scheduler        *localscheduler.Scheduler
	resolved         resolvedInterventionRun
	releaseAdmission func()
	releaseActive    func()
	untrack          func()
	reacquiredClaims bool
	retainAdmission  bool
	releaseRetained  bool
}

func (s *runInterventionService) execute(
	admission context.Context,
	execution context.Context,
	background bool,
	resolved resolvedInterventionRun,
	reacquireClaims bool,
	action string,
	input httpapi.InterventionRequest,
	run func(context.Context) (runner.Result, error),
) (runner.Result, error) {
	if err := admission.Err(); err != nil {
		return runner.Result{}, httpapi.NewInterventionError(
			http.StatusServiceUnavailable, "request_budget_exceeded", "the intervention was not accepted within the request budget", err,
		)
	}
	if err := execution.Err(); err != nil {
		return runner.Result{}, httpapi.NewInterventionError(
			http.StatusServiceUnavailable, "daemon_stopping", "the daemon is stopping", err,
		)
	}
	if background && input.IdempotencyKey == "" {
		return runner.Result{}, interventionBadRequest("idempotency_key_required", "Idempotency-Key is required")
	}
	lease, err := s.beginExecution(resolved, reacquireClaims)
	if err != nil {
		return runner.Result{}, err
	}
	if err := admission.Err(); err != nil {
		lease.Close()
		return runner.Result{}, httpapi.NewInterventionError(
			http.StatusServiceUnavailable, "request_budget_exceeded", "the intervention was not accepted within the request budget", err,
		)
	}
	if err := recordInterventionMarker(resolved, action, input); err != nil {
		lease.Close()
		return runner.Result{}, err
	}
	if background {
		if s.wg != nil {
			s.wg.Add(1)
		}
		go func() {
			if s.wg != nil {
				defer s.wg.Done()
			}
			if _, runErr := s.finishExecution(execution, lease, run); runErr != nil && s.errorLog != nil {
				s.errorLog.Printf("%s run intervention failed after acceptance: %v", action, runErr)
			}
		}()
		return runner.Result{}, nil
	}
	if s.wg != nil {
		s.wg.Add(1)
		defer s.wg.Done()
	}
	return s.finishExecution(execution, lease, run)
}

func (s *runInterventionService) finishExecution(
	ctx context.Context,
	lease *interventionExecutionLease,
	run func(context.Context) (runner.Result, error),
) (runner.Result, error) {
	defer lease.Close()
	result, runErr := run(ctx)
	phase := result.Phase
	if phase == "" {
		var phaseErr error
		phase, phaseErr = lease.phase()
		if phaseErr != nil {
			lease.retainAdmission = true
			return result, errors.Join(runErr, phaseErr)
		}
	}
	if !terminalInterventionPhase(phase) {
		lease.retainAdmission = true
	} else if !errors.Is(runErr, runner.ErrTerminalGenerationChanged) {
		lease.releaseRetained = true
	}
	if runErr != nil && terminalInterventionPhase(phase) {
		runErr = errors.Join(runErr, lease.releaseReacquiredClaims())
	}
	return result, runErr
}

func (s *runInterventionService) beginExecution(resolved resolvedInterventionRun, reacquireClaims bool) (*interventionExecutionLease, error) {
	releaseActive, exclusive := s.trackActiveIntervention(resolved.runID)
	if !exclusive {
		return nil, interventionConflict("intervention_in_progress", "another intervention is already active for this run")
	}
	lease := &interventionExecutionLease{
		service: s, resolved: resolved, releaseActive: releaseActive,
	}

	untrack, compatible := s.runnerRegistry.TrackCompatible(resolved.runID, resolved.runner)
	if !compatible {
		lease.Close()
		return nil, interventionConflict("run_owner_changed", "run ownership changed while the intervention was being accepted")
	}
	lease.untrack = untrack

	scheduler := s.scheduler.Load()
	if scheduler == nil {
		lease.Close()
		return nil, httpapi.NewInterventionError(
			http.StatusServiceUnavailable, "scheduler_unavailable", "run admission is not available", nil,
		)
	}
	lease.scheduler = scheduler
	release, admitted, reason := scheduler.ReserveContinuation(resolved.runID, resolved.gaggle, resolved.workflow)
	if !admitted {
		lease.Close()
		return nil, interventionConflict("run_not_admitted", "run could not reacquire workflow admission: "+reason)
	}
	lease.releaseAdmission = release

	if reacquireClaims {
		if err := s.reacquireClaims(resolved); err != nil {
			lease.Close()
			return nil, err
		}
		lease.reacquiredClaims = true
	}
	return lease, nil
}

func (s *runInterventionService) trackActiveIntervention(runID string) (func(), bool) {
	s.activeMu.Lock()
	if s.active == nil {
		s.active = make(map[string]struct{})
	}
	if _, exists := s.active[runID]; exists {
		s.activeMu.Unlock()
		return func() {}, false
	}
	s.active[runID] = struct{}{}
	s.activeMu.Unlock()

	var once sync.Once
	return func() {
		once.Do(func() {
			s.activeMu.Lock()
			delete(s.active, runID)
			s.activeMu.Unlock()
		})
	}, true
}

func (s *runInterventionService) interventionActive(runID string) bool {
	s.activeMu.Lock()
	defer s.activeMu.Unlock()
	_, active := s.active[runID]
	return active
}

func (l *interventionExecutionLease) Close() {
	if l == nil {
		return
	}
	if l.retainAdmission && l.scheduler != nil {
		l.scheduler.RetainContinuation(l.resolved.runID, l.resolved.workflow)
	}
	if l.releaseAdmission != nil {
		l.releaseAdmission()
		l.releaseAdmission = nil
	}
	if l.releaseRetained && l.scheduler != nil {
		l.scheduler.ReleaseRetainedContinuation(l.resolved.runID, l.resolved.workflow)
	}
	if l.untrack != nil {
		l.untrack()
		l.untrack = nil
	}
	if l.releaseActive != nil {
		l.releaseActive()
		l.releaseActive = nil
	}
}

func (l *interventionExecutionLease) phase() (journal.RunPhase, error) {
	reader, err := journal.OpenRead(l.resolved.runDir)
	if err != nil {
		return "", fmt.Errorf("inspect run after intervention: %w", err)
	}
	phase, err := reader.Phase()
	if err != nil {
		return "", fmt.Errorf("inspect run phase after intervention: %w", err)
	}
	return phase, nil
}

func terminalInterventionPhase(phase journal.RunPhase) bool {
	switch phase {
	case journal.PhaseCompleted, journal.PhaseFailed, journal.PhaseAborted, journal.PhaseEscalated:
		return true
	default:
		return false
	}
}

func (l *interventionExecutionLease) releaseReacquiredClaims() error {
	if l == nil || !l.reacquiredClaims {
		return nil
	}
	return releaseClaimsForRun(l.service.layout, l.service.instanceLog, l.resolved.runID)
}

func (s *runInterventionService) reacquireClaims(resolved resolvedInterventionRun) error {
	claims, err := s.claimHistory(resolved)
	if err != nil {
		return httpapi.NewInterventionError(
			http.StatusInternalServerError, "claim_history_failed", "run claim history could not be read", err,
		)
	}
	var acquired bool
	var holder string
	lockPath := filepath.Join(s.layout.SchedulerDir(), claimLockFileName)
	err = withClaimLockForRun(lockPath, claimLockOperationIntervention, resolved.gaggle, resolved.runID, func() error {
		ledger, err := localscheduler.OpenClaimLedger(
			filepath.Join(s.layout.SchedulerDir(), claimLedgerFileName),
			localscheduler.WithInstanceLog(s.instanceLog),
		)
		if err != nil {
			return err
		}
		acquired, holder, err = ledger.ReclaimAll(claims, resolved.runID, resolved.workflow, DefaultClaimLease)
		return err
	})
	if err != nil {
		return httpapi.NewInterventionError(
			http.StatusInternalServerError, "claim_reacquire_failed", "run claims could not be reacquired", err,
		)
	}
	if !acquired {
		return interventionConflict(
			"claim_unavailable",
			fmt.Sprintf("run claims are now held by run %q", holder),
		)
	}
	return nil
}

func (s *runInterventionService) claimHistory(resolved resolvedInterventionRun) ([]localscheduler.ClaimEntry, error) {
	ledger, err := localscheduler.OpenClaimLedger(filepath.Join(s.layout.SchedulerDir(), claimLedgerFileName))
	if err != nil {
		return nil, err
	}
	durable := ledger.HistoryForRun(resolved.runID)
	claims := make(map[string]localscheduler.ClaimEntry, len(durable))
	for _, entry := range durable {
		key := entry.Gaggle + "\x00" + entry.Provider + "\x00" + entry.ExternalID
		claims[key] = entry
	}

	events, err := journal.ReadInstanceLog(s.layout.SchedulerDir())
	if err != nil {
		if len(durable) > 0 {
			return durable, nil
		}
		return nil, err
	}
	for _, event := range events {
		if event.Type != journal.EventClaimAcquired || event.RunID != resolved.runID {
			continue
		}
		itemID := strings.TrimSpace(event.Name)
		if itemID == "" {
			return nil, errors.New("claim acquisition event has no item identity")
		}
		externalID, _ := event.Runner["claimExternalId"].(string)
		if externalID == "" {
			externalID = itemID
		}
		provider, _ := event.Runner["claimProvider"].(string)
		if provider == "" && event.Gaggle != "" {
			provider = string(resolved.repoRef.Provider)
		}
		entry := localscheduler.ClaimEntry{
			ItemID:     itemID,
			Gaggle:     event.Gaggle,
			Provider:   provider,
			ExternalID: externalID,
			RunID:      resolved.runID,
			Workflow:   resolved.workflow,
		}
		key := entry.Gaggle + "\x00" + entry.Provider + "\x00" + entry.ExternalID
		claims[key] = entry
	}
	result := make([]localscheduler.ClaimEntry, 0, len(claims))
	for _, entry := range claims {
		result = append(result, entry)
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Gaggle != result[j].Gaggle {
			return result[i].Gaggle < result[j].Gaggle
		}
		if result[i].Provider != result[j].Provider {
			return result[i].Provider < result[j].Provider
		}
		return result[i].ExternalID < result[j].ExternalID
	})
	return result, nil
}

func interventionBranch(machine *workflow.Machine, events []journal.Event, gateName, decision string) (apiv1.Gate, string, error) {
	gateName = strings.TrimSpace(gateName)
	if gateName == "" {
		return apiv1.Gate{}, "", interventionBadRequest("stage_required", "gate name is required")
	}
	gate, ok := machine.Gate(gateName)
	if !ok {
		return apiv1.Gate{}, "", interventionBadRequest(
			"gate_not_found",
			fmt.Sprintf("stage %q is not a gate in workflow %q", gateName, machine.Def.Name),
		)
	}
	target, ok := workflow.BranchTarget(gate, decision)
	if !ok {
		return gate, "", interventionBadRequest(
			"decision_not_found",
			fmt.Sprintf("gate %q has no %q decision branch", gateName, decision),
		)
	}
	if target == workflow.TerminalComplete {
		return gate, target, nil
	}
	if target == workflow.TargetJoin {
		if _, _, ok := interventionParallelContext(events, machine, gateName); !ok {
			return gate, "", interventionConflict(
				"branch_not_resumable",
				fmt.Sprintf("gate %q no longer has parallel branch context", gateName),
			)
		}
		return gate, target, nil
	}
	if workflow.IsReservedAnyTarget(target) || !machine.Has(target) {
		return gate, "", interventionConflict(
			"branch_not_resumable",
			fmt.Sprintf("gate %q decision %q does not continue at a workflow state", gateName, decision),
		)
	}
	return gate, target, nil
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
			!interventionBranchContainsState(machine, spec.Branches[branch-1].Start, gateName) {
			continue
		}
		return spec.Name, branch, true
	}
	return "", 0, false
}

func interventionBranchContainsState(machine *workflow.Machine, start, target string) bool {
	seen := make(map[string]bool)
	stack := []string{start}
	for len(stack) > 0 {
		state := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if state == target {
			return true
		}
		if state == "" || workflow.IsReservedAnyTarget(state) || seen[state] || !machine.Has(state) {
			continue
		}
		seen[state] = true
		stack = append(stack, machine.Outgoing(state)...)
	}
	return false
}

func interventionResumeTarget(target string) (string, bool) {
	if target == workflow.TerminalComplete {
		return "", true
	}
	return target, false
}

func unresolvedGatePause(events []journal.Event, gate string) (uint64, bool) {
	for i := len(events) - 1; i >= 0; i-- {
		event := events[i]
		if event.Gate != gate {
			continue
		}
		switch event.Type {
		case journal.EventGateEvaluated:
			return 0, false
		case journal.EventGatePaused:
			return event.Seq, true
		}
	}
	return 0, false
}

func gateEvaluatedInCurrentSegment(events []journal.Event, gate string) bool {
	for i := len(events) - 1; i >= 0; i-- {
		event := events[i]
		if event.Type == journal.EventRunResumed || event.Type == journal.EventStageRerunRequested {
			return false
		}
		if event.Type == journal.EventGateEvaluated && event.Gate == gate {
			return true
		}
	}
	return false
}

const interventionIdempotencyMarker = "intervention.idempotency"

func interventionFingerprint(action string, input httpapi.InterventionRequest) (string, error) {
	payload := struct {
		Action              string `json:"action"`
		RunID               string `json:"runId"`
		Stage               string `json:"stage"`
		Actor               string `json:"actor"`
		Decision            string `json:"decision"`
		Rationale           string `json:"rationale"`
		InstructionAddendum string `json:"instructionAddendum"`
	}{
		Action: action, RunID: input.RunID, Stage: input.Stage, Actor: input.Actor,
		Decision: input.Decision, Rationale: input.Rationale, InstructionAddendum: input.InstructionAddendum,
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("sha256:%x", sha256.Sum256(encoded)), nil
}

func recordInterventionMarker(resolved resolvedInterventionRun, action string, input httpapi.InterventionRequest) error {
	if input.IdempotencyKey == "" {
		return nil
	}
	for _, event := range resolved.events {
		if interventionMarkerKey(event) == input.IdempotencyKey {
			return nil
		}
	}
	fingerprint, err := interventionFingerprint(action, input)
	if err != nil {
		return fmt.Errorf("fingerprint intervention: %w", err)
	}
	_, scrubber := journal.DefaultScrubber()
	run, _, err := journal.Recover(resolved.runDir, journal.WithScrubber(scrubber))
	if err != nil {
		return fmt.Errorf("recover run to record intervention idempotency: %w", err)
	}
	defer func() { _ = run.Close() }()
	if err := run.Append(journal.Event{
		Type: journal.EventRunnerAnnotation,
		Runner: map[string]any{
			"kind":           interventionIdempotencyMarker,
			"idempotencyKey": input.IdempotencyKey,
			"fingerprint":    fingerprint,
			"action":         action,
			"stage":          input.Stage,
		},
	}); err != nil {
		return fmt.Errorf("journal intervention idempotency: %w", err)
	}
	return nil
}

func interventionMarkerKey(event journal.Event) string {
	if event.Type != journal.EventRunnerAnnotation || event.Runner["kind"] != interventionIdempotencyMarker {
		return ""
	}
	key, _ := event.Runner["idempotencyKey"].(string)
	return key
}

func replayIntervention(resolved resolvedInterventionRun, action string, input httpapi.InterventionRequest) (httpapi.InterventionResult, bool, error) {
	if input.IdempotencyKey == "" {
		return httpapi.InterventionResult{}, false, nil
	}
	fingerprint, err := interventionFingerprint(action, input)
	if err != nil {
		return httpapi.InterventionResult{}, false, fmt.Errorf("fingerprint intervention: %w", err)
	}
	for i, event := range resolved.events {
		if interventionMarkerKey(event) != input.IdempotencyKey {
			continue
		}
		recorded, _ := event.Runner["fingerprint"].(string)
		if recorded != fingerprint {
			return httpapi.InterventionResult{}, true, interventionConflict(
				"idempotency_key_reused",
				"Idempotency-Key was already used for a different intervention",
			)
		}
		for _, later := range resolved.events[i+1:] {
			if interventionCompleted(action, input.Stage, later) {
				result, resultErr := currentInterventionResult(resolved)
				return result, true, resultErr
			}
		}
		return httpapi.InterventionResult{}, false, nil
	}
	return httpapi.InterventionResult{}, false, nil
}

func interventionCompleted(action, stage string, event journal.Event) bool {
	switch action {
	case "rerun":
		return event.Type == journal.EventStageRerunRequested && event.Stage == stage
	case "override":
		return event.Type == journal.EventRunResumed && event.Action == action && event.Gate == stage
	case "approve":
		return (event.Type == journal.EventRunResumed && event.Action == action && event.Gate == stage) ||
			(event.Type == journal.EventGateEvaluated && event.Gate == stage)
	default:
		return false
	}
}

func interventionResult(resolved resolvedInterventionRun) (httpapi.InterventionResult, error) {
	return currentInterventionResult(resolved)
}

func currentInterventionResult(resolved resolvedInterventionRun) (httpapi.InterventionResult, error) {
	reader, err := journal.OpenRead(resolved.runDir)
	if err != nil {
		return httpapi.InterventionResult{}, httpapi.NewInterventionError(
			http.StatusInternalServerError, "run_read_failed", "intervention result could not be read", err,
		)
	}
	phase, err := reader.Phase()
	if err != nil {
		return httpapi.InterventionResult{}, httpapi.NewInterventionError(
			http.StatusInternalServerError, "run_read_failed", "intervention phase could not be read", err,
		)
	}
	state, err := reader.State()
	if err != nil {
		return httpapi.InterventionResult{}, httpapi.NewInterventionError(
			http.StatusInternalServerError, "run_read_failed", "intervention state could not be read", err,
		)
	}
	events, err := reader.Events()
	if err != nil || len(events) == 0 {
		return httpapi.InterventionResult{}, httpapi.NewInterventionError(
			http.StatusInternalServerError, "run_read_failed", "intervention journal position could not be read", err,
		)
	}
	return httpapi.InterventionResult{
		Phase: string(phase), State: state.MachineState, JournalSeq: events[len(events)-1].Seq,
	}, nil
}

func interventionBadRequest(code, message string) error {
	return httpapi.NewInterventionError(http.StatusBadRequest, code, message, nil)
}

func interventionConflict(code, message string) error {
	return httpapi.NewInterventionError(http.StatusConflict, code, message, nil)
}

func interventionForbidden(code, message string) error {
	return httpapi.NewInterventionError(http.StatusForbidden, code, message, nil)
}

func interventionExecutionError(action string, err error) error {
	var interventionErr *httpapi.InterventionError
	if errors.As(err, &interventionErr) {
		return err
	}
	if errors.Is(err, runner.ErrTerminalGenerationChanged) {
		return interventionConflict(
			"terminal_generation_changed",
			"the run reached a newer terminal segment before the intervention was applied",
		)
	}
	return httpapi.NewInterventionError(
		http.StatusInternalServerError,
		"intervention_failed",
		action+" failed while advancing the run",
		err,
	)
}
