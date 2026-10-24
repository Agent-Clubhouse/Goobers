package remediationselect

import (
	"os/exec"
	"strings"
	"testing"
)

// The cloud control role never executes a stage, and this package is what lets
// it share the stages' selection. Its dependency closure must therefore stay free
// of the runner and the engine (and of every command package), which is why the
// claim read takes a caller's context instead of importing the stage scheduling
// package. localscheduler is inherent: the claim ledger's entry and key types
// are defined there.
func TestPackageClosureExcludesTheRunnerAndEngine(t *testing.T) {
	out, err := exec.Command("go", "list", "-deps", "github.com/goobers/goobers/internal/remediationselect").Output()
	if err != nil {
		t.Skipf("go list unavailable: %v", err)
	}
	for _, dep := range strings.Fields(string(out)) {
		switch {
		case dep == "github.com/goobers/goobers/internal/runner",
			dep == "github.com/goobers/goobers/internal/engine",
			dep == "github.com/goobers/goobers/internal/controlplane/scheduling",
			strings.HasPrefix(dep, "github.com/goobers/goobers/cmd/"):
			t.Fatalf("internal/remediationselect depends on %s", dep)
		}
	}
}
