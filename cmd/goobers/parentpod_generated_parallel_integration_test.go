//go:build integration

package main

import (
	"github.com/goobers/goobers/test/testsupport/testdep"
	"testing"
)

func TestIntegrationParentAuthorsParallelChildThroughRealWorkers(t *testing.T) {
	testdep.RequireEnv(t, "GOOBERS_CHILD_KUBE_QUALIFICATION")
	qualifyContainedParentJourney(t, "generated-parallel")
}
