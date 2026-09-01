package executor

import (
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

func TestDeterministicCommandRejectsBlankExecutable(t *testing.T) {
	for _, executable := range []string{"", "   ", "\t"} {
		argv, env, cleanup, err := DeterministicCommand(apiv1.DeterministicRun{
			Command: []string{executable, "--version"},
		})
		if err == nil || !strings.Contains(err.Error(), "must name a non-whitespace executable") {
			t.Fatalf("DeterministicCommand(%q) error = %v, want blank-executable rejection", executable, err)
		}
		if argv != nil || env != nil || cleanup != nil {
			t.Fatalf("DeterministicCommand(%q) returned non-nil results alongside its error", executable)
		}
	}
}

func TestDeterministicCommandKeepsValidCommand(t *testing.T) {
	argv, _, cleanup, err := DeterministicCommand(apiv1.DeterministicRun{
		Command: []string{"echo", " padded "},
	})
	if err != nil {
		t.Fatalf("DeterministicCommand: %v", err)
	}
	cleanup()
	if len(argv) != 2 || argv[0] != "echo" || argv[1] != " padded " {
		t.Fatalf("argv = %#v, want the declared command verbatim", argv)
	}
}
