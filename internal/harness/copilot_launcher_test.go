package harness

import (
	"context"
	"os"
	"reflect"
	"testing"
)

func TestCopilotPreflightProbesCompleteLauncherPrefix(t *testing.T) {
	program, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	command := []string{program, "wrapper-subcommand", "--profile", "fixture"}
	original := append([]string(nil), command...)
	var calls [][]string
	runner := &fakeProcessRunner{
		result: ProcessResult{Transcript: []byte("copilot fixture version")},
		act: func(req ProcessRequest) error {
			calls = append(calls, append([]string(nil), req.Command...))
			return nil
		},
	}
	adapter := &CopilotAdapter{Command: command, VersionArgs: []string{"version", "--short"}, AuthCheckArgs: []string{"auth", "status"}, Runner: runner}
	if _, err := adapter.Preflight(context.Background()); err != nil {
		t.Fatal(err)
	}
	want := [][]string{
		append(append([]string(nil), resolveHarnessCommand(original)...), "version", "--short"),
		append(append([]string(nil), resolveHarnessCommand(original)...), "auth", "status"),
	}
	if !reflect.DeepEqual(calls, want) {
		t.Fatalf("preflight checked a different launcher than dispatch: got %q, want %q", calls, want)
	}
	if !reflect.DeepEqual(command, original) {
		t.Fatalf("preflight mutated configured launcher: %q", command)
	}
}
