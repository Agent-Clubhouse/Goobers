package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

// InvokeGooberExact restores exact JSON map-item inputs before invoking a goober.
func (a *Activities) InvokeGooberExact(
	ctx context.Context,
	env apiv1.InvocationEnvelope,
	workspaceBranch string,
	workspaceDelta string,
	workspace apiv1.WorkspaceMode,
	onTimeout string,
	exact map[string]json.RawMessage,
) (stageActivityResult, error) {
	if err := applyExactInputs(&env, exact); err != nil {
		return stageActivityResult{}, err
	}
	return a.InvokeGoober(ctx, env, workspaceBranch, workspaceDelta, workspace, onTimeout)
}

// RunDeterministicExact restores exact JSON map-item inputs before deterministic execution.
func (a *Activities) RunDeterministicExact(
	ctx context.Context,
	env apiv1.InvocationEnvelope,
	run apiv1.DeterministicRun,
	workspaceBranch string,
	workspaceDelta string,
	exact map[string]json.RawMessage,
) (stageActivityResult, error) {
	if err := applyExactInputs(&env, exact); err != nil {
		return stageActivityResult{}, err
	}
	return a.RunDeterministic(ctx, env, run, workspaceBranch, workspaceDelta)
}

func applyExactInputs(env *apiv1.InvocationEnvelope, exact map[string]json.RawMessage) error {
	for name, raw := range exact {
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.UseNumber()
		var value any
		if err := decoder.Decode(&value); err != nil {
			return fmt.Errorf("engine: decode exact input %q: %w", name, err)
		}
		var trailing any
		if err := decoder.Decode(&trailing); err != io.EOF {
			return fmt.Errorf("engine: decode exact input %q: trailing JSON value", name)
		}
		if env.Inputs == nil {
			env.Inputs = make(map[string]any)
		}
		env.Inputs[name] = value
	}
	return nil
}
