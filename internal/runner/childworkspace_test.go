package runner

import (
	"path/filepath"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/workflow"
	"github.com/goobers/goobers/internal/worktree"
)

func childWorkspaceMachine(t *testing.T, mode apiv1.WorkspaceMode, pause bool) *workflow.Machine {
	t.Helper()
	spec := apiv1.WorkflowSpec{Gaggle: "web", Start: "work", Triggers: []apiv1.Trigger{{Type: apiv1.TriggerManual}}, Tasks: []apiv1.Task{{Name: "work", Type: apiv1.TaskAgentic, Goober: "coder", Goal: "work in inherited state", Workspace: mode, Retry: &apiv1.RetryPolicy{MaxAttempts: 2}}}}
	if pause {
		spec.Start = "approval"
		spec.Gates = []apiv1.Gate{{Name: "approval", Evaluator: apiv1.EvaluatorHuman, Human: &apiv1.HumanGate{}, Branches: map[string]string{"pass": "work", "reject": workflow.TargetAbort}}}
	}
	machine, err := workflow.Compile(workflow.Definition{Name: "generated-child", Version: 1, DSLVersion: "3.1", Spec: spec}, workflow.WithPreviewFeatures(true))
	if err != nil {
		t.Fatal(err)
	}
	return machine
}

func childWorkspaceStart(machine *workflow.Machine) StartInput {
	return StartInput{RunID: "child-run", Gaggle: "web", Machine: machine, GooberDigest: journal.Digest([]byte("goober")), Trigger: journal.Trigger{Kind: journal.TriggerManual},
		RepoRef: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web", Branch: "main"},
		Child:   &journal.ChildLineage{Gaggle: "web", ParentRunID: "parent-run", StageOccurrence: "parent-stage", InvocationKey: "check", AcceptanceID: "trigger-child-run", SourceDigest: journal.Digest([]byte("source")), EnvelopeDigest: journal.Digest([]byte("admitted-envelope"))}}
}

func TestChildWorkspaceScratchRunNeedsNoRepositoryCustody(t *testing.T) {
	root := t.TempDir()
	manager, err := worktree.NewManager(filepath.Join(root, "workcopies"))
	if err != nil {
		t.Fatal(err)
	}
	goober := &childOriginGoober{}
	runner, err := New(Config{Worktrees: manager, RunsDir: filepath.Join(root, "runs"), ScratchDir: filepath.Join(root, "scratch"), ConfigGeneration: journal.Digest([]byte("config")),
		RepoCloneURL: func(apiv1.RepoRef) (string, error) { t.Fatal("scratch child accessed repository"); return "", nil },
		NewAgentic:   func(string, ArtifactRecorder, SecretRegistrar) (invoke.Goober, error) { return goober, nil }})
	if err != nil {
		t.Fatal(err)
	}
	result, err := runner.Start(t.Context(), childWorkspaceStart(childWorkspaceMachine(t, apiv1.WorkspaceScratch, false)))
	if err != nil || result.Phase != journal.PhaseCompleted || len(goober.envelopes) != 1 {
		t.Fatalf("scratch child: %+v %v", result, err)
	}
}

func TestChildWorkspaceRequiresExplicitManagedAdmission(t *testing.T) {
	in := childWorkspaceStart(childWorkspaceMachine(t, apiv1.WorkspaceRepo, false))
	runner := &Runner{}
	if err := runner.validateChildWorkspacePlan(in); err == nil {
		t.Fatal("repository child accepted without custody")
	}
	in.ChildWorkspace = &ChildWorkspaceAdmission{WorkspaceID: "child-workspace", ForkSHA: strings.Repeat("a", 40), RepositoryDigest: strings.Repeat("b", 64)}
	in.Child = nil
	if err := runner.validateChildWorkspacePlan(in); err == nil {
		t.Fatal("ordinary run accepted child workspace")
	}
	in.Child = childWorkspaceStart(in.Machine).Child
	in.Machine.Def.Spec.Parallels = []apiv1.Parallel{{Name: "fan"}}
	if err := runner.validateChildWorkspacePlan(in); err == nil || !strings.Contains(err.Error(), "fan-in") {
		t.Fatalf("parallel isolation gap was not refused: %v", err)
	}
}

func TestChildWorkspaceResultCannotChangeCustody(t *testing.T) {
	in := StartInput{ChildWorkspace: &ChildWorkspaceAdmission{}, WorkspaceBranch: "goobers/children/child-run"}
	for _, result := range []apiv1.ResultEnvelope{{WorkspaceRevision: &apiv1.WorkspaceRevision{}}, {Outputs: map[string]any{WorkspaceBranchOutput: "foreign-branch"}}} {
		if err := validateChildWorkspaceResult(in, result); err == nil {
			t.Fatal("child result changed custody")
		}
	}
}
