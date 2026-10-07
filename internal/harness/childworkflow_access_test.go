package harness

import (
	"context"
	"errors"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/mcpio"
)

func TestExecutorOwnsChildWorkflowAccessForInvocation(t *testing.T) {
	for _, adapterFails := range []bool{false, true} {
		rec := &fakeRecorder{}
		closed := 0
		called := 0
		grant := &mcpio.ChildWorkflowAccess{Endpoint: "https://daemon.invalid", BearerToken: "goobers-child.test.signature"}
		adapter := &FakeAdapter{Act: func(_ context.Context, req RunRequest) error {
			called++
			if closed != 0 || req.ChildWorkflows == nil || req.ChildWorkflows.BearerToken != grant.BearerToken {
				t.Fatal("adapter did not receive live trusted access")
			}
			if req.ChildWorkflows == grant {
				t.Fatal("adapter aliases issuer access")
			}
			if adapterFails {
				return errors.New("adapter failed")
			}
			return WriteCompletion(req.Workspace, req.CompletionPath, apiv1.ResultEnvelope{Status: apiv1.ResultSuccess})
		}}
		exec, err := NewExecutor(adapter, testInjector(t, "", "", noopRegistrar{}), rec, rec, rec, journal.NewPatternScrubber(), "",
			WithChildWorkflowAccess(func(_ context.Context, env apiv1.InvocationEnvelope) (*mcpio.ChildWorkflowAccess, func() error, error) {
				if env.ChildWorkflowOrigin.AttemptID != "attempt" {
					t.Fatal("missing trusted origin")
				}
				return grant, func() error { closed++; return nil }, nil
			}))
		if err != nil {
			t.Fatal(err)
		}
		env := testEnvelope(t.TempDir())
		env.ChildWorkflowOrigin = &apiv1.ChildWorkflowOrigin{StageOccurrence: "occurrence", AttemptID: "attempt"}
		_, err = exec.Invoke(t.Context(), env)
		if (err != nil) != adapterFails || closed != 1 || called != 1 {
			t.Fatalf("invocation error=%v, closed=%d, called=%d", err, closed, called)
		}
	}
}

func TestExecutorRefusesChildInvocationWithoutGrant(t *testing.T) {
	for _, provider := range []ChildWorkflowAccessProvider{nil, func(context.Context, apiv1.InvocationEnvelope) (*mcpio.ChildWorkflowAccess, func() error, error) {
		return nil, nil, errors.New("revoked")
	}} {
		called := false
		rec := &fakeRecorder{}
		adapter := &FakeAdapter{Act: func(context.Context, RunRequest) error { called = true; return nil }}
		exec, err := NewExecutor(adapter, testInjector(t, "", "", noopRegistrar{}), rec, rec, rec, journal.NewPatternScrubber(), "", WithChildWorkflowAccess(provider))
		if err != nil {
			t.Fatal(err)
		}
		env := testEnvelope(t.TempDir())
		env.ChildWorkflowOrigin = &apiv1.ChildWorkflowOrigin{StageOccurrence: "occurrence", AttemptID: "attempt"}
		if _, err := exec.Invoke(t.Context(), env); err == nil || called {
			t.Fatalf("ungranted stage invoked adapter: called=%v err=%v", called, err)
		}
	}
}
