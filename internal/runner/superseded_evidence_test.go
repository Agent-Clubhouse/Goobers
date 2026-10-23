package runner

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/gate"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/workflow"
	"github.com/goobers/goobers/internal/worktree"
)

func contextNames(pointers []apiv1.ContextPointer) []string {
	names := make([]string, 0, len(pointers))
	for _, p := range pointers {
		names = append(names, p.Name)
	}
	return names
}

// staleCIGoober plays both the implementing coder and the reviewer for the
// #5901 end-to-end walk: every implement pass commits a distinct change, and
// every review records the context it was handed and passes.
type staleCIGoober struct {
	t           *testing.T
	implemented int
	implementCx [][]apiv1.ContextPointer
	reviewCx    [][]apiv1.ContextPointer
}

func (g *staleCIGoober) Invoke(_ context.Context, env apiv1.InvocationEnvelope) (apiv1.ResultEnvelope, error) {
	g.t.Helper()
	g.implemented++
	g.implementCx = append(g.implementCx, env.ContextPointers)
	body := strings.Repeat("line\r\n", g.implemented)
	if err := os.WriteFile(filepath.Join(env.Workspace, "change.txt"), []byte(body), 0o644); err != nil {
		return apiv1.ResultEnvelope{}, err
	}
	runGit(g.t, env.Workspace, "add", "-A")
	runGit(g.t, env.Workspace, "commit", "-m", "implement pass")
	return apiv1.ResultEnvelope{Status: apiv1.ResultSuccess}, nil
}

func (g *staleCIGoober) Review(_ context.Context, env apiv1.InvocationEnvelope) (apiv1.Verdict, error) {
	g.reviewCx = append(g.reviewCx, env.ContextPointers)
	return apiv1.Verdict{Decision: apiv1.VerdictPass}, nil
}

// failOnceCI is local-ci: the first visit fails and records a failure log,
// every later visit passes and records a clean log.
type failOnceCI struct {
	rec    ArtifactRecorder
	visits *int
}

func (c *failOnceCI) Run(_ context.Context, _ apiv1.InvocationEnvelope, _ apiv1.DeterministicRun) (apiv1.ResultEnvelope, error) {
	*c.visits++
	status, log := apiv1.ResultSuccess, "ci passed\n"
	if *c.visits == 1 {
		status, log = apiv1.ResultFailure, "change.txt: CRLF line endings\n"
	}
	ref, err := c.rec.RecordArtifact("ci.log", []byte(log))
	if err != nil {
		return apiv1.ResultEnvelope{}, err
	}
	return apiv1.ResultEnvelope{
		Status:    status,
		Artifacts: []apiv1.ArtifactPointer{{Path: ref.Path, Digest: ref.Digest, Size: ref.Size}},
	}, nil
}

func hasContextNamed(pointers []apiv1.ContextPointer, name string) bool {
	for _, p := range pointers {
		if p.Name == name {
			return true
		}
	}
	return false
}

// TestRunnerReviewerDoesNotSeeSupersededLocalCI walks the #5901 loop through
// the real runner: local-ci fails, local-gate sends the run back to
// implement, implement fixes it, and the next review must judge the new diff
// without the pre-fix failure log — while implement itself still saw it.
func TestRunnerReviewerDoesNotSeeSupersededLocalCI(t *testing.T) {
	spec := apiv1.WorkflowSpec{
		Gaggle:   "acme-web",
		Triggers: []apiv1.Trigger{{Type: apiv1.TriggerBacklogItem}},
		Start:    "implement",
		Tasks: []apiv1.Task{
			{Name: "implement", Type: apiv1.TaskAgentic, Goober: "coder", Goal: "produce a diff", Next: "review"},
			{Name: "local-ci", Type: apiv1.TaskDeterministic, Run: &apiv1.DeterministicRun{Command: []string{"make", "ci"}}, Next: "local-gate"},
		},
		Gates: []apiv1.Gate{
			{
				Name:      "review",
				Evaluator: apiv1.EvaluatorAgentic,
				Agentic:   &apiv1.AgenticGate{Goober: "reviewer"},
				Branches:  map[string]string{"pass": "local-ci", "needs-changes": "implement", "fail": workflow.TargetAbort},
			},
			{
				Name:      "local-gate",
				Evaluator: apiv1.EvaluatorAutomated,
				Automated: &apiv1.AutomatedGate{Check: "failure-class"},
				Branches:  map[string]string{"pass": workflow.TerminalComplete, "fail": "implement", "infra": "local-ci"},
			},
		},
	}
	m, err := workflow.Compile(workflow.Definition{Name: "stale-ci-fixture", Version: 1, Spec: spec},
		workflow.WithKnownChecks([]string{"failure-class"}), workflow.WithPreviewFeatures(true))
	if err != nil {
		t.Fatalf("compile: %v", err)
	}

	instanceRoot := t.TempDir()
	wtMgr, err := worktree.NewManager(filepath.Join(instanceRoot, "workcopies"))
	if err != nil {
		t.Fatalf("new worktree manager: %v", err)
	}
	fixtureRepo := newFixtureRepo(t)
	goober := &staleCIGoober{t: t}
	ciVisits := 0
	r, err := New(Config{
		NewDeterministic: func(rec ArtifactRecorder, _ SecretRegistrar) (invoke.Deterministic, error) {
			return &failOnceCI{rec: rec, visits: &ciVisits}, nil
		},
		NewAgentic: func(string, ArtifactRecorder, SecretRegistrar) (invoke.Goober, error) {
			return goober, nil
		},
		Automated:    gate.NewAutomatedEvaluator(),
		Worktrees:    wtMgr,
		RunsDir:      filepath.Join(instanceRoot, "runs"),
		RepoCloneURL: func(apiv1.RepoRef) (string, error) { return fixtureRepo, nil },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	res, err := r.Start(context.Background(), StartInput{
		RunID: "run-stale-ci", Machine: m, Gaggle: "acme-web",
		RepoRef: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web", Branch: "main"},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if res.Phase != journal.PhaseCompleted {
		t.Fatalf("phase = %q, want completed", res.Phase)
	}
	if len(goober.implementCx) != 2 || len(goober.reviewCx) != 2 || ciVisits != 2 {
		t.Fatalf("implement=%d review=%d local-ci=%d, want 2/2/2", len(goober.implementCx), len(goober.reviewCx), ciVisits)
	}
	if !hasContextNamed(goober.implementCx[1], "local-ci.artifact[0]") {
		t.Fatalf("remediating implement context = %v, want the failing local-ci log", contextNames(goober.implementCx[1]))
	}
	if hasContextNamed(goober.reviewCx[1], "local-ci.artifact[0]") {
		t.Fatalf("second review context = %v, must not carry the superseded local-ci failure", contextNames(goober.reviewCx[1]))
	}
}
