package scheduler

import (
	"context"
	"strings"
	"testing"

	"go.temporal.io/sdk/testsuite"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/engine"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/temporaltest"
	"github.com/goobers/goobers/providers"
)

type integrityRunner struct {
	invocations []apiv1.InvocationEnvelope
}

func (r *integrityRunner) Run(_ context.Context, env apiv1.InvocationEnvelope, _ apiv1.DeterministicRun) (apiv1.ResultEnvelope, error) {
	r.invocations = append(r.invocations, env)
	return apiv1.ResultEnvelope{Status: apiv1.ResultSuccess}, nil
}

type integrityWorkspace string

func (w integrityWorkspace) Path() string               { return string(w) }
func (integrityWorkspace) Remove(context.Context) error { return nil }

type integrityWorkspaces struct {
	path string
}

func (w integrityWorkspaces) Provision(context.Context, engine.WorkspaceRequest) (engine.Workspace, error) {
	return integrityWorkspace(w.path), nil
}

type executingStarter struct {
	activities *engine.Activities
	projection engine.JournalProjection
}

func (s *executingStarter) Start(_ context.Context, in engine.RunInput) (engine.StartResult, error) {
	var suite testsuite.WorkflowTestSuite
	env := temporaltest.NewWorkflowEnvironment(&suite)
	env.RegisterActivity(s.activities)
	env.ExecuteWorkflow(engine.Run, in)
	workflowErr := env.GetWorkflowError()

	value, err := env.QueryWorkflow(engine.JournalQuery)
	if err != nil {
		return engine.StartResult{}, err
	}
	if err := value.Get(&s.projection); err != nil {
		return engine.StartResult{}, err
	}
	if workflowErr != nil {
		return engine.StartResult{}, workflowErr
	}
	return engine.StartResult{RunID: in.RunID}, nil
}

func TestDispatchClassifiesDirectBacklogItemsBeforeEngineAdmission(t *testing.T) {
	const trustLabel = "team-approved"
	const routingLabel = "goobers:ready"
	spec := apiv1.WorkflowSpec{
		Gaggle: "web",
		Triggers: []apiv1.Trigger{{
			Type:       apiv1.TriggerBacklogItem,
			TrustLabel: trustLabel,
			Selector: map[string]string{
				trustLabel:   "true",
				routingLabel: "true",
			},
		}},
		Start: "implement",
		Tasks: []apiv1.Task{{
			Name:             "implement",
			Type:             apiv1.TaskDeterministic,
			Goal:             "implement",
			MinimumIntegrity: apiv1.IntegrityMaintainer,
			Run: &apiv1.DeterministicRun{
				Command:   []string{"true"},
				Workspace: apiv1.WorkspaceScratch,
			},
		}},
	}
	registry := engine.NewRegistryWithPreviewFeatures(true)
	if _, err := registry.Register("flow", spec); err != nil {
		t.Fatalf("register workflow: %v", err)
	}
	routingOnlySpec := spec
	routingOnlySpec.Triggers = []apiv1.Trigger{{
		Type:     apiv1.TriggerBacklogItem,
		Selector: map[string]string{routingLabel: "true"},
	}}
	if _, err := registry.Register("routing-only", routingOnlySpec); err != nil {
		t.Fatalf("register routing-only workflow: %v", err)
	}

	t.Run("approved item reaches stage", func(t *testing.T) {
		runner := &integrityRunner{}
		starter := &executingStarter{activities: &engine.Activities{
			Det: runner, Workspaces: integrityWorkspaces{path: t.TempDir()},
		}}
		s := newScheduler(t, Config{Starter: starter, Registry: registry})
		item := providers.WorkItem{
			Provider:  providers.ProviderGitHub,
			ID:        "approved",
			Labels:    []string{routingLabel, trustLabel},
			Integrity: apiv1.IntegrityUnapproved,
		}

		decision, err := s.Dispatch(context.Background(), Event{
			WorkflowName: "flow",
			Item:         &item,
			Reason:       "backlog-item",
			DedupeKey:    "github:approved",
		})
		if err != nil {
			t.Fatalf("Dispatch: %v", err)
		}
		if !decision.Started {
			t.Fatalf("decision = %+v, want started", decision)
		}
		if len(runner.invocations) != 1 {
			t.Fatalf("stage invocations = %d, want 1", len(runner.invocations))
		}
		if got := runner.invocations[0].Item.Integrity; got != apiv1.IntegrityMaintainer {
			t.Fatalf("invocation item integrity = %q, want maintainer", got)
		}
		if starter.projection.Item == nil || starter.projection.Item.Integrity != apiv1.IntegrityMaintainer {
			t.Fatalf("projected item = %+v, want maintainer integrity", starter.projection.Item)
		}
	})

	for _, tc := range []struct {
		name     string
		workflow string
	}{
		{name: "unapproved item is journaled and refused", workflow: "flow"},
		{name: "sole routing selector does not grant integrity", workflow: "routing-only"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runner := &integrityRunner{}
			starter := &executingStarter{activities: &engine.Activities{
				Det: runner, Workspaces: integrityWorkspaces{path: t.TempDir()},
			}}
			s := newScheduler(t, Config{Starter: starter, Registry: registry})
			item := providers.WorkItem{
				Provider:  providers.ProviderGitHub,
				ID:        tc.workflow,
				Labels:    []string{routingLabel},
				Integrity: apiv1.IntegrityUnapproved,
			}

			_, err := s.Dispatch(context.Background(), Event{
				WorkflowName: tc.workflow,
				Item:         &item,
				Reason:       "backlog-item",
				DedupeKey:    "github:" + tc.workflow,
			})
			if err == nil {
				t.Fatal("Dispatch succeeded, want integrity refusal")
			}
			if !strings.Contains(err.Error(), `integrity "unapproved" below minimum "maintainer"`) {
				t.Fatalf("Dispatch error = %v, want integrity refusal", err)
			}
			if len(runner.invocations) != 0 {
				t.Fatalf("stage invocations = %d, want 0", len(runner.invocations))
			}

			var refusal *journal.Event
			for _, op := range starter.projection.Ops {
				if op.Event != nil && op.Event.Error != nil &&
					op.Event.Error.Code == apiv1.IntegrityAdmissionErrorCode {
					refusal = op.Event
					break
				}
			}
			if refusal == nil ||
				refusal.Integrity != apiv1.IntegrityUnapproved ||
				refusal.MinimumIntegrity != apiv1.IntegrityMaintainer {
				t.Fatalf("journaled refusal = %+v", refusal)
			}
		})
	}
}
