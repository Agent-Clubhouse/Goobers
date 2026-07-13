package executor

import (
	"context"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/providers"
)

func TestTaskExecutor_DefaultsToShell(t *testing.T) {
	shell, _ := newTestExecutor(t, nil)
	te, err := NewTaskExecutor(shell, nil)
	if err != nil {
		t.Fatal(err)
	}

	env := apiv1.InvocationEnvelope{TaskID: "t1", Workspace: t.TempDir()}
	result, err := te.Run(context.Background(), env, apiv1.DeterministicRun{Command: []string{"sh", "-c", "echo hi"}})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.Status != apiv1.ResultSuccess {
		t.Fatalf("status = %v, want success", result.Status)
	}
}

func TestTaskExecutor_RoutesToCIPoll(t *testing.T) {
	shell, _ := newTestExecutor(t, nil)
	poller := &fakePoller{results: []providers.CheckState{providers.CheckStatePassing}}
	ciPoll, err := NewCIPollExecutor(poller)
	if err != nil {
		t.Fatal(err)
	}
	ciPoll.Sleep = noSleep
	te, err := NewTaskExecutor(shell, ciPoll)
	if err != nil {
		t.Fatal(err)
	}

	env := apiv1.InvocationEnvelope{
		TaskID:  "t1",
		RepoRef: apiv1.RepoRef{Owner: "acme", Name: "widgets"},
		Inputs:  map[string]interface{}{InputKind: KindCIPoll, InputPRNumber: "9"},
	}
	result, err := te.Run(context.Background(), env, apiv1.DeterministicRun{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.Outputs[OutputCIStatus] != string(apiv1.ResultSuccess) {
		t.Fatalf("outputs = %+v, want ciStatus=success", result.Outputs)
	}
}

func TestTaskExecutor_CIPollWithoutConfiguredExecutorFailsClosed(t *testing.T) {
	shell, _ := newTestExecutor(t, nil)
	te, err := NewTaskExecutor(shell, nil)
	if err != nil {
		t.Fatal(err)
	}
	env := apiv1.InvocationEnvelope{Inputs: map[string]interface{}{InputKind: KindCIPoll}}
	if _, err := te.Run(context.Background(), env, apiv1.DeterministicRun{}); err == nil {
		t.Fatal("expected an error when kind=ci-poll is declared but no CIPollExecutor is configured")
	}
}

func TestTaskExecutor_UnknownKindIsError(t *testing.T) {
	shell, _ := newTestExecutor(t, nil)
	te, err := NewTaskExecutor(shell, nil)
	if err != nil {
		t.Fatal(err)
	}
	env := apiv1.InvocationEnvelope{Inputs: map[string]interface{}{InputKind: "something-else"}}
	if _, err := te.Run(context.Background(), env, apiv1.DeterministicRun{}); err == nil {
		t.Fatal("expected an error for an unknown kind")
	}
}

func TestNewTaskExecutor_RequiresShell(t *testing.T) {
	if _, err := NewTaskExecutor(nil, nil); err == nil {
		t.Fatal("expected an error for a nil shell executor")
	}
}
