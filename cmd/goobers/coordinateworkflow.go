package main

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"reflect"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/coordination"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/providers"
)

type coordinationKindExecutor struct {
	input     deterministicExecutorInput
	reconcile func(context.Context, *instance.Config, instance.Layout, coordination.Authority, coordination.Plan, *coordination.Evidence, string, terminalSecretRegistry) (coordination.Result, error)
}

func coordinateWorkflow(layout instance.Layout, gaggle, name string) (apiv1.Workflow, error) {
	set, report, err := instance.LoadConfigDir(layout.ConfigDir())
	if err != nil {
		return apiv1.Workflow{}, err
	}
	if report.HasErrors() {
		return apiv1.Workflow{}, fmt.Errorf("coordination workflow configuration failed validation")
	}
	var matches []apiv1.Workflow
	for _, w := range set.Workflows {
		if w.Name == name && w.Spec.Gaggle == gaggle {
			matches = append(matches, w)
		}
	}
	if len(matches) != 1 {
		return apiv1.Workflow{}, fmt.Errorf("coordination requires one exact loaded gaggle/workflow identity")
	}
	w := matches[0]
	if err := coordination.ValidateWorkflow(w.Spec); err != nil {
		return w, err
	}
	if len(w.Spec.Tasks) != 1 || w.Spec.Tasks[0].Inputs["kind"] != coordination.WorkflowKind {
		return w, fmt.Errorf("workflow does not declare native coordination")
	}
	return w, nil
}

func (e *coordinationKindExecutor) Run(ctx context.Context, env apiv1.InvocationEnvelope, run apiv1.DeterministicRun) (apiv1.ResultEnvelope, error) {
	// The CLI stage guard remains intact. Only this in-process kind can request
	// runner-owned authority; no shell, injector env, or agent is involved.
	if err := coordinateLocalOnly(); err != nil {
		return apiv1.ResultEnvelope{}, err
	}
	layout := instance.Layout{Root: e.input.InstanceRoot}
	w, err := coordinateWorkflow(layout, env.Gaggle, env.WorkflowID)
	if err != nil {
		return apiv1.ResultEnvelope{}, err
	}
	authority, err := admitCoordinateWorkflow(e.input, w, env, run)
	if err != nil {
		return apiv1.ResultEnvelope{}, err
	}
	task := w.Spec.Tasks[0]
	var plan coordination.Plan
	if err := readCoordinateJSON(coordinateInputPath(layout, task.Inputs["planFile"]), &plan); err != nil {
		return apiv1.ResultEnvelope{}, err
	}
	if err := plan.Validate(authority, true); err != nil {
		return apiv1.ResultEnvelope{}, err
	}
	var evidence *coordination.Evidence
	if path := task.Inputs["evidenceFile"]; path != "" {
		evidence = &coordination.Evidence{}
		if err := readCoordinateJSON(coordinateInputPath(layout, path), evidence); err != nil {
			return apiv1.ResultEnvelope{}, err
		}
	}
	if err := coordination.ValidateEvidence(plan, evidence, authority, true); err != nil {
		return apiv1.ResultEnvelope{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	result, err := e.reconcile(ctx, e.input.Config, layout, authority, plan, evidence, coordinateInputPath(layout, task.Inputs["artifactFile"]), e.input.SharedRegistry)
	if err != nil {
		return apiv1.ResultEnvelope{}, scrubTerminalError(e.input.SharedRegistry, err)
	}
	data, err := json.Marshal(result)
	if err != nil {
		return apiv1.ResultEnvelope{}, err
	}
	var outputs map[string]interface{}
	if err := json.Unmarshal(data, &outputs); err != nil {
		return apiv1.ResultEnvelope{}, err
	}
	outputs["complete"] = result.State == "complete"
	return apiv1.ResultEnvelope{Status: apiv1.ResultSuccess, Summary: "coordination: " + result.State, Outputs: outputs}, nil
}

func admitCoordinateWorkflow(input deterministicExecutorInput, w apiv1.Workflow, env apiv1.InvocationEnvelope, run apiv1.DeterministicRun) (coordination.Authority, error) {
	empty := coordination.Authority{}
	if input.Config.Coordination == nil || env.RunID == "" || env.Goober != "" || env.ParentPlatformPolicy != nil || len(env.Capabilities) != 0 {
		return empty, fmt.Errorf("coordination requires an authorized runner-owned deterministic invocation, not agentic or nested execution")
	}
	if err := coordination.ValidateWorkflow(w.Spec); err != nil {
		return empty, err
	}
	if len(w.Spec.Tasks) != 1 || w.Spec.Tasks[0].Inputs["kind"] != coordination.WorkflowKind {
		return empty, fmt.Errorf("coordination workflow kind is required")
	}
	task := w.Spec.Tasks[0]
	if w.Spec.Gaggle != env.Gaggle || w.Name != env.WorkflowID || env.RunID+":"+task.Name != env.TaskID || !reflect.DeepEqual(*task.Run, run) {
		return empty, fmt.Errorf("coordination invocation does not match its configured workflow")
	}
	if len(env.Inputs) != len(task.Inputs) {
		return empty, fmt.Errorf("coordination rejects additional or dynamic request inputs")
	}
	for key, value := range task.Inputs {
		if env.Inputs[key] != value {
			return empty, fmt.Errorf("coordination input %q differs from approved static configuration", key)
		}
	}
	digest, _ := coordination.Digest(w.Spec)
	project := providers.RepositoryRef{Provider: providers.ProviderKind(input.GaggleProject.Provider), Owner: input.GaggleProject.Owner, Project: input.GaggleProject.Project, Name: input.GaggleProject.Name, URL: input.GaggleProject.BaseURL}
	invokedProject := providers.RepositoryRef{Provider: providers.ProviderKind(env.RepoRef.Provider), Owner: env.RepoRef.Owner, Project: env.RepoRef.Project, Name: env.RepoRef.Name, URL: env.RepoRef.BaseURL}
	if invokedProject.CanonicalKey() != project.CanonicalKey() {
		return empty, fmt.Errorf("coordination invocation repository differs from its runner-owned project")
	}
	for _, a := range input.Config.Coordination.Gaggles {
		if a.Name == env.Gaggle && a.ParentRepo.Key() == project.CanonicalKey() && a.ApprovedWorkflows[w.Name] == digest {
			return a, nil
		}
	}
	return empty, fmt.Errorf("coordination workflow/gaggle/project has no exact operator-approved workflow digest")
}

func coordinateInputPath(layout instance.Layout, path string) string {
	if path == "" || filepath.IsAbs(path) {
		return path
	}
	return filepath.Join(layout.Root, path)
}
