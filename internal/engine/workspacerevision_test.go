package engine

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	historypb "go.temporal.io/api/history/v1"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/testsuite"
	temporalworker "go.temporal.io/sdk/worker"
	"google.golang.org/protobuf/encoding/protojson"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/temporaltest"
	wf "github.com/goobers/goobers/internal/workflow"
	"github.com/goobers/goobers/internal/workspacerevision"
)

func selectedRevisionFixture() *apiv1.WorkspaceRevision {
	return &apiv1.WorkspaceRevision{
		Repository: apiv1.RepositoryIdentity{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web"},
		CommitSHA:  strings.Repeat("a", 40), SourceRef: "moving-branch", SourceID: "pr:42",
	}
}

func TestWorkspaceRevisionAcceptance(t *testing.T) {
	for _, tc := range []struct {
		name    string
		kind    apiv1.TaskType
		status  apiv1.ResultStatus
		change  func(*apiv1.WorkspaceRevision)
		current bool
		code    string
	}{
		{name: "first", kind: apiv1.TaskDeterministic, status: apiv1.ResultSuccess},
		{name: "identical", kind: apiv1.TaskDeterministic, status: apiv1.ResultSuccess, current: true},
		{name: "agentic", kind: apiv1.TaskAgentic, status: apiv1.ResultSuccess, code: workspacerevision.CodeUnauthorized},
		{name: "failed", kind: apiv1.TaskDeterministic, status: apiv1.ResultFailure},
		{name: "conflict", kind: apiv1.TaskDeterministic, status: apiv1.ResultSuccess, current: true,
			change: func(r *apiv1.WorkspaceRevision) { r.CommitSHA = strings.Repeat("b", 40) }, code: workspacerevision.CodeConflict},
		{name: "malformed", kind: apiv1.TaskDeterministic, status: apiv1.ResultSuccess,
			change: func(r *apiv1.WorkspaceRevision) { r.CommitSHA = "main" }, code: workspacerevision.CodeInvalid},
		{name: "unauthorized", kind: apiv1.TaskDeterministic, status: apiv1.ResultSuccess,
			change: func(r *apiv1.WorkspaceRevision) { r.Repository.Owner = "attacker" }, code: workspacerevision.CodeUnauthorized},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := runInput("revision", linearSpec())
			if tc.current {
				in.WorkspaceRevision = selectedRevisionFixture()
			}
			candidate := selectedRevisionFixture()
			if tc.change != nil {
				tc.change(candidate)
			}
			res, err := acceptWorkspaceRevision(in, apiv1.Task{Type: tc.kind},
				apiv1.ResultEnvelope{Status: tc.status, WorkspaceRevision: candidate})
			if got := workspaceRevisionErrorCode(err); got != tc.code {
				t.Fatalf("code = %q, want %q: %v", got, tc.code, err)
			}
			if tc.code != "" {
				var app *temporal.ApplicationError
				if !errors.As(err, &app) || !app.NonRetryable() {
					t.Fatalf("not nonretryable: %v", err)
				}
			} else if tc.status != apiv1.ResultSuccess {
				if res.WorkspaceRevision != nil {
					t.Fatal("failed result established authority")
				}
			} else if !reflect.DeepEqual(res.WorkspaceRevision, candidate) || res.WorkspaceRevision == candidate {
				t.Fatal("accepted selection was not independently copied")
			}
		})
	}
}

func TestWorkspaceRevisionWorkflowRetryAndContinuity(t *testing.T) {
	spec := fixtureSpec("select", []apiv1.Task{
		{Name: "select", Type: apiv1.TaskDeterministic, Goal: "select",
			Run: &apiv1.DeterministicRun{Command: []string{"true"}, Workspace: apiv1.WorkspaceScratch}, Next: "inspect"},
		{Name: "inspect", Type: apiv1.TaskDeterministic, Goal: "inspect",
			Run: &apiv1.DeterministicRun{Command: []string{"true"}, Workspace: apiv1.WorkspaceRepoReadOnly}, Next: wf.TerminalComplete},
	}, nil)
	in := runInput("revision", spec)
	in.RepoRef.Checkout = &apiv1.CheckoutSpec{Sparse: []string{"src"}}
	workspaces := testWorkspaces(t)
	inspectionAttempts := 0
	runner := &fakeRunner{run: func(_ context.Context, env apiv1.InvocationEnvelope, _ apiv1.DeterministicRun) (apiv1.ResultEnvelope, error) {
		if strings.HasSuffix(env.TaskID, ":select") {
			if env.WorkspaceRevision != nil {
				t.Error("selector received a future selection")
			}
			return apiv1.ResultEnvelope{Status: apiv1.ResultSuccess, WorkspaceRevision: selectedRevisionFixture(),
				Outputs: map[string]interface{}{"workspaceBranch": "goobers/pr-42"}}, nil
		}
		inspectionAttempts++
		if !reflect.DeepEqual(env.WorkspaceRevision, selectedRevisionFixture()) {
			t.Errorf("attempt %d selection = %+v", inspectionAttempts, env.WorkspaceRevision)
		}
		if env.RepoRef.Owner != "acme" || env.BaseBranch != "main" {
			t.Error("base operation identity changed")
		}
		if inspectionAttempts == 1 {
			return apiv1.ResultEnvelope{}, invoke.InfrastructureFailure(errors.New("worker lost"))
		}
		return apiv1.ResultEnvelope{Status: apiv1.ResultSuccess, WorkspaceRevision: selectedRevisionFixture()}, nil
	}}
	var ts testsuite.WorkflowTestSuite
	env := temporaltest.NewWorkflowEnvironment(&ts)
	env.RegisterActivity(&Activities{Det: runner, Workspaces: workspaces})
	env.ExecuteWorkflow(Run, in)
	if err := env.GetWorkflowError(); err != nil {
		t.Fatal(err)
	}
	if inspectionAttempts != 2 {
		t.Fatalf("inspection attempts = %d", inspectionAttempts)
	}
	for _, req := range workspaces.requests {
		if req.Stage != "inspect" {
			continue
		}
		if !reflect.DeepEqual(req.WorkspaceRevision, selectedRevisionFixture()) ||
			req.WorkspaceBranch != "" || req.WorkspaceDelta != "" || req.SyncBase {
			t.Fatalf("readonly request mixed continuity controls: %+v", req)
		}
		if !reflect.DeepEqual(req.Checkout, in.RepoRef.Checkout) {
			t.Fatalf("checkout policy lost: %+v", req.Checkout)
		}
	}
	accepted := 0
	for _, ev := range projectEngineJournal(t, env) {
		if ev.Type == journal.EventStageFinished && ev.WorkspaceRevision != nil {
			accepted++
			if !reflect.DeepEqual(ev.WorkspaceRevision, selectedRevisionFixture()) {
				t.Fatalf("journal selection = %+v", ev.WorkspaceRevision)
			}
		}
		if ev.Type == journal.EventRunnerWorkspaceDelta {
			t.Fatal("readonly selection published or selected a delta")
		}
	}
	if accepted != 2 {
		t.Fatalf("accepted journal controls = %d", accepted)
	}
}

func TestWorkspaceRevisionRecordedLegacyHistories(t *testing.T) {
	for _, name := range []string{"continuity-prechange-history.json", "dispatchstage_history.json", "learning-episode-prechange-history.json"} {
		t.Run(name, func(t *testing.T) {
			data, err := os.ReadFile(filepath.Join("testdata", name))
			if err != nil {
				t.Fatal(err)
			}
			history := &historypb.History{}
			if err := protojson.Unmarshal(data, history); err != nil {
				t.Fatal(err)
			}
			replayer := temporalworker.NewWorkflowReplayer()
			replayer.RegisterWorkflow(Run)
			if err := replayer.ReplayWorkflowHistory(nil, history); err != nil {
				t.Fatalf("legacy recorded history failed replay: %v", err)
			}
		})
	}
}

func TestWorkspaceRevisionProvisionFailureClassification(t *testing.T) {
	for _, code := range []string{workspacerevision.CodeUnauthorized, workspacerevision.CodeAcquisition,
		workspacerevision.CodeObjectType, workspacerevision.CodeSHAMismatch} {
		err := classifySeamError(&workspacerevision.Error{Code: code, Message: "refused"})
		if got := workspaceRevisionErrorCode(err); got != code {
			t.Fatalf("classification = %q, want %q", got, code)
		}
		var app *temporal.ApplicationError
		if !errors.As(err, &app) || !app.NonRetryable() {
			t.Fatalf("retryable: %v", err)
		}
	}

}

func TestWorkspaceRevisionWorkflowConflictIsFatal(t *testing.T) {
	spec := fixtureSpec("select", []apiv1.Task{
		{Name: "select", Type: apiv1.TaskDeterministic, Goal: "select",
			Run: &apiv1.DeterministicRun{Command: []string{"true"}, Workspace: apiv1.WorkspaceScratch}, Next: "replace"},
		{Name: "replace", Type: apiv1.TaskDeterministic, Goal: "replace", ContinueOnError: true,
			Run:   &apiv1.DeterministicRun{Command: []string{"true"}, Workspace: apiv1.WorkspaceScratch},
			Retry: &apiv1.RetryPolicy{MaxAttempts: 3}, Next: wf.TerminalComplete},
	}, nil)
	calls := 0
	runner := &fakeRunner{run: func(_ context.Context, env apiv1.InvocationEnvelope, _ apiv1.DeterministicRun) (apiv1.ResultEnvelope, error) {
		calls++
		revision := selectedRevisionFixture()
		if strings.HasSuffix(env.TaskID, ":replace") {
			revision.CommitSHA = strings.Repeat("b", 40)
		}
		return apiv1.ResultEnvelope{Status: apiv1.ResultSuccess, WorkspaceRevision: revision}, nil
	}}
	var ts testsuite.WorkflowTestSuite
	env := temporaltest.NewWorkflowEnvironment(&ts)
	env.RegisterActivity(&Activities{Det: runner, Workspaces: testWorkspaces(t)})
	env.ExecuteWorkflow(Run, runInput("conflict", spec))
	if err := env.GetWorkflowError(); err == nil || !strings.Contains(err.Error(), workspacerevision.CodeConflict) {
		t.Fatalf("conflict did not fail workflow: %v", err)
	}
	if calls != 2 {
		t.Fatalf("nonretryable conflict retried: %d calls", calls)
	}
	for _, ev := range projectEngineJournal(t, env) {
		if ev.Type == journal.EventStageFinished && ev.Stage == "replace" {
			t.Fatal("conflicting authority was journaled as accepted")
		}
	}
}

func TestWorkspaceRevisionSourcePolicyDoesNotReplaceWritableBase(t *testing.T) {
	in := runInput("policy", linearSpec())
	in.RepoRef.Checkout = &apiv1.CheckoutSpec{Sparse: []string{"base"}}
	source := in.RepoRef
	source.Owner = "fork"
	source.Checkout = &apiv1.CheckoutSpec{Sparse: []string{"source"}}
	in.AdditionalRepos = []apiv1.RepoRef{source}
	in.WorkspaceRevision = selectedRevisionFixture()
	in.WorkspaceRevision.Repository.Owner = source.Owner
	if got := invocationCheckoutCones(in, false)[""]; !reflect.DeepEqual(got, []string{"base"}) {
		t.Fatalf("writable base policy = %v", got)
	}
	if got := invocationCheckoutCones(in, true)[""]; !reflect.DeepEqual(got, []string{"source"}) {
		t.Fatalf("readonly source policy = %v", got)
	}
	task := apiv1.Task{Workspace: apiv1.WorkspaceRepoReadOnly}
	if err := validateRevisionPublication(in, task, stageActivityResult{WorkspaceDelta: "sha256:forged"}); workspaceRevisionErrorCode(err) != workspacerevision.CodeInvalid {
		t.Fatalf("selected readonly publication accepted: %v", err)
	}
}
