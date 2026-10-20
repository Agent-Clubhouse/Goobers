package validate

import (
	"fmt"
	"slices"

	wf "github.com/goobers/goobers/internal/workflow"
)

// Interactive has its own Gaggle pin. Do not probe these fields through a
// Workflow pin: ordinary DSL 2.0 workflows can coexist with interactive 3.0.
func (ix *index) checkGaggleInteractive(r *Report) {
	for name, g := range ix.gaggles {
		if g.Spec.Interactive == nil {
			continue
		}
		file := ix.gaggleFile[name]
		fail := func(format string, args ...any) {
			r.add(errorInteractiveConfig, Error, file, "Gaggle", name, format, args...)
		}
		if g.DSLVersion != "3.0" {
			fail("spec.interactive requires explicit dslVersion 3.0")
			continue
		}
		if value, ok := g.Annotations[wf.PreviewFeaturesAnnotation]; ok && value != "true" && value != "false" {
			fail("metadata.annotations[%q] must be true or false", wf.PreviewFeaturesAnnotation)
		}
		r.addFeatureDiagnostics(file, name, "Gaggle", name, wf.CheckInteractiveFeatureSupport(g))
		authorities := map[string]bool{}
		for _, a := range g.Spec.Interactive.Authorities {
			if authorities[a.Name] {
				fail("spec.interactive.authorities repeats name %q", a.Name)
			}
			authorities[a.Name] = true
		}
		surfaces := map[string]bool{}
		for _, s := range g.Spec.Interactive.Surfaces {
			prefix := fmt.Sprintf("spec.interactive.surfaces[%s]", s.Name)
			if surfaces[s.Name] {
				fail("spec.interactive.surfaces repeats name %q", s.Name)
			}
			surfaces[s.Name] = true
			if !slices.Contains(s.ExecutionModes, s.DefaultExecutionMode) {
				fail("%s.defaultExecutionMode must belong to executionModes", prefix)
			}
			if s.Session.MaxActivePerOwner > s.Session.MaxActive {
				fail("%s.session.maxActivePerOwner must not exceed maxActive", prefix)
			}
			if s.Session.LeaseSeconds > s.Session.MaxTurnSeconds {
				fail("%s.session.leaseSeconds must not exceed maxTurnSeconds", prefix)
			}
			goober, ok := ix.goobers[s.GooberRef]
			if !ok {
				ix.referenceNotFound(r, errorInteractiveReference, file, "Gaggle", name, "%s.gooberRef names missing Goober/%s", prefix, s.GooberRef)
			} else if gooberInAnotherGaggle(goober.Spec, name) {
				fail("%s.gooberRef names Goober/%s owned by another gaggle %q", prefix, s.GooberRef, goober.Spec.Gaggle)
			}
			for _, ref := range s.Workflows {
				if _, ok := ix.workflows[workflowIdentity{gaggle: name, name: ref}]; !ok {
					ix.referenceNotFound(r, errorInteractiveReference, file, "Gaggle", name, "%s.workflows names missing Workflow/%s in gaggle %q", prefix, ref, name)
				}
			}
			for _, ref := range s.AuthorityRefs {
				if !authorities[ref] {
					fail("%s.authorityRefs names undeclared local authority %q", prefix, ref)
				}
			}
		}
	}
}
