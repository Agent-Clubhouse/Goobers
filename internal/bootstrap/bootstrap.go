// Package bootstrap wires the Goobers engine components together: it carries
// the engine worker registration seam (worker.go: EngineDeps, RegisterEngine,
// DialTemporal) shared by cmd/goobers' engine-worker wiring and
// internal/workerhost, and RegisterGaggleWorkflows, the config-to-registry
// step cmd/goobers engine-start uses to build an engine.Registry from a
// loaded instance.ConfigSet.
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
	"github.com/goobers/goobers/internal/engine"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/workflow"
)

// RegisterGaggleWorkflows builds an engine.Registry from a loaded config set,
// registering every workflow belonging to gaggle, with the instance's
// explicit preview-feature acknowledgement applied. It also returns the
// gaggle's configured repo. This is exactly what cmd/goobers engine-start
// does before pinning a RunInput, extracted here so other callers (the
// #2903 acceptance test included) build a registry through the same
// production path rather than a parallel one nothing in production calls.
func RegisterGaggleWorkflows(set *instance.ConfigSet, gaggle string) (*engine.Registry, apiv1.RepoRef, error) {
	reg := engine.NewRegistryWithPreviewFeatures(set.Manifest != nil && workflow.PreviewFeaturesEnabled(set.Manifest.Annotations))
	var project apiv1.RepoRef
	for i := range set.Gaggles {
		if set.Gaggles[i].Name == gaggle {
			project = set.Gaggles[i].Spec.Project
			break
		}
	}
	for i := range set.Workflows {
		w := set.Workflows[i]
		if w.Spec.Gaggle == gaggle {
			if _, err := reg.Register(w.Name, w.Spec); err != nil {
				return nil, apiv1.RepoRef{}, fmt.Errorf("register workflow %q: %w", w.Name, err)
			}
		}
	}
	return reg, project, nil
}
