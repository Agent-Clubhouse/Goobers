package engine

import (
	"fmt"
	"strings"
	"sync"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	wf "github.com/goobers/goobers/internal/workflow"
)

// Registry holds workflow definitions by name, each as an append-only list of
// versions. Registering a new version never rewrites earlier ones, so a run that
// pinned an older version (via StartInput) keeps executing it to completion
// (WF-016). The Registry is used by the run starter (outside the workflow
// function), so its mutex does not affect workflow determinism.
type Registry struct {
	mu                   sync.RWMutex
	defs                 map[string][]wf.Definition // name -> versions; index+1 == version
	allowPreviewFeatures bool
}

// NewRegistry returns an empty Registry.
func NewRegistry() *Registry {
	return &Registry{defs: make(map[string][]wf.Definition)}
}

// NewRegistryWithPreviewFeatures returns an empty Registry with the instance's
// explicit preview-feature acknowledgement.
func NewRegistryWithPreviewFeatures(enabled bool) *Registry {
	return &Registry{
		defs:                 make(map[string][]wf.Definition),
		allowPreviewFeatures: enabled,
	}
}

// RegisterDefinition appends a parsed workflow definition, assigning its
// registry run-pin version while retaining its independent DSL version.
// Version assignment, validation, and the append run under one critical
// section, so concurrent registrations serialize and version numbers stay
// unique and monotonic (#626).
func (r *Registry) RegisterDefinition(def wf.Definition) (int, error) {
	if problems := shapeProblems(def.Spec); len(problems) > 0 {
		return 0, fmt.Errorf("invalid workflow %q: %s", def.Name, strings.Join(problems, "; "))
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	def.Version = len(r.defs[def.Name]) + 1
	if _, err := r.Compile(def); err != nil {
		return 0, err
	}
	r.defs[def.Name] = append(r.defs[def.Name], def)
	return def.Version, nil
}

// shapeProblems re-asserts the schema-owned task shape invariants at the
// registry boundary (#626): agentic requires a goober and forbids a run
// block; deterministic requires a run with a command or script and forbids a goober —
// the same allOf rules api/schemas/workflow.schema.json enforces, which the
// compiler deliberately does not own. Defense in depth: the schema remains
// the owner, the registry mirrors it, so a definition the schema would
// refuse can never enter the registry unchallenged.
func shapeProblems(spec apiv1.WorkflowSpec) []string {
	var problems []string
	for _, t := range spec.Tasks {
		switch t.Type {
		case apiv1.TaskAgentic:
			if t.Goober == "" {
				problems = append(problems, fmt.Sprintf("task %q is agentic but names no goober (schema: agentic requires goober)", t.Name))
			}
			if t.Run != nil {
				problems = append(problems, fmt.Sprintf("task %q is agentic but declares a run block (schema: agentic forbids run)", t.Name))
			}
		case apiv1.TaskDeterministic:
			if t.Run == nil {
				problems = append(problems, fmt.Sprintf("task %q is deterministic but declares no run (schema: deterministic requires run)", t.Name))
			} else if len(t.Run.Command) == 0 && t.Run.Script == "" {
				problems = append(problems, fmt.Sprintf("task %q run declares no command or script (schema: run.command requires at least one element or run.script requires at least one character)", t.Name))
			}
			if t.Goober != "" {
				problems = append(problems, fmt.Sprintf("task %q is deterministic but names goober %q (schema: deterministic forbids goober)", t.Name, t.Goober))
			}
		}
	}
	return problems
}

// Compile validates def with the same preview policy used for registration.
func (r *Registry) Compile(def wf.Definition) (*wf.Machine, error) {
	return wf.Compile(def, wf.WithPreviewFeatures(r.allowPreviewFeatures))
}

// Get returns a specific pinned version of a workflow (1-based).
func (r *Registry) Get(name string, version int) (wf.Definition, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	versions := r.defs[name]
	if version < 1 || version > len(versions) {
		return wf.Definition{}, false
	}
	return versions[version-1], true
}

// Latest returns the most recently registered version of a workflow.
func (r *Registry) Latest(name string) (wf.Definition, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	versions := r.defs[name]
	if len(versions) == 0 {
		return wf.Definition{}, false
	}
	return versions[len(versions)-1], true
}

// StartSpec describes a run to start; it is the non-pinned part of a RunInput.
type StartSpec struct {
	RunID   string
	Gaggle  string
	RepoRef apiv1.RepoRef
	Item    *apiv1.BacklogItem
	// TriggerRef identifies the event or item that caused the run (bounded
	// scheduler metadata, threaded into every stage envelope).
	TriggerRef string
	// TriggerKind is how the run was started (journal.TriggerKind vocabulary),
	// pinned for the run.yaml identity the journal projection writes (#629).
	TriggerKind string
	// BranchNamespace is the gaggle's run-branch namespace root; empty means
	// the default namespace.
	BranchNamespace string
	// GateGooberCapabilities maps reviewer goober names to their granted
	// capabilities; instance policy pinned into the run at start (WF-016).
	GateGooberCapabilities map[string][]string
	// LiveJournal pins live journal authorship (DS4) into the run: the
	// starter sets it when the daemon's journal plane serves this instance.
	LiveJournal bool
	// Placements pins each task's resolved execution placement (#3588) —
	// bootstrap.PinStagePlacements' output for this definition. Nil for
	// every zero-declaration and local-mode instance, which leaves every
	// stage on the legacy self path byte for byte.
	Placements []PinnedPlacement
	// RunControls is the run's already-resolved run-control policy: the
	// starter collapses the instance → repo → gaggle → workflow inheritance
	// (#1671) into one effective block before dispatch, exactly as the
	// daemon's scheduler entry does, and this pins it for the run.
	//
	// The zero value is not "inherit later" — nothing downstream re-reads
	// the config — it is "no configured layer", which newRunJournal resolves
	// to the built-in 3-repass / 45m defaults. A starter that has the
	// instance config must fill this in or every run it dispatches pins the
	// defaults no matter what the author declared (#3820).
	RunControls apiv1.RunControls
	// BacklogQueryAssignedTo is this gaggle's resolved self identity
	// (instance.EffectiveSelfIdentity) and BacklogQueryRequireLabels its
	// comma-joined GaggleSpec.RequireLabels — the gaggle defaults
	// cmd/goobers' selfIdentitiesByGaggle / requireLabelsByGaggle resolve for
	// the local runner's Config. Pinning them here is what gives an
	// engine-driven run the same MIRC-2 claim partition the runner has had
	// since #1901: a starter that leaves them empty dispatches a
	// backlog-query stage with no partition at all, which on a shared backlog
	// claims the sibling instance's goobers:local items (#3873).
	BacklogQueryAssignedTo    string
	BacklogQueryRequireLabels string
	// GooberDigest is the content digest of the goober kit this run's stages
	// must execute (localscheduler.WorkflowEntry.GooberDigest — what
	// gooberDigestStarter stamps onto a runner-driven run's StartRequest).
	//
	// Pinning it here puts the same value in the engine run's run.yaml
	// identity, so an engine-driven run's provenance names the kit exactly as
	// a runner-driven one does and the two are comparable in the parity
	// harness. It does NOT yet SELECT the kit the worker executes: the
	// worker resolves its kit from its own mounted config, so a
	// mid-flight kit change can still be observed by an in-flight engine run.
	// That gap is tracked as #3884 and is deliberately out of D1's scope —
	// see docs/reference/engine-parity.md.
	GooberDigest string
	// HITL pins this run's human-in-the-loop posture (#3883, decision 005
	// R8). Nil — the value every run started before the protocol existed
	// carries, and the value an instance that did not opt in carries — means
	// the run settles at its terminal exactly as it always did, and an
	// operator intent addressed to it is refused by name.
	//
	// It is PINNED at start rather than read from config mid-run for the
	// WF-016 reason every other control here is pinned: the workflow must
	// decide the same way on replay as it did live, and a config edit between
	// the two would otherwise change the command sequence and wedge the run.
	HITL *HITLPolicy
}

// StartInput resolves the latest version of a workflow and pins it into a
// RunInput. The returned RunInput carries the definition snapshot, so the run is
// immune to later re-registrations of the same workflow.
func (r *Registry) StartInput(name string, s StartSpec) (RunInput, error) {
	def, ok := r.Latest(name)
	if !ok {
		return RunInput{}, fmt.Errorf("workflow %q is not registered", name)
	}
	return r.StartInputVersion(name, def.Version, s)
}

// StartInputVersion pins a specific version into a RunInput.
func (r *Registry) StartInputVersion(name string, version int, s StartSpec) (RunInput, error) {
	def, ok := r.Get(name, version)
	if !ok {
		return RunInput{}, fmt.Errorf("workflow %q version %d is not registered", name, version)
	}
	return RunInputFor(name, def, r.allowPreviewFeatures, s)
}

// RunInputFor pins an already-resolved definition into a RunInput, applying
// the same R9 refusal StartInputVersion applies.
//
// It is the registry-free form of StartInputVersion, and it exists for the
// daemon: cmd/goobers already holds each lane's compiled wf.Definition (the
// scheduler entry pins it at build time), and routing that definition through
// a throwaway single-entry Registry would renumber its Version — Register
// assigns versions by insertion order, so a lane's v7 becomes v1 and the run's
// pinned identity stops matching the definition the operator deployed.
// Building the RunInput directly from the definition keeps name, Version,
// DSLVersion and Spec exactly as compiled.
//
// allowPreviewFeatures is the instance's preview-feature posture
// (Registry.allowPreviewFeatures); it is pinned into the run so the walk's
// preview gating is decided at start, not re-read mid-run.
func RunInputFor(name string, def wf.Definition, allowPreviewFeatures bool, s StartSpec) (RunInput, error) {
	// R9 run-start refusal: a definition declaring parallels, a bandit
	// experiment, a cumulative usage budget or an outbox has no engine walk
	// implementation, and the walk would otherwise IGNORE the declaration
	// silently. Refusing here rather than at RegisterDefinition keeps a
	// gaggle's other lanes startable — see registryrefusal.go for why that
	// placement is load-bearing.
	if err := refuseUnsupportedEngineFeatures(name, def.Spec); err != nil {
		return RunInput{}, err
	}
	previewEnabled := allowPreviewFeatures
	return RunInput{
		RunID:                  s.RunID,
		Gaggle:                 s.Gaggle,
		WorkflowName:           name,
		Version:                def.Version,
		DSLVersion:             def.DSLVersion,
		PreviewFeaturesEnabled: &previewEnabled,
		Spec:                   def.Spec,
		RepoRef:                s.RepoRef,
		Item:                   s.Item,
		TriggerRef:             s.TriggerRef,
		TriggerKind:            s.TriggerKind,
		BranchNamespace:        s.BranchNamespace,
		GateGooberCapabilities: s.GateGooberCapabilities,
		LiveJournal:            s.LiveJournal,
		Placements:             s.Placements,
		RunControls:            s.RunControls,
		GooberDigest:           s.GooberDigest,
		HITL:                   s.HITL,

		BacklogQueryAssignedTo:    s.BacklogQueryAssignedTo,
		BacklogQueryRequireLabels: s.BacklogQueryRequireLabels,
	}, nil
}
