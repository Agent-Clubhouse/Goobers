package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/supportmatrix"
	"github.com/goobers/goobers/internal/workflow"
)

// StageRestartInputName names a trusted, immutable restart epoch snapshot. It
// is produced by human admission, never accepted as workflow-authored policy.
const StageRestartInputName = "operator-stage-restart"
const maxStageRestartBytes = 4 << 20

// StageRestartRequest selects retained human instructions and a source terminal
// generation. Provider, claims and active execution admission remain mandatory
// responsibilities of the trusted caller before publishing the continuation.
type StageRestartRequest struct {
	EpochID             string
	Stage               string
	PrincipalRef        string
	ExpectedTerminalSeq uint64
	GuidanceIDs         []string
	Rationale           string
}

// StageRestartPlan is a fully snapshotted continuation request. Its source
// branch/repository and verification callback must be supplied by admission.
type StageRestartPlan struct {
	Continuation            journal.ContinuationRequest
	Source                  journal.RunIdentity
	GuidanceDigest          string
	PreviousRepasses        map[string]int
	SourceWorkspaceRevision *apiv1.WorkspaceRevision
}

type stageRestartManifest struct {
	Version           int             `json:"version"`
	EpochID           string          `json:"epochId"`
	SourceRunID       string          `json:"sourceRunId"`
	SourceTerminalSeq uint64          `json:"sourceTerminalSeq"`
	WorkflowDigest    string          `json:"workflowDigest"`
	Stage             string          `json:"stage"`
	Actor             string          `json:"actor"`
	GuidanceIDs       []string        `json:"guidanceIds"`
	Guidance          string          `json:"guidance"`
	GuidanceDigest    string          `json:"guidanceDigest"`
	Rationale         string          `json:"rationale"`
	NextAttempt       int             `json:"nextAttempt"`
	History           []journal.Event `json:"history"`
	BudgetHistory     []journal.Event `json:"budgetHistory"`
	Upstream          []journal.Event `json:"upstream"`
}

// PrepareStageRestart snapshots the exact retained DSL 3.1 local execution and
// explicitly selected scrubbed guidance. It never writes either journal or
// chooses provider credentials. Settled child runs require a separate accepted
// parent-observation/workspace mapping and are refused here until that exists.
func PrepareStageRestart(reader *journal.Reader, machine *workflow.Machine, request StageRestartRequest, scrubber journal.Scrubber) (StageRestartPlan, error) {
	return prepareStageRestart(reader, machine, request, scrubber, false)
}

// PrepareChildStageRestart snapshots a generated child's prior context and
// selected guidance. This is preparation only: a trusted caller must admit its
// new linked epoch and workspace through the durable child queue before launch.
func PrepareChildStageRestart(reader *journal.Reader, machine *workflow.Machine, request StageRestartRequest, scrubber journal.Scrubber) (StageRestartPlan, error) {
	return prepareStageRestart(reader, machine, request, scrubber, true)
}

func prepareStageRestart(reader *journal.Reader, machine *workflow.Machine, request StageRestartRequest, scrubber journal.Scrubber, child bool) (StageRestartPlan, error) {
	if reader == nil || scrubber == nil {
		return StageRestartPlan{}, errors.New("restart requires retained journal and credential scrubber")
	}
	id, err := reader.Identity()
	if err != nil {
		return StageRestartPlan{}, err
	}
	if err := validateStageRestartIdentity(id, machine, request, child); err != nil {
		return StageRestartPlan{}, err
	}
	phase, err := reader.Phase()
	if err != nil {
		return StageRestartPlan{}, err
	}
	if phase != journal.PhaseEscalated && phase != journal.PhaseFailed && phase != journal.PhaseAborted {
		return StageRestartPlan{}, errors.New("restart requires a settled failed, escalated or aborted source")
	}
	events, err := reader.Events()
	if err != nil {
		return StageRestartPlan{}, err
	}
	if err := validateTerminalGeneration(id.RunID, events, request.ExpectedTerminalSeq); err != nil {
		return StageRestartPlan{}, err
	}
	isGate, err := validateRerunTarget(machine, request.Stage)
	if err != nil {
		return StageRestartPlan{}, err
	}
	upstream, err := rerunSeedEvents(events, request.Stage, isGate)
	if err != nil {
		return StageRestartPlan{}, err
	}
	previous, err := readStageRestartManifest(reader, id, machine)
	if err != nil {
		return StageRestartPlan{}, err
	}
	budgetHistory := restartHistory(events, id.RunID)
	if previous != nil {
		budgetHistory = append(resetRestartHistory(previous.BudgetHistory, machine, previous.Stage), budgetHistory...)
	}
	history := restartHistory(events, id.RunID)
	if previous != nil {
		history = append(append([]journal.Event(nil), previous.History...), history...)
		upstream = append(append([]journal.Event(nil), previous.Upstream...), restartHistory(upstream, id.RunID)...)
	} else {
		upstream = restartHistory(upstream, id.RunID)
	}
	sourceRevision, err := reconstructWorkspaceRevision(history, machine)
	if err != nil {
		return StageRestartPlan{}, err
	}
	guidance, err := selectedRestartGuidance(events, request.GuidanceIDs, scrubber)
	if err != nil {
		return StageRestartPlan{}, err
	}
	manifest := stageRestartManifest{Version: 1, EpochID: request.EpochID, SourceRunID: id.RunID, SourceTerminalSeq: request.ExpectedTerminalSeq, WorkflowDigest: id.WorkflowDigest, Stage: request.Stage, Actor: request.PrincipalRef, GuidanceIDs: slices.Clone(request.GuidanceIDs), Guidance: guidance, GuidanceDigest: journal.Digest([]byte(guidance)), Rationale: string(scrubber.Scrub([]byte(request.Rationale))), NextAttempt: nextRerunAttempt(history, request.Stage, isGate), History: history, BudgetHistory: budgetHistory, Upstream: upstream}
	raw, err := json.Marshal(manifest)
	if err != nil {
		return StageRestartPlan{}, err
	}
	if len(raw) > maxStageRestartBytes {
		return StageRestartPlan{}, errors.New("retained restart context exceeds 4 MiB")
	}
	inputs, grades, sources, err := restartInputs(reader, id)
	if err != nil {
		return StageRestartPlan{}, err
	}
	inputs[StageRestartInputName] = raw
	grades[StageRestartInputName] = apiv1.IntegrityTrusted
	sources[StageRestartInputName] = request.PrincipalRef
	pointers := retainedRestartPointers(id, inputs)
	for _, pointer := range reconstructPointers(events, machine) {
		pointer.RunID = id.RunID
		pointers = append(pointers, pointer)
	}
	if len(pointers) > 128 {
		return StageRestartPlan{}, errors.New("retained restart context exceeds 128 artifact pointers")
	}
	return StageRestartPlan{Source: id, SourceWorkspaceRevision: sourceRevision.DeepCopy(), GuidanceDigest: manifest.GuidanceDigest, PreviousRepasses: targetRepassSeed(budgetHistory), Continuation: journal.ContinuationRequest{RunID: request.EpochID, SourceRunID: id.RunID, ExpectedTerminalSeq: request.ExpectedTerminalSeq, Operator: request.PrincipalRef, Target: request.Stage, Inputs: inputs, InputIntegrity: grades, InputSource: sources, ContextPointers: pointers}}, nil
}

func validateStageRestartIdentity(id journal.RunIdentity, machine *workflow.Machine, request StageRestartRequest, child bool) error {
	if err := validateStageRestartTarget(id, machine, request.Stage, child); err != nil {
		return err
	}
	if !apiv1.ValidRunID(request.EpochID) || request.EpochID == id.RunID || request.ExpectedTerminalSeq == 0 || strings.TrimSpace(request.PrincipalRef) == "" || len(request.PrincipalRef) > 1024 || strings.TrimSpace(request.Rationale) == "" || len(request.Rationale) > 4096 {
		return errors.New("restart identity, actor, source generation and bounded rationale are required")
	}

	return nil
}

// ValidateStageRestartTarget explains whether the local runner can restore this
// settled target without changing its workflow or child custody identity.
func ValidateStageRestartTarget(id journal.RunIdentity, machine *workflow.Machine, stage string) error {
	return validateStageRestartTarget(id, machine, stage, false)
}

// ValidateChildStageRestartTarget validates the common restart target after
// a trusted adapter declares durable child custody support.
func ValidateChildStageRestartTarget(id journal.RunIdentity, machine *workflow.Machine, stage string) error {
	return validateStageRestartTarget(id, machine, stage, true)
}

func validateStageRestartTarget(id journal.RunIdentity, machine *workflow.Machine, stage string, child bool) error {
	if machine == nil || machine.Def.DSLVersion != supportmatrix.V31DSLVersion {
		return errors.New("stage restart requires the local DSL 3.1 preview runner")
	}
	if id.EngineDriven() || (!child && id.Child != nil) {
		return errors.New("this stage restart adapter does not support engine runs or settled generated children")
	}
	if child && (id.Child == nil || id.ValidateChildLineage() != nil) {
		return errors.New("child stage restart requires exact generated provenance")
	}
	if id.WorkflowDigest == "" || id.WorkflowDigest != machine.Digest() {
		return errors.New("stage restart requires the exact retained workflow pin")
	}
	for _, parallel := range machine.Def.Spec.Parallels {
		if branchOwningState(machine, parallel, stage) != "" || parallel.Join == stage {
			return errors.New("restart inside a parallel or at its fan-in requires branch restoration support")
		}
	}
	_, err := validateRerunTarget(machine, stage)
	return err
}

func selectedRestartGuidance(events []journal.Event, ids []string, scrubber journal.Scrubber) (string, error) {
	if len(ids) == 0 || len(ids) > 16 {
		return "", errors.New("select between one and 16 saved guidance records")
	}
	records := journal.ReplayOperatorMessages(events)
	seen := map[string]bool{}
	var text strings.Builder
	for _, id := range ids {
		if id == "" || seen[id] {
			return "", errors.New("selected guidance IDs must be nonempty and unique")
		}
		seen[id] = true
		found := false
		for _, record := range records {
			if record.Request.RequestID != id {
				continue
			}
			if record.Request.DeliveryMode != "shared-guidance" || record.Request.Content.Artifact != nil {
				return "", errors.New("selected record is not saved inline human guidance")
			}
			found = true
			text.WriteString("Human guidance from ")
			text.WriteString(record.Request.PrincipalRef)
			text.WriteString(":\n")
			text.WriteString(record.Request.Content.Text)
			text.WriteString("\n\n")
			break
		}
		if !found {
			return "", errors.New("selected guidance is not retained in the source run")
		}
	}
	out := string(scrubber.Scrub([]byte(text.String())))
	if len(out) > 64<<10 {
		return "", errors.New("selected guidance exceeds 64 KiB")
	}
	return out, nil
}

func restartInputs(reader *journal.Reader, id journal.RunIdentity) (map[string][]byte, map[string]apiv1.Integrity, map[string]string, error) {
	inputs := map[string][]byte{}
	grades := map[string]apiv1.Integrity{}
	sources := map[string]string{}
	total := 0
	for _, input := range id.Inputs {
		if input.Name == StageRestartInputName {
			continue
		}
		raw, err := reader.ArtifactBytesBounded(input.Ref, maxStageRestartBytes)
		if err != nil {
			return nil, nil, nil, err
		}
		total += len(raw)
		if total > maxStageRestartBytes {
			return nil, nil, nil, errors.New("retained restart inputs exceed 4 MiB")
		}
		inputs[input.Name] = raw
		grades[input.Name] = input.Integrity
		sources[input.Name] = input.Source
	}
	return inputs, grades, sources, nil
}

func restartHistory(events []journal.Event, runID string) []journal.Event {
	result := []journal.Event{}
	for _, event := range events {
		switch event.Type {
		case journal.EventStageStarted, journal.EventStageFinished, journal.EventGateStarted, journal.EventGateEvaluated, journal.EventRunnerAnnotation:
			result = append(result, journal.Event{Type: event.Type, RunID: runID, Stage: event.Stage, Gate: event.Gate, Attempt: event.Attempt, AttemptClass: event.AttemptClass, Status: event.Status, Verdict: event.Verdict, Target: event.Target, Escalated: event.Escalated, Integrity: event.Integrity, Outputs: event.Outputs, WorkspaceRevision: event.WorkspaceRevision.DeepCopy(), Runner: maps.Clone(event.Runner)})
		}
	}
	return result
}

func readStageRestartManifest(reader *journal.Reader, id journal.RunIdentity, machine *workflow.Machine) (*stageRestartManifest, error) {
	for _, input := range id.Inputs {
		if input.Name != StageRestartInputName {
			continue
		}
		if input.Integrity != apiv1.IntegrityTrusted || id.ContinuedFromRunID == "" || machine == nil || machine.Def.DSLVersion != supportmatrix.V31DSLVersion || id.EngineDriven() || !validRestartManifestChild(id) {
			return nil, errors.New("restart manifest requires a trusted continuation input")
		}
		raw, err := reader.ArtifactBytesBounded(input.Ref, maxStageRestartBytes)
		if err != nil {
			return nil, err
		}
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.DisallowUnknownFields()
		var manifest stageRestartManifest
		if err := decoder.Decode(&manifest); err != nil {
			return nil, err
		}
		var extra any
		if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
			return nil, errors.New("restart manifest must contain exactly one JSON document")
		}
		if manifest.Version != 1 || manifest.EpochID != id.RunID || manifest.SourceRunID != id.ContinuedFromRunID || manifest.SourceTerminalSeq != id.SourceTerminalSeq || manifest.Stage != id.RequestedTarget || manifest.Actor != id.Operator || manifest.WorkflowDigest != machine.Digest() || manifest.GuidanceDigest != journal.Digest([]byte(manifest.Guidance)) || manifest.NextAttempt < 1 || len(manifest.Guidance) > 64<<10 || len(manifest.GuidanceIDs) == 0 || len(manifest.GuidanceIDs) > 16 {
			return nil, errors.New("restart manifest differs from pinned continuation identity")
		}
		return &manifest, nil
	}
	return nil, nil
}

func validRestartManifestChild(id journal.RunIdentity) bool {
	return id.Child == nil || (id.Child.ExecutionEpoch > 0 && id.ValidateChildLineage() == nil)
}

func pendingStageRestart(manifest *stageRestartManifest, events []journal.Event) *rerunContext {
	for _, event := range events {
		if event.Type == journal.EventStageFinished && event.Stage == manifest.Stage && !isInterruptedAttemptMarker(event) {
			return nil
		}
		if event.Type == journal.EventGateEvaluated && event.Gate == manifest.Stage {
			return nil
		}
	}
	policy, infra := pendingRerunRetryUsage(events, manifest.Stage)
	request := journal.Event{Stage: manifest.Stage, Attempt: manifest.NextAttempt}
	return &rerunContext{stage: manifest.Stage, attempt: pendingRerunAttempt(events, request), requestAttempt: manifest.NextAttempt, policyAttempts: policy, infrastructureFailures: infra, gateAttempts: pendingRerunGateAttempts(events, manifest.Stage), instructionAddendum: manifest.Guidance}
}

func restoreRestartFrame(f *resumeFrame, machine *workflow.Machine) {
	if f.restart == nil {
		return
	}
	source := reconstructStageOutputs(f.restart.Upstream, machine)
	if source == nil {
		source = stageOutputs{}
	}
	maps.Copy(source, f.ws.completed)
	f.ws.completed = source
	visited := stageVisitSeed(f.restart.Upstream)
	if visited == nil {
		visited = map[string]bool{}
	}
	maps.Copy(visited, f.ws.visitedStages)
	f.ws.visitedStages = visited
	if !f.hasLast {
		stage, result, ok := lastFinishedSubject(f.restart.Upstream)
		if ok {
			f.ws.lastStage = stage
			f.ws.lastResult = discardToleratedFailureOutputs(machine, stage, result)
		}
	}
}

func seedRestartGateBudgets(f *resumeFrame, machine *workflow.Machine) {
	if f.restart == nil {
		return
	}
	history := f.restart.BudgetHistory
	stage := f.restart.Stage
	gates := map[string]bool{}
	for _, gate := range machine.Def.Spec.Gates {
		if gate.Name == stage {
			gates[gate.Name] = true
		}
		for _, target := range gate.Branches {
			if target == stage {
				gates[gate.Name] = true
			}
		}
	}
	f.ws.gateAttempts = restartBudgetSeed(gateRepassSeed(history), f.ws.gateAttempts, gates)
	f.ws.infraGateAttempts = restartBudgetSeed(gateInfrastructureSeed(history), f.ws.infraGateAttempts, gates)
	targets := map[string]bool{stage: true}
	f.ws.repassAttempts = restartBudgetSeed(targetRepassSeed(history), f.ws.repassAttempts, targets)
	f.ws.infraRepassAttempts = restartBudgetSeed(infrastructureTargetRepassSeed(history), f.ws.infraRepassAttempts, targets)
	f.ws.pollAttempts = restartBudgetSeed(pollingTargetSeed(history), f.ws.pollAttempts, targets)
	digests := gateDiffSeed(history)
	if digests == nil {
		digests = map[string]string{}
	}
	for gate := range gates {
		delete(digests, gate)
	}
	maps.Copy(digests, f.ws.gateDiffDigests)
	f.ws.gateDiffDigests = digests
	evidence := remediationEvidenceRejectionSeed(history)
	if evidence == nil {
		evidence = map[string]evidenceRejectionBudget{}
	}
	for gate := range gates {
		delete(evidence, gate)
	}
	maps.Copy(evidence, f.ws.evidenceRejections)
	f.ws.evidenceRejections = evidence
}

func restartBudgetSeed(previous, current map[string]int, reset map[string]bool) map[string]int {
	if previous == nil {
		previous = map[string]int{}
	}
	for key := range reset {
		delete(previous, key)
	}
	maps.Copy(previous, current)
	return previous
}

func resetRestartHistory(history []journal.Event, machine *workflow.Machine, stage string) []journal.Event {
	gates := map[string]bool{stage: true}
	for _, gate := range machine.Def.Spec.Gates {
		for _, target := range gate.Branches {
			if target == stage {
				gates[gate.Name] = true
			}
		}
	}
	result := make([]journal.Event, 0, len(history))
	for _, event := range history {
		target, _ := event.Runner["repassTarget"].(string)
		if gates[event.Gate] || event.Target == stage || target == stage {
			continue
		}
		result = append(result, event)
	}
	return result
}

// IsStageRestart identifies a retained restart marker so dispatch and recovery
// select interactive credential execution. Full validation still happens in Resume.
func IsStageRestart(id journal.RunIdentity) bool {
	for _, input := range id.Inputs {
		if input.Name == StageRestartInputName {
			return true
		}
	}
	return false
}

func (r *Runner) restoreResumeFrame(ctx context.Context, jr *journal.Run, rd *journal.Reader, in ResumeInput, id journal.RunIdentity, registrar SecretRegistrar, events []journal.Event, humanProgress humanGateProgress) (context.Context, *resumeFrame, error) {
	rerun, seedEvents, err := pendingRerun(events, in.Machine)
	if err != nil {
		return ctx, nil, fmt.Errorf("runner: restore pending stage rerun for run %q: %w", in.RunID, err)
	}
	if rerun == nil {
		seedEvents = events
	}
	restart, err := readStageRestartManifest(rd, id, in.Machine)
	if err != nil {
		return ctx, nil, fmt.Errorf("runner: restore human restart epoch: %w", err)
	}
	if restart == nil && r.cfg.stageRestartOnly != "" {
		return ctx, nil, errors.New("runner: human epoch lacks durable restart authority")
	}
	var cleanup func()
	success := false
	defer func() {
		if !success && cleanup != nil {
			cleanup()
		}
	}()
	if restart != nil {
		if r.cfg.StageRestartContext == nil {
			return ctx, nil, errors.New("runner: human restart requires its configured interactive credential execution")
		}
		if ctx, cleanup, err = r.cfg.StageRestartContext(ctx, id, registrar); err != nil {
			return ctx, nil, fmt.Errorf("runner: authorize human restart: %w", err)
		}
	}
	if rerun == nil && restart != nil {
		rerun = pendingStageRestart(restart, events)
	}

	f, err := r.newResumeFrame(ctx, jr, rd, in, id, registrar, events, seedEvents, rerun, humanProgress)
	if err != nil {
		return ctx, nil, fmt.Errorf("runner: reconstruct workspace revision for run %q: %w", in.RunID, err)
	}
	f.restart = restart
	f.restartCleanup = cleanup
	if err := r.restoreRestartWorkspace(ctx, f); err != nil {
		return ctx, nil, err
	}
	restoreRestartFrame(f, in.Machine)
	success = true

	return ctx, f, nil
}

func retainedRestartPointers(id journal.RunIdentity, inputs map[string][]byte) []apiv1.ContextPointer {
	result := []apiv1.ContextPointer{}
	for _, pointer := range id.ContextPointers {
		// Input snapshots are copied into the new journal. Reusing their old
		// pointers would require unnecessary cross-run access and expose a prior
		// restart's guidance manifest even when its notes were not selected.
		if pointer.Artifact != nil && strings.HasPrefix(pointer.Artifact.Path, "inputs/") {
			name := strings.TrimPrefix(pointer.Artifact.Path, "inputs/")
			if name == StageRestartInputName {
				continue
			}
			if _, copied := inputs[name]; copied && (pointer.RunID == "" || pointer.RunID == id.RunID) {
				continue
			}
		}
		result = append(result, pointer)
	}
	return result
}

func (r *Runner) restoreRestartWorkspace(ctx context.Context, f *resumeFrame) error {
	if f.restart == nil {
		return nil
	}
	history := append(append([]journal.Event(nil), f.restart.History...), f.events...)
	restored, err := r.restoreWorkspaceRevision(ctx, f.ws.in, history)
	if err != nil {
		return fmt.Errorf("runner: restore human restart workspace authority: %w", err)
	}
	f.ws.in = restored
	return nil
}
