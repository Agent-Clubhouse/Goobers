//go:build integration

package main

import (
	"testing"

	"github.com/goobers/goobers/test/testsupport/testdep"
)

func TestIntegrationParentComposesExistingGooberThroughRealWorkers(t *testing.T) {
	testdep.RequireEnv(t, "GOOBERS_CHILD_KUBE_QUALIFICATION")
	for _, harness := range []string{"claude-code", "codex"} {
		t.Run(harness, func(t *testing.T) {
			t.Setenv("GOOBERS_PARENT_QUALIFICATION_HARNESS", harness)
			qualifyContainedParentJourney(t, "agentic")
		})
	}
}
