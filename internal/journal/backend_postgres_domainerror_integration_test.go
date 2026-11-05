//go:build cloudintegration

package journal_test

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"

	"go.temporal.io/sdk/testsuite"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/engine"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/readmodel"
	"github.com/goobers/goobers/internal/runsemantics"
	"github.com/goobers/goobers/internal/temporaltest"
)

// The actual engine/activity boundary authors these events. Persist and read
// them with the real PG backend under the production controller role, then
// derive the operator projection from the stored bytes (not the test's errors).
func TestPostgresEngineDomainFailureEnvelope(t *testing.T) {
	config := postgresJournalConfig(t)
	var role string
	if err := config.Pool.QueryRow(t.Context(), "SELECT current_user").Scan(&role); err != nil || role != "goobers_controller" {
		t.Fatalf("role=%s err=%v", role, err)
	}
	backend := postgresBackend(t, config)
	for index, tc := range []struct {
		name               string
		failure            error
		agentic, committed bool
		failures           int
	}{
		{name: "infrastructure", failure: invoke.InfrastructureFailure(errors.New("fixture infrastructure retry")), failures: 1},
		{name: "typed infrastructure", failure: invoke.InfrastructureFailure(runsemantics.CodedStageFailure("github_auth_failed", errors.New("denied"))), failures: 1},
		{name: "policy timeout", failure: invoke.Timeout(errors.New("session expired")), failures: 1},
		{name: "committed work survives next failure", failure: invoke.InfrastructureFailure(errors.New("provider lost")), agentic: true, committed: true, failures: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			preview := true
			task := apiv1.Task{Name: "work", Type: apiv1.TaskDeterministic, Goal: "exercise domain failure", Run: &apiv1.DeterministicRun{Command: []string{"true"}, Workspace: apiv1.WorkspaceScratch}, Retry: &apiv1.RetryPolicy{MaxAttempts: 2}}
			if tc.agentic {
				task.Type = apiv1.TaskAgentic
				task.Goober = "test"
				task.Run = nil
			}
			in := engine.RunInput{RunID: fmt.Sprintf("%032x", index+100), Gaggle: "domain", WorkflowName: "errors", Version: 1, PreviewFeaturesEnabled: &preview, Spec: apiv1.WorkflowSpec{Gaggle: "domain", Start: "work", Triggers: []apiv1.Trigger{{Type: apiv1.TriggerBacklogItem}}, Tasks: []apiv1.Task{task}}, RepoRef: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "test", Name: "repo"}}
			script := &domainFailureScript{failure: tc.failure, remaining: tc.failures}
			spaces := &domainFailureWorkspaces{root: t.TempDir(), firstCommitted: tc.committed}
			var suite testsuite.WorkflowTestSuite
			env := temporaltest.NewWorkflowEnvironment(&suite)
			env.RegisterActivity(&engine.Activities{Det: script, Goober: script, Workspaces: spaces})
			env.ExecuteWorkflow(engine.Run, in)
			if err := env.GetWorkflowError(); (err != nil) != (tc.failures > 1) {
				t.Fatalf("workflow err=%v, exhausted=%v", err, tc.failures > 1)
			}
			value, err := env.QueryWorkflow(engine.JournalQuery)
			if err != nil {
				t.Fatal(err)
			}
			var projection engine.JournalProjection
			if err := value.Get(&projection); err != nil {
				t.Fatal(err)
			}
			handle, owner, err := backend.CreateRun(t.Context(), journal.CreateRunRequest{Identity: projection.Identity})
			if err != nil {
				t.Fatal(err)
			}
			for _, op := range projection.Ops {
				// Context artifacts are unrelated to this regression. All authored event
				// ops, including stage boundaries and the terminal, go through PG.
				if op.Event == nil || op.Event.Type == journal.EventRunStarted {
					continue
				}
				if _, err := backend.AppendBatch(t.Context(), handle, owner, []journal.Event{*op.Event}); err != nil {
					t.Fatal(err)
				}
			}
			if err := backend.Retire(t.Context(), handle, owner); err != nil {
				t.Fatal(err)
			}
			reader, err := backend.OpenRun(t.Context(), journal.RunRef{ID: in.RunID})
			if err != nil {
				t.Fatal(err)
			}
			events, err := reader.Events()
			if err != nil {
				t.Fatal(err)
			}
			want := journal.ErrorDetailFor("executor_error", tc.failure)
			code, class := runsemantics.ClassifyDispatchError(tc.failure)
			count := 0
			for _, event := range events {
				if event.Type != journal.EventError || event.Error.Code != "executor_error" {
					continue
				}
				count++
				if !reflect.DeepEqual(event.Error, want) {
					t.Fatalf("stored error=%+v want=%+v", event.Error, want)
				}
				if event.Runner["errorCode"] != code || event.Runner["errorClass"] != string(class) {
					t.Fatalf("classification=%v", event.Runner)
				}
				committed, exists := event.Runner[runsemantics.InfraCommittedWorkKey].(bool)
				if !exists || committed != tc.committed {
					t.Fatalf("committed-work marker=%v want=%v", event.Runner, tc.committed)
				}
			}
			if count != tc.failures {
				t.Fatalf("errors=%d want=%d", count, tc.failures)
			}
			if tc.failures > 1 {
				want = journal.ErrorDetailFor("run_failed", tc.failure)
			}
			projected := readmodel.ProjectRun(projection.Identity, readmodel.Projection{}, events)
			if !reflect.DeepEqual(projected.Run.Operator.LatestError, want) {
				t.Fatalf("operator error=%+v want=%+v", projected.Run.Operator.LatestError, want)
			}
		})
	}
}

type domainFailureScript struct {
	failure   error
	remaining int
}

func (s *domainFailureScript) Invoke(context.Context, apiv1.InvocationEnvelope) (apiv1.ResultEnvelope, error) {
	if s.remaining > 0 {
		s.remaining--
		return apiv1.ResultEnvelope{}, s.failure
	}
	return apiv1.ResultEnvelope{Status: apiv1.ResultSuccess}, nil
}
func (s *domainFailureScript) Run(ctx context.Context, env apiv1.InvocationEnvelope, _ apiv1.DeterministicRun) (apiv1.ResultEnvelope, error) {
	return s.Invoke(ctx, env)
}
func (*domainFailureScript) Review(context.Context, apiv1.InvocationEnvelope) (apiv1.Verdict, error) {
	return apiv1.Verdict{}, errors.New("unexpected review")
}

type domainFailureWorkspaces struct {
	root           string
	firstCommitted bool
}

func (w *domainFailureWorkspaces) Provision(context.Context, engine.WorkspaceRequest) (engine.Workspace, error) {
	committed := w.firstCommitted
	w.firstCommitted = false
	return domainFailureWorkspace{path: w.root, committed: committed}, nil
}

type domainFailureWorkspace struct {
	path      string
	committed bool
}

func (w domainFailureWorkspace) Path() string                                { return w.path }
func (domainFailureWorkspace) Remove(context.Context) error                  { return nil }
func (w domainFailureWorkspace) HasNewCommits(context.Context) (bool, error) { return w.committed, nil }
