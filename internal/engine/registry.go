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

// Register appends spec as the next version of the named workflow and returns the
// new version number (1-based). It validates the definition compiles before
// accepting it, so a broken definition can never be started.
func (r *Registry) Register(name string, spec apiv1.WorkflowSpec) (int, error) {
	return r.RegisterDefinition(wf.Definition{Name: name, Spec: spec})
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
// block; deterministic requires a run with a command and forbids a goober —
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
			} else if len(t.Run.Command) == 0 {
				problems = append(problems, fmt.Sprintf("task %q run declares no command (schema: run.command requires at least one element)", t.Name))
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

// PreviewFeaturesEnabled reports the policy carried by registered definitions.
func (r *Registry) PreviewFeaturesEnabled() bool {
	return r.allowPreviewFeatures
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
	allowPreviewFeatures := r.allowPreviewFeatures
	return RunInput{
		RunID:                  s.RunID,
		Gaggle:                 s.Gaggle,
		WorkflowName:           name,
		Version:                def.Version,
		DSLVersion:             def.DSLVersion,
		PreviewFeaturesEnabled: &allowPreviewFeatures,
		Spec:                   def.Spec,
		RepoRef:                s.RepoRef,
		Item:                   s.Item,
		TriggerRef:             s.TriggerRef,
		TriggerKind:            s.TriggerKind,
		BranchNamespace:        s.BranchNamespace,
		GateGooberCapabilities: s.GateGooberCapabilities,
	}, nil
}
