package bootstrap

import (
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/runnersolve"
)

// placementPinInventory preserves zero-declaration/local-mode invariance while
// ensuring an operator mandate always reaches the solve, even on implicit self.
func placementPinInventory(cfg *instance.Config) (runnersolve.Inventory, bool) {
	if cfg == nil || (len(cfg.Runners) == 0 && !cfg.HasIsolationMandates()) {
		return runnersolve.Inventory{}, false
	}
	inv := cfg.PlacementInventory(runnersolve.HostOS())
	return inv, !inv.LocalMode() || cfg.HasIsolationMandates()
}
