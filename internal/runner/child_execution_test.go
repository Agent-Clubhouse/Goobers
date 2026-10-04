package runner

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/credentials"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/worktree"
)

type isolatedChildGoober struct{ calls int }

func (g *isolatedChildGoober) Invoke(ctx context.Context, _ apiv1.InvocationEnvelope) (apiv1.ResultEnvelope, error) {
	if _, ok := credentials.ChildCeilingFromContext(ctx); !ok {
		return apiv1.ResultEnvelope{}, errors.New("child credential custody absent")
	}
	g.calls++
	return apiv1.ResultEnvelope{Status: apiv1.ResultSuccess}, nil
}
func (*isolatedChildGoober) Review(context.Context, apiv1.InvocationEnvelope) (apiv1.Verdict, error) {
	return apiv1.Verdict{}, errors.New("unexpected reviewer")
}

func TestChildExecutionDriverUsesOnlyIsolatedFactoriesAndNoProviderHooks(t *testing.T) {
	root := t.TempDir()
	manager, err := worktree.NewManager(filepath.Join(root, "workcopies"))
	if err != nil {
		t.Fatal(err)
	}
	base, err := New(Config{Worktrees: manager, RunsDir: filepath.Join(root, "runs"), ScratchDir: filepath.Join(root, "scratch"), ConfigGeneration: journal.Digest([]byte("config")),
		SelfExecutionDenied: true, NewAgentic: func(string, ArtifactRecorder, SecretRegistrar) (invoke.Goober, error) {
			t.Fatal("selected ordinary automation executor")
			return nil, nil
		},
		PrepareTerminal: func(string, journal.RunPhase, *journal.Run) error {
			t.Fatal("child called provider preparation")
			return nil
		},
		FinalizeTerminal: func(string, journal.RunPhase) error { t.Fatal("child called automation cleanup"); return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	in := childWorkspaceStart(childWorkspaceMachine(t, apiv1.WorkspaceScratch, false))
	id := journal.RunIdentity{RunID: in.RunID, Gaggle: in.Gaggle, Workflow: in.Machine.Def.Name, WorkflowDigest: in.Machine.Digest(), GooberDigest: in.GooberDigest, ConfigGeneration: base.cfg.ConfigGeneration, Child: in.Child}
	goober := &isolatedChildGoober{}
	isolation, err := base.ForChildExecution(id, ChildExecutionFactories{
		NewAgentic: func(string, ArtifactRecorder, SecretRegistrar) (invoke.Goober, error) { return goober, nil },
		NewDeterministic: func(ArtifactRecorder, SecretRegistrar) (invoke.Deterministic, error) {
			return nil, errors.New("unexpected deterministic stage")
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	wrong := in
	wrong.Child = nil
	if _, err := isolation.Start(t.Context(), wrong); err == nil {
		t.Fatal("bound driver admitted ordinary run")
	}
	wrong = in
	wrong.RunID = "foreign-child"
	if _, err := isolation.Start(t.Context(), wrong); err == nil {
		t.Fatal("bound driver admitted foreign child")
	}
	result, err := isolation.Start(t.Context(), in)
	if err != nil || result.Phase != journal.PhaseCompleted || goober.calls != 1 {
		t.Fatalf("result=%+v calls=%d err=%v", result, goober.calls, err)
	}
	if base.cfg.PrepareTerminal == nil || base.cfg.FinalizeTerminal == nil || !base.cfg.SelfExecutionDenied {
		t.Fatal("child construction mutated ordinary runner")
	}
	if isolation.recordsPlacement() {
		t.Fatal("remote child advertised host placement")
	}
}
