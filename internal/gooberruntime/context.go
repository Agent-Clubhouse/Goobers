// Package gooberruntime implements the runtime side of the neutral invoke.Goober
// boundary used by the workflow engine.
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
	TaskID          string                 `json:"taskId"`
	WorkflowID      string                 `json:"workflowId"`
	RunID           string                 `json:"runId"`
	Gaggle          string                 `json:"gaggle"`
	Goal            string                 `json:"goal"`
	Instructions    string                 `json:"instructions,omitempty"`
	RepoRef         apiv1.RepoRef          `json:"repoRef"`
	Item            *apiv1.BacklogItem     `json:"item,omitempty"`
	Inputs          map[string]interface{} `json:"inputs,omitempty"`
	ContextPointers []apiv1.ContextPointer `json:"contextPointers,omitempty"`
	Limits          apiv1.Limits           `json:"limits,omitempty"`
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
	if resolver == nil {
		resolver = InputInstructionResolver{}
	}
	instructions, err := resolver.ResolveInstructions(ctx, env)
	if err != nil {
		return GooberContext{}, err
	}
	return GooberContext{
		TaskID:          env.TaskID,
		WorkflowID:      env.WorkflowID,
		RunID:           env.RunID,
		Gaggle:          env.Gaggle,
		Goal:            env.Goal,
		Instructions:    instructions,
		RepoRef:         env.RepoRef,
		Item:            env.Item,
		Inputs:          copyInputs(env.Inputs),
		ContextPointers: copyContextPointers(env.ContextPointers),
		Limits:          env.Limits,
	}, nil
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
