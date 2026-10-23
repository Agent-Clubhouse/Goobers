package runner

import (
	"os"

	"github.com/goobers/goobers/internal/runsemantics"
)

// recordsPlacement is placementDeclared bound to this runner's configuration
// and the live process environment.
func (r *Runner) recordsPlacement() bool {
	return (r.cfg.childExecution == nil || r.cfg.childTrustedLocal) && runsemantics.PlacementDeclared(r.cfg.RunnersDeclared, os.Environ())
}
