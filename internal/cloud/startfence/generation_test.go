package startfence

import (
	"errors"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/engine"
)

type testGenerationAdmission struct{}

func (testGenerationAdmission) Admit(string, string, string) error { return nil }

type refusingGeneration struct {
	calls int
	got   [3]string
}

func (g *refusingGeneration) Admit(digest, gaggle, name string) error {
	g.calls++
	g.got = [3]string{digest, gaggle, name}
	return errors.New("generation denied")
}

func TestGenerationGuardRefusesBeforeAcceptanceOrTemporal(t *testing.T) {
	for _, missing := range []bool{false, true} {
		fake := &fakeStarter{}
		guard := &refusingGeneration{}
		starter := &TemporalStarter{client: fake, taskQueue: "cloud", generation: guard}
		if missing {
			starter.generation = nil
		}
		_, err := starter.Start(t.Context(), engine.RunInput{ConfigGeneration: "generation-a", Gaggle: "web", WorkflowName: "build"})
		if err == nil || (missing && !strings.Contains(err.Error(), "configuration admission guard")) || (!missing && !strings.Contains(err.Error(), "generation denied")) {
			t.Fatal("missing or denied generation was admitted")
		}
		if fake.gotOpts.ID != "" {
			t.Fatal("generation refusal reached Temporal")
		}
		// No acceptance adapter is provided: the guard must run before it is needed.
		if !missing && (guard.calls != 1 || guard.got != [3]string{"generation-a", "web", "build"}) {
			t.Fatalf("wrong guard input: %+v", guard)
		}
	}
}
