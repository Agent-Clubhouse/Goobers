package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/childworkflow"
	"github.com/goobers/goobers/internal/credentials"
	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/internal/gooberassets"
	"github.com/goobers/goobers/internal/harness"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/interactiveaccess"
	"github.com/goobers/goobers/internal/intervention"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/runner"
	"github.com/goobers/goobers/internal/workflow"
	"github.com/goobers/goobers/internal/worktree"
)

type interactiveRestartExecution struct {
	setup        *schedulerSetup
	layout       instance.Layout
	source       journal.RunIdentity
	gaggle       apiv1.Gaggle
	machine      *workflow.Machine
	goobers      map[string]apiv1.GooberSpec
	instructions map[string]string
	skills       map[string][]workflow.SkillFile
	assets       map[string]*gooberassets.Bundle
	models       map[string]func(context.Context) (string, error)
}

// buildInteractiveRestartExecution performs only offline pin/backend validation.
// Admission holds the policy lease; live credential access starts in Resume.
func (s *schedulerSetup) buildInteractiveRestartExecution(ctx context.Context, plan runner.StageRestartPlan) (intervention.Execution, error) {
	id := plan.Source
	id.RunID = plan.Continuation.RunID
	return s.buildInteractiveRestartIdentity(ctx, id)
}

func (s *schedulerSetup) buildInteractiveRestartIdentity(ctx context.Context, id journal.RunIdentity) (intervention.Execution, error) {
	if s == nil || s.Config == nil || s.InteractiveAccess == nil {
		return intervention.Execution{}, errors.New("interactive restart execution unavailable")
	}
	execution, err := s.loadInteractiveRestartExecution(ctx, id)
	if err != nil {
		return intervention.Execution{}, err
	}
	base, _ := s.RunnerRegistry.Resolve("", id.Gaggle, s.Runners[id.Gaggle])
	if base == nil {
		return intervention.Execution{}, errors.New("interactive restart has no configured local runner")
	}
	caps := map[string][]string{}
	for name, spec := range execution.goobers {
		caps[name] = slices.Clone(spec.Capabilities)
	}
	dedicated, err := base.ForStageRestartExecution(id, runner.StageRestartExecutionFactories{
		NewAgentic: execution.agentic, NewDeterministic: execution.deterministic, Context: execution.begin,
		RepositoryIdentity: execution.repositoryIdentity, GateCapabilities: caps, AdditionalRepos: execution.gaggle.Spec.AdditionalRepos,
	})
	return intervention.Execution{Runner: dedicated, Machine: execution.machine, GooberDigest: id.GooberDigest, RepoRef: execution.gaggle.Spec.Project}, err
}

func (s *schedulerSetup) loadInteractiveRestartExecution(ctx context.Context, source journal.RunIdentity) (*interactiveRestartExecution, error) {
	layout := instance.NewLayout(s.Root)
	owner, err := layout.ReadIdentity()
	if err != nil || owner != source.InstanceID {
		return nil, errors.New("interactive restart instance identity mismatch")
	}
	store, err := executionGenerationStore(layout)
	if err != nil {
		return nil, err
	}
	directory, lease, err := store.Acquire(ctx, source.ConfigGeneration)
	if err != nil {
		return nil, err
	}
	defer func() { _ = lease.Release() }()
	set, _, err := loadConfigDirectory(directory)
	if err != nil {
		return nil, err
	}
	selected, err := childSelectedConfig(set, childworkflow.ParentSelection{Gaggle: source.Gaggle, Workflow: source.Workflow})
	if err != nil {
		return nil, err
	}
	instance.ApplyGaggleCICommand(selected)
	instance.ApplyGaggleOutboxMirror(selected)
	raw := goobersByName(selected)
	admitted, err := admitChildValidationGoobers(s.Config, raw)
	if err != nil {
		return nil, err
	}
	machines, err := compileWorkflowMachines(selected, raw, admitted.HarnessNames, s.Config.ExternalTelemetryConnectorNames())
	if err != nil {
		return nil, err
	}
	machine := machines[localscheduler.WorkflowIdentity{Gaggle: source.Gaggle, Workflow: source.Workflow}]
	if machine == nil || machine.Digest() != source.WorkflowDigest {
		return nil, errors.New("interactive restart retained workflow mismatch")
	}
	instructions, err := loadGooberInstructions(directory, admitted.Goobers)
	if err != nil {
		return nil, err
	}
	skills, err := loadGooberSkillPackages(directory, source.Gaggle, admitted.Goobers)
	if err != nil {
		return nil, err
	}
	digest, err := workflow.ComputeGooberDigest(machine.Def, admitted.Goobers, instructions, skills)
	if err != nil || digest != source.GooberDigest {
		return nil, errors.New("interactive restart retained goober mismatch")
	}
	execution := &interactiveRestartExecution{setup: s, layout: layout.WithConfigDir(directory).ForGaggle(source.Gaggle), source: source, gaggle: selected.Gaggles[0], machine: machine, goobers: admitted.Goobers, instructions: instructions, skills: skills, assets: map[string]*gooberassets.Bundle{}, models: map[string]func(context.Context) (string, error){}}
	if err = execution.validateBackend(); err != nil {
		return nil, err
	}
	for name, spec := range execution.goobers {
		assets, err := gooberassets.Load(filepath.Join(gooberDefinitionDir(directory, spec, name), gooberassets.SourceDir))
		if err != nil {
			return nil, err
		}
		execution.assets[name] = assets
	}
	return execution, nil
}

type interactiveRestartContextKey struct{}
type interactiveRestartContext struct {
	lease     *interactiveaccess.ExecutionLease
	home      string
	execution *interactiveRestartExecution
	registrar credentials.SecretRegistrar
	proof     *invoke.WorkspaceQuiescence
}

func (e *interactiveRestartExecution) begin(ctx context.Context, id journal.RunIdentity, reg runner.SecretRegistrar) (context.Context, func(), error) {
	reader, err := journal.OpenRead(filepath.Join(e.layout.RunsDir(), id.RunID))
	if err != nil {
		return nil, nil, err
	}
	authority, err := interactiveaccess.LoadRestartAuthority(reader, id)
	if err != nil {
		return nil, nil, err
	}
	store, err := executionGenerationStore(e.layout)
	if err != nil {
		return nil, nil, err
	}
	_, pin, err := store.Acquire(ctx, id.ConfigGeneration)
	if err != nil {
		return nil, nil, err
	}
	lease, err := e.setup.InteractiveAccess.BeginExecution(ctx, authority.Principal(), id.Gaggle)
	if err != nil {
		_ = pin.Release()
		return nil, nil, err
	}
	if err = lease.RequireSources(e.gaggle.Spec.Project, e.gaggle.Spec.Backlog, e.gaggle.Spec.AdditionalRepos); err != nil {
		lease.Close()
		_ = pin.Release()
		return nil, nil, err
	}
	home, err := os.MkdirTemp("", "goobers-human-runtime-")
	if err != nil {
		lease.Close()
		_ = pin.Release()
		return nil, nil, err
	}
	ctx, proof := invoke.WithWorkspaceQuiescence(lease.Context())
	runtime := &interactiveRestartContext{lease: lease, home: home, execution: e, registrar: teeRegistrar{run: reg, shared: e.setup.SharedRegistry}, proof: proof}
	ctx = context.WithValue(ctx, interactiveRestartContextKey{}, runtime)
	ctx = worktree.WithGitExecution(ctx, worktree.GitExecution{Environment: runtime.gitEnvironment, Prepare: runtime.prepareGit})
	cleanup := func() {
		if proof.VerifyIdle() != nil {
			_ = lease.CloseAfter(proof.VerifyIdle())
			return
		}
		_ = os.RemoveAll(home)
		_ = pin.Release()
		lease.Close()
	}
	return ctx, cleanup, nil
}

func interactiveRuntime(ctx context.Context) (*interactiveRestartContext, error) {
	runtime, ok := ctx.Value(interactiveRestartContextKey{}).(*interactiveRestartContext)
	if !ok || runtime == nil {
		return nil, errors.New("interactive execution context is missing")
	}
	if err := runtime.lease.Context().Err(); err != nil {
		return nil, err
	}
	return runtime, nil
}

func interactiveOperationContext(ctx context.Context) (context.Context, func(), error) {
	runtime, err := interactiveRuntime(ctx)
	if err != nil {
		return nil, nil, err
	}
	child, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(runtime.lease.Context(), cancel)
	return child, func() { stop(); cancel() }, nil
}

func (e *interactiveRestartExecution) validateBackend() error {
	if e.source.Child != nil || e.source.EngineDriven() || e.machine.Def.DSLVersion != "3.1" {
		return errors.New("interactive restart supports only local DSL 3.1")
	}
	if instance.EffectiveAgenticSandbox(e.setup.Config, &e.gaggle) != instance.SandboxEnforced {
		return errors.New("interactive restart requires enforced local sandbox")
	}
	if len(instance.GuardedCredentialPaths(e.setup.Config)) != 0 {
		return harness.ErrGuardedCredentialFiles
	}
	for _, task := range e.machine.Def.Spec.Tasks {
		if err := validateInteractiveTask(task); err != nil {
			return err
		}
	}
	used := map[string]bool{}
	for _, task := range e.machine.Def.Spec.Tasks {
		used[task.Goober] = true
	}
	for _, gate := range e.machine.Def.Spec.Gates {
		if gate.RunsOn != nil {
			return errors.New("interactive restart reviewer placement is unavailable")
		}
		if gate.Agentic != nil {
			used[gate.Agentic.Goober] = true
		}
	}
	for name, spec := range e.goobers {
		if !used[name] {
			delete(e.goobers, name)
			continue
		}
		if err := e.validateGoober(name, spec); err != nil {
			return err
		}
	}
	return nil
}

func validateInteractiveCapabilities(caps []string) error {
	for _, cap := range caps {
		if _, _, ok := interactiveCapability(cap); !ok {
			return fmt.Errorf("interactive restart capability %q is unsupported", cap)
		}
	}
	return nil
}

// credentials.Resolver is deliberately backed only by the active human lease
// and the separately configured model key, never buildGaggleCredentials.
type interactiveCredentialResolver struct {
	execution *interactiveRestartExecution
	goober    string
	registrar credentials.SecretRegistrar
}

func validateInteractiveTask(task apiv1.Task) error {
	if task.RunsOn != nil {
		return errors.New("interactive restart remote placement is unavailable")
	}
	if task.ChildWorkflows != nil || task.Experiment != nil {
		return errors.New("interactive restart child delegation and experiments are unavailable")
	}
	if task.Type == apiv1.TaskDeterministic && (task.Inputs[executor.InputKind] != executor.KindCIPoll || task.Run == nil || task.Run.InjectRunContext || len(task.Run.Env) > 0) {
		return fmt.Errorf("interactive restart deterministic stage %q is not yet identity-bound", task.Name)
	}
	return validateInteractiveCapabilities(task.Capabilities)
}
func (e *interactiveRestartExecution) validateGoober(name string, spec apiv1.GooberSpec) error {
	if spec.Harness != apiv1.HarnessClaudeCode && spec.Harness != apiv1.HarnessCodex {
		return fmt.Errorf("interactive restart goober %q requires Claude Code or Codex API-key mode", name)
	}
	if len(spec.MCPServers) > 0 || harness.CodexUsesAmbientChatGPT(spec.HarnessOptions) {
		return errors.New("interactive restart forbids external MCP and ambient model login")
	}
	if len(e.setup.Config.Runner.HarnessCommand[string(spec.Harness)]) > 0 {
		return errors.New("interactive restart custom harness launchers are unavailable")
	}
	if err := validateInteractiveCapabilities(spec.Capabilities); err != nil {
		return err
	}
	grant, _ := agentModelGrant(e.setup.Config, spec.Harness)
	if grant == nil || grant.GitHubApp != nil || grant.Token.GitHubCLI != nil {
		return errors.New("interactive restart requires an explicit model API-key credential")
	}
	resolver, _, err := agentModelCredentialResolver(e.setup.Config, e.setup.SecretStores, spec.Harness)
	if err != nil {
		return err
	}
	e.models[name] = resolver
	return nil
}
