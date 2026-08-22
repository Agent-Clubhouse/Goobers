// Package gooberruntime implements the runtime side of the neutral invoke.Goober
// boundary used by the workflow engine.
//
// Superseded — folds into the local runner's stage execution (the `goobers`
// binary, via internal/harness); kept compiling as the tier-3 agent-pod
// reference its only consumer, cmd/goober-runtime, is. See
// docs/ARCHITECTURE.md §11.
package gooberruntime

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

const (
	defaultInstructionsKey     = "instructions"
	defaultInstructionsPathKey = "instructionsPath"
)

// GooberContext is the complete task or reviewer context delivered to the
// harness.
type GooberContext struct {
	TaskID           string                      `json:"taskId"`
	WorkflowID       string                      `json:"workflowId"`
	RunID            string                      `json:"runId"`
	Gaggle           string                      `json:"gaggle"`
	Goal             string                      `json:"goal"`
	Instructions     string                      `json:"instructions,omitempty"`
	RepoRef          apiv1.RepoRef               `json:"repoRef"`
	Item             *apiv1.BacklogItem          `json:"item,omitempty"`
	Inputs           map[string]interface{}      `json:"inputs,omitempty"`
	ContextPointers  []apiv1.ContextPointer      `json:"contextPointers,omitempty"`
	MinimumIntegrity apiv1.Integrity             `json:"minimumIntegrity,omitempty"`
	Limits           apiv1.Limits                `json:"limits,omitempty"`
	ExecutionPolicy  *apiv1.ChildExecutionPolicy `json:"executionPolicy,omitempty"`
	// ExecutionEnvelope is always present for nested agents, including fresh
	// context children. It carries execution authority without conversation
	// history or unrelated task context.
	ExecutionEnvelope *ExecutionEnvelope `json:"executionEnvelope,omitempty"`
	// EnvelopeSections contains only sections explicitly selected by an
	// explicit-context child. Mandatory execution policy is not filtered.
	EnvelopeSections map[string]interface{} `json:"envelopeSections,omitempty"`
}

type ExecutionEnvelope struct {
	RunID              string               `json:"runId"`
	StageID            string               `json:"stageId"`
	ParentAgent        string               `json:"parentAgent"`
	Objective          string               `json:"objective"`
	Capabilities       []string             `json:"capabilities,omitempty"`
	PlatformPolicy     apiv1.PlatformPolicy `json:"platformPolicy"`
	CompletionContract string               `json:"completionContract"`
	Cancellation       string               `json:"cancellation"`
	Budget             apiv1.Limits         `json:"budget"`
}

// InstructionResolver resolves the instruction markdown for an invocation.
type InstructionResolver interface {
	ResolveInstructions(context.Context, apiv1.InvocationEnvelope) (string, error)
}

// InputInstructionResolver reads instruction text or an instruction file path
// from the invocation inputs. This keeps M8 independent of the config-sync store
// while preserving the runtime contract shape.
type InputInstructionResolver struct {
	InstructionsKey  string
	PathKey          string
	InstructionsRoot string
}

// ResolveInstructions returns inline instructions first, then file-backed
// instructions if an instructionsPath input is present.
func (r InputInstructionResolver) ResolveInstructions(ctx context.Context, env apiv1.InvocationEnvelope) (string, error) {
	key := r.InstructionsKey
	if key == "" {
		key = defaultInstructionsKey
	}
	if value, ok := env.Inputs[key]; ok {
		text, ok := value.(string)
		if !ok {
			return "", fmt.Errorf("input %q must be a string when provided", key)
		}
		return text, nil
	}

	pathKey := r.PathKey
	if pathKey == "" {
		pathKey = defaultInstructionsPathKey
	}
	value, ok := env.Inputs[pathKey]
	if !ok {
		return "", nil
	}
	path, ok := value.(string)
	if !ok {
		return "", fmt.Errorf("input %q must be a string when provided", pathKey)
	}
	if path == "" {
		return "", fmt.Errorf("input %q must not be empty when provided", pathKey)
	}
	select {
	case <-ctx.Done():
		return "", ctx.Err()
	default:
	}
	safePath, err := r.safeInstructionsPath(path)
	if err != nil {
		return "", err
	}
	data, err := os.ReadFile(safePath)
	if err != nil {
		return "", fmt.Errorf("read instructions %q: %w", safePath, err)
	}
	return string(data), nil
}

func (r InputInstructionResolver) safeInstructionsPath(path string) (string, error) {
	if r.InstructionsRoot == "" {
		return "", fmt.Errorf("input %q requires a configured instructions root", defaultInstructionsPathKey)
	}
	if filepath.IsAbs(path) {
		return "", fmt.Errorf("instructions path must be relative")
	}
	clean := filepath.Clean(path)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("instructions path must stay within instructions root")
	}
	root, err := filepath.Abs(r.InstructionsRoot)
	if err != nil {
		return "", fmt.Errorf("resolve instructions root: %w", err)
	}
	target := filepath.Join(root, clean)
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", fmt.Errorf("resolve instructions root symlinks: %w", err)
	}
	realTarget, err := filepath.EvalSymlinks(target)
	if err != nil {
		return "", fmt.Errorf("resolve instructions path symlinks: %w", err)
	}
	rel, err := filepath.Rel(realRoot, realTarget)
	if err != nil {
		return "", fmt.Errorf("resolve instructions path: %w", err)
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("instructions path must stay within instructions root")
	}
	return realTarget, nil
}

func buildContext(ctx context.Context, env apiv1.InvocationEnvelope, resolver InstructionResolver) (GooberContext, error) {
	if env.TaskID == "" {
		return GooberContext{}, fmt.Errorf("taskId is required")
	}
	if env.WorkflowID == "" {
		return GooberContext{}, fmt.Errorf("workflowId is required")
	}
	if env.RunID == "" {
		return GooberContext{}, fmt.Errorf("runId is required")
	}
	if env.Goal == "" {
		return GooberContext{}, fmt.Errorf("goal is required")
	}
	if env.RepoRef.Provider == "" {
		return GooberContext{}, fmt.Errorf("repoRef.provider is required")
	}
	if env.RepoRef.Name == "" {
		return GooberContext{}, fmt.Errorf("repoRef.name is required")
	}
	if env.NestedAgentPolicy != nil {
		if err := env.NestedAgentPolicy.Validate(); err != nil {
			return GooberContext{}, err
		}
	}
	if resolver == nil {
		resolver = InputInstructionResolver{}
	}
	instructions, err := resolver.ResolveInstructions(ctx, env)
	if err != nil {
		return GooberContext{}, err
	}
	contextPointers := copyContextPointers(env.ContextPointers)
	var envelope *ExecutionEnvelope
	var executionPolicy *apiv1.ChildExecutionPolicy
	var envelopeSections map[string]interface{}
	if env.NestedAgentPolicy != nil {
		policy := env.NestedAgentPolicy
		executionPolicy = &apiv1.ChildExecutionPolicy{
			RunID: env.RunID, StageID: env.TaskID, ParentAgent: env.Goober,
			Objective: env.Goal, Capabilities: append([]string(nil), env.Capabilities...),
			PlatformPolicy: policy.PlatformPolicy, Delegation: policy.Delegation,
			MaxDepth: policy.MaxDepth, Context: policy.Context, Model: policy.Model,
			PeerMessaging: policy.PeerMessaging,
		}
		envelope = &ExecutionEnvelope{
			RunID: env.RunID, StageID: env.TaskID, ParentAgent: env.Goober,
			Objective: env.Goal, Capabilities: append([]string(nil), env.Capabilities...),
			PlatformPolicy:     policy.PlatformPolicy,
			CompletionContract: policy.PlatformPolicy.CompletionContract,
			Cancellation:       policy.PlatformPolicy.Cancellation,
			Budget:             policy.PlatformPolicy.Budget,
		}
		switch env.NestedAgentPolicy.Context.Mode {
		case apiv1.ContextFresh:
			contextPointers = nil
		case apiv1.ContextExplicit:
			contextPointers = selectContextPointers(contextPointers, env.NestedAgentPolicy.Context.ArtifactNames)
			envelopeSections = selectEnvelopeSections(*envelope, env.NestedAgentPolicy.Context.EnvelopeSections)
		}
	}
	return GooberContext{
		TaskID:            env.TaskID,
		WorkflowID:        env.WorkflowID,
		RunID:             env.RunID,
		Gaggle:            env.Gaggle,
		Goal:              env.Goal,
		Instructions:      instructions,
		RepoRef:           env.RepoRef,
		Item:              env.Item,
		Inputs:            copyInputs(env.Inputs),
		ContextPointers:   contextPointers,
		MinimumIntegrity:  env.MinimumIntegrity,
		Limits:            env.Limits,
		ExecutionPolicy:   executionPolicy,
		ExecutionEnvelope: envelope,
		EnvelopeSections:  envelopeSections,
	}, nil
}

func selectEnvelopeSections(envelope ExecutionEnvelope, names []string) map[string]interface{} {
	sections := map[string]interface{}{}
	values := map[string]interface{}{
		"run":                envelope.RunID,
		"stage":              envelope.StageID,
		"parentAgent":        envelope.ParentAgent,
		"objective":          envelope.Objective,
		"capabilities":       envelope.Capabilities,
		"platformPolicy":     envelope.PlatformPolicy,
		"completionContract": envelope.CompletionContract,
		"cancellation":       envelope.Cancellation,
		"budget":             envelope.Budget,
	}
	for _, name := range names {
		if value, ok := values[name]; ok {
			sections[name] = value
		}
	}
	return sections
}

func selectContextPointers(pointers []apiv1.ContextPointer, names []string) []apiv1.ContextPointer {
	selected := make([]apiv1.ContextPointer, 0, len(names))
	for _, pointer := range pointers {
		for _, name := range names {
			if pointer.Name == name {
				selected = append(selected, pointer)
				break
			}
		}
	}
	return selected
}

func copyInputs(in map[string]interface{}) map[string]interface{} {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]interface{}, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func copyContextPointers(in []apiv1.ContextPointer) []apiv1.ContextPointer {
	if len(in) == 0 {
		return nil
	}
	out := make([]apiv1.ContextPointer, len(in))
	copy(out, in)
	return out
}
