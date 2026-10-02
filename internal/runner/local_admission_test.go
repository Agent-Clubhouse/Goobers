package runner

import (
	"errors"
	"testing"
)

func TestHostAdmissionPrecedesEveryLocalEntryPreparation(t *testing.T) {
	refusal := errors.New("host refuses execution")
	// No directories, machine, worktree or executors exist: every entry must
	// refuse before inspecting or preparing any workflow-controlled resource.
	r := &Runner{cfg: Config{ExecutionRefusal: refusal}}
	calls := map[string]func() (Result, error){
		"start":    func() (Result, error) { return r.Start(t.Context(), StartInput{}) },
		"resume":   func() (Result, error) { return r.Resume(t.Context(), ResumeInput{}) },
		"terminal": func() (Result, error) { return r.ResumeFromTerminal(t.Context(), ResumeFromTerminalInput{}) },
		"rerun":    func() (Result, error) { return r.RerunStage(t.Context(), RerunStageInput{}) },
		"override": func() (Result, error) { return r.OverrideGate(t.Context(), OverrideGateInput{}) },
	}
	for name, call := range calls {
		t.Run(name, func(t *testing.T) {
			if _, err := call(); !errors.Is(err, refusal) {
				t.Fatalf("admission: %v", err)
			}
		})
	}
}
