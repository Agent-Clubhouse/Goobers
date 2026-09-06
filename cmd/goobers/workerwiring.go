package main

import (
	"context"
	"fmt"
	"log"
	"maps"
	"sync"
	"sync/atomic"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/blobstore"
	"github.com/goobers/goobers/internal/engine"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/runner"
	"github.com/goobers/goobers/internal/secretstore"
	"github.com/goobers/goobers/internal/workerhost"
	"github.com/goobers/goobers/internal/workflow"
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
	// logf receives reload diagnostics. A rejected reload is loud but never
	// fatal — the worker keeps serving from its last-known-good snapshot —
	// so it needs somewhere to say so that is not the failed stage's error.
	logf func(format string, args ...any)

	// snapshot is the WHOLE config-tree view every seam resolves against,
	// swapped as one pointer so a concurrent reader observes either the whole
	// old tree or the whole new one and never a mixture (#3884). Everything it
	// points at — including its gaggle map — is immutable once published.
	snapshot atomic.Pointer[workerConfigSnapshot]
	// history holds the snapshots this worker has superseded, most-recent
	// first, bounded to historyDepth. It is what lets an attempt of a run
	// pinned to an older goober digest still be served ITS OWN kit after a
	// reload rolled the tree forward, instead of being handed the new
	// instructions or refused outright (#3884).
	//
	// Bounded on purpose, and small: a worker that retained every tree it had
	// ever read would grow without limit in the one process that must not,
	// and would keep alive kits — credential resolvers, worktree managers —
	// for trees no live run can still be pinned to. Past the bound the pin
	// FAILS CLOSED with a named refusal rather than silently resolving the
	// current tree. Guarded by mu.
	history []*workerConfigSnapshot
	// historyDepth bounds history. Zero disables retention entirely, which
	// makes any pin the current tree cannot satisfy an immediate refusal.
	historyDepth int
	// lastPinRefusal dedupes a persisting gate_pin_missing/run_pin_unverifiable
	// refusal per (gaggle, workflow): #4153 found this refusal previously never
	// logged at all, so a worker whose config tree has no writer to ever bring
	// the pinned tree into force (rather than merely being one reload behind)
	// retried the same refusal forever with nothing an operator could act on.
	// Logging only the FIRST occurrence of each distinct expected digest — and
	// clearing the entry once that digest resolves — turns the invisible
	// infinite retry into a surfaced, alertable condition without spamming the
	// log once per retried attempt. Guarded by mu.
	lastPinRefusal map[localscheduler.WorkflowIdentity]string
	// mu serializes the two writers that publish a snapshot: forGaggle's lazy
	// seam construction and the config watcher's reload. Both read the current
	// pointer, derive a successor, and store it, so neither may interleave
	// with the other. It also guards history and lastPinRefusal.
	mu sync.Mutex
}

// workerConfigSnapshot is one whole, self-consistent view of the worker's
// config tree: the digest the rest of it was read at, the parsed instance
// config and config directory, and the per-gaggle seams built from THAT tree.
//
// It exists because the worker used to cache seams per gaggle against a
// boot-time snapshot it never re-read (I-51: a worker one Workflows revision
// behind the daemon resolved credentials against a stale gaggle for 32
// minutes). Reload replaces this value wholesale rather than mutating the
// cache in place, which is what lets an in-flight attempt keep the kit it was
// handed while the next attempt gets the new tree.
type workerConfigSnapshot struct {
	digest string
	cfg    *instance.Config
	set    *instance.ConfigSet
	// instructions holds every configured goober's instruction body, and
	// skillPackages every gaggle's resolved skill files, read ONCE when this
	// snapshot was taken.
	//
	// They are captured rather than re-read because a snapshot must stay
	// answerable after the tree on disk has moved past it (#3884). The
	// worker retains a bounded history of superseded snapshots so a run
	// pinned to an older goober digest can still be served its own kit across
	// a reload; a history entry that went back to disk for its instruction
	// bytes would resolve the CURRENT tree while claiming to be the old one,
	// which is the silent substitution the pin exists to prevent.
	instructions  map[string]string
	skillPackages map[string]map[string][]workflow.SkillFile
	// digests is the lazily-computed (gaggle, workflow) → goober-digest index
	// for THIS tree — the value a run's pinned GooberDigest is matched
	// against. Shared by pointer across withGaggle copies so the compile is
	// paid at most once per snapshot.
	digests *gooberDigestIndex
	// gaggles holds the seams already built from this snapshot's tree. The map
	// is never written after publication: both writers publish a copy.
	gaggles map[string]*builtGaggleSeams
}

// builtGaggleSeams pairs a gaggle's constructed seams with the fingerprint of
// the config inputs they were built from, so a reload can tell whether a new
// tree actually changed anything this gaggle reads.
type builtGaggleSeams struct {
	seams       *gaggleSeams
	fingerprint string
}

// withGaggle returns a copy of the snapshot carrying one more built gaggle.
func (s *workerConfigSnapshot) withGaggle(gaggle string, built *builtGaggleSeams) *workerConfigSnapshot {
	next := &workerConfigSnapshot{
		digest:        s.digest,
		cfg:           s.cfg,
		set:           s.set,
		instructions:  s.instructions,
		skillPackages: s.skillPackages,
		digests:       s.digests,
		gaggles:       make(map[string]*builtGaggleSeams, len(s.gaggles)+1),
	}
	maps.Copy(next.gaggles, s.gaggles)
	next.gaggles[gaggle] = built
	return next
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
		root:         root,
		scrubber:     scrub,
		shared:       shared,
		store:        store,
		logf:         log.Printf,
		historyDepth: workerConfigHistoryDepth,
	}, nil
}

// SharedRegistry exposes the instance-global exact-value secret registry every
// executor this process builds registers resolved credentials into. It is what
// the #2931 dispatch canary asserts serialized envelopes against.
func (w *workerSeams) SharedRegistry() *journal.RegistryScrubber { return w.shared }

// forGaggle builds (once per config tree) the runner config for a gaggle from
// the CURRENT tree. Agentic stages go through forPinnedGaggle instead, which
// resolves the tree the run was admitted against (#3884); this is the
// unpinned path every other seam takes.
//
// It builds (once per config tree) the runner config for a gaggle,
// reusing the daemon's own wiring so the worker's executors are configured
// identically to tier 1 — same credential grants, same env allowlist, same
// stage timeouts, same instance root for goobers-CLI stages.
//
// The result is cached in the CURRENT config snapshot. A reload that changes
// what this gaggle reads drops the cached entry, so the next attempt builds
// against the new tree; an attempt already holding a *gaggleSeams keeps the
// exact kit it was handed, because nothing here ever mutates a published one.
func (w *workerSeams) forGaggle(gaggle string) (*gaggleSeams, error) {
	// Fast path: no lock, no config read. The published snapshot is immutable,
	// so a hit here is a whole-tree-consistent kit.
	if snapshot := w.snapshot.Load(); snapshot != nil {
		if built, ok := snapshot.gaggles[gaggle]; ok {
			return built.seams, nil
		}
	}

	w.mu.Lock()
	defer w.mu.Unlock()
	snapshot, err := w.currentSnapshotLocked()
	if err != nil {
		return nil, err
	}
	if built, ok := snapshot.gaggles[gaggle]; ok {
		return built.seams, nil
	}
	built, err := w.buildGaggleSeams(snapshot, gaggle)
	if err != nil {
		return nil, err
	}
	w.snapshot.Store(snapshot.withGaggle(gaggle, built))
	return built.seams, nil
}

// buildGaggleSeams constructs one gaggle's executors from a snapshot's tree.
// Callers must hold w.mu.
//
// Everything it reads about CONFIG comes from the snapshot, never from disk:
// that is what makes a snapshot answerable after the tree has moved, which is
// what history retention and the digest pin both rest on (#3884). What it
// still reads from the environment — harness binaries, secret stores — is
// deliberately not config-tree state.
func (w *workerSeams) buildGaggleSeams(snapshot *workerConfigSnapshot, gaggle string) (*builtGaggleSeams, error) {
	l := instance.NewLayout(w.root)
	cfg, set := snapshot.cfg, snapshot.set

	goobers, err := resolveGoobersForGaggle(set, gaggle)
	if err != nil {
		return nil, err
	}
	if err := validateStoredCopilotAuthBoundaries(cfg, set, goobers); err != nil {
		return nil, fmt.Errorf("worker: credential admission: %w", err)
	}
	instructions, err := snapshot.instructionsFor(goobers)
	if err != nil {
		return nil, fmt.Errorf("worker: load goober instructions: %w", err)
	}
	fingerprint, err := gaggleConfigFingerprint(cfg, set, gaggle, goobers, instructions)
	if err != nil {
		return nil, fmt.Errorf("worker: fingerprint gaggle %q config: %w", gaggle, err)
	}
	stores, err := secretstore.NewRegistry(cfg.SecretStores)
	if err != nil {
		return nil, fmt.Errorf("worker: secret stores: %w", err)
	}
	modelCredential, err := agentModelCredentialResolver(cfg, stores)
	if err != nil {
		return nil, fmt.Errorf("worker: agent:model credential: %w", err)
	}
	harnessInfo, err := preflightHarnesses(goobers, set.Workflows, cfg.Runner.EnvPassthrough, cfg.Runner.HarnessCommand, modelCredential)
	if err != nil {
		return nil, fmt.Errorf("worker: harness preflight: %w", err)
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
	return &builtGaggleSeams{
		seams:       &gaggleSeams{cfg: runnerCfg, runsDir: scoped.RunsDir(), manager: credentialedMgr},
		fingerprint: fingerprint,
	}, nil
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
	// Store is the same --blob-store the worker's artifact recorder writes
	// through: the RWX volume the daemon's blob plane serves pods from, so a
	// bundle a pod PUT is what this provisioner GETs (#3803), and vice versa.
	delegate := &workerhost.WorktreeWorkspaces{Manager: g.manager, ScratchDir: p.scratchRoot, Store: p.seams.store}
	return delegate.Provision(ctx, req)
}

// Run executes a deterministic stage against the worker's CURRENT config tree,
// deliberately unpinned.
//
// GooberDigest is the content identity of a goober KIT — resolved goober
// specs, instruction bodies, skill packages (workflow.ComputeGooberDigest).
// A deterministic stage executes none of them: it runs a declared command
// under the gaggle's credentials. Refusing it on a kit pin would fail stages
// over a fact they do not read, and pinning it to a retained tree would run
// commands from a config the operator has already replaced. The staleness that
// DID hurt these stages — credentials resolved against a superseded gaggle,
// Infra LEDGER I-51 — is what the config reload (#3912) fixed, and they stay
// on the reloaded current tree for exactly that reason.
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
	// Resolve the gaggle's kit ONCE for the whole call, BY THE RUN'S PIN.
	// Resolving again for materialize would let a reload landing
	// mid-invocation hand the same attempt two different trees; resolving
	// without the pin would let a reload landing between two attempts hand
	// the same RUN two different curators (#3884).
	g, err := a.seams.forPinnedGaggle(env.Gaggle, env.WorkflowID, env.GooberDigest)
	if err != nil {
		return apiv1.ResultEnvelope{}, err
	}
	exec, err := a.executor(g, env)
	if err != nil {
		return apiv1.ResultEnvelope{}, err
	}
	if err := a.seams.materialize(ctx, g, env); err != nil {
		return apiv1.ResultEnvelope{}, err
	}
	return exec.Invoke(ctx, env)
}

func (a workerGoober) Review(ctx context.Context, env apiv1.InvocationEnvelope) (apiv1.Verdict, error) {
	g, err := a.seams.forPinnedGaggle(env.Gaggle, env.WorkflowID, env.GooberDigest)
	if err != nil {
		return apiv1.Verdict{}, err
	}
	exec, err := a.executor(g, env)
	if err != nil {
		return apiv1.Verdict{}, err
	}
	if err := a.seams.materialize(ctx, g, env); err != nil {
		return apiv1.Verdict{}, err
	}
	return exec.Review(ctx, env)
}

func (a workerGoober) executor(g *gaggleSeams, env apiv1.InvocationEnvelope) (invoke.Goober, error) {
	if env.Goober == "" {
		// Fail closed and say why. Before the envelope carried a goober name
		// this seam could not route at all, which is the gap that blocked the
		// whole wiring slice.
		return nil, fmt.Errorf("worker: envelope for run %q stage %q carries no goober name", env.RunID, env.TaskID)
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
