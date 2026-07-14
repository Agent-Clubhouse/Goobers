package main

import (
	"os"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

// TestMain neutralizes the harness-preflight seam for the whole cmd/goobers test
// suite. These tests drive `goobers up`/`run` against configs that declare
// agentic stages, but CI has no real, installed Copilot CLI, so the production
// preflight (LookPath("copilot")) would fail every such test. The real preflight
// logic is exercised directly in preflight_test.go via preflightAgenticHarnesses,
// so no coverage is lost by stubbing the wiring seam here.
func TestMain(m *testing.M) {
	preflightHarnesses = func(map[string]apiv1.GooberSpec, []apiv1.Workflow) error { return nil }
	os.Exit(m.Run())
}
