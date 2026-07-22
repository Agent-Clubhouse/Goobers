package runner

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/workflow"
	"github.com/goobers/goobers/internal/worktree"
	"github.com/goobers/goobers/providers"
)

// The branch a rebinding stage points the rest of the run at, and the file
// that exists ONLY on it — the observable proof a later stage's worktree is
// really on the PR's branch rather than a fresh one cut from main.
const (
	rebindBranch     = "goobers/implementation/pr-branch-392"
	rebindMarkerFile = "only-on-pr-branch.txt"
)

// newRebindFixtureRepo is newFixtureRepo plus a second branch carrying a
// marker file, standing in for the existing PR branch pr-remediation
// re-enters on.
func newRebindFixtureRepo(t *testing.T) string {
	t.Helper()
	work := t.TempDir()
	bare := filepath.Join(t.TempDir(), "fixture.git")
	runGit(t, work, "init", "--initial-branch=main")
	runGit(t, work, "config", "user.email", "test@example.com")
	runGit(t, work, "config", "user.name", "test")
	if err := os.WriteFile(filepath.Join(work, "README.md"), []byte("fixture\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, work, "add", "README.md")
	runGit(t, work, "commit", "-m", "initial")

	runGit(t, work, "checkout", "-b", rebindBranch)
	if err := os.WriteFile(filepath.Join(work, rebindMarkerFile), []byte("pr work\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, work, "add", "-A")
	runGit(t, work, "commit", "-m", "work on the PR branch")
	runGit(t, work, "checkout", "main")

	runGit(t, "", "clone", "--bare", work, bare)
	return bare
}

// observedWorkspace is what each stage saw: the branch its worktree was
// actually checked out on, and whether the PR branch's marker file was
// present in it.
type observedWorkspace struct {
	branch    string
	hasMarker bool
}

// branchObservingDeterministic records the real git branch of every stage's
// workspace, then defers to the canned per-task outputs. Observing the
// worktree itself — not the CreateOptions the runner passed — is the point:
// it proves the rebinding survives all the way through worktree provisioning
// into the directory the stage's command actually runs in.
type branchObservingDeterministic struct {
	t        *testing.T
	rec      ArtifactRecorder
	byTask   map[string]stubTaskResult
	observed map[string]observedWorkspace
}

func (b *branchObservingDeterministic) Run(ctx context.Context, env apiv1.InvocationEnvelope, dr apiv1.DeterministicRun) (apiv1.ResultEnvelope, error) {
	b.t.Helper()
	cmd := exec.Command("git", "rev-parse", "--abbrev-ref", "HEAD")
	cmd.Dir = env.Workspace
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Run(); err != nil {
		b.t.Fatalf("resolve branch in %s: %v\n%s", env.Workspace, err, out.String())
	}
	_, statErr := os.Stat(filepath.Join(env.Workspace, rebindMarkerFile))
	// TaskID is "<runID>:<stageName>"; key on the stage name alone.
	_, stage, _ := strings.Cut(env.TaskID, ":")
	b.observed[stage] = observedWorkspace{
		branch:    strings.TrimSpace(out.String()),
		hasMarker: statErr == nil,
	}
	return (&stubDeterministic{rec: b.rec, byTask: b.byTask}).Run(ctx, env, dr)
}

// rebindFixtureMachine is a three-stage chain shaped like pr-remediation's:
// "select" is the entrypoint that rebinds the workspace branch (gather-pr-
// context), and "rework"/"verify" stand in for the shared implement/local-ci
// stages that must land on the PR's branch without doing anything themselves.
func rebindFixtureMachine(t *testing.T) *workflow.Machine {
	t.Helper()
	spec := apiv1.WorkflowSpec{
		Gaggle:   "acme-web",
		Triggers: []apiv1.Trigger{{Type: apiv1.TriggerSchedule, Schedule: "0 * * * *"}},
		Start:    "select",
		Tasks: []apiv1.Task{
			{
				Name: "select", Type: apiv1.TaskDeterministic, Goal: "select a PR and rebind",
				Run: &apiv1.DeterministicRun{Command: []string{"true"}}, Next: "rework",
			},
			{
				Name: "rework", Type: apiv1.TaskDeterministic, Goal: "rework the PR",
				Run: &apiv1.DeterministicRun{Command: []string{"true"}}, Next: "verify",
			},
			{
				Name: "verify", Type: apiv1.TaskDeterministic, Goal: "verify the rework",
				Run: &apiv1.DeterministicRun{Command: []string{"true"}}, Next: workflow.TerminalComplete,
			},
		},
	}
	m, err := workflow.Compile(workflow.Definition{Name: "rebind-fixture", Version: 1, Spec: spec})
	if err != nil {
		t.Fatalf("compile rebind fixture machine: %v", err)
	}
	return m
}

func newRebindRunner(t *testing.T, byTask map[string]stubTaskResult) (*Runner, string, map[string]observedWorkspace) {
	t.Helper()
	instanceRoot := t.TempDir()
	wtMgr, err := worktree.NewManager(filepath.Join(instanceRoot, "workcopies"))
	if err != nil {
		t.Fatalf("new worktree manager: %v", err)
	}
	runsDir := filepath.Join(instanceRoot, "runs")
	fixtureRepo := newRebindFixtureRepo(t)
	observed := map[string]observedWorkspace{}

	r, err := New(Config{
		NewDeterministic: func(rec ArtifactRecorder, _ SecretRegistrar) (invoke.Deterministic, error) {
			return &branchObservingDeterministic{t: t, rec: rec, byTask: byTask, observed: observed}, nil
		},
		Worktrees: wtMgr,
		RunsDir:   runsDir,
		RepoCloneURL: func(apiv1.RepoRef) (string, error) {
			return fixtureRepo, nil
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return r, runsDir, observed
}

// TestWorkspaceBranchOutputRebindsEveryLaterStage is #392's core guarantee:
// a stage emitting WorkspaceBranchOutput moves every LATER stage's worktree
// onto that branch, while the emitting stage itself still ran on the run's
// own branch (gather-pr-context checks the PR's branch out for itself; the
// rebinding is for the stages that cannot).
//
// This is what lets pr-remediation reuse implementation's implement/review/
// local-ci chain verbatim, so the assertion is the marker file: a stage that
// can see it is genuinely on the PR's branch, not on a pristine branch cut
// from main that merely happens to be named something.
func TestWorkspaceBranchOutputRebindsEveryLaterStage(t *testing.T) {
	runID := "rebind-run-1"
	r, _, observed := newRebindRunner(t, map[string]stubTaskResult{
		runID + ":select": {status: apiv1.ResultSuccess, outputs: map[string]interface{}{
			WorkspaceBranchOutput: rebindBranch,
		}},
		runID + ":rework": {status: apiv1.ResultSuccess},
		runID + ":verify": {status: apiv1.ResultSuccess},
	})

	machine := rebindFixtureMachine(t)
	res, err := r.Start(context.Background(), StartInput{
		RunID: runID, Machine: machine, Gaggle: "acme-web",
		Trigger: journal.Trigger{Kind: journal.TriggerSchedule},
		RepoRef: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web", Branch: "main"},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if res.Phase != journal.PhaseCompleted {
		t.Fatalf("phase = %q, want %q", res.Phase, journal.PhaseCompleted)
	}

	runBranch := providers.BranchName("rebind-fixture", runID)
	if got := observed["select"]; got.branch != runBranch {
		t.Errorf("select ran on branch %q, want the run's own branch %q — the rebinding must apply to LATER stages only", got.branch, runBranch)
	}
	if observed["select"].hasMarker {
		t.Error("select saw the PR branch's marker file; it should have run on a fresh branch off main")
	}

	for _, stage := range []string{"rework", "verify"} {
		got := observed[stage]
		if got.branch != rebindBranch {
			t.Errorf("%s ran on branch %q, want the rebound branch %q", stage, got.branch, rebindBranch)
		}
		if !got.hasMarker {
			t.Errorf("%s could not see %s — its worktree is not really on the PR's branch, so it would remediate the wrong tree", stage, rebindMarkerFile)
		}
	}
}

// TestWorkspaceBranchAfterEmittingStageCheckedItOutItself covers the exact
// live sequence, which the simpler test above does not: gather-pr-context
// checks the PR's branch out INSIDE ITS OWN worktree (checkoutExistingBranch's
// `git checkout -B`) and only then emits the rebinding. So when the next
// stage's `git worktree add <path> <branch>` runs, that branch was until
// moments ago checked out in another worktree — and git refuses to check out
// one branch in two live worktrees.
//
// This passes only because the runner tears each stage's worktree down before
// provisioning the next. It is the interaction most likely to break if that
// teardown ordering ever changes, and it would break as a hard
// worktree-provisioning failure on every remediation run.
func TestWorkspaceBranchAfterEmittingStageCheckedItOutItself(t *testing.T) {
	runID := "rebind-run-4"
	instanceRoot := t.TempDir()
	wtMgr, err := worktree.NewManager(filepath.Join(instanceRoot, "workcopies"))
	if err != nil {
		t.Fatalf("new worktree manager: %v", err)
	}
	fixtureRepo := newRebindFixtureRepo(t)
	observed := map[string]observedWorkspace{}

	byTask := map[string]stubTaskResult{
		runID + ":select": {status: apiv1.ResultSuccess, outputs: map[string]interface{}{
			WorkspaceBranchOutput: rebindBranch,
		}},
		runID + ":rework": {status: apiv1.ResultSuccess},
		runID + ":verify": {status: apiv1.ResultSuccess},
	}

	r, err := New(Config{
		NewDeterministic: func(rec ArtifactRecorder, _ SecretRegistrar) (invoke.Deterministic, error) {
			return &checkoutThenObserveDeterministic{
				inner: &branchObservingDeterministic{t: t, rec: rec, byTask: byTask, observed: observed},
				t:     t,
			}, nil
		},
		Worktrees: wtMgr,
		RunsDir:   filepath.Join(instanceRoot, "runs"),
		RepoCloneURL: func(apiv1.RepoRef) (string, error) {
			return fixtureRepo, nil
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	res, err := r.Start(context.Background(), StartInput{
		RunID: runID, Machine: rebindFixtureMachine(t), Gaggle: "acme-web",
		Trigger: journal.Trigger{Kind: journal.TriggerSchedule},
		RepoRef: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web", Branch: "main"},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if res.Phase != journal.PhaseCompleted {
		t.Fatalf("phase = %q, want %q — provisioning a stage on a branch the previous stage had checked out must succeed", res.Phase, journal.PhaseCompleted)
	}
	for _, stage := range []string{"rework", "verify"} {
		if got := observed[stage]; got.branch != rebindBranch || !got.hasMarker {
			t.Errorf("%s = %+v, want branch %q with the PR marker present", stage, got, rebindBranch)
		}
	}
}

// checkoutThenObserveDeterministic makes the "select" stage behave like the
// real gather-pr-context: fetch and check the PR's branch out into its own
// worktree before reporting the rebinding.
type checkoutThenObserveDeterministic struct {
	inner *branchObservingDeterministic
	t     *testing.T
}

func (c *checkoutThenObserveDeterministic) Run(ctx context.Context, env apiv1.InvocationEnvelope, dr apiv1.DeterministicRun) (apiv1.ResultEnvelope, error) {
	c.t.Helper()
	if strings.HasSuffix(env.TaskID, ":select") {
		runGit(c.t, env.Workspace, "fetch", "origin", "refs/heads/"+rebindBranch)
		runGit(c.t, env.Workspace, "checkout", "-B", rebindBranch, "FETCH_HEAD")
	}
	return c.inner.Run(ctx, env, dr)
}

// TestWorkspaceBranchDefaultsToRunBranch pins the unchanged behavior every
// other workflow depends on: with no stage emitting the key, all stages share
// the run's own branch exactly as they did before #392.
func TestWorkspaceBranchDefaultsToRunBranch(t *testing.T) {
	runID := "rebind-run-2"
	r, _, observed := newRebindRunner(t, map[string]stubTaskResult{
		runID + ":select": {status: apiv1.ResultSuccess},
		runID + ":rework": {status: apiv1.ResultSuccess},
		runID + ":verify": {status: apiv1.ResultSuccess},
	})

	machine := rebindFixtureMachine(t)
	if _, err := r.Start(context.Background(), StartInput{
		RunID: runID, Machine: machine, Gaggle: "acme-web",
		Trigger: journal.Trigger{Kind: journal.TriggerSchedule},
		RepoRef: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web", Branch: "main"},
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}

	runBranch := providers.BranchName("rebind-fixture", runID)
	for _, stage := range []string{"select", "rework", "verify"} {
		if got := observed[stage].branch; got != runBranch {
			t.Errorf("%s ran on branch %q, want %q", stage, got, runBranch)
		}
	}
}

// TestWorkspaceBranchIgnoresEmptyEmission proves an empty value is a no-op
// rather than a reset — a stage that declares the output but has nothing to
// bind (a no-work tick) must not silently detach later stages.
func TestWorkspaceBranchIgnoresEmptyEmission(t *testing.T) {
	runID := "rebind-run-3"
	r, _, observed := newRebindRunner(t, map[string]stubTaskResult{
		runID + ":select": {status: apiv1.ResultSuccess, outputs: map[string]interface{}{
			WorkspaceBranchOutput: rebindBranch,
		}},
		runID + ":rework": {status: apiv1.ResultSuccess, outputs: map[string]interface{}{
			WorkspaceBranchOutput: "   ",
		}},
		runID + ":verify": {status: apiv1.ResultSuccess},
	})

	machine := rebindFixtureMachine(t)
	if _, err := r.Start(context.Background(), StartInput{
		RunID: runID, Machine: machine, Gaggle: "acme-web",
		Trigger: journal.Trigger{Kind: journal.TriggerSchedule},
		RepoRef: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web", Branch: "main"},
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}

	if got := observed["verify"].branch; got != rebindBranch {
		t.Errorf("verify ran on branch %q after an empty emission, want the still-bound %q", got, rebindBranch)
	}
}

// TestLastWorkspaceBranchRecoversTheNewestBinding covers the resume half:
// walk's binding is in-memory only, so a crash anywhere after the rebinding
// stage must rebuild it from the journal — otherwise the rest of the chain
// resumes against a pristine branch off main and remediates the wrong tree.
func TestLastWorkspaceBranchRecoversTheNewestBinding(t *testing.T) {
	tests := []struct {
		name   string
		events []journal.Event
		want   string
	}{
		{
			name:   "no stage ever rebound",
			events: []journal.Event{{Type: journal.EventStageFinished, Stage: "select"}},
			want:   "",
		},
		{
			name: "newest binding wins",
			events: []journal.Event{
				{Type: journal.EventStageFinished, Stage: "select", Outputs: map[string]any{WorkspaceBranchOutput: "goobers/a"}},
				{Type: journal.EventStageFinished, Stage: "rework", Outputs: map[string]any{WorkspaceBranchOutput: "goobers/b"}},
			},
			want: "goobers/b",
		},
		{
			name: "later non-emitting stages do not clear it",
			events: []journal.Event{
				{Type: journal.EventStageFinished, Stage: "select", Outputs: map[string]any{WorkspaceBranchOutput: "goobers/a"}},
				{Type: journal.EventStageFinished, Stage: "rework"},
				{Type: journal.EventStageFinished, Stage: "verify", Outputs: map[string]any{"other": "x"}},
			},
			want: "goobers/a",
		},
		{
			name: "synthetic interrupted-attempt markers are skipped",
			events: []journal.Event{
				{Type: journal.EventStageFinished, Stage: "select", Outputs: map[string]any{WorkspaceBranchOutput: "goobers/a"}},
				{
					Type: journal.EventStageFinished, Stage: "rework", AttemptClass: journal.AttemptInfra,
					Error:   &journal.ErrorDetail{Code: interruptedAttemptErrorCode},
					Outputs: map[string]any{WorkspaceBranchOutput: "goobers/never"},
					Runner:  map[string]any{interruptedAttemptMarkerKey: true},
				},
			},
			want: "goobers/a",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := lastWorkspaceBranch(tt.events, rebindFixtureMachine(t), providers.DefaultBranchNamespace); got != tt.want {
				t.Errorf("lastWorkspaceBranch = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestWorkspaceBranchSurvivesCrashAndResume is the durability half of the
// rebinding, end to end rather than through lastWorkspaceBranch alone. walk
// holds the binding in memory only, so a crash after the rebinding stage —
// which for pr-remediation means anywhere across implement/review/local-ci,
// the long, expensive part of the run — must rebuild it from the journal.
//
// If it did not, the resumed run would quietly finish the remaining stages on
// a pristine branch cut from main: the reviewer would see the wrong diff and
// the rework would be lost, with the run still reporting success.
func TestWorkspaceBranchSurvivesCrashAndResume(t *testing.T) {
	const runID = "rebind-resume"
	machine := rebindFixtureMachine(t)

	instanceRoot := t.TempDir()
	wtMgr, err := worktree.NewManager(filepath.Join(instanceRoot, "workcopies"))
	if err != nil {
		t.Fatalf("new worktree manager: %v", err)
	}
	runsDir := filepath.Join(instanceRoot, "runs")
	fixtureRepo := newRebindFixtureRepo(t)

	// A journal for a run that got as far as finishing "select" — which
	// emitted the rebinding — and then died before "rework" was dispatched.
	jr, err := journal.Create(runsDir, journal.RunIdentity{
		RunID: runID, Workflow: machine.Def.Name, WorkflowVersion: machine.Def.Version,
		WorkflowDigest: machine.Digest(), Gaggle: "acme-web",
		Trigger: journal.Trigger{Kind: journal.TriggerSchedule},
	}, nil)
	if err != nil {
		t.Fatalf("journal.Create: %v", err)
	}
	jr.SetMachineState("select")
	if err := jr.Append(journal.Event{Type: journal.EventStageStarted, Stage: "select", Attempt: 1}); err != nil {
		t.Fatalf("append stage.started: %v", err)
	}
	if err := jr.Append(journal.Event{
		Type: journal.EventStageFinished, Stage: "select", Attempt: 1,
		Status:  string(apiv1.ResultSuccess),
		Outputs: map[string]any{WorkspaceBranchOutput: rebindBranch},
	}); err != nil {
		t.Fatalf("append stage.finished: %v", err)
	}
	if err := jr.Close(); err != nil {
		t.Fatalf("close journal: %v", err)
	}

	observed := map[string]observedWorkspace{}
	byTask := map[string]stubTaskResult{
		runID + ":rework": {status: apiv1.ResultSuccess},
		runID + ":verify": {status: apiv1.ResultSuccess},
	}
	r, err := New(Config{
		NewDeterministic: func(rec ArtifactRecorder, _ SecretRegistrar) (invoke.Deterministic, error) {
			return &branchObservingDeterministic{t: t, rec: rec, byTask: byTask, observed: observed}, nil
		},
		Worktrees:    wtMgr,
		RunsDir:      runsDir,
		RepoCloneURL: func(apiv1.RepoRef) (string, error) { return fixtureRepo, nil },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	res, err := r.Resume(context.Background(), ResumeInput{
		RunID:   runID,
		Machine: machine,
		RepoRef: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web", Branch: "main"},
	})
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if res.Phase != journal.PhaseCompleted {
		t.Fatalf("phase = %q, want %q", res.Phase, journal.PhaseCompleted)
	}

	for _, stage := range []string{"rework", "verify"} {
		got := observed[stage]
		if got.branch != rebindBranch {
			t.Errorf("resumed %s ran on branch %q, want the rebound %q recovered from the journal", stage, got.branch, rebindBranch)
		}
		if !got.hasMarker {
			t.Errorf("resumed %s could not see %s — the resumed run reverted to a branch off main", stage, rebindMarkerFile)
		}
	}
}

// TestWorkspaceBranchRejectsUntrustedRebindings is the guard on WHO may rebind.
//
// An agentic stage's Outputs are authored by the model (internal/harness's
// result-shape hint invites a free-form "outputs" map and passes it through
// verbatim), so honoring this key from one would let any goober in ANY
// workflow — `implementation` above all — move every later stage onto a branch
// of its choosing. `push-branch` pushes whatever branch its worktree is on, so
// the blast radius is the run's real work never being published, or with
// "main", a push straight at the trunk.
//
// Each case here would be a silent hijack if the corresponding check were
// dropped, so each asserts the run still ends on the DEFAULT branch.
func TestWorkspaceBranchRejectsUntrustedRebindings(t *testing.T) {
	tests := []struct {
		name    string
		agentic bool
		value   interface{}
	}{
		{name: "agentic stage may not rebind", agentic: true, value: rebindBranch},
		{name: "branch outside the run namespace", value: "main"},
		{name: "branch outside the run namespace, plausible", value: "refs/heads/main"},
		{name: "non-string value is not coerced", value: false},
		{name: "numeric value is not coerced", value: 42},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			task := apiv1.Task{Name: "select", Type: apiv1.TaskDeterministic}
			if tt.agentic {
				task = apiv1.Task{Name: "select", Type: apiv1.TaskAgentic, Goober: "implementer"}
			}
			got := rebindWorkspaceBranch(task, apiv1.ResultEnvelope{
				Outputs: map[string]interface{}{WorkspaceBranchOutput: tt.value},
			}, providers.DefaultBranchNamespace)
			if got != "" {
				t.Errorf("rebindWorkspaceBranch = %q, want \"\" — this emission must not be honored", got)
			}
		})
	}

	// The legitimate case still works, so the guards above are not vacuous.
	got := rebindWorkspaceBranch(
		apiv1.Task{Name: "gather-pr-context", Type: apiv1.TaskDeterministic},
		apiv1.ResultEnvelope{Outputs: map[string]interface{}{WorkspaceBranchOutput: rebindBranch}},
		providers.DefaultBranchNamespace,
	)
	if got != rebindBranch {
		t.Errorf("rebindWorkspaceBranch = %q, want %q for a deterministic stage naming a run-namespace branch", got, rebindBranch)
	}
}

// TestWorkspaceBranchRejectsUntrustedRebindingsOnResume closes the same hole on
// the resume path. A stage.finished event records outputs but NOT the producing
// task's type, so recovering the binding from the journal has to look the type
// back up in the machine — otherwise an agentic stage's rebinding would be
// ignored while running and silently honored after the first crash.
func TestWorkspaceBranchRejectsUntrustedRebindingsOnResume(t *testing.T) {
	spec := apiv1.WorkflowSpec{
		Gaggle:   "acme-web",
		Triggers: []apiv1.Trigger{{Type: apiv1.TriggerSchedule, Schedule: "0 * * * *"}},
		Start:    "implement",
		Tasks: []apiv1.Task{
			{Name: "implement", Type: apiv1.TaskAgentic, Goober: "implementer", Goal: "implement", Next: workflow.TerminalComplete},
		},
	}
	machine, err := workflow.Compile(
		workflow.Definition{Name: "agentic-only", Version: 1, Spec: spec},
		workflow.WithGoobers(map[string]apiv1.GooberSpec{
			"implementer": {Workflows: []string{"agentic-only"}, Capabilities: []string{"repo:push", "agent:model"}},
		}),
	)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}

	events := []journal.Event{{
		Type: journal.EventStageFinished, Stage: "implement", Attempt: 1,
		Status:  string(apiv1.ResultSuccess),
		Outputs: map[string]any{WorkspaceBranchOutput: rebindBranch},
	}}
	if got := lastWorkspaceBranch(events, machine, providers.DefaultBranchNamespace); got != "" {
		t.Errorf("lastWorkspaceBranch = %q, want \"\" — an agentic stage's rebinding must not be honored on resume either", got)
	}

	// A stage the machine does not know is ignored rather than trusted.
	stray := []journal.Event{{
		Type: journal.EventStageFinished, Stage: "does-not-exist",
		Outputs: map[string]any{WorkspaceBranchOutput: rebindBranch},
	}}
	if got := lastWorkspaceBranch(stray, machine, providers.DefaultBranchNamespace); got != "" {
		t.Errorf("lastWorkspaceBranch = %q, want \"\" for an unknown stage", got)
	}
}

// TestRebindingAMissingBranchFailsLoudly covers the silent-substitution hazard.
// worktree.Create's default is to CREATE a branch it cannot find, which is
// correct for a run's own branch and catastrophic for a rebound one: the stage
// would get a pristine checkout off main wearing the PR's branch name, pass CI
// on it, and a later force-push would replace the PR's real content with base.
// The mirror only holds the PR's branch because an earlier stage fetched it —
// WorkingCopy's refspec excludes that namespace — so this is reachable, not
// theoretical.
func TestRebindingAMissingBranchFailsLoudly(t *testing.T) {
	runID := "rebind-missing"
	r, _, _ := newRebindRunner(t, map[string]stubTaskResult{
		runID + ":select": {status: apiv1.ResultSuccess, outputs: map[string]interface{}{
			// In the run-branch namespace and well-formed, but no such branch
			// exists on the remote.
			WorkspaceBranchOutput: "goobers/implementation/never-existed",
		}},
		runID + ":rework": {status: apiv1.ResultSuccess},
		runID + ":verify": {status: apiv1.ResultSuccess},
	})

	res, err := r.Start(context.Background(), StartInput{
		RunID: runID, Machine: rebindFixtureMachine(t), Gaggle: "acme-web",
		Trigger: journal.Trigger{Kind: journal.TriggerSchedule},
		RepoRef: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web", Branch: "main"},
	})
	if err == nil && res.Phase == journal.PhaseCompleted {
		t.Fatal("run completed on a rebound branch that does not exist — the stage silently got a fresh branch off main")
	}
}
