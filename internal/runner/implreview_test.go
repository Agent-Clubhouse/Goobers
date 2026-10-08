package runner

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/workflow"
	"github.com/goobers/goobers/internal/worktree"
)

func compileImplReviewMachine(t *testing.T, spec apiv1.WorkflowSpec) *workflow.Machine {
	t.Helper()
	spec.Gaggle = "acme-web"
	spec.Triggers = []apiv1.Trigger{{Type: apiv1.TriggerBacklogItem}}
	m, err := workflow.Compile(workflow.Definition{Name: "impl-review-fixture", Version: 1, Spec: spec}, workflow.WithPreviewFeatures(true))
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	return m
}

func reviewGate(name, onPass, onChanges string, ws apiv1.WorkspaceMode) apiv1.Gate {
	return apiv1.Gate{
		Name:      name,
		Evaluator: apiv1.EvaluatorAgentic,
		Agentic:   &apiv1.AgenticGate{Goober: "reviewer", Workspace: ws},
		Branches: map[string]string{
			"pass":          onPass,
			"needs-changes": onChanges,
			"fail":          workflow.TargetAbort,
		},
	}
}

// implementCheckReviewMachine is the #5414 shape: an agentic implementer, a
// deterministic stage between it and the review, and a reviewer that
// declared a workspace other than the writable run branch.
func implementCheckReviewMachine(t *testing.T, reviewerWorkspace apiv1.WorkspaceMode) *workflow.Machine {
	t.Helper()
	return compileImplReviewMachine(t, apiv1.WorkflowSpec{
		Start: "implement",
		Tasks: []apiv1.Task{
			{Name: "implement", Type: apiv1.TaskAgentic, Goober: "coder", Goal: "produce a diff", Next: "check"},
			{Name: "check", Type: apiv1.TaskDeterministic, Run: &apiv1.DeterministicRun{Command: []string{"true"}}, Next: "review"},
		},
		Gates: []apiv1.Gate{reviewGate("review", workflow.TerminalComplete, "implement", reviewerWorkspace)},
	})
}

func TestReviewsImplementationClassifiesByDominance(t *testing.T) {
	t.Run("review behind an agentic implementer and a deterministic stage", func(t *testing.T) {
		m := implementCheckReviewMachine(t, apiv1.WorkspaceRepoReadOnly)
		if !ReviewsImplementation(m, "review") {
			t.Fatal("review gate after implement->check must classify as implementation review")
		}
		if !RequiresReviewerDiff(m, "review", "check") {
			t.Fatal("a deterministic subject must not disable the diff requirement of an implementation review")
		}
	})

	t.Run("spec check reachable before the implementer", func(t *testing.T) {
		m := compileImplReviewMachine(t, apiv1.WorkflowSpec{
			Start: "plan",
			Tasks: []apiv1.Task{
				{Name: "plan", Type: apiv1.TaskDeterministic, Run: &apiv1.DeterministicRun{Command: []string{"true"}}, Next: "spec-review"},
				{Name: "implement", Type: apiv1.TaskAgentic, Goober: "coder", Goal: "produce a diff", Next: "review"},
			},
			Gates: []apiv1.Gate{
				reviewGate("spec-review", "implement", "plan", apiv1.WorkspaceRepoReadOnly),
				reviewGate("review", workflow.TerminalComplete, "implement", ""),
			},
		})
		if ReviewsImplementation(m, "spec-review") {
			t.Fatal("a gate reachable before any implementer must not classify as implementation review")
		}
		if !ReviewsImplementation(m, "review") {
			t.Fatal("the post-implement review must classify as implementation review")
		}
	})

	t.Run("deterministic-only subject (merge-review shape)", func(t *testing.T) {
		m := agenticGateMachine(t)
		if ReviewsImplementation(m, "review") || RequiresReviewerDiff(m, "review", "implement") {
			t.Fatal("a review fed only by deterministic stages must keep its historical behaviour")
		}
	})

	t.Run("read-only agentic task does not implement", func(t *testing.T) {
		m := compileImplReviewMachine(t, apiv1.WorkflowSpec{
			Start: "research",
			Tasks: []apiv1.Task{
				{Name: "research", Type: apiv1.TaskAgentic, Goober: "coder", Goal: "read", Workspace: apiv1.WorkspaceRepoReadOnly, Next: "review"},
			},
			Gates: []apiv1.Gate{reviewGate("review", workflow.TerminalComplete, "research", apiv1.WorkspaceRepoReadOnly)},
		})
		if ReviewsImplementation(m, "review") {
			t.Fatal("a repo-readonly agentic task commits nothing and must not make its review an implementation review")
		}
	})
}

// committingCoder is an agentic goober fake that commits a change in its
// workspace when commit is set, and nothing otherwise.
type committingCoder struct {
	t      *testing.T
	commit bool
}

func (c *committingCoder) Invoke(_ context.Context, env apiv1.InvocationEnvelope) (apiv1.ResultEnvelope, error) {
	c.t.Helper()
	if c.commit {
		if err := os.WriteFile(filepath.Join(env.Workspace, "impl.txt"), []byte("implementation-5414\n"), 0o644); err != nil {
			return apiv1.ResultEnvelope{}, err
		}
		runGit(c.t, env.Workspace, "add", "-A")
		runGit(c.t, env.Workspace, "commit", "-m", "implement")
	}
	return apiv1.ResultEnvelope{Status: apiv1.ResultSuccess}, nil
}

func (c *committingCoder) Review(context.Context, apiv1.InvocationEnvelope) (apiv1.Verdict, error) {
	return apiv1.Verdict{Decision: apiv1.VerdictPass}, nil
}

func newImplReviewRunner(t *testing.T, runID string, coder, reviewer invoke.Goober) (*Runner, string) {
	t.Helper()
	instanceRoot := t.TempDir()
	wtMgr, err := worktree.NewManager(filepath.Join(instanceRoot, "workcopies"))
	if err != nil {
		t.Fatalf("new worktree manager: %v", err)
	}
	fixtureRepo := newFixtureRepo(t)
	runsDir := filepath.Join(instanceRoot, "runs")
	r, err := New(Config{
		NewDeterministic: func(rec ArtifactRecorder, _ SecretRegistrar) (invoke.Deterministic, error) {
			return &stubDeterministic{rec: rec, byTask: map[string]stubTaskResult{runID + ":check": {status: apiv1.ResultSuccess}}}, nil
		},
		NewAgentic: func(gooberName string, _ ArtifactRecorder, _ SecretRegistrar) (invoke.Goober, error) {
			if gooberName == "reviewer" {
				return reviewer, nil
			}
			return coder, nil
		},
		Worktrees:    wtMgr,
		RunsDir:      runsDir,
		ScratchDir:   filepath.Join(instanceRoot, "scratch"),
		RepoCloneURL: func(apiv1.RepoRef) (string, error) { return fixtureRepo, nil },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return r, runsDir
}

// TestRunnerImplementationReviewGetsRunDiffOffBranch is #5414: a reviewer
// that declared repo-readonly (detached at base) or scratch (no checkout),
// reached through a deterministic stage after the implementer, is still
// handed the run's committed diff rather than silently judging base.
func TestRunnerImplementationReviewGetsRunDiffOffBranch(t *testing.T) {
	for _, ws := range []apiv1.WorkspaceMode{apiv1.WorkspaceRepoReadOnly, apiv1.WorkspaceScratch} {
		t.Run(string(ws), func(t *testing.T) {
			runID := "run-impl-review-" + string(ws)
			reviewer := &capturingReviewer{}
			r, runsDir := newImplReviewRunner(t, runID, &committingCoder{t: t, commit: true}, reviewer)
			res, err := r.Start(context.Background(), StartInput{
				RunID:   runID,
				Machine: implementCheckReviewMachine(t, ws),
				Gaggle:  "acme-web",
				RepoRef: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web", Branch: "main"},
			})
			if err != nil {
				t.Fatalf("Start: %v", err)
			}
			if res.Phase != journal.PhaseCompleted {
				t.Fatalf("phase = %q, want completed", res.Phase)
			}
			if !reviewer.called {
				t.Fatal("reviewer was never invoked")
			}
			var diffPtr *apiv1.ContextPointer
			for i := range reviewer.gotPointers {
				if reviewer.gotPointers[i].Name == "review.diff" {
					diffPtr = &reviewer.gotPointers[i]
				}
			}
			if diffPtr == nil || diffPtr.Artifact == nil {
				t.Fatalf("reviewer got no run diff evidence; pointers = %+v", reviewer.gotPointers)
			}
			rd, err := journal.OpenRead(filepath.Join(runsDir, runID))
			if err != nil {
				t.Fatalf("OpenRead: %v", err)
			}
			diff, err := rd.ArtifactBytes(journal.Ref{Path: diffPtr.Artifact.Path, Digest: diffPtr.Artifact.Digest})
			if err != nil {
				t.Fatalf("read diff evidence: %v", err)
			}
			if !strings.Contains(string(diff), "implementation-5414") {
				t.Fatalf("diff evidence does not carry the run's commit:\n%s", diff)
			}
		})
	}
}

// TestRunnerImplementationReviewEmptyDiffFailsClosed is #5414's (c): an
// implementation review whose run diff is empty fast-fails without asking
// the reviewer, even with a deterministic stage as its direct subject.
func TestRunnerImplementationReviewEmptyDiffFailsClosed(t *testing.T) {
	runID := "run-impl-review-empty"
	reviewer := &capturingReviewer{}
	r, _ := newImplReviewRunner(t, runID, &committingCoder{t: t}, reviewer)
	res, err := r.Start(context.Background(), StartInput{
		RunID:   runID,
		Machine: implementCheckReviewMachine(t, apiv1.WorkspaceRepoReadOnly),
		Gaggle:  "acme-web",
		RepoRef: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web", Branch: "main"},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if res.Phase == journal.PhaseCompleted {
		t.Fatal("an empty implementation diff must not complete the run")
	}
	if reviewer.called {
		t.Fatal("reviewer was invoked on an empty implementation diff")
	}
}

// TestReviewerDiffReadsPinnedWorkspaceForScratchReviewer guards the pinned
// arm of #5414: production always sets ScratchDir, so a scratch reviewer in a
// pinned-workspace run has no worktree of its own, and the run branch lives
// in the pinned checkout rather than the shared mirror. Its diff must come
// from there, never read as "empty" and fail the review closed.
func TestReviewerDiffReadsPinnedWorkspaceForScratchReviewer(t *testing.T) {
	r, in := readOnlyWorkspaceRunner(t)
	in.Machine = implementCheckReviewMachine(t, apiv1.WorkspaceScratch)
	repoURL, err := r.cfg.RepoCloneURL(in.RepoRef)
	if err != nil {
		t.Fatal(err)
	}
	lease, err := r.cfg.Worktrees.AcquirePinned(context.Background(), worktree.PinnedOptions{
		RepoURL: repoURL, RunID: in.RunID, BaseRef: "main", Branch: "goobers/test/" + in.RunID,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lease.Release() }()
	in.pinnedWorkspace = lease.Worktree
	in.pinnedStage = &sync.Mutex{}
	if err := os.WriteFile(filepath.Join(lease.Worktree.Path, "impl.txt"), []byte("pinned-5414\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, lease.Worktree.Path, "add", "-A")
	runGit(t, lease.Worktree.Path, "commit", "-m", "implement")

	diff, err := r.reviewerDiff(context.Background(), in, "review", "", nil)
	if err != nil {
		t.Fatalf("reviewerDiff: %v", err)
	}
	if !strings.Contains(string(diff), "pinned-5414") {
		t.Fatalf("scratch reviewer in a pinned run did not get the pinned branch's diff:\n%s", diff)
	}
}

// TestReviewerDiffObservesRunBranchOnlyWhenItReadsIt keeps #5334 alongside
// #5414: an empty diff fails closed only when it was read from the run branch,
// never from a detached reviewer checkout nothing else stood in for.
func TestReviewerDiffObservesRunBranchOnlyWhenItReadsIt(t *testing.T) {
	m := implementCheckReviewMachine(t, apiv1.WorkspaceRepoReadOnly)
	r := &Runner{}
	detached := &worktree.Worktree{}
	for _, tc := range []struct {
		name string
		in   StartInput
		wt   *worktree.Worktree
		want bool
	}{
		{name: "on-branch worktree", in: StartInput{Machine: m}, wt: &worktree.Worktree{Branch: "goobers/run"}, want: true},
		{name: "detached checkout with no mirror", in: StartInput{Machine: m}, wt: detached, want: false},
		{name: "detached checkout in a pinned run", in: StartInput{Machine: m, pinnedWorkspace: &worktree.Worktree{Branch: "goobers/run"}}, wt: detached, want: false},
		{name: "scratch reviewer in a pinned run", in: StartInput{Machine: m, pinnedWorkspace: &worktree.Worktree{Branch: "goobers/run"}, pinnedStage: &sync.Mutex{}}, wt: nil, want: true},
	} {
		if got := r.reviewerDiffObservesRunBranch(tc.in, "review", tc.wt); got != tc.want {
			t.Errorf("%s: reviewerDiffObservesRunBranch = %t, want %t", tc.name, got, tc.want)
		}
	}
}
