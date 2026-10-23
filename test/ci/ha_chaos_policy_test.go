package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/goobers/goobers/test/ha/chaos"
	"github.com/goobers/goobers/test/ha/invariants"
)

// TestHAChaosPendingScenariosAreCountedAndWired is the CI policy for the HA
// chaos harness: every scenario either runs or carries an explicit
// "pending #<PR>" reason, the pending count is surfaced, and the nightly
// actually invokes the harness so a skip is reported rather than absent.
func TestHAChaosPendingScenariosAreCountedAndWired(t *testing.T) {
	t.Parallel()
	pending := 0
	for _, s := range chaos.Scenarios() {
		if err := s.Validate(); err != nil {
			t.Error(err)
		}
		if s.Pending != "" {
			pending++
			t.Logf("pending scenario %s: %s", s.ID, s.Pending)
		}
	}
	t.Logf("HA chaos scenarios pending: %d of %d", pending, len(chaos.Scenarios()))
	pendingInvariants := 0
	for _, inv := range invariants.All() {
		if inv.Pending != "" {
			pendingInvariants++
			t.Logf("pending invariant %s: %s", inv.Name, inv.Pending)
		}
	}
	t.Logf("HA invariants pending: %d of %d", pendingInvariants, len(invariants.All()))
	workflow, err := os.ReadFile(filepath.Join(moduleRoot(t), ".github", "workflows", "ha-nightly.yml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(workflow), "go run ./test/ha/chaos/cmd/chaos run") {
		t.Fatal("ha-nightly.yml does not run the chaos harness")
	}
}
