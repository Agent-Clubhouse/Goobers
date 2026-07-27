package harness

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/api/validate"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/sandbox"
)

// sandboxCapturingAdapter records the RunRequest the Executor built, so tests
// can assert whether (and which) sandbox reached the adapter seam.
type sandboxCapturingAdapter struct {
	FakeAdapter
	calls []RunRequest
}

func (a *sandboxCapturingAdapter) Run(ctx context.Context, req RunRequest) (Outcome, error) {
	a.calls = append(a.calls, req)
	return a.FakeAdapter.Run(ctx, req)
}

func newSandboxTestRun(t *testing.T) *journal.Run {
	t.Helper()
	run, err := journal.Create(t.TempDir(), journal.RunIdentity{
		RunID:           "0af7651916cd43dd8448eb211c80319c",
		Workflow:        "default-implement",
		WorkflowVersion: 1,
		Gaggle:          "example",
		Trigger:         journal.Trigger{Kind: journal.TriggerItem, Ref: "issue-1"},
	}, nil)
	if err != nil {
		t.Fatalf("journal.Create: %v", err)
	}
	t.Cleanup(func() { _ = run.Close() })
	return run
}

func postureEvents(t *testing.T, run *journal.Run) []map[string]any {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(run.Dir(), "events.jsonl"))
	if err != nil {
		t.Fatalf("read events.jsonl: %v", err)
	}
	var out []map[string]any
	sc := bufio.NewScanner(bytes.NewReader(data))
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var ev map[string]any
		if err := json.Unmarshal(line, &ev); err != nil {
			t.Fatalf("decode event line %q: %v", line, err)
		}
		if ev["type"] == string(journal.EventRunnerIsolationPosture) {
			out = append(out, ev)
		}
	}
	return out
}

func TestExecutorEnforcedSandboxJournalsPostureAndConfinesAdapter(t *testing.T) {
	run := newSandboxTestRun(t)
	sb := &stubSandbox{}
	adapter := &sandboxCapturingAdapter{FakeAdapter: FakeAdapter{
		Act: func(ctx context.Context, req RunRequest) error {
			return WriteCompletion(req.Workspace, req.CompletionPath, apiv1.ResultEnvelope{
				Status: apiv1.ResultSuccess, Summary: "done",
			})
		},
	}}
	exec, err := NewExecutor(adapter, testInjector(t, "", "", noopRegistrar{}), run, run,
		NewContextResolver(run, t.TempDir()), journal.NewPatternScrubber(), "instructions",
		WithSandboxEnforcement())
	if err != nil {
		t.Fatalf("NewExecutor: %v", err)
	}
	exec.newSandbox = func() (sandbox.Sandbox, error) { return sb, nil }

	env := testEnvelope(t.TempDir(), "repo:read")
	if _, err := exec.Invoke(context.Background(), env); err != nil {
		t.Fatalf("Invoke: %v", err)
	}

	if len(adapter.calls) != 1 {
		t.Fatalf("adapter calls = %d, want 1", len(adapter.calls))
	}
	if adapter.calls[0].Sandbox != sandbox.Sandbox(sb) {
		t.Fatalf("adapter received sandbox %v, want the executor-constructed one", adapter.calls[0].Sandbox)
	}

	events := postureEvents(t, run)
	if len(events) != 1 {
		t.Fatalf("runner.isolation.posture events = %d, want exactly 1 per enforced attempt", len(events))
	}
	ev := events[0]
	if ev["stage"] != "implement" {
		t.Fatalf("posture event stage = %v, want implement", ev["stage"])
	}
	payload, _ := ev["runner"].(map[string]any)
	if payload["posture"] != "enforced" {
		t.Fatalf("posture payload = %v, want posture=enforced", payload)
	}
	if payload["mechanism"] != "stub" {
		t.Fatalf("mechanism = %v, want the sandbox's own mechanism name", payload["mechanism"])
	}
	if payload["workspace"] != env.Workspace {
		t.Fatalf("workspace scope = %v, want %q", payload["workspace"], env.Workspace)
	}

	// The emitted bytes must validate against the shipped journal-event
	// schema — the wire contract Track A registered the event type in.
	v, err := validate.New()
	if err != nil {
		t.Fatalf("build validator: %v", err)
	}
	raw, err := json.Marshal(ev)
	if err != nil {
		t.Fatal(err)
	}
	if err := v.ValidateJSON("journal-event.schema.json", raw); err != nil {
		t.Fatalf("posture event fails journal-event schema: %v\n%s", err, raw)
	}
}

func TestExecutorEnforcedSandboxUnavailableFailsClosed(t *testing.T) {
	run := newSandboxTestRun(t)
	adapter := &sandboxCapturingAdapter{}
	exec, err := NewExecutor(adapter, testInjector(t, "", "", noopRegistrar{}), run, run,
		NewContextResolver(run, t.TempDir()), journal.NewPatternScrubber(), "instructions",
		WithSandboxEnforcement())
	if err != nil {
		t.Fatalf("NewExecutor: %v", err)
	}
	exec.newSandbox = func() (sandbox.Sandbox, error) {
		return nil, fmt.Errorf("%w: bubblewrap (bwrap) not found on PATH", sandbox.ErrUnavailable)
	}

	_, err = exec.Invoke(context.Background(), testEnvelope(t.TempDir(), "repo:read"))
	if !errors.Is(err, ErrSandboxUnavailable) {
		t.Fatalf("Invoke error = %v, want ErrSandboxUnavailable", err)
	}
	if !errors.Is(err, sandbox.ErrUnavailable) {
		t.Fatalf("Invoke error = %v, must preserve the sandbox package's unavailability cause", err)
	}
	for _, fragment := range []string{"enforced", "disabled", "bubblewrap"} {
		if !containsString(err.Error(), fragment) {
			t.Fatalf("error %q lacks actionable fragment %q", err, fragment)
		}
	}
	if len(adapter.calls) != 0 {
		t.Fatal("adapter ran despite an unavailable sandbox under an enforced posture — silent downgrade")
	}
	if events := postureEvents(t, run); len(events) != 0 {
		t.Fatalf("fail-closed attempt journaled %d posture events, want 0 (nothing was confined)", len(events))
	}
}

// TestExecutorEnforcedSandboxUnsupportedOSFailsClosed drives the REAL
// sandbox.New constructor path: on windows (and any other OS with no native
// implementation) an enforced posture must fail closed exactly like a missing
// bwrap. On darwin/linux with a usable sandbox the constructor succeeds and
// the stage proceeds confined instead, which the test accepts — the
// fail-closed contract is only observable where the platform lacks a sandbox.
func TestExecutorEnforcedSandboxRealConstructorNeverRunsUnconfined(t *testing.T) {
	run := newSandboxTestRun(t)
	adapter := &sandboxCapturingAdapter{FakeAdapter: FakeAdapter{
		Act: func(ctx context.Context, req RunRequest) error {
			return WriteCompletion(req.Workspace, req.CompletionPath, apiv1.ResultEnvelope{
				Status: apiv1.ResultSuccess,
			})
		},
	}}
	exec, err := NewExecutor(adapter, testInjector(t, "", "", noopRegistrar{}), run, run,
		NewContextResolver(run, t.TempDir()), journal.NewPatternScrubber(), "instructions",
		WithSandboxEnforcement())
	if err != nil {
		t.Fatalf("NewExecutor: %v", err)
	}

	_, invokeErr := exec.Invoke(context.Background(), testEnvelope(t.TempDir(), "repo:read"))
	if _, sandboxErr := sandbox.New(); sandboxErr != nil {
		if !errors.Is(invokeErr, ErrSandboxUnavailable) {
			t.Fatalf("Invoke error = %v, want ErrSandboxUnavailable on a host without a usable sandbox", invokeErr)
		}
		if len(adapter.calls) != 0 {
			t.Fatal("adapter ran unconfined on a host without a usable sandbox")
		}
		return
	}
	if invokeErr != nil {
		t.Fatalf("Invoke: %v", invokeErr)
	}
	if len(adapter.calls) != 1 || adapter.calls[0].Sandbox == nil {
		t.Fatal("adapter did not receive the platform sandbox under an enforced posture")
	}
}

func TestExecutorEnforcedSandboxRequiresJournalBackedRecorder(t *testing.T) {
	rec := &fakeRecorder{} // no Append: cannot journal the posture
	adapter := &sandboxCapturingAdapter{}
	exec, err := NewExecutor(adapter, testInjector(t, "", "", noopRegistrar{}), rec, rec, rec,
		journal.NewPatternScrubber(), "instructions", WithSandboxEnforcement())
	if err != nil {
		t.Fatalf("NewExecutor: %v", err)
	}
	exec.newSandbox = func() (sandbox.Sandbox, error) { return &stubSandbox{}, nil }

	_, err = exec.Invoke(context.Background(), testEnvelope(t.TempDir(), "repo:read"))
	if err == nil || !containsString(err.Error(), "isolation posture") {
		t.Fatalf("Invoke error = %v, want a recorder-cannot-journal-posture failure", err)
	}
	if len(adapter.calls) != 0 {
		t.Fatal("adapter ran despite an unauditable enforced posture")
	}
}

func TestExecutorDisabledPostureEmitsNothingAndPassesNoSandbox(t *testing.T) {
	run := newSandboxTestRun(t)
	adapter := &sandboxCapturingAdapter{FakeAdapter: FakeAdapter{
		Act: func(ctx context.Context, req RunRequest) error {
			return WriteCompletion(req.Workspace, req.CompletionPath, apiv1.ResultEnvelope{
				Status: apiv1.ResultSuccess,
			})
		},
	}}
	exec, err := NewExecutor(adapter, testInjector(t, "", "", noopRegistrar{}), run, run,
		NewContextResolver(run, t.TempDir()), journal.NewPatternScrubber(), "instructions")
	if err != nil {
		t.Fatalf("NewExecutor: %v", err)
	}
	exec.newSandbox = func() (sandbox.Sandbox, error) {
		t.Fatal("sandbox constructed under a disabled posture")
		return nil, nil
	}

	if _, err := exec.Invoke(context.Background(), testEnvelope(t.TempDir(), "repo:read")); err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if len(adapter.calls) != 1 || adapter.calls[0].Sandbox != nil {
		t.Fatalf("disabled posture leaked a sandbox into the adapter: %+v", adapter.calls)
	}
	if events := postureEvents(t, run); len(events) != 0 {
		t.Fatalf("disabled posture journaled %d isolation events, want 0 — unconfigured instances must emit nothing new", len(events))
	}
}

func containsString(haystack, needle string) bool {
	return bytes.Contains([]byte(haystack), []byte(needle))
}
