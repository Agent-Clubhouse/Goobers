package gate_test

import (
	"context"
	"runtime"
	"strings"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/credentials"
	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/internal/gate"
	"github.com/goobers/goobers/internal/journal"
)

type discardRecorder struct{}

func (discardRecorder) RecordArtifact(name string, data []byte) (journal.Ref, error) {
	return journal.Ref{Path: name, Size: int64(len(data))}, nil
}

// Exercise the warm-cache path without network access: recognizable fetch
// failures must retain their cause through command summarization and route to
// infrastructure handling, while ordinary code/dependency errors still fail.
func TestModuleDownloadFailureReachesInfrastructureGate(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test uses Unix shell semantics")
	}
	for _, tc := range []struct {
		name, message, hint, outcome string
	}{
		{
			name:    "dns",
			message: `go: example.com/mod@v1.2.3: Get "https://modules.example.com/mod.zip": dial tcp: lookup modules.example.com: no such host`,
			hint:    "check runner DNS resolution", outcome: gate.OutcomeInfra,
		},
		{
			name:    "authentication",
			message: `go: example.com/mod@v1.2.3: reading https://modules.example.com/mod.zip: 401 Unauthorized`,
			hint:    "credentials and repository access", outcome: gate.OutcomeInfra,
		},
		{
			name:    "Go filename connection failure",
			message: `--- FAIL: TestAPI: foo.go: connection refused`,
			outcome: gate.OutcomeFail,
		},
		{
			name:    "Go filename authentication failure",
			message: `--- FAIL: TestAPI: main.go: 401 Unauthorized`,
			outcome: gate.OutcomeFail,
		},
		{
			name:    "non-download go command failure",
			message: `go: invoking tool: connection refused`,
			outcome: gate.OutcomeFail,
		},
		{
			name:    "checksum mismatch",
			message: `go: downloading example.com/mod v1.2.3: checksum mismatch against sum.golang.org`,
			outcome: gate.OutcomeFail,
		},
		{
			name:    "application authentication test failure",
			message: `--- FAIL: TestAuth: want 200, got 401 Unauthorized from https://api.example.com/widgets`,
			outcome: gate.OutcomeFail,
		},
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
				TaskID: "run-1:warm-module-cache", Workspace: t.TempDir(),
			}, apiv1.DeterministicRun{
				Command: []string{"sh", "-c", `printf '%s\n' "$1"; printf 'make: *** [Makefile:12: ci] Error 1\n' >&2; exit 1`, "module-download-test", tc.message},
			})
			if err != nil {
				t.Fatal(err)
			}
			if result.Status != apiv1.ResultFailure || result.Error == nil || result.Error.Code != "nonzero_exit" {
				t.Fatalf("result = %+v, want command failure", result)
			}
			if tc.hint != "" {
				if !strings.Contains(result.Error.Message, tc.message) || !strings.Contains(result.Error.Message, tc.hint) {
					t.Fatalf("diagnostic = %q, want original cause and %q", result.Error.Message, tc.hint)
				}
			} else if strings.Contains(result.Error.Message, "; hint:") {
				t.Fatalf("ordinary failure received infrastructure guidance: %q", result.Error.Message)
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

// unusedResolver is never called: warm-module-cache declares no
// capabilities, so nothing asks the injector to resolve a credential.
type unusedResolver struct{}

func (unusedResolver) Resolve(context.Context, string) (string, error) {
	return "", nil
}

// discardRegistrar is never called for the same reason unusedResolver isn't.
type discardRegistrar struct{}

func (discardRegistrar) Register([]byte) {}

// TestStalledModuleDownloadFailsFastAndAttributably is #4179's regression
// guard for the mechanism warm-module-cache relies on: a stalled dependency
// download must be bounded by ITS OWN short timeout, distinct from the
// agentic implement budget, and must fail with a signal that names the
// timeout specifically rather than a generic "budget exhausted" message —
// and it must do so within an order of magnitude of its OWN declared bound,
// not anywhere near implement's multi-thousand-second budget (the reported
// incident burned 5400s of budget on a download that should have failed in
// under a minute).
//
// It exercises the real ShellExecutor (the same one the warm-module-cache
// stage's `run.command` dispatches through) and the real failure-class
// automated check (the same one warm-module-cache-gate declares), rather
// than asserting against either in isolation — a stalled download must
// survive the full path from "process didn't finish" to "gate outcome" to
// count as fixed.
func TestStalledModuleDownloadFailsFastAndAttributably(t *testing.T) {
	injector, err := credentials.NewInjector(unusedResolver{}, nil, discardRegistrar{})
	if err != nil {
		t.Fatalf("new injector: %v", err)
	}
	exec, err := executor.NewShellExecutor(injector, discardRecorder{})
	if err != nil {
		t.Fatalf("new executor: %v", err)
	}

	env := apiv1.InvocationEnvelope{
		TaskID:    "run-1:warm-module-cache",
		Workspace: t.TempDir(),
		// Mirrors warm-module-cache's own declared timeoutSeconds (a short
		// bound distinct from implement's), just compressed so the test
		// itself runs fast — the mechanism under test is "bounded separately
		// from the agentic budget", not the specific number of seconds.
		Inputs: map[string]interface{}{executor.InputTimeout: "500ms"},
	}

	start := time.Now()
	// Stands in for a stalled `go mod download`: a process that never
	// produces output and never exits on its own, exactly like the reported
	// incident's `read_bash` loop waiting on a hung fetch.
	result, err := exec.Run(context.Background(), env, apiv1.DeterministicRun{
		Command: []string{"sh", "-c", "while :; do :; done"},
	})
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	const implementBudget = 5400 * time.Second
	if elapsed >= implementBudget/10 {
		t.Fatalf("stalled download took %s to fail — must fail on its OWN short bound, nowhere near the %s agentic implement budget it used to silently consume", elapsed, implementBudget)
	}
	if result.Status != apiv1.ResultFailure {
		t.Fatalf("status = %v, want failure", result.Status)
	}
	if result.Error == nil || result.Error.Code != "timeout" {
		t.Fatalf("error = %+v, want a distinct \"timeout\" code — not a generic failure that would be indistinguishable from budget exhaustion", result.Error)
	}
	if !result.Error.Retryable {
		t.Fatalf("error = %+v, want retryable — a stalled download is a transient/infrastructure condition, not an implementation defect", result.Error)
	}

	inputs, err := gate.AutomatedInputs(result)
	if err != nil {
		t.Fatalf("AutomatedInputs: %v", err)
	}
	check := gate.DefaultChecks()["failure-class"]
	outcome, err := check(inputs, nil)
	if err != nil {
		t.Fatalf("failure-class check: %v", err)
	}
	if outcome != gate.OutcomeInfra {
		t.Fatalf("failure-class outcome = %q, want %q — warm-module-cache-gate's infra branch would not fire, and a stall would misroute as an ordinary implementation failure", outcome, gate.OutcomeInfra)
	}
}
