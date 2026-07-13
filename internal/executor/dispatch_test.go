package executor

import (
	"context"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/providers"
)

func TestTaskExecutor_DefaultsToShell(t *testing.T) {
	te := NewTaskExecutor(nil)
	result, produced, err := te.Run(context.Background(), apiv1.InvocationEnvelope{Workspace: t.TempDir()}, apiv1.DeterministicRun{Command: []string{"sh", "-c", "echo hi"}}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.Status != apiv1.ResultSuccess {
		t.Fatalf("status = %v, want success", result.Status)
	}
	if len(produced) == 0 {
		t.Fatal("expected produced artifacts from the shell path")
	}
}

func TestTaskExecutor_RoutesToCIPoll(t *testing.T) {
	poller := &fakePoller{results: []providers.CheckState{providers.CheckStatePassing}}
	ciPoll, err := NewCIPollExecutor(poller)
	if err != nil {
		t.Fatal(err)
	}
	ciPoll.Sleep = noSleep
	te := NewTaskExecutor(ciPoll)

	env := apiv1.InvocationEnvelope{
		Workspace: t.TempDir(),
		RepoRef:   apiv1.RepoRef{Owner: "acme", Name: "widgets"},
		Inputs:    map[string]interface{}{InputKind: KindCIPoll, InputPRNumber: "9"},
	}
	result, produced, err := te.Run(context.Background(), env, apiv1.DeterministicRun{}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.Outputs[OutputCIStatus] != string(apiv1.ResultSuccess) {
		t.Fatalf("outputs = %+v, want ciStatus=success", result.Outputs)
	}
	if len(produced) != 0 {
		t.Fatalf("ci-poll should produce no artifacts, got %d", len(produced))
	}
}

func TestTaskExecutor_CIPollWithoutConfiguredExecutorFailsClosed(t *testing.T) {
	te := NewTaskExecutor(nil)
	env := apiv1.InvocationEnvelope{Inputs: map[string]interface{}{InputKind: KindCIPoll}}
	if _, _, err := te.Run(context.Background(), env, apiv1.DeterministicRun{}, nil); err == nil {
		t.Fatal("expected an error when kind=ci-poll is declared but no CIPollExecutor is configured")
	}
}

func TestTaskExecutor_UnknownKindIsError(t *testing.T) {
	te := NewTaskExecutor(nil)
	env := apiv1.InvocationEnvelope{Inputs: map[string]interface{}{InputKind: "something-else"}}
	if _, _, err := te.Run(context.Background(), env, apiv1.DeterministicRun{}, nil); err == nil {
		t.Fatal("expected an error for an unknown kind")
	}
}
