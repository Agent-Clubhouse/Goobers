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

func (r *Runner) handoffValidationContext(ctx context.Context, jr executionJournal, machine *workflow.Machine, task apiv1.Task, attempt int, class journal.AttemptClass, pointers []apiv1.ContextPointer) (context.Context, error) {
	report := buildHandoffValidationReport(ctx, jr.Dir(), handoffBindingsForContext(machine, pointers), pointers, r.cfg.HandoffSchemaLoader)
	if report == nil {
		return ctx, nil
	}
	if err := jr.Append(journal.Event{
		Type: journal.EventRunnerAnnotation, Stage: task.Name, Attempt: attempt, AttemptClass: class,
		Runner: handoffValidationRunnerFields(*report),
	}); err != nil {
		return ctx, fmt.Errorf("task %q: journal handoff validation: %w", task.Name, err)
	}
	return handoffcheck.WithReport(ctx, *report), nil
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
