// Package bootstrap wires the Goobers engine components together from a
// config-as-code directory: it loads the config (M12 configsync) and registers
// the Workflow definitions into an engine.Registry (M7), and it carries the
// engine worker registration seam (worker.go: EngineDeps, RegisterEngine,
// DialTemporal) shared by cmd/goobers' engine-worker wiring and
// internal/workerhost.
//
// The tier-3 scheduler fork this package used to wire (internal/scheduler,
// consumed by cmd/scheduler) was deleted per goobernetes-architecture.md D5/§4
// (#2055 resolved: supersede) — the daemon's internal/localscheduler is the one
// scheduler. test/e2e/walking_skeleton_test.go (the V0-live e2e harness) wires
// internal/runner directly instead, not through this package.
package bootstrap

import (
	"fmt"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/configsync"
	"github.com/goobers/goobers/internal/engine"
	"github.com/goobers/goobers/internal/workflow"
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

	allowPreview := set.Manifest != nil && workflow.PreviewFeaturesEnabled(set.Manifest.Annotations)
	out := &Loaded{
		Manifest: set.Manifest,
		Registry: engine.NewRegistryWithPreviewFeatures(allowPreview),
	}
	for _, obj := range set.Objects {
		switch o := obj.(type) {
		case *apiv1.Gaggle:
			out.Gaggles = append(out.Gaggles, *o)
		case *apiv1.Goober:
			out.Goobers = append(out.Goobers, *o)
		case *apiv1.Workflow:
			out.Workflows = append(out.Workflows, *o)
			if _, err := out.Registry.RegisterDefinition(workflow.Definition{
				Name: o.Name, DSLVersion: o.DSLVersion, Spec: o.Spec,
			}); err != nil {
				return nil, fmt.Errorf("bootstrap: register workflow %q: %w", o.Name, err)
			}
		}
	}
	return out, nil
}
