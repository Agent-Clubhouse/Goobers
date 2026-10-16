//go:build integration

package runner

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/test/testsupport/testdep"
)

type unacknowledgedChildAgent struct{ calls int }

func (g *unacknowledgedChildAgent) Invoke(context.Context, apiv1.InvocationEnvelope) (apiv1.ResultEnvelope, error) {
	g.calls++
	return apiv1.ResultEnvelope{Status: apiv1.ResultSuccess}, nil
}
func (*unacknowledgedChildAgent) Review(context.Context, apiv1.InvocationEnvelope) (apiv1.Verdict, error) {
	return apiv1.Verdict{}, errors.New("unexpected review")
}

func TestIntegrationChildWorkspaceUnknownRuntimeCannotSealResultOrHumanRerun(t *testing.T) {
	testdep.Require(t, "git")
	f := prepareChildWorkspaceFixture(t, false)
	agent := &unacknowledgedChildAgent{}
	f.config.NewAgentic = func(string, ArtifactRecorder, SecretRegistrar) (invoke.Goober, error) { return agent, nil }
	r, err := New(f.config)
	if err != nil {
		t.Fatal(err)
	}
	result, err := r.Start(t.Context(), f.input)
	if result.Phase == journal.PhaseCompleted || agent.calls == 0 {
		t.Fatalf("unknown writer completed: %+v %v", result, err)
	}
	rd, err := journal.OpenReadOnly(filepath.Join(f.config.RunsDir, f.input.RunID))
	if err != nil {
		t.Fatal(err)
	}
	id, err := rd.Identity()
	if err != nil {
		t.Fatal(err)
	}
	events, err := rd.Events()
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyChildWorkspaceQuiescence(rd, id, events); err == nil {
		t.Fatal("terminal journal substituted for actual writer acknowledgement")
	}
	calls := agent.calls
	_, err = r.RerunStage(t.Context(), RerunStageInput{RunID: f.input.RunID, Machine: f.input.Machine, RepoRef: f.input.RepoRef, GooberDigest: f.input.GooberDigest, Stage: "work", Actor: "operator", InstructionAddendum: "retry", ExpectedTerminalSeq: terminalRunSequence(t, f.config.RunsDir, f.input.RunID)})
	if err == nil || agent.calls != calls {
		t.Fatalf("human rerun bypassed unresolved writer: calls=%d err=%v", agent.calls, err)
	}
	f.assertParentUnchanged(t)
}
