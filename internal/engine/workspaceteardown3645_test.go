package engine

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

// workspaceteardown3645_test.go covers #3645: a stage attempt's workspace
// teardown must be bounded and must never fail silently. A wedged git
// cleanup previously ran detached with no deadline (retaining worker
// resources indefinitely) and every removal error was discarded, so locked
// worktrees accumulated with nothing an operator could act on.

// teardownWorkspace is a Workspace whose Remove behavior each test scripts.
type teardownWorkspace struct {
	path        string
	err         error
	block       bool
	called      chan struct{}
	removeErr   error
	hadDeadline bool
}

func (w *teardownWorkspace) Path() string { return w.path }

func (w *teardownWorkspace) Remove(ctx context.Context) error {
	if w.called != nil {
		w.removeErr = ctx.Err()
		_, w.hadDeadline = ctx.Deadline()
		close(w.called)
	}
	if w.block {
		<-ctx.Done()
		return ctx.Err()
	}
	return w.err
}

// captureTeardownLogs redirects the default slog logger (the non-activity
// fallback reportWorkspaceTeardownFailure uses) for the duration of a test.
func captureTeardownLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	buf := &bytes.Buffer{}
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelError})))
	t.Cleanup(func() { slog.SetDefault(previous) })
	return buf
}

func shortenTeardownTimeout(t *testing.T, d time.Duration) {
	t.Helper()
	previous := workspaceTeardownTimeout
	workspaceTeardownTimeout = d
	t.Cleanup(func() { workspaceTeardownTimeout = previous })
}

func TestRemoveWorkspaceBoundsHungTeardown(t *testing.T) {
	shortenTeardownTimeout(t, 50*time.Millisecond)
	logs := captureTeardownLogs(t)
	ws := &teardownWorkspace{path: "/tmp/wedged-worktree", block: true}

	done := make(chan struct{})
	go func() {
		defer close(done)
		removeWorkspace(context.Background(), "run-1:implement", ws)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("a hung workspace teardown was never bounded; it retains the worker's resources")
	}

	logged := logs.String()
	for _, want := range []string{"exceeded", "run-1:implement", "/tmp/wedged-worktree"} {
		if !strings.Contains(logged, want) {
			t.Errorf("timed-out teardown diagnostics %q must name %q", logged, want)
		}
	}
}

func TestRemoveWorkspaceReportsFailure(t *testing.T) {
	logs := captureTeardownLogs(t)
	ws := &teardownWorkspace{path: "/tmp/locked-worktree", err: errors.New("worktree is locked")}

	removeWorkspace(context.Background(), "run-2:review", ws)

	logged := logs.String()
	for _, want := range []string{"worktree is locked", "run-2:review", "/tmp/locked-worktree"} {
		if !strings.Contains(logged, want) {
			t.Errorf("failed teardown diagnostics %q must name %q", logged, want)
		}
	}
}

func TestRemoveWorkspaceSucceedsQuietlyAndOutlivesTheAttempt(t *testing.T) {
	logs := captureTeardownLogs(t)
	called := make(chan struct{})
	ws := &teardownWorkspace{path: "/tmp/clean-worktree", called: called}

	// An already-cancelled attempt context still tears its workspace down:
	// teardown is detached (bounded, not cancelled with the attempt).
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	removeWorkspace(ctx, "run-3:implement", ws)

	select {
	case <-called:
		if ws.removeErr != nil {
			t.Fatalf("teardown context is already done (%v); an expired attempt must still clean up", ws.removeErr)
		}
		if !ws.hadDeadline {
			t.Error("teardown context has no deadline; detached cleanup must stay bounded")
		}
	default:
		t.Fatal("Remove was never called")
	}
	if logged := logs.String(); logged != "" {
		t.Errorf("successful teardown logged %q; it must stay quiet", logged)
	}
}

func TestRemoveWorkspaceIgnoresNilWorkspace(t *testing.T) {
	logs := captureTeardownLogs(t)
	removeWorkspace(context.Background(), "run-4:implement", nil)
	if logged := logs.String(); logged != "" {
		t.Errorf("a nil workspace logged %q; there is nothing to tear down", logged)
	}
}

// TestTeardownFailureLeavesStageResultIntact holds the additive contract
// (#136): a teardown failure is reported, never promoted into the stage's own
// result or error.
func TestTeardownFailureLeavesStageResultIntact(t *testing.T) {
	logs := captureTeardownLogs(t)
	activities := &Activities{
		Goober: &fakeInvoker{
			invoke: func(context.Context, apiv1.InvocationEnvelope) (apiv1.ResultEnvelope, error) {
				return apiv1.ResultEnvelope{Status: apiv1.ResultSuccess, Summary: "done"}, nil
			},
		},
		Workspaces: workspaceProvisionerFunc(func(context.Context, WorkspaceRequest) (Workspace, error) {
			return &teardownWorkspace{path: "/tmp/locked-worktree", err: errors.New("worktree is locked")}, nil
		}),
	}

	res, err := activities.InvokeGoober(context.Background(), apiv1.InvocationEnvelope{
		RunID: "run-5", TaskID: "run-5:implement",
	}, "")
	if err != nil {
		t.Fatalf("InvokeGoober = %v, want the stage's own success despite a teardown failure", err)
	}
	if res.Status != apiv1.ResultSuccess || res.Summary != "done" {
		t.Errorf("stage result = %+v, want the seam's success/summary unchanged", res.ResultEnvelope)
	}
	if !strings.Contains(logs.String(), "worktree is locked") {
		t.Errorf("teardown failure %q was not surfaced as a diagnostic", logs.String())
	}
}

type workspaceProvisionerFunc func(context.Context, WorkspaceRequest) (Workspace, error)

func (f workspaceProvisionerFunc) Provision(ctx context.Context, req WorkspaceRequest) (Workspace, error) {
	return f(ctx, req)
}
