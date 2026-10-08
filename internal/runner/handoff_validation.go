package runner

import (
	"context"
	"fmt"
	"sort"
	"strings"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/artifactset"
	"github.com/goobers/goobers/internal/gate"
	"github.com/goobers/goobers/internal/handoffcheck"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/runcontrol"
	"github.com/goobers/goobers/internal/workflow"
	"github.com/goobers/goobers/internal/workflowgraph"
)

const handoffValidationAnnotationKind = "handoff.validation"
const handoffValidationRetryAnnotationKind = "handoff.validation.retry"

const invalidHandoffErrorCode = "invalid_handoff"

const invalidHandoffOutputKey = "invalidHandoff"

// invalidHandoffUnsupportedTopologyReason classifies an invalid handoff whose
// producer cannot be rerouted from the consumer's execution scope (for example
// a producer that ran before a parallel and is consumed inside a branch). The
// run escalates instead of re-executing the producer in the wrong scope.
const invalidHandoffUnsupportedTopologyReason = "invalid_handoff_unsupported_topology"

type invalidHandoffRetry struct {
	Consumer string               `json:"consumer"`
	Producer string               `json:"producer"`
	Input    string               `json:"input"`
	Slot     string               `json:"slot"`
	SchemaID string               `json:"schemaId,omitempty"`
	Issues   []handoffcheck.Issue `json:"issues,omitempty"`
}

// HandoffSchemaLoader resolves one schemaPath from trusted configuration into a
// compiled handoffcheck schema. Nil leaves handoff validation disabled.
type HandoffSchemaLoader func(schemaPath string) (*handoffcheck.Schema, error)

type handoffBinding struct {
	LocalName    string
	ProducerTask string
	SlotName     string
	MediaType    string
	SchemaPath   string
}

func (r *Runner) handoffValidationContext(ctx context.Context, jr executionJournal, machine *workflow.Machine, task apiv1.Task, attempt int, class journal.AttemptClass, pointers []apiv1.ContextPointer) (context.Context, *handoffcheck.Report, error) {
	report := buildHandoffValidationReport(ctx, jr.Dir(), handoffBindingsForContext(machine, pointers), pointers, r.cfg.HandoffSchemaLoader)
	if report == nil {
		return ctx, nil, nil
	}
	if err := jr.Append(journal.Event{
		Type: journal.EventRunnerAnnotation, Stage: task.Name, Attempt: attempt, AttemptClass: class,
		Runner: handoffValidationRunnerFields(*report),
	}); err != nil {
		return ctx, nil, fmt.Errorf("task %q: journal handoff validation: %w", task.Name, err)
	}
	return handoffcheck.WithReport(ctx, *report), report, nil
}

func handoffBindingsForContext(machine *workflow.Machine, pointers []apiv1.ContextPointer) map[string]handoffBinding {
	if machine == nil {
		return nil
	}
	presentProducers := map[string]bool{}
	for _, pointer := range pointers {
		if pointer.Artifact == nil || pointer.External != nil || pointer.RunID != "" {
			continue
		}
		producer, _, ok := strings.Cut(pointer.Name, ".artifact[")
		if !ok || producer == "" {
			continue
		}
		presentProducers[producer] = true
	}
	if len(presentProducers) == 0 {
		return nil
	}
	out := map[string]handoffBinding{}
	for _, task := range machine.Def.Spec.Tasks {
		if !presentProducers[task.Name] {
			continue
		}
		for _, slot := range task.ArtifactSlots {
			if slot.SchemaPath == "" || slot.MediaType != "application/json" {
				continue
			}
			key := task.Name + "." + slot.Name
			out[key] = handoffBinding{
				LocalName:    key,
				ProducerTask: task.Name,
				SlotName:     slot.Name,
				MediaType:    slot.MediaType,
				SchemaPath:   slot.SchemaPath,
			}
		}
	}
	return out
}

func buildHandoffValidationReport(ctx context.Context, journalRoot string, bindings map[string]handoffBinding, pointers []apiv1.ContextPointer, load HandoffSchemaLoader) *handoffcheck.Report {
	if load == nil || len(bindings) == 0 || len(pointers) == 0 {
		return nil
	}
	type bindingCheck struct {
		local   string
		binding handoffBinding
	}
	checks := make([]bindingCheck, 0, len(bindings))
	requiredByProducer := map[string][]string{}
	for local, binding := range bindings {
		if binding.SchemaPath == "" || binding.MediaType != "application/json" {
			continue
		}
		checks = append(checks, bindingCheck{local: local, binding: binding})
		requiredByProducer[binding.ProducerTask] = append(requiredByProducer[binding.ProducerTask], binding.SlotName)
	}
	if len(checks) == 0 {
		return nil
	}
	sort.Slice(checks, func(i, j int) bool { return checks[i].local < checks[j].local })
	for producer := range requiredByProducer {
		sort.Strings(requiredByProducer[producer])
	}
	reader, err := artifactset.OpenJournal(journalRoot)
	if err != nil {
		return &handoffcheck.Report{InputValid: handoffcheck.InputValidUnknown, Error: fmt.Sprintf("open journal: %v", err)}
	}
	defer func() { _ = reader.Close() }()
	resolvedByProducer := make(map[string]map[string]artifactset.Payload, len(requiredByProducer))
	report := &handoffcheck.Report{Entries: make([]handoffcheck.ReportEntry, 0, len(checks))}
	var (
		anyInvalid bool
		errs       []string
	)
	for _, check := range checks {
		payloads, ok := resolvedByProducer[check.binding.ProducerTask]
		if !ok {
			payloads, err = artifactset.Resolve(ctx, reader, pointers, check.binding.ProducerTask, requiredByProducer[check.binding.ProducerTask]...)
			if err != nil {
				errs = append(errs, fmt.Sprintf("%s: resolve %s.%s: %v", check.local, check.binding.ProducerTask, check.binding.SlotName, err))
				continue
			}
			resolvedByProducer[check.binding.ProducerTask] = payloads
		}
		schema, err := load(check.binding.SchemaPath)
		if err != nil {
			errs = append(errs, fmt.Sprintf("%s: load schema %q: %v", check.local, check.binding.SchemaPath, err))
			continue
		}
		payload, ok := payloads[check.binding.SlotName]
		if !ok {
			errs = append(errs, fmt.Sprintf("%s: resolved payload %s.%s missing", check.local, check.binding.ProducerTask, check.binding.SlotName))
			continue
		}
		verdict := schema.Check(payload.Bytes)
		entry := handoffcheck.ReportEntry{
			Input:    check.local,
			Producer: check.binding.ProducerTask,
			Slot:     check.binding.SlotName,
			SchemaID: verdict.SchemaID,
			Valid:    verdict.Valid,
		}
		if !verdict.Valid {
			anyInvalid = true
			entry.Issues = append([]handoffcheck.Issue(nil), verdict.Issues...)
		}
		report.Entries = append(report.Entries, entry)
	}
	if len(errs) > 0 {
		report.Error = strings.Join(errs, "; ")
	}
	switch {
	case anyInvalid:
		report.InputValid = handoffcheck.InputValidFalse
	case len(report.Entries) > 0 && report.Error == "":
		report.InputValid = handoffcheck.InputValidTrue
	default:
		report.InputValid = handoffcheck.InputValidUnknown
	}
	if len(report.Entries) == 0 && report.Error == "" {
		return nil
	}
	return report
}

func handoffValidationRunnerFields(report handoffcheck.Report) map[string]any {
	fields := map[string]any{
		"kind":       handoffValidationAnnotationKind,
		"inputValid": string(report.InputValid),
	}
	if len(report.Entries) > 0 {
		fields["entries"] = report.Entries
	}
	if report.Error != "" {
		fields["error"] = report.Error
	}
	return fields
}

func invalidHandoffResult(task apiv1.Task, report *handoffcheck.Report, enabled bool) (apiv1.ResultEnvelope, bool) {
	if !enabled || report == nil || report.InputValid != handoffcheck.InputValidFalse {
		return apiv1.ResultEnvelope{}, false
	}
	retry, ok := invalidHandoffRetryForReport(task.Name, *report)
	if !ok {
		return apiv1.ResultEnvelope{}, false
	}
	message := invalidHandoffMessage(retry)
	return apiv1.ResultEnvelope{
		Status:  apiv1.ResultFailure,
		Summary: message,
		Error: &apiv1.ErrorInfo{
			Code:      invalidHandoffErrorCode,
			Message:   message,
			Retryable: true,
		},
		Outputs: map[string]interface{}{
			invalidHandoffOutputKey: retry,
			handoffcheck.OutputKey:  *report,
		},
	}, true
}

func invalidHandoffRetryForReport(consumer string, report handoffcheck.Report) (invalidHandoffRetry, bool) {
	for _, entry := range report.Entries {
		if entry.Valid || entry.Producer == "" {
			continue
		}
		return invalidHandoffRetry{
			Consumer: consumer,
			Producer: entry.Producer,
			Input:    entry.Input,
			Slot:     entry.Slot,
			SchemaID: entry.SchemaID,
			Issues:   append([]handoffcheck.Issue(nil), entry.Issues...),
		}, true
	}
	return invalidHandoffRetry{}, false
}

func invalidHandoffRetryFromResult(result apiv1.ResultEnvelope) (invalidHandoffRetry, bool) {
	if result.Status != apiv1.ResultFailure || result.Error == nil || result.Error.Code != invalidHandoffErrorCode {
		return invalidHandoffRetry{}, false
	}
	raw, ok := result.Outputs[invalidHandoffOutputKey]
	if !ok {
		return invalidHandoffRetry{}, false
	}
	switch retry := raw.(type) {
	case invalidHandoffRetry:
		return retry, retry.Producer != ""
	case map[string]interface{}:
		return invalidHandoffRetryFromMap(retry)
	default:
		return invalidHandoffRetry{}, false
	}
}

func invalidHandoffRetryFromMap(raw map[string]interface{}) (invalidHandoffRetry, bool) {
	retry := invalidHandoffRetry{
		Consumer: stringFromMap(raw, "consumer"),
		Producer: stringFromMap(raw, "producer"),
		Input:    stringFromMap(raw, "input"),
		Slot:     stringFromMap(raw, "slot"),
		SchemaID: stringFromMap(raw, "schemaId"),
	}
	return retry, retry.Producer != ""
}

func stringFromMap(raw map[string]interface{}, key string) string {
	if value, ok := raw[key].(string); ok {
		return value
	}
	return ""
}

func invalidHandoffMessage(retry invalidHandoffRetry) string {
	input := retry.Input
	if input == "" {
		input = retry.Producer + "." + retry.Slot
	}
	return fmt.Sprintf("invalid handoff %q from producer %q failed schema validation; rerouting producer for retry", input, retry.Producer)
}

func invalidHandoffRetryAddendum(retry invalidHandoffRetry, attempt, limit int) string {
	var b strings.Builder
	input := retry.Input
	if input == "" {
		input = retry.Producer + "." + retry.Slot
	}
	fmt.Fprintf(&b, "Your prior artifact handoff %q did not match its declared schema. This is producer retry %d of %d for that invalid handoff. Re-emit the artifact as raw JSON that satisfies the declared schema.", input, attempt, limit)
	if retry.SchemaID != "" {
		fmt.Fprintf(&b, " Schema: %s.", retry.SchemaID)
	}
	for _, issue := range retry.Issues {
		if issue.Path != "" {
			fmt.Fprintf(&b, " Issue at %s: %s.", issue.Path, issue.Message)
		} else if issue.Message != "" {
			fmt.Fprintf(&b, " Issue: %s.", issue.Message)
		}
	}
	return b.String()
}

// invalidHandoffRerouteUnsupported explains why the producer cannot be retried
// from the consumer's execution scope, or returns "" when the reroute is safe.
// branchStart is the consumer's parallel branch start ("" at the run's root)
// and inherited holds the pointers the branch inherited from before the
// parallel. A retry inside a branch may only re-execute a producer that lives
// in that branch and whose artifacts are branch-local; a retry at the root may
// not re-enter a producer that only runs inside a parallel branch.
func invalidHandoffRerouteUnsupported(machine *workflow.Machine, branchStart string, inherited []apiv1.ContextPointer, producer string) string {
	if branchStart == "" {
		for _, name := range sortedParallelNames(machine) {
			spec, _ := machine.Parallel(name)
			for _, branch := range spec.Branches {
				if workflowgraph.BranchContainsState(machine, branch.Start, producer) {
					return fmt.Sprintf("producer %q runs inside parallel %q branch %q and cannot be retried outside that branch", producer, name, branch.Name)
				}
			}
		}
		return ""
	}
	if hasStageArtifactPointers(inherited, producer) {
		return fmt.Sprintf("producer %q artifact was inherited from before the parallel and cannot be retried inside a branch", producer)
	}
	if !workflowgraph.BranchContainsState(machine, branchStart, producer) {
		return fmt.Sprintf("producer %q is not part of the consumer's parallel branch", producer)
	}
	return ""
}

func sortedParallelNames(machine *workflow.Machine) []string {
	names := make([]string, 0, len(machine.Parallels()))
	for name := range machine.Parallels() {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// invalidHandoffDecision is the journaled outcome of one invalid handoff: the
// annotation fields, and either an escalation or the producer retry addendum.
type invalidHandoffDecision struct {
	fields   map[string]any
	escalate bool
	addendum string
}

// decideInvalidHandoff charges the producer retry against budget (unless the
// topology is unsupported, which escalates without charging).
func decideInvalidHandoff(budget *runcontrol.RepassBudget, consumer string, retry invalidHandoffRetry, unsupported string, reentry bool, maxRepasses int) invalidHandoffDecision {
	fields := map[string]any{
		"kind":     handoffValidationRetryAnnotationKind,
		"consumer": consumer,
		"producer": retry.Producer,
		"input":    retry.Input,
	}
	if unsupported != "" {
		fields["escalated"] = true
		fields["reason"] = invalidHandoffUnsupportedTopologyReason
		fields["detail"] = unsupported
		return invalidHandoffDecision{fields: fields, escalate: true}
	}
	synthetic := apiv1.Gate{
		Name:      invalidHandoffGateName(consumer),
		Evaluator: apiv1.EvaluatorAutomated,
		Branches:  map[string]string{gate.OutcomeFail: retry.Producer},
	}
	charge := budget.Charge(synthetic, gate.OutcomeFail, retry.Producer, reentry, maxRepasses)
	fields["target"] = retry.Producer
	fields["repassAttempt"] = charge.Attempt
	fields["repassLimit"] = charge.Bound
	if charge.Exceeded {
		fields["escalated"] = true
		fields["reason"] = charge.EscalationReason()
		return invalidHandoffDecision{fields: fields, escalate: true}
	}
	return invalidHandoffDecision{fields: fields, addendum: invalidHandoffRetryAddendum(retry, charge.Attempt, charge.Bound)}
}

func invalidHandoffGateName(consumer string) string {
	return "handoff.validation:" + consumer
}

// invalidHandoffPrunedProducer returns the producer whose artifacts a journaled
// invalid-handoff retry annotation discarded. Escalated annotations never
// reroute, so they prune nothing. Every pointer replay (resume, rerun, parallel
// join and branch restore) must apply this so a crash between the annotation
// and the producer retry cannot resurrect the invalid artifact.
func invalidHandoffPrunedProducer(e journal.Event) (string, bool) {
	if e.Type != journal.EventRunnerAnnotation || e.Runner["kind"] != handoffValidationRetryAnnotationKind {
		return "", false
	}
	if escalated, _ := e.Runner["escalated"].(bool); escalated {
		return "", false
	}
	target, _ := e.Runner["target"].(string)
	return target, target != ""
}

func isInvalidHandoffFailure(result apiv1.ResultEnvelope) bool {
	return result.Status == apiv1.ResultFailure && result.Error != nil && result.Error.Code == invalidHandoffErrorCode
}

func isInvalidHandoffFailureEvent(e journal.Event) bool {
	return e.Status == string(apiv1.ResultFailure) && e.Error != nil && e.Error.Code == invalidHandoffErrorCode
}

func hasStageArtifactPointers(pointers []apiv1.ContextPointer, stage string) bool {
	prefix := stage + ".artifact["
	for _, pointer := range pointers {
		if strings.HasPrefix(pointer.Name, prefix) {
			return true
		}
	}
	return false
}

// removeStageArtifactPointers returns a copy of pointers without stage's
// positional artifact pointers; the input slice is never modified.
func removeStageArtifactPointers(pointers []apiv1.ContextPointer, stage string) []apiv1.ContextPointer {
	prefix := stage + ".artifact["
	kept := make([]apiv1.ContextPointer, 0, len(pointers))
	for _, pointer := range pointers {
		if !strings.HasPrefix(pointer.Name, prefix) {
			kept = append(kept, pointer)
		}
	}
	return kept
}

// invalidHandoffOutcome reroutes the root walk (or a sequential parallel
// branch) to the producer of an invalid handoff, or escalates when the retry
// budget is exhausted or the producer is outside the consumer's scope.
func (r *Runner) invalidHandoffOutcome(ctx context.Context, ws *walkState, consumer apiv1.Task, retry invalidHandoffRetry) (string, Result, bool, error, bool) {
	jr, in := ws.jr, ws.in
	if _, ok := in.Machine.Task(retry.Producer); !ok {
		terminal, err := r.finishStageFailure(ctx, in.RunID, jr, in.RepoRef, consumer.Name, ws.steps, &apiv1.ErrorInfo{
			Code:    invalidHandoffErrorCode,
			Message: fmt.Sprintf("invalid handoff producer %q is not a workflow task", retry.Producer),
		})
		return "", terminal, false, err, true
	}
	branchStart := ""
	if ws.parallel != nil {
		if current := ws.parallel.current(); current != nil {
			branchStart = current.start
		}
	}
	unsupported := invalidHandoffRerouteUnsupported(in.Machine, branchStart, ws.parallelRootPointers, retry.Producer)
	budget := ws.repassBudget()
	decision := decideInvalidHandoff(&budget, consumer.Name, retry, unsupported, ws.visitedStages[retry.Producer], int(in.RunControls.MaxRepasses))
	ws.applyRepassBudget(budget)
	if err := jr.Append(journal.Event{Type: journal.EventRunnerAnnotation, Stage: consumer.Name, Runner: decision.fields}); err != nil {
		terminal, failErr := r.failTerminal(ctx, in.RunID, jr, in.RepoRef, consumer.Name, ws.steps, fmt.Errorf("runner: journal invalid handoff reroute for %q: %w", consumer.Name, err))
		return "", terminal, false, failErr, true
	}
	if decision.escalate {
		if ws.parallel != nil {
			if err := r.closeParallelForLoudExit(jr, ws.parallel, workflow.TargetEscalate); err != nil {
				terminal, failErr := r.failTerminal(ctx, in.RunID, jr, in.RepoRef, consumer.Name, ws.steps, err)
				return "", terminal, false, failErr, true
			}
			ws.parallel, ws.fanIn = nil, nil
		}
		terminal, err := r.finish(in.RunID, jr, journal.PhaseEscalated, consumer.Name, ws.steps)
		return "", terminal, false, err, true
	}
	if ws.parallel == nil {
		ws.pointers = removeStageArtifactPointers(ws.pointers, retry.Producer)
	} else {
		ws.parallel.removeCurrentStageArtifactPointers(retry.Producer)
	}
	ws.retryInstructionAddendum = decision.addendum
	return retry.Producer, Result{}, true, nil, true
}

// repassBudget returns the walk's live repass counters. The gate evaluator
// owns them once it exists: Charge may allocate maps the walk state never sees.
func (ws *walkState) repassBudget() runcontrol.RepassBudget {
	if ws.gateEval != nil {
		return evaluatorRepassBudget(ws.gateEval)
	}
	return runcontrol.RepassBudget{
		Attempts:                     ws.gateAttempts,
		InfrastructureAttempts:       ws.infraGateAttempts,
		RepassAttempts:               ws.repassAttempts,
		InfrastructureRepassAttempts: ws.infraRepassAttempts,
		PollAttempts:                 ws.pollAttempts,
	}
}

func (ws *walkState) applyRepassBudget(budget runcontrol.RepassBudget) {
	ws.gateAttempts = budget.Attempts
	ws.infraGateAttempts = budget.InfrastructureAttempts
	ws.repassAttempts = budget.RepassAttempts
	ws.infraRepassAttempts = budget.InfrastructureRepassAttempts
	ws.pollAttempts = budget.PollAttempts
	if ws.gateEval != nil {
		applyEvaluatorRepassBudget(ws.gateEval, budget)
	}
}

func evaluatorRepassBudget(eval *gate.Evaluator) runcontrol.RepassBudget {
	return runcontrol.RepassBudget{
		Attempts:                     eval.Attempts,
		InfrastructureAttempts:       eval.InfrastructureAttempts,
		RepassAttempts:               eval.RepassAttempts,
		InfrastructureRepassAttempts: eval.InfrastructureRepassAttempts,
		PollAttempts:                 eval.PollAttempts,
	}
}

func applyEvaluatorRepassBudget(eval *gate.Evaluator, budget runcontrol.RepassBudget) {
	eval.Attempts = budget.Attempts
	eval.InfrastructureAttempts = budget.InfrastructureAttempts
	eval.RepassAttempts = budget.RepassAttempts
	eval.InfrastructureRepassAttempts = budget.InfrastructureRepassAttempts
	eval.PollAttempts = budget.PollAttempts
}
func isInvalidHandoffEscalation(e journal.Event) bool {
	if e.Type != journal.EventRunnerAnnotation || e.Runner["kind"] != handoffValidationRetryAnnotationKind {
		return false
	}
	escalated, _ := e.Runner["escalated"].(bool)
	return escalated
}

// replayInvalidHandoffAnnotation applies a journaled invalid-handoff decision
// to a resumed parallel branch and reports whether event was such a decision.
func replayInvalidHandoffAnnotation(branch *branchState, event journal.Event) bool {
	if producer, ok := invalidHandoffPrunedProducer(event); ok {
		branch.pointers = removeStageArtifactPointers(branch.pointers, producer)
		branch.artifacts = artifactPointerCount(branch.pointers)
		return true
	}
	if isInvalidHandoffEscalation(event) {
		branch.failed = true
		return true
	}
	return false
}

// failsParallelBranch reports whether a failed stage fails its sequential or
// resumed parallel branch. Invalid-handoff failures are rerouted to their
// producer instead, and continueOnError or a gate successor own the failure.
func failsParallelBranch(machine *workflow.Machine, task apiv1.Task, invalidHandoff bool) bool {
	if task.ContinueOnError || invalidHandoff {
		return false
	}
	_, nextIsGate := machine.Gate(task.Next)
	return !nextIsGate
}

// pruneInvalidHandoffPointers replays a journaled invalid-handoff reroute: the
// producer's artifacts are discarded in the scope (root or branch) that
// observed them, exactly as the live run discarded them. It reports whether e
// was such a reroute and returns the updated root pointers.
func pruneInvalidHandoffPointers(root []apiv1.ContextPointer, branches map[int][]apiv1.ContextPointer, e journal.Event) ([]apiv1.ContextPointer, bool) {
	producer, ok := invalidHandoffPrunedProducer(e)
	if !ok {
		return root, false
	}
	if e.Branch <= 0 {
		return removeStageArtifactPointers(root, producer), true
	}
	if pointers, known := branches[e.Branch]; known {
		branches[e.Branch] = removeStageArtifactPointers(pointers, producer)
	}
	return root, true
}
