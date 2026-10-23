// Package startinput reconstructs the pinned engine.RunInput for a cloud
// acceptance. It is the cloud counterpart of the local daemon's start-spec
// assembly (deleted with the mode-3 engine path, #7783): one place that turns a
// durable start envelope plus the generation it pinned into the run's immutable
// input, so a field added to engine.RunInput has exactly one place to learn it.
//
// The acceptance already carries the admitted generation and the workflow and
// goober digests (internal/cloud/startpin). Resolve re-derives everything else
// from that retained generation and refuses on any drift, so a start can never
// run against a workflow other than the one that was admitted.
package startinput

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/bootstrap"
	"github.com/goobers/goobers/internal/childworkflow"
	"github.com/goobers/goobers/internal/cloud/acceptance"
	"github.com/goobers/goobers/internal/cloud/startpin"
	"github.com/goobers/goobers/internal/engine"
	"github.com/goobers/goobers/internal/engineadmission"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/providerconfig"
	"github.com/goobers/goobers/internal/runcontrol"
	"github.com/goobers/goobers/internal/runtimewiring"
	"github.com/goobers/goobers/internal/startintent"
	"github.com/goobers/goobers/internal/triggerqueue"
	"github.com/goobers/goobers/internal/workflow"
)

// permanent wraps a refusal that can never succeed on retry so the controller
// finishes the acceptance as rejected (acceptance.ErrPermanentStart).
func permanent(format string, args ...any) error {
	return fmt.Errorf("%w: %w", acceptance.ErrPermanentStart, fmt.Errorf(format, args...))
}

// ErrPinDrift means the retained generation no longer compiles to the digests
// the acceptance pinned. The start is refused; it is never re-pinned.
var ErrPinDrift = fmt.Errorf("%w: cloud start: retained generation differs from the pinned digests", acceptance.ErrPermanentStart)

// Resolver builds RunInputs for accepted starts.
type Resolver struct {
	// Generations retains a generation by digest (the object-store source).
	Generations childworkflow.GenerationSource
	// Config returns the instance-wide policy config (runners, run conditions,
	// HITL, self-execution). The retained archive excludes instance.yaml.
	Config     func() *instance.Config
	InstanceID string
	// Now is the admission clock (default time.Now). Report receives a failed
	// generation Release: a failed unpin leaks a reader pin.
	Now    func() time.Time
	Report func(error)

	mu       sync.Mutex
	compiled map[string]*startpin.Compiled
	order    []string
}

// compiledCacheDepth bounds retained compilations: a controller resolves
// starts for the applied generation and the few it superseded.
const compiledCacheDepth = 4

// Resolve implements acceptance.StartInputResolver.
func (r *Resolver) Resolve(ctx context.Context, record triggerqueue.Record) (engine.RunInput, error) {
	if r.Generations == nil || r.Config == nil || r.InstanceID == "" {
		return engine.RunInput{}, errors.New("cloud start resolver requires a generation source, instance config and instance identity")
	}
	var header struct {
		Kind string `json:"kind"`
	}
	if json.Unmarshal(record.Payload, &header) != nil || header.Kind == "" {
		return engine.RunInput{}, acceptance.ErrCorruptStart
	}
	if header.Kind != startintent.Kind {
		// Not an ordinary start (a generated child's custody, for one): leave it
		// to its owner. Never permanent: rejecting it would destroy that custody.
		return engine.RunInput{}, fmt.Errorf("%w: kind %q", acceptance.ErrNotOrdinaryStart, header.Kind)
	}
	envelope, err := startintent.Parse(record.Payload)
	if err != nil {
		// An ordinary start envelope that does not parse can never become valid.
		return engine.RunInput{}, permanent("invalid start envelope: %v", err)
	}
	target := envelope.Target
	if target.Gaggle == "" || target.Workflow == "" || target.ConfigGeneration == "" || target.WorkflowDigest == "" || target.GooberDigest == "" {
		return engine.RunInput{}, permanent("cloud start envelope is not pinned to a generation and digests")
	}
	if record.RunID == "" {
		return engine.RunInput{}, permanent("cloud start acceptance has no run identity")
	}
	// Deadline bounds admission, exactly as the local startintent.Service.Dispatch
	// enforces it: a start accepted while the controller was down for longer
	// than the pending age is refused, not started late.
	now := time.Now
	if r.Now != nil {
		now = r.Now
	}
	// The deadline bounds ADMISSION (accepted -> dispatch). A dispatching row is
	// an uncertain start that may already run; refusing it here would strand it
	// past starter.Start's mine/adopt confirmation.
	if record.State == triggerqueue.Accepted && !envelope.Deadline.IsZero() && !now().Before(envelope.Deadline) {
		return engine.RunInput{}, permanent("admission deadline %s has passed", envelope.Deadline.UTC().Format(time.RFC3339))
	}
	cfg := r.Config()
	if cfg == nil {
		return engine.RunInput{}, errors.New("cloud start resolver has no instance config")
	}
	generation, err := r.Generations.Acquire(ctx, target.ConfigGeneration)
	if err != nil {
		return engine.RunInput{}, fmt.Errorf("retain pinned generation: %w", err)
	}
	defer func() {
		if err := generation.Release(); err != nil && r.Report != nil {
			r.Report(fmt.Errorf("release pinned generation %s: %w", target.ConfigGeneration, err))
		}
	}()
	return r.build(generation, cfg, envelope, record)
}

func (r *Resolver) build(generation childworkflow.Generation, cfg *instance.Config, envelope startintent.Envelope, record triggerqueue.Record) (engine.RunInput, error) {
	target := envelope.Target
	set := generation.Set
	instance.ApplyGaggleCICommand(set)
	instance.ApplyGaggleOutboxMirror(set)
	compiled, err := r.compile(target.ConfigGeneration, generation.Directory, cfg, set)
	if err != nil {
		return engine.RunInput{}, err
	}
	identity := localscheduler.WorkflowIdentity{Gaggle: target.Gaggle, Workflow: target.Workflow}
	machine, ok := compiled.Machines[identity]
	if !ok {
		return engine.RunInput{}, permanent("pinned workflow %s/%s is unavailable in generation %s", target.Gaggle, target.Workflow, target.ConfigGeneration)
	}
	if machine.Digest() != target.WorkflowDigest || compiled.GooberDigests[identity] != target.GooberDigest {
		return engine.RunInput{}, fmt.Errorf("%w: %s/%s", ErrPinDrift, target.Gaggle, target.Workflow)
	}
	def := machine.Def
	gaggle, project, ok := gaggleOf(set, target.Gaggle)
	if !ok {
		return engine.RunInput{}, permanent("pinned gaggle %q is unavailable in generation %s", target.Gaggle, target.ConfigGeneration)
	}
	placements, err := bootstrap.PinStagePlacements(cfg, set, target.Gaggle, def)
	if err != nil {
		return engine.RunInput{}, fmt.Errorf("resolve stage placements: %w", err)
	}
	// Cloud is engine-only: a definition the engine cannot walk is refused here
	// too, the last layer behind config apply and the scheduler guard. It is the
	// same check, with placements, that cloudreadiness applies at config apply.
	if err := engineadmission.RefusePlacedDefinition(def.Name, def.Spec, placements); err != nil {
		return engine.RunInput{}, fmt.Errorf("%w: %w", acceptance.ErrPermanentStart, err)
	}
	controls, err := runControls(cfg, project, gaggle, def)
	if err != nil {
		return engine.RunInput{}, err
	}
	kind, ref := trigger(envelope, def.Name)
	preview := workflow.PreviewFeaturesEnabled(def.Annotations)
	return engine.RunInput{
		InstanceID:             r.InstanceID,
		RunID:                  record.RunID,
		Gaggle:                 target.Gaggle,
		WorkflowName:           def.Name,
		Version:                def.Version,
		DSLVersion:             def.DSLVersion,
		WorkflowDigest:         machine.Digest(),
		PreviewFeaturesEnabled: &preview,
		Spec:                   def.Spec,
		RepoRef:                project,
		TriggerKind:            kind,
		TriggerRef:             ref,
		BranchNamespace:        runtimewiring.BranchNamespacesByGaggle(set)[target.Gaggle],
		GateGooberCapabilities: gateGooberCapabilities(set, target.Gaggle),
		LiveJournal:            true,
		Placements:             placements,
		SelfExecutionDenied:    cfg.SelfExecutionDenied(),
		RunControls:            controls,
		GooberDigest:           target.GooberDigest,
		ConfigGeneration:       target.ConfigGeneration,
		HITL:                   hitlPolicy(cfg),

		RoleRoutedBacklogProvider: roleRoutedBacklogProvider(gaggle),

		BacklogQueryAssignedTo:     runtimewiring.SelfIdentitiesByGaggle(cfg, set)[target.Gaggle],
		BacklogQueryRequireLabels:  runtimewiring.RequireLabelsByGaggle(set)[target.Gaggle],
		BacklogQueryBacklogLabels:  runtimewiring.BacklogLabelsByGaggle(set)[target.Gaggle],
		BacklogQueryLabelPredicate: runtimewiring.BacklogLabelPredicatesByGaggle(set)[target.Gaggle],
		IssueOwnershipAssignees:    runtimewiring.IssueOwnershipAssigneesByGaggle(set)[target.Gaggle],
		IssueOwnershipUnassigned:   runtimewiring.IssueOwnershipUnassignedByGaggle(set)[target.Gaggle],
	}, nil
}

// compile returns the generation's compilation, cached by generation digest
// (the digest names immutable content).
func (r *Resolver) compile(digest, dir string, cfg *instance.Config, set *instance.ConfigSet) (*startpin.Compiled, error) {
	r.mu.Lock()
	if hit, ok := r.compiled[digest]; ok {
		r.mu.Unlock()
		return hit, nil
	}
	r.mu.Unlock()
	compiled, err := startpin.Compile(dir, cfg, set)
	if err != nil {
		return nil, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.compiled == nil {
		r.compiled = map[string]*startpin.Compiled{}
	}
	if _, ok := r.compiled[digest]; !ok {
		r.compiled[digest] = compiled
		r.order = append(r.order, digest)
		if len(r.order) > compiledCacheDepth {
			delete(r.compiled, r.order[0])
			r.order = r.order[1:]
		}
	}
	return compiled, nil
}

func gaggleOf(set *instance.ConfigSet, name string) (apiv1.Gaggle, apiv1.RepoRef, bool) {
	for i := range set.Gaggles {
		if set.Gaggles[i].Name == name {
			return set.Gaggles[i], set.Gaggles[i].Spec.Project, true
		}
	}
	return apiv1.Gaggle{}, apiv1.RepoRef{}, false
}

// trigger is the local provenance mapping (SourceTrigger.Trigger): a counted
// worker is an item, a signal carries its ref, and a source with neither is a
// schedule; no source at all is a manual start. The engine branches on the kind
// (it defers run-branch provenance for schedule and item), so every source must
// map exactly as the local scheduler does.
func trigger(envelope startintent.Envelope, workflowName string) (string, string) {
	if envelope.Source == nil {
		return string(journal.TriggerManual), workflowName
	}
	t := envelope.Source.Trigger(workflowName)
	return string(t.Kind), t.Ref
}

// runControls collapses instance, matched repo, gaggle and workflow run-control
// inheritance (#1671) into the effective policy pinned at start (#3820): the
// same four layers, in the same order, as the local scheduler entry.
func runControls(cfg *instance.Config, project apiv1.RepoRef, gaggle apiv1.Gaggle, def workflow.Definition) (apiv1.RunControls, error) {
	var base apiv1.RunControls
	if cfg != nil {
		base = cfg.RunConditions.RunControls()
	}
	if repo, ok := runtimewiring.ConfiguredRepoForProject(cfg, project); ok {
		base = repo.EffectiveRunControls(base)
	}
	effective, err := runcontrol.Resolve(base, gaggle.Spec.RunControls, def.Spec.RunControls)
	if err != nil {
		return apiv1.RunControls{}, fmt.Errorf("workflow %q run controls: %w", def.Name, err)
	}
	return effective.Overrides(), nil
}

// gateGooberCapabilities maps each goober a gaggle's stages may name to its
// declared capabilities, gaggle-scoped like a worker's goober admission. nil
// when no goober declares any.
func gateGooberCapabilities(set *instance.ConfigSet, gaggle string) map[string][]string {
	var caps map[string][]string
	for i := range set.Goobers {
		g := set.Goobers[i]
		if (g.Spec.Gaggle != "" && g.Spec.Gaggle != gaggle) || len(g.Spec.Capabilities) == 0 {
			continue
		}
		if caps == nil {
			caps = make(map[string][]string)
		}
		caps[g.Name] = append([]string(nil), g.Spec.Capabilities...)
	}
	return caps
}

// hitlPolicy is the instance's operator-hold posture, nil unless it opted in
// (nil is also what history recorded before the protocol, so rollback is total).
func hitlPolicy(cfg *instance.Config) *engine.HITLPolicy {
	if !cfg.EngineHITLEnabled() {
		return nil
	}
	hitl := cfg.EffectiveEngineConfig().HITL
	policy := &engine.HITLPolicy{Enabled: true, Actors: hitl.Actors}
	if window := hitl.HITLWindow(); window > 0 {
		policy.WaitSeconds = int(window / time.Second)
	}
	return policy
}

// roleRoutedBacklogProvider names the gaggle's backlog provider when it is
// routed by role to another provider than its code (topology (b)).
func roleRoutedBacklogProvider(gaggle apiv1.Gaggle) apiv1.Provider {
	if providerconfig.CrossProviderBacklog(gaggle.Spec.Project.Provider, gaggle.Spec.Backlog.Provider) {
		return gaggle.Spec.Backlog.Provider
	}
	return ""
}
