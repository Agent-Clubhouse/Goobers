package main

import "testing"

// TestProductionRuntimeDepsWiresEveryFactory catches a runtimeDeps factory
// that production never fills in. A nil factory would otherwise surface only
// as a panic inside a poll goroutine.
func TestProductionRuntimeDepsWiresEveryFactory(t *testing.T) {
	t.Parallel()
	deps := productionRuntimeDeps()
	if deps.openPRListers.github == nil {
		t.Error("productionRuntimeDeps().openPRListers.github is nil")
	}
	if deps.openPRListers.ado == nil {
		t.Error("productionRuntimeDeps().openPRListers.ado is nil")
	}
}
