package engine

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/testsuite"
	corev1 "k8s.io/api/core/v1"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/dispatcher"
	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/temporaltest"
	wf "github.com/goobers/goobers/internal/workflow"
	"github.com/goobers/goobers/internal/workspacerevision"
)

func TestSelectedRevisionDispatchRefusesBeforeWorkspaceOrPodEffects(t *testing.T) {
	env := apiv1.InvocationEnvelope{WorkspaceRevision: &apiv1.WorkspaceRevision{
		Repository: apiv1.RepositoryIdentity{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web"},
		CommitSHA:  strings.Repeat("a", 40),
	}}
	activities := &Activities{}
	_, localErr := activities.provisionWorkspace(context.Background(), &env, apiv1.WorkspaceScratch, false, "", "")
	_, podErr := activities.DispatchStage(context.Background(), DispatchStageInput{Envelope: env})
	for name, err := range map[string]error{"workerhost": localErr, "pod": podErr} {
		var applicationErr *temporal.ApplicationError
		if !errors.As(err, &applicationErr) || applicationErr.Type() != workspacerevision.CodeInvalid || !applicationErr.NonRetryable() {
			t.Errorf("%s admission error = %v", name, err)
		}
	}
}

func TestWorkspaceRevisionMalformedSurrenderIsNonRetryable(t *testing.T) {
	for _, raw := range []string{
		`{"result":{"status":"failure","error":{"code":"command","message":"failed"},"workspaceRevision":null}}`,
		`{"result":{"status":"success","workspaceRevision":{"repository":{"provider":"github","owner":"acme","name":"web"},"commitSha":"short"}}}`,
	} {
		store := surrenderStore(t)
		fake := &fakeStageDispatcher{report: dispatcher.Report{
			Runner: "win-ci", Pod: "pod", Phase: corev1.PodSucceeded, SurrenderConfirmed: true,
		}}
		produce := detTask("produce", wf.TerminalComplete)
		produce.Retry = &apiv1.RetryPolicy{MaxAttempts: 3}
		produce.ContinueOnError = true
		in := runInput("bad-revision", fixtureSpec("produce", []apiv1.Task{produce}, nil))
		if err := store.Put(context.Background(), in.RunID, "produce", 1, []byte(raw)); err != nil {
			t.Fatal(err)
		}
		in.Placements = []PinnedPlacement{{
			Stage: "produce", Queue: dispatcher.QueueName("web", "win-ci"),
			Eligible: remoteEligible(), Memory: "1Gi",
		}}
		var suite testsuite.WorkflowTestSuite
		env := temporaltest.NewWorkflowEnvironment(&suite)
		env.RegisterActivity(&Activities{Dispatcher: fake, Surrenders: store})
		env.ExecuteWorkflow(Run, in)
		err := env.GetWorkflowError()
		var applicationErr *temporal.ApplicationError
		if err == nil || !strings.Contains(err.Error(), workspacerevision.CodeInvalid) || fake.calls.Load() != 1 {
			t.Fatalf("malformed surrender was retried or recoded: dispatches=%d err=%v", fake.calls.Load(), err)
		}
		// Check the activity error itself before the workflow serializes the
		// terminal revision error returned by its explicit refusal branch.
		_, activityErr := (&Activities{Surrenders: store}).readDispatchSurrender(context.Background(), dispatcher.Attempt{
			RunID: in.RunID, Stage: "produce", Number: 1,
		}, true)
		classified := classifySeamError(activityErr)
		if !errors.As(classified, &applicationErr) || applicationErr.Type() != workspacerevision.CodeInvalid || !applicationErr.NonRetryable() {
			t.Fatalf("activity lost its nonretryable code: %v", classified)
		}
		value, err := env.QueryWorkflow(JournalQuery)
		if err != nil {
			t.Fatal(err)
		}
		var projection JournalProjection
		if err := value.Get(&projection); err != nil {
			t.Fatal(err)
		}
		dir, err := ProjectRun(t.TempDir(), projection)
		if err != nil {
			t.Fatal(err)
		}
		events := readJournalEvents(t, dir)
		starts, refusals := 0, 0
		for _, event := range events {
			if event.Type == journal.EventStageStarted {
				starts++
			}
			if event.Type == journal.EventStageFinished {
				t.Fatal("malformed surrender was recorded as an accepted result")
			}
			if event.Type == journal.EventError && event.Error != nil && event.Error.Code == workspacerevision.CodeInvalid {
				refusals++
			}
		}
		if starts != 1 || refusals != 1 {
			t.Fatalf("manual retry loop: starts=%d refusals=%d", starts, refusals)
		}
	}
}

func TestWorkspaceRevisionEngineRejectsUnsupportedAuthorityBeforeDownstreamDispatch(t *testing.T) {
	produce := detTask("produce", "consume")
	produce.ContinueOnError = true
	result := apiv1.ResultEnvelope{Status: apiv1.ResultSuccess}
	data := []byte(`{"workspaceRevision":{"repository":{"provider":"github","owner":"acme","name":"web"},"commitSha":"` + strings.Repeat("a", 40) + `"}}`)
	if err := executor.MergeResultFileOutputs(&result, data); err != nil {
		t.Fatal(err)
	}

	plane, err := dispatcher.NewSurrenderDir(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	surrendered, err := json.Marshal(dispatcher.SurrenderedResult{Result: result})
	if err != nil {
		t.Fatal(err)
	}
	if err := plane.Put(context.Background(), "unsupported-revision", "produce", 1, surrendered); err != nil {
		t.Fatal(err)
	}
	admitted, err := dispatcher.ReadSurrenderedResult(context.Background(), plane, "unsupported-revision", "produce", 1)
	if err != nil {
		t.Fatal(err)
	}
	events := runEngineFixture(t, conformanceFixture{
		spec:          fixtureSpec("produce", []apiv1.Task{produce, detTask("consume", wf.TerminalComplete)}, nil),
		script:        map[string][]scriptedCall{"produce": {{result: admitted.Result}}},
		wantEngineErr: true,
	}, "unsupported-revision")
	rejected := false
	for _, event := range events {
		if event.Type == journal.EventStageFinished || event.Stage == "consume" {
			t.Errorf("unsupported authority accepted or dispatched: %+v", event)
		}
		if event.Type == journal.EventError && event.Stage == "produce" && event.Error != nil && event.Error.Code == workspacerevision.CodeInvalid {
			rejected = true
		}
	}
	if !rejected {
		t.Fatal("refusal was not journaled")
	}
}

func TestWorkspaceRevisionDispatchStageNormalizesBeforeReturn(t *testing.T) {
	for _, status := range []apiv1.ResultStatus{apiv1.ResultSuccess, apiv1.ResultFailure, apiv1.ResultBlocked, apiv1.ResultNoWork} {
		for _, deterministic := range []bool{false, true} {
			store := surrenderStore(t)
			result := apiv1.ResultEnvelope{Status: status, Summary: "reported outcome", WorkspaceRevision: &apiv1.WorkspaceRevision{
				Repository: apiv1.RepositoryIdentity{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web"},
				CommitSHA:  strings.Repeat("a", 40),
			}}
			if status == apiv1.ResultFailure {
				result.Error = &apiv1.ErrorInfo{Code: "command", Message: "failed"}
			}
			putSurrendered(t, store, "normalize", "produce", 1, dispatcher.SurrenderedResult{Result: result})
			input := dispatchInput("normalize", "produce", 1)
			if deterministic {
				input.Run = &apiv1.DeterministicRun{Command: []string{"true"}, Workspace: apiv1.WorkspaceScratch}
			}
			activities := &Activities{Surrenders: store, Dispatcher: &fakeStageDispatcher{
				report: dispatcher.Report{SurrenderConfirmed: true, Phase: corev1.PodSucceeded},
			}}
			normalized, err := activities.DispatchStage(context.Background(), input)
			want := workspacerevision.CodeUnauthorized
			if deterministic {
				want = ""
				if status == apiv1.ResultSuccess {
					want = workspacerevision.CodeInvalid
				}
			}
			var appErr *temporal.ApplicationError
			if (err == nil) != (want == "") || (err != nil && (!errors.As(err, &appErr) || appErr.Type() != want || !appErr.NonRetryable())) {
				t.Fatalf("status=%s deterministic=%t: %v", status, deterministic, err)
			}
			if err == nil && (normalized.Status != status || normalized.WorkspaceRevision != nil) {
				t.Fatalf("invalid normalized result: %+v", normalized)
			}
		}
	}
}
