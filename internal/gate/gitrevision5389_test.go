package gate_test

import (
	"context"
	"os"
	"strconv"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/credentials"
	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/internal/gate"
)

const gitRevisionHelperMarker = "goobers-gitrevision5389-helper"

// TestGitRevisionHelperProcess is not a test: the cases below re-execute the
// test binary into it as a stand-in for a stage command, so the real shell
// executor sees a process with a chosen exit status and output streams
// without the test running git.
func TestGitRevisionHelperProcess(t *testing.T) {
	args := os.Args
	for len(args) > 0 && args[0] != gitRevisionHelperMarker {
		args = args[1:]
	}
	if len(args) != 4 {
		return
	}
	code, err := strconv.Atoi(args[1])
	if err != nil {
		os.Exit(2)
	}
	_, _ = os.Stdout.WriteString(args[2])
	_, _ = os.Stderr.WriteString(args[3])
	os.Exit(code)
}

// A deterministic stage whose git command names a revision the workcopy does
// not have (`origin/main...HEAD` where the base is refs/heads/main) fails the
// same way for every diff. It must reach the failure-class gate's infra branch
// rather than its fail branch, which would spend an agentic implement repass
// on work that was never at fault (#5389).
func TestUnresolvableGitRevisionReachesInfrastructureGate(t *testing.T) {
	const gitStderr = "fatal: ambiguous argument 'origin/main...HEAD': unknown revision or path not in the working tree.\n" +
		"Use '--' to separate paths from revisions, like this:\n" +
		"'git <command> [<revision>...] -- [<file>...]'\n"
	for _, tc := range []struct {
		name, exitCode, stdout, stderr, outcome string
	}{
		{name: "bare git command", exitCode: "128", stderr: gitStderr, outcome: gate.OutcomeInfra},
		{name: "bad revision from plumbing", exitCode: "128", stderr: "fatal: bad revision 'origin/main'\n", outcome: gate.OutcomeInfra},
		{name: "behind a wrapper trailer", exitCode: "128", stderr: gitStderr + "make: *** [Makefile:9: check-diff] Error 128\n", outcome: gate.OutcomeInfra},
		{name: "wrapper exit status", exitCode: "2", stderr: gitStderr, outcome: gate.OutcomeFail},
		{name: "named test failure outranks logged git line", exitCode: "128", stdout: "--- FAIL: TestDiffBase (0.01s)\n", stderr: gitStderr, outcome: gate.OutcomeFail},
		{name: "other git fatal", exitCode: "128", stderr: "fatal: refusing to merge unrelated histories\n", outcome: gate.OutcomeFail},
	} {
		t.Run(tc.name, func(t *testing.T) {
			injector, err := credentials.NewInjector(unusedResolver{}, nil, discardRegistrar{})
			if err != nil {
				t.Fatal(err)
			}
			exec, err := executor.NewShellExecutor(injector, discardRecorder{})
			if err != nil {
				t.Fatal(err)
			}
			result, err := exec.Run(context.Background(), apiv1.InvocationEnvelope{
				TaskID: "run-1:check-diff", Workspace: t.TempDir(),
			}, apiv1.DeterministicRun{
				Command: []string{os.Args[0], "-test.run=^TestGitRevisionHelperProcess$", "--", gitRevisionHelperMarker, tc.exitCode, tc.stdout, tc.stderr},
			})
			if err != nil {
				t.Fatal(err)
			}
			if result.Status != apiv1.ResultFailure || result.Error == nil || result.Error.Code != "nonzero_exit" {
				t.Fatalf("result = %+v, want command failure", result)
			}
			inputs, err := gate.AutomatedInputs(result)
			if err != nil {
				t.Fatal(err)
			}
			outcome, err := gate.DefaultChecks()["failure-class"](inputs, nil)
			if err != nil {
				t.Fatal(err)
			}
			if outcome != tc.outcome {
				t.Fatalf("outcome = %q, want %q (diagnostic: %s)", outcome, tc.outcome, result.Error.Message)
			}
		})
	}
}
