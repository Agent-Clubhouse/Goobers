package main

import (
	"context"
	"fmt"
	"sync"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/blobstore"
	"github.com/goobers/goobers/internal/engine"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/runner"
	"github.com/goobers/goobers/internal/secretstore"
	"github.com/goobers/goobers/internal/workerhost"
	"github.com/goobers/goobers/internal/worktree"
)

// workerSeams supplies the engine's agentic and deterministic executors inside
// `goobers worker`.
//
// WHY AN ADAPTER RATHER THAN A RESHAPE OF EngineDeps.
//
// bootstrap.EngineDeps holds single interface values — one Goober, one Det —
// constructed once per process. The local runner's equivalent seams are per-RUN
// FACTORIES (runner.NewDeterministicFunc / NewAgenticFunc) because an executor
// binds its ArtifactRecorder at construction time and must not be shared across
// runs. Those two shapes do not meet.
//
// They do not have to. Every activity hands the seam an InvocationEnvelope, and
// the envelope carries RunID and Gaggle — so one long-lived value can dispatch
// to a per-run executor on each call. That keeps the change additive: neither
// internal/engine nor internal/bootstrap is touched, and the executors used are
// the SAME ones the local runner builds, from the same buildRunnerConfig, which
// is what conformance between the two tiers rests on.
type workerSeams struct {
	root     string
	scrubber journal.Scrubber
	shared   *journal.RegistryScrubber
	// store is the fleet-wide content-addressed store stage artifacts travel
	// through. Nil means node-local only: every stage of a run must then be
	// polled by THIS worker or the first cross-node pointer fails closed.
	store blobstore.Store

	mu       sync.Mutex
	byGaggle map[string]*gaggleSeams
}

type gaggleSeams struct {
	cfg     runner.Config
	runsDir string
	// manager is buildRunnerConfig's own worktree manager — the CREDENTIALED
	// one. workerEngineDeps builds a bare manager with no git auth, which
	// clones fine for a public repo and fails on a private one with
	// "could not read Username for 'https://github.com'". Workspace
	// provisioning has to use this one instead.
	manager *worktree.Manager
}

// newWorkerSeams loads an instance from root and prepares per-gaggle executor
// factories. It fails closed: a worker that cannot construct the same executors
// the local runner would is a worker that will fail every real stage at
// dispatch, and it is better to know that at startup.
func newWorkerSeams(root string, store blobstore.Store) (*workerSeams, error) {
	l := instance.NewLayout(root)
	if _, err := instance.LoadConfig(l.ConfigFile()); err != nil {
		return nil, fmt.Errorf("worker: load instance config: %w", err)
	}
	shared, scrub := journal.DefaultScrubber()
	return &workerSeams{
		root:     root,
		scrubber: scrub,
		shared:   shared,
		store:    store,
		byGaggle: map[string]*gaggleSeams{},
	}, nil
}

// forGaggle builds (once) the runner config for a gaggle, reusing the daemon's
// own wiring so the worker's executors are configured identically to tier 1 —
// same credential grants, same env allowlist, same stage timeouts, same
// instance root for goobers-CLI stages.
func (w *workerSeams) forGaggle(gaggle string) (*gaggleSeams, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if g, ok := w.byGaggle[gaggle]; ok {
		return g, nil
	}

	l := instance.NewLayout(w.root)
	cfg, err := instance.LoadConfig(l.ConfigFile())
	if err != nil {
		return nil, fmt.Errorf("worker: load instance config: %w", err)
	}
	// The validation report travels with the error rather than being dropped.
	// A worker that executed a stage against config carrying known problems,
	// silently, is worse than one that refuses and says which problems — and
	// this seam has no terminal to print to, so the issues are folded into the
	// returned error where the activity failure will carry them.
	// TestCallersDoNotDiscardConfigReports enforces this, and caught it here.
	set, report, err := loadConfigDirectory(l.ConfigDir())
	if err != nil {
		if issues := validationIssueSummary(report); issues != "" {
			return nil, fmt.Errorf("worker: load config directory: %w (%s)", err, issues)
		}
		return nil, fmt.Errorf("worker: load config directory: %w", err)
	}
	instance.ApplyGaggleCICommand(set)
	instance.ApplyGaggleOutboxMirror(set)

	goobers, err := resolveGoobersForGaggle(set, gaggle)
	if err != nil {
		return nil, err
	}
	instructions, err := loadGooberInstructions(l.ConfigDir(), goobers)
	if err != nil {
		return nil, fmt.Errorf("worker: load goober instructions: %w", err)
	}
	harnessInfo, err := preflightHarnesses(goobers, set.Workflows, cfg.Runner.EnvPassthrough, cfg.Runner.HarnessCommand)
	if err != nil {
		return nil, fmt.Errorf("worker: harness preflight: %w", err)
	}
	stores, err := secretstore.NewRegistry(cfg.SecretStores)
	if err != nil {
		return nil, fmt.Errorf("worker: secret stores: %w", err)
	}

	scoped := l.ForGaggle(gaggle)
	project := gaggleProjectRef(set, gaggle)
	runnerCfg, credentialedMgr, err := buildRunnerConfig(runnerCompositionInput{
		Layout:               scoped,
		Config:               cfg,
		Goobers:              goobers,
		InstructionsByGoober: instructions,
		// The worker's spans come from the engine, not this client.
		Telemetry: nil,
		// nil manager ON PURPOSE. buildRunnerConfig builds its own only when
		// this is nil, and that is the branch that attaches the git
		// environment — WithGitEnvironment, the askpass resolver that
		// authenticates mirror clone/fetch with the repo's configured
		// credential (#667). Passing a manager in SUPPRESSES it, which is how
		// the first engine dispatches failed: a bare manager clones a public
		// repo happily and dies on a private one with "could not read Username
		// for 'https://github.com'".
		SharedRegistry:   w.shared,
		WorktreeManager:  nil,
		BranchNamespaces: branchNamespacesByGaggle(set),
		GaggleProject:    project,
		HarnessInfo:      harnessInfo,
		CredentialStores: stores,
		SandboxPosture:   instance.EffectiveAgenticSandbox(cfg, nil),
		// Provider quota is a scheduler-side concern, not the executor's.
		ProviderQuota: nil,
	})
	if err != nil {
		return nil, fmt.Errorf("worker: build runner config for gaggle %q: %w", gaggle, err)
	}

	if credentialedMgr == nil {
		return nil, fmt.Errorf("worker: buildRunnerConfig returned no worktree manager for gaggle %q", gaggle)
	}
	g := &gaggleSeams{cfg: runnerCfg, runsDir: scoped.RunsDir(), manager: credentialedMgr}
	w.byGaggle[gaggle] = g
	return g, nil
}

// recorderFor returns the artifact recorder and secret registrar for one run.
// See internal/workerhost.StagingArtifacts for why this is not the run's
// journal: the worker did not mint the run, cannot author its identity without
// inventing conformance-normative fields, and must not create a directory the
// engine's projection will later try to create itself.
func (w *workerSeams) recorderFor(g *gaggleSeams, runID string) (runner.ArtifactRecorder, runner.SecretRegistrar) {
	dir := workerhost.StagingArtifactsDir(g.runsDir, runID)
	return workerhost.NewStagingArtifacts(dir, w.scrubber, w.store), w.shared
}

// materialize fetches every context blob this node does not already hold, so
// the harness's local Resolve finds them. It is the fetch half of the data
// plane; the recorder's write-through is the other half. Called at the top of
// every stage seam, because a stage cannot know whether its predecessor ran
// here — and with a shared store it no longer has to.
func (w *workerSeams) materialize(ctx context.Context, g *gaggleSeams, env apiv1.InvocationEnvelope) error {
	dir := workerhost.StagingArtifactsDir(g.runsDir, env.RunID)
	return workerhost.MaterializeContext(ctx, w.store, dir, env.ContextPointers)
}

// Deterministic returns the engine's deterministic seam.
func (w *workerSeams) Deterministic() invoke.Deterministic { return workerDet{seams: w} }

// Agentic returns the engine's agentic seam.
func (w *workerSeams) Agentic() invoke.Goober { return workerGoober{seams: w} }

// Workspaces returns the engine's workspace provisioner, dispatching per
// gaggle to that gaggle's CREDENTIALED worktree manager.
//
// This matters more than it looks. workerEngineDeps builds a worktree manager
// with no git environment at all — no GIT_ASKPASS, no per-repo token — because
// at that point the worker has no instance and therefore no credentials. It
// clones a public repo happily and fails a private one with
//
//	could not read Username for 'https://github.com': No such device or address
//
// which reads like a missing tty and is actually a missing token. Observed on
// the first real engine dispatch (run 018119aa…), where the activity reached
// the worker correctly and died provisioning its workspace.
func (w *workerSeams) Workspaces(scratchRoot string) engine.WorkspaceProvisioner {
	return &workerWorkspaces{seams: w, scratchRoot: scratchRoot}
}

type workerWorkspaces struct {
	seams       *workerSeams
	scratchRoot string
}

func (p *workerWorkspaces) Provision(ctx context.Context, req engine.WorkspaceRequest) (engine.Workspace, error) {
	g, err := p.seams.forGaggle(req.Gaggle)
	if err != nil {
		return nil, err
	}
	delegate := &workerhost.WorktreeWorkspaces{Manager: g.manager, ScratchDir: p.scratchRoot}
	return delegate.Provision(ctx, req)
}

type workerDet struct{ seams *workerSeams }

func (d workerDet) Run(ctx context.Context, env apiv1.InvocationEnvelope, run apiv1.DeterministicRun) (apiv1.ResultEnvelope, error) {
	g, err := d.seams.forGaggle(env.Gaggle)
	if err != nil {
		return apiv1.ResultEnvelope{}, err
	}
	if g.cfg.NewDeterministic == nil {
		return apiv1.ResultEnvelope{}, fmt.Errorf("worker: no deterministic executor configured for gaggle %q", env.Gaggle)
	}
	if err := d.seams.materialize(ctx, g, env); err != nil {
		return apiv1.ResultEnvelope{}, err
	}
	rec, reg := d.seams.recorderFor(g, env.RunID)
	exec, err := g.cfg.NewDeterministic(rec, reg)
	if err != nil {
		return apiv1.ResultEnvelope{}, fmt.Errorf("worker: construct deterministic executor: %w", err)
	}
	return exec.Run(ctx, env, run)
}

type workerGoober struct{ seams *workerSeams }

func (a workerGoober) Invoke(ctx context.Context, env apiv1.InvocationEnvelope) (apiv1.ResultEnvelope, error) {
	exec, err := a.executor(env)
	if err != nil {
		return apiv1.ResultEnvelope{}, err
	}
	if err := a.materialize(ctx, env); err != nil {
		return apiv1.ResultEnvelope{}, err
	}
	return exec.Invoke(ctx, env)
}

func (a workerGoober) Review(ctx context.Context, env apiv1.InvocationEnvelope) (apiv1.Verdict, error) {
	exec, err := a.executor(env)
	if err != nil {
		return apiv1.Verdict{}, err
	}
	if err := a.materialize(ctx, env); err != nil {
		return apiv1.Verdict{}, err
	}
	return exec.Review(ctx, env)
}

func (a workerGoober) materialize(ctx context.Context, env apiv1.InvocationEnvelope) error {
	g, err := a.seams.forGaggle(env.Gaggle)
	if err != nil {
		return err
	}
	return a.seams.materialize(ctx, g, env)
}

func (a workerGoober) executor(env apiv1.InvocationEnvelope) (invoke.Goober, error) {
	if env.Goober == "" {
		// Fail closed and say why. Before the envelope carried a goober name
		// this seam could not route at all, which is the gap that blocked the
		// whole wiring slice.
		return nil, fmt.Errorf("worker: envelope for run %q stage %q carries no goober name", env.RunID, env.TaskID)
	}
	g, err := a.seams.forGaggle(env.Gaggle)
	if err != nil {
		return nil, err
	}
	if g.cfg.NewAgentic == nil {
		return nil, fmt.Errorf("worker: no agentic executor configured for gaggle %q", env.Gaggle)
	}
	rec, reg := a.seams.recorderFor(g, env.RunID)
	exec, err := g.cfg.NewAgentic(env.Goober, rec, reg)
	if err != nil {
		return nil, fmt.Errorf("worker: construct agentic executor for goober %q: %w", env.Goober, err)
	}
	return exec, nil
}

// gaggleProjectRef is the gaggle's project repo, zero when not configured —
// which leaves credentials on the first-repo default, matching the daemon's
// legacy-runtime path.
func gaggleProjectRef(set *instance.ConfigSet, gaggle string) apiv1.RepoRef {
	for i := range set.Gaggles {
		if set.Gaggles[i].Name == gaggle {
			return set.Gaggles[i].Spec.Project
		}
	}
	return apiv1.RepoRef{}
}

// resolveGoobersForGaggle returns the goober specs a gaggle's stages may name.
func resolveGoobersForGaggle(set *instance.ConfigSet, gaggle string) (map[string]apiv1.GooberSpec, error) {
	out := map[string]apiv1.GooberSpec{}
	for i := range set.Goobers {
		g := set.Goobers[i]
		if g.Spec.Gaggle == "" || g.Spec.Gaggle == gaggle {
			out[g.Name] = g.Spec
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("worker: no goobers configured for gaggle %q", gaggle)
	}
	return out, nil
}
