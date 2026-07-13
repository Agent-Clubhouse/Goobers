package engine

import (
	"fmt"
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
	mu   sync.RWMutex
	defs map[string][]apiv1.WorkflowSpec // name -> versions; index+1 == version
}

// NewRegistry returns an empty Registry.
func NewRegistry() *Registry {
	return &Registry{defs: make(map[string][]apiv1.WorkflowSpec)}
}

// Register appends spec as the next version of the named workflow and returns the
// new version number (1-based). It validates the definition compiles before
// accepting it, so a broken definition can never be started.
func (r *Registry) Register(name string, spec apiv1.WorkflowSpec) (int, error) {
	version := len(r.peek(name)) + 1
	if _, err := wf.Compile(wf.Definition{Name: name, Version: version, Spec: spec}); err != nil {
		return 0, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.defs[name] = append(r.defs[name], spec)
	return len(r.defs[name]), nil
}

func (r *Registry) peek(name string) []apiv1.WorkflowSpec {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.defs[name]
}

// Get returns a specific pinned version of a workflow (1-based).
func (r *Registry) Get(name string, version int) (wf.Definition, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	versions := r.defs[name]
	if version < 1 || version > len(versions) {
		return wf.Definition{}, false
	}
	return wf.Definition{Name: name, Version: version, Spec: versions[version-1]}, true
}

// Latest returns the most recently registered version of a workflow.
func (r *Registry) Latest(name string) (wf.Definition, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	versions := r.defs[name]
	if len(versions) == 0 {
		return wf.Definition{}, false
	}
	return wf.Definition{Name: name, Version: len(versions), Spec: versions[len(versions)-1]}, true
}

// StartSpec describes a run to start; it is the non-pinned part of a RunInput.
type StartSpec struct {
	RunID   string
	Gaggle  string
	RepoRef apiv1.RepoRef
	Item    *apiv1.BacklogItem
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
	return RunInput{
		RunID:        s.RunID,
		Gaggle:       s.Gaggle,
		WorkflowName: name,
		Version:      def.Version,
		Spec:         def.Spec,
		RepoRef:      s.RepoRef,
		Item:         s.Item,
	}, nil
}
