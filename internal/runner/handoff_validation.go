package runner

import (
	"context"
	"fmt"
	"sort"
	"strings"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/artifactset"
	"github.com/goobers/goobers/internal/handoffcheck"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/workflow"
)

const handoffValidationAnnotationKind = "handoff.validation"
const handoffValidationRetryAnnotationKind = "handoff.validation.retry"

const invalidHandoffErrorCode = "invalid_handoff"

const invalidHandoffOutputKey = "invalidHandoff"

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
