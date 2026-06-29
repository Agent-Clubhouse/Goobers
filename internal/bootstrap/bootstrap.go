// Package bootstrap wires the Goobers control-plane components together from a
// config-as-code directory: it loads the config (M12 configsync), registers the
// Workflow definitions into an engine.Registry (M7), and builds gaggle-scoped
// schedulers (M11) over an injected run Starter, telemetry, and readiness.
//
// It is the single wiring seam shared by the real /cmd entrypoints (which inject
// a Temporal-backed engine.Starter and the real goober runtime) and the e2e
// walking-skeleton harness (which injects test doubles). Keeping the wiring here
// means the binaries and the acceptance harness construct the system the same
// way.
package bootstrap

import (
	"fmt"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/configsync"
	"github.com/goobers/goobers/internal/engine"
	"github.com/goobers/goobers/internal/scheduler"
)

// Loaded is the typed, registered result of loading a config repo.
type Loaded struct {
	Manifest  *apiv1.Manifest
	Registry  *engine.Registry
	Gaggles   []apiv1.Gaggle
	Goobers   []apiv1.Goober
	Workflows []apiv1.Workflow
}

// LoadAndRegister loads the config-as-code directory at root and registers every
// Workflow definition into a fresh engine.Registry. namespace is the target
// Kubernetes namespace stamped onto objects (configsync default applies if "").
// A config that fails validation returns an error.
func LoadAndRegister(root, namespace string) (*Loaded, error) {
	loader, err := configsync.NewLoader(namespace)
	if err != nil {
		return nil, fmt.Errorf("bootstrap: new loader: %w", err)
	}
	set, report, err := loader.Load(root)
	if err != nil {
		if report != nil && report.HasErrors() {
			return nil, fmt.Errorf("bootstrap: invalid config at %s: %w", root, err)
		}
		return nil, fmt.Errorf("bootstrap: load %s: %w", root, err)
	}

	out := &Loaded{Manifest: set.Manifest, Registry: engine.NewRegistry()}
	for _, obj := range set.Objects {
		switch o := obj.(type) {
		case *apiv1.Gaggle:
			out.Gaggles = append(out.Gaggles, *o)
		case *apiv1.Goober:
			out.Goobers = append(out.Goobers, *o)
		case *apiv1.Workflow:
			out.Workflows = append(out.Workflows, *o)
			if _, err := out.Registry.Register(o.Name, o.Spec); err != nil {
				return nil, fmt.Errorf("bootstrap: register workflow %q: %w", o.Name, err)
			}
		}
	}
	return out, nil
}

// Gaggle returns the loaded gaggle with the given name.
func (l *Loaded) Gaggle(name string) (apiv1.Gaggle, bool) {
	for _, g := range l.Gaggles {
		if g.Name == name {
			return g, true
		}
	}
	return apiv1.Gaggle{}, false
}

// SchedulerDeps are the injected runtime dependencies for a scheduler: the run
// Starter (real = engine.TemporalStarter; e2e = a fake), telemetry, the backlog
// claimer, and readiness conditions.
type SchedulerDeps struct {
	Starter   engine.Starter
	Telemetry scheduler.SpanStarter
	Claimer   scheduler.Claimer
	Readiness []scheduler.ReadinessCondition
}

// SchedulerFor builds a scheduler scoped to one loaded gaggle, sharing the
// registry of workflow definitions.
func (l *Loaded) SchedulerFor(gaggleName string, deps SchedulerDeps) (*scheduler.Scheduler, error) {
	g, ok := l.Gaggle(gaggleName)
	if !ok {
		return nil, fmt.Errorf("bootstrap: gaggle %q not found in config", gaggleName)
	}
	return scheduler.New(scheduler.Config{
		Gaggle:    g.Name,
		Repo:      g.Spec.Project,
		Registry:  l.Registry,
		Starter:   deps.Starter,
		Telemetry: deps.Telemetry,
		Claimer:   deps.Claimer,
		Readiness: deps.Readiness,
	})
}
