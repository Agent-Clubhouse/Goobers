// Package readiness adapts the shared cloud-readiness library to the
// config package's Assessor. It is the only place the admission code touches
// workflow-placement machinery.
package readiness

import (
	"fmt"

	"github.com/goobers/goobers/internal/bootstrap"
	cloudconfig "github.com/goobers/goobers/internal/cloud/config"
	"github.com/goobers/goobers/internal/cloudreadiness"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/workflow"
)

// Assess reports every RUNTIME001-003 finding for the loaded configuration.
//
// cloudreadiness skips its placement checks when pinning itself fails, so a
// workflow no runner can host would read as ready. The cloud has no control
// plane host to fall back to, so that case is reported here as RUNTIME002.
func Assess(cfg *instance.Config, set *instance.ConfigSet) cloudconfig.Assessment {
	out := cloudconfig.Assessment{}
	for _, w := range cloudreadiness.Assess(cfg, set).Workflows {
		key := w.Gaggle + "/" + w.Name
		for _, b := range w.Blockers {
			out[key] = append(out[key], cloudconfig.Finding{
				Code: b.Code, Stage: b.Stage, Path: b.Path, Message: b.Message, Unblock: unblock(b.Action),
			})
		}
	}
	for i := range set.Workflows {
		w := &set.Workflows[i]
		def := workflow.Definition{Name: w.Name, Version: 1, DSLVersion: w.DSLVersion, Spec: w.Spec}
		if _, err := bootstrap.PinChildStagePlacements(cfg, set, w.Spec.Gaggle, def); err != nil {
			key := w.Spec.Gaggle + "/" + w.Name
			out[key] = append(out[key], cloudconfig.Finding{
				Code: cloudreadiness.CodePlacement, Path: "/spec", Message: err.Error(),
				Unblock: "declare a non-self runner that satisfies the workflow's placement requirements",
			})
		}
	}
	return out
}

func unblock(a cloudreadiness.Action) string {
	text := a.Description
	if a.IssueURL != "" {
		text += fmt.Sprintf(" (%s)", a.IssueURL)
	}
	if a.Command != "" {
		text += fmt.Sprintf(" (`%s`)", a.Command)
	}
	return text
}
