package harness

import "fmt"

// Registry maps a harness name (a Goober's `spec.harness`, e.g. "copilot" or
// "fake") to its Adapter. It is the concrete proof of GBO-051's swappability
// claim: an Executor only ever holds an Adapter obtained through a Registry
// lookup, so adding a third harness is "register one more entry," never a
// change to Executor or the runner (see TestRegistrySwapRequiresNoExecutorChange).
type Registry struct {
	adapters map[string]Adapter
}

// NewRegistry returns an empty Registry.
func NewRegistry() *Registry {
	return &Registry{adapters: make(map[string]Adapter)}
}

// Register adds adapter under its own Name(). It is an error to register two
// adapters with the same name, or a nil adapter.
func (r *Registry) Register(adapter Adapter) error {
	if adapter == nil {
		return fmt.Errorf("harness: cannot register a nil adapter")
	}
	name := adapter.Name()
	if name == "" {
		return fmt.Errorf("harness: adapter has an empty Name()")
	}
	if _, dup := r.adapters[name]; dup {
		return fmt.Errorf("harness: adapter %q already registered", name)
	}
	r.adapters[name] = adapter
	return nil
}

// ErrAdapterNotFound is returned by Get for an unregistered name.
type ErrAdapterNotFound string

func (e ErrAdapterNotFound) Error() string {
	return fmt.Sprintf("harness: no adapter registered for %q", string(e))
}

// Get returns the adapter registered under name.
func (r *Registry) Get(name string) (Adapter, error) {
	a, ok := r.adapters[name]
	if !ok {
		return nil, ErrAdapterNotFound(name)
	}
	return a, nil
}

// Names returns the registered adapter names, for diagnostics.
func (r *Registry) Names() []string {
	names := make([]string, 0, len(r.adapters))
	for name := range r.adapters {
		names = append(names, name)
	}
	return names
}
