package engine

import (
	"context"
	"errors"
	"fmt"
	"strings"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/invoke"
)

// Activity names. The workflow refers to activities by these names so it is
// decoupled from the concrete receiver instance; they must equal the method
// names on Activities exactly (Temporal registers struct methods by name).
const (
	ActInvokeGoober      = "InvokeGoober"
	ActReviewGoober      = "ReviewGoober"
	ActRunDeterministic  = "RunDeterministic"
	ActEvaluateAutomated = "EvaluateAutomated"
)

// Activities bundles the engine's side-effecting operations as Temporal
// activities. Each seam (defined in package invoke) is optional; a nil seam
// yields a clear "not configured" error if the workflow reaches a node that needs
// it, rather than a panic. The runtime (M8) constructs this with a real
// invoke.Goober.
type Activities struct {
	Goober invoke.Goober
	Det    invoke.Deterministic
	Auto   invoke.Automated
	// Workspaces provisions the fresh working copy each stage attempt runs
	// in. Required for any stage that executes in a workspace (agentic tasks,
	// deterministic tasks, agentic reviewer gates); an automated gate's checks
	// are pure functions over env.Inputs and get no workspace, matching the
	// local runner (#112).
	Workspaces WorkspaceProvisioner
}

// ErrNotConfigured is returned by an activity whose backing seam was not wired.
var ErrNotConfigured = errors.New("engine: activity dependency not configured")

// provisionWorkspace provisions the working copy for one stage attempt and
// stamps its path into env's required workspace field. It fails closed
// (#621/#156): a missing provisioner, a provision failure, or an empty path
// is an error — the stage never dispatches with a partial envelope, which is
// what previously made every capability-scoped credential fail closed the
// moment a real executor was wired.
func (a *Activities) provisionWorkspace(ctx context.Context, env *apiv1.InvocationEnvelope, mode apiv1.WorkspaceMode) (Workspace, error) {
	if a.Workspaces == nil {
		return nil, fmt.Errorf("stage %q requires a workspace but no provisioner is wired: %w", env.TaskID, ErrNotConfigured)
	}
	ws, err := a.Workspaces.Provision(ctx, WorkspaceRequest{
		RunID:   env.RunID,
		Stage:   strings.TrimPrefix(env.TaskID, env.RunID+":"),
		Gaggle:  env.Gaggle,
		RepoRef: env.RepoRef,
		Mode:    mode,
	})
	if err != nil {
		return nil, fmt.Errorf("provision workspace for stage %q: %w", env.TaskID, err)
	}
	if ws == nil || ws.Path() == "" {
		removeWorkspace(ctx, ws)
		return nil, fmt.Errorf("workspace provisioner returned no path for stage %q (the closed invocation schema requires workspace)", env.TaskID)
	}
	env.Workspace = ws.Path()
	return ws, nil
}

// removeWorkspace tears one attempt's working copy down. Best-effort by
// design: a teardown failure never overrides the stage's own result/error
// (the local runner's additive removeErr contract, issue #136); until the
// history→journal projection (#629) exists there is no journal to surface it
// to. Detached from ctx so an already-expired attempt still cleans up.
func removeWorkspace(ctx context.Context, ws Workspace) {
	if ws == nil {
		return
	}
	_ = ws.Remove(context.WithoutCancel(ctx))
}

// InvokeGoober executes an agentic task.
func (a *Activities) InvokeGoober(ctx context.Context, env apiv1.InvocationEnvelope) (apiv1.ResultEnvelope, error) {
	if a.Goober == nil {
		return apiv1.ResultEnvelope{}, ErrNotConfigured
	}
	ws, err := a.provisionWorkspace(ctx, &env, apiv1.WorkspaceRepo)
	if err != nil {
		return apiv1.ResultEnvelope{}, err
	}
	defer removeWorkspace(ctx, ws)
	return a.Goober.Invoke(ctx, env)
}

// ReviewGoober executes an agentic reviewer gate. Like the local runner, the
// reviewer runs a real goober subprocess and therefore gets a repository
// workspace (unlike an automated gate).
func (a *Activities) ReviewGoober(ctx context.Context, env apiv1.InvocationEnvelope) (apiv1.Verdict, error) {
	if a.Goober == nil {
		return apiv1.Verdict{}, ErrNotConfigured
	}
	ws, err := a.provisionWorkspace(ctx, &env, apiv1.WorkspaceRepo)
	if err != nil {
		return apiv1.Verdict{}, err
	}
	defer removeWorkspace(ctx, ws)
	return a.Goober.Review(ctx, env)
}

// RunDeterministic executes a deterministic task in the workspace mode the
// task's run block declares (repo by default, scratch on request).
func (a *Activities) RunDeterministic(ctx context.Context, env apiv1.InvocationEnvelope, run apiv1.DeterministicRun) (apiv1.ResultEnvelope, error) {
	if a.Det == nil {
		return apiv1.ResultEnvelope{}, ErrNotConfigured
	}
	ws, err := a.provisionWorkspace(ctx, &env, run.Workspace)
	if err != nil {
		return apiv1.ResultEnvelope{}, err
	}
	defer removeWorkspace(ctx, ws)
	return a.Det.Run(ctx, env, run)
}

// EvaluateAutomated runs an automated gate check. Automated gates are pure
// functions over env.Inputs and never receive a workspace, matching the local
// runner (#112) — no provisioning here.
func (a *Activities) EvaluateAutomated(ctx context.Context, gate apiv1.AutomatedGate, env apiv1.InvocationEnvelope) (string, error) {
	if a.Auto == nil {
		return "", ErrNotConfigured
	}
	return a.Auto.Evaluate(ctx, gate, env)
}
