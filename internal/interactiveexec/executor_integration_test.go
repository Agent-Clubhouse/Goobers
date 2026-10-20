//go:build integration

package interactiveexec

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/harness"
	"github.com/goobers/goobers/internal/interactive"
	auth "github.com/goobers/goobers/internal/interactiveauth"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/test/testsupport/testdep"
)

func integrationExecutor(t *testing.T, mode string) *Executor {
	t.Helper()
	testdep.Require(t, "git", "node")
	p, err := auth.LocalPrincipal("test-owner")
	if err != nil {
		t.Fatal(err)
	}
	broker, err := auth.Open(auth.Config{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = broker.Close() })
	for _, provider := range []auth.Provider{auth.ADO, auth.AppInsights} {
		if _, err := broker.ConnectDemo(context.Background(), p, provider); err != nil {
			t.Fatal(err)
		}
	}
	e, err := New(Config{StateDir: t.TempDir(), SourceDir: "../../samples/interactive-investigation", Mode: mode, Broker: broker, Principal: p, TrustedLocalModelLogin: mode == "codex-local"})
	if err != nil {
		t.Fatal(err)
	}
	e.config.ResolveAuthority = func(_ context.Context, req interactive.ExecutionRequest, provider auth.Provider) (auth.ConnectionRef, error) {
		wanted := "demo-ado"
		if provider == auth.AppInsights {
			wanted = "demo-insights"
		}
		configured := false
		for _, ref := range req.AuthorityRefs {
			if ref == wanted {
				configured = true
			}
		}
		if !configured {
			return auth.ConnectionRef{}, nil
		}
		connections, err := broker.ListConnections(p)
		if err != nil {
			return auth.ConnectionRef{}, err
		}
		for _, connection := range connections {
			if connection.Provider == provider {
				return connection.Ref, nil
			}
		}
		return auth.ConnectionRef{}, nil
	}
	return e
}
func integrationRequest() interactive.ExecutionRequest {
	return interactive.ExecutionRequest{SessionID: "test-session", TurnID: "turn-one", Owner: "test-owner", Mode: "workbench", Fence: 1, Attempt: 1, LeaseID: "lease-one", AuthorityRefs: []string{"demo-ado", "demo-insights"}}
}

func TestIntegrationActualEditTestsAndCumulativeChildIsolation(t *testing.T) {
	testdep.Require(t, "git", "node")
	e := integrationExecutor(t, "local-tools")
	req := integrationRequest()
	req.Text = "investigate"
	var toolOutput bool
	emit := func(event interactive.ExecutionEvent) error {
		if event.Type == "tool.output" {
			toolOutput = true
		}
		return nil
	}
	result, err := e.Execute(context.Background(), req, emit)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result.Message, "reproduced the failure") {
		t.Fatal(result.Message)
	}
	req.TurnID = "turn-two"
	req.Text = "fix"
	result, err = e.Execute(context.Background(), req, emit)
	if err != nil {
		t.Fatal(err)
	}
	if !toolOutput || !strings.Contains(result.Diff, "Math.max") || len(result.Changes) != 1 {
		t.Fatalf("missing actual edit/test evidence %+v", result)
	}
	// A native agent's private checkpoint commit must not erase cumulative review.
	for _, args := range [][]string{{"add", "invoice.mjs"}, {"-c", "user.name=Test", "-c", "user.email=test@local", "-c", "commit.gpgsign=false", "commit", "-qm", "private checkpoint"}} {
		if _, err := run(context.Background(), result.WorkspacePath, append([]string{"git"}, args...), nil); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(result.WorkspacePath, "new.txt"), []byte("new evidence\n"), 0600); err != nil {
		t.Fatal(err)
	}
	changes, diff, err := e.changes(context.Background(), result.WorkspacePath)
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != 2 || !strings.Contains(diff, "Math.max") || !strings.Contains(diff, "new evidence") {
		t.Fatalf("lost cumulative changes: %s", diff)
	}
	req.ChildID = "review-one"
	req.Mode = "helper"
	parentControlInvoked := false
	e.config.Helper = func(context.Context, interactive.ExecutionRequest, func(interactive.ExecutionEvent) error) (string, error) {
		parentControlInvoked = true
		return "parent mutation", nil
	}
	req.ReadOnly = true
	req.TurnID = "child-turn"
	req.Text = "review"
	child, err := e.Execute(context.Background(), req, emit)
	if err != nil {
		t.Fatal(err)
	}
	if parentControlInvoked {
		t.Fatal("read-only Helper child invoked parent controls")
	}
	if child.WorkspacePath == result.WorkspacePath || !strings.Contains(child.Message, "all tests pass") {
		t.Fatalf("child result %+v", child)
	}
	_, after, err := e.changes(context.Background(), result.WorkspacePath)
	if err != nil || after != diff {
		t.Fatal("child altered parent snapshot")
	}
	req.Owner = "another-user"
	if _, err := e.Execute(context.Background(), req, emit); err == nil {
		t.Fatal("cross-owner execution admitted")
	}
	// Reverting all edits must actively clear the previously published diff.
	baseline, err := os.ReadFile(e.metadata(result.WorkspacePath, "baseline"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := run(context.Background(), result.WorkspacePath, []string{"git", "restore", "--source=" + strings.TrimSpace(string(baseline)), "--staged", "--worktree", "."}, nil); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(result.WorkspacePath, "new.txt")); err != nil {
		t.Fatal(err)
	}
	changes, diff, err = e.changes(context.Background(), result.WorkspacePath)
	if err != nil || changes == nil || len(changes) != 0 || diff != "" {
		t.Fatalf("reverted diff was not explicitly cleared: %+v %q %v", changes, diff, err)
	}
}

func TestIntegrationSteeringCancellationAndCustody(t *testing.T) {
	testdep.Require(t, "git", "node")
	e := integrationExecutor(t, "local-tools")
	req := integrationRequest()
	req.Text = "fix"
	steering := make(chan interactive.Steering, 1)
	req.Steering = steering
	ack := make(chan error, 1)
	steering <- interactive.Steering{CommandID: "steer-one", Text: "Do not edit; read only", Ack: func(err error) { ack <- err }}
	result, err := e.Execute(context.Background(), req, func(interactive.ExecutionEvent) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if err := <-ack; err != nil {
		t.Fatal(err)
	}
	if len(result.Changes) != 0 || !strings.Contains(result.Message, "No edit") {
		t.Fatalf("steer not applied: %+v", result)
	}
	req.TurnID = "wait-turn"
	req.Text = "wait"
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := e.Execute(ctx, req, func(ev interactive.ExecutionEvent) error {
			if ev.Type == "tool.started" && ev.Data["name"] == "bounded.wait" {
				cancel()
			}
			return nil
		})
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("cancel reported success")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("cancel failed to release worker")
	}
	if err := e.Recover(context.Background(), req); err != nil {
		t.Fatal(err)
	}
}

func TestIntegrationNativeCodexInvestigation(t *testing.T) {
	testdep.RequireEnv(t, "GOOBERS_INTERACTIVE_NATIVE_SMOKE")
	testdep.Require(t, "codex")
	e := integrationExecutor(t, "codex-local")
	req := integrationRequest()
	req.Text = "Investigate invoice.mjs and the failing test. Run node --test to reproduce the problem, fix the incorrect negative total without changing the public API, and rerun tests. Preserve uncommitted changes."
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	result, err := e.Execute(ctx, req, func(event interactive.ExecutionEvent) error {
		if event.Type == "tool.started" || event.Type == "tool.completed" {
			t.Logf("%s %v", event.Type, event.Data["name"])
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Changes) == 0 || !strings.Contains(result.Diff, "Math.max") {
		t.Fatalf("native harness did not preserve fix: %+v", result)
	}
	if _, err := run(ctx, result.WorkspacePath, []string{"node", "--test"}, nil); err != nil {
		t.Fatal(err)
	}
	t.Log("Actual Codex harness edited the private repository; independent Node tests passed. Provider observations were controlled fakes.")
}

func TestIntegrationFinalInspectionRetainsCustodyAndEarlierError(t *testing.T) {
	testdep.Require(t, "git", "node")
	e := integrationExecutor(t, "local-tools")
	if _, err := e.prepare(context.Background(), integrationRequest()); err != nil {
		t.Fatal(err)
	}
	e.runProcess = func(context.Context, string, []string, func(harness.TranscriptDelta) error) (harness.ProcessResult, error) {
		return harness.ProcessResult{}, invoke.ErrWorkspaceNotQuiescent
	}
	fenced := errors.New("event_fenced")
	result, err := e.Execute(context.Background(), integrationRequest(), func(interactive.ExecutionEvent) error { return fenced })
	if !result.CustodyPending || !errors.Is(err, invoke.ErrWorkspaceNotQuiescent) || !errors.Is(err, fenced) {
		t.Fatalf("lost final-inspection custody/error: %+v %v", result, err)
	}
	if err := e.Recover(context.Background(), integrationRequest()); err == nil {
		t.Fatal("final-inspection writer marker was removed")
	}
}
