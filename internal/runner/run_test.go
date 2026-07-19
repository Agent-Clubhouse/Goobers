package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/gate"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/telemetry"
	"github.com/goobers/goobers/internal/workflow"
	"github.com/goobers/goobers/internal/worktree"
	"github.com/goobers/goobers/providers"
)

// fakeSpanStarter records every span opened, mirroring runner.SpanStarter's
// three methods (issue #126). Returns the zero telemetry.Span throughout, so
// its End/Succeed/Fail calls no-op exactly like a nil Telemetry would.
type fakeSpanStarter struct {
	mu    sync.Mutex
	calls []string
}

func (f *fakeSpanStarter) record(s string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, s)
}

func (f *fakeSpanStarter) StartRun(ctx context.Context, attrs telemetry.RunAttributes) (context.Context, telemetry.Span, error) {
	f.record("run:" + attrs.RunID)
	return ctx, telemetry.Span{}, nil
}

func (f *fakeSpanStarter) StartTask(ctx context.Context, attrs telemetry.TaskAttributes) (context.Context, telemetry.Span, error) {
	f.record("task:" + attrs.TaskID)
	return ctx, telemetry.Span{}, nil
}

func (f *fakeSpanStarter) StartGate(ctx context.Context, attrs telemetry.GateAttributes) (context.Context, telemetry.Span, error) {
	f.record("gate:" + attrs.GateID)
	return ctx, telemetry.Span{}, nil
}

// --- stub executors (the "stub stage effects" issue #17 asks for), built
// against the real invoke.Deterministic/invoke.Goober interfaces #18/#19
// implement — not a bespoke seam of this package's own. ---

// stubTaskResult scripts one task's canned outcome, keyed by TaskID.
type stubTaskResult struct {
	status            apiv1.ResultStatus
	summary           string
	errorInfo         *apiv1.ErrorInfo
	artifactName      string
	artifactData      []byte
	artifactMediaType string
	outputs           map[string]interface{}
}

// stubDeterministic implements invoke.Deterministic like a real executor
// would: it commits its own artifact to the run's journal (via the
// ArtifactRecorder bound at construction) and returns a ResultEnvelope whose
// Artifacts are already real ArtifactPointers — mirroring
// internal/executor.ShellExecutor exactly, just without the shell.
type stubDeterministic struct {
	rec    ArtifactRecorder
	byTask map[string]stubTaskResult
}

func (s *stubDeterministic) Run(_ context.Context, env apiv1.InvocationEnvelope, _ apiv1.DeterministicRun) (apiv1.ResultEnvelope, error) {
	cfg, ok := s.byTask[env.TaskID]
	if !ok {
		return apiv1.ResultEnvelope{}, fmt.Errorf("stub executor: no canned output for %q", env.TaskID)
	}
	result := apiv1.ResultEnvelope{Status: cfg.status, Summary: cfg.summary, Error: cfg.errorInfo, Outputs: cfg.outputs}
	if cfg.artifactName != "" {
		ref, err := s.rec.RecordArtifact(cfg.artifactName, cfg.artifactData)
		if err != nil {
			return apiv1.ResultEnvelope{}, err
		}
		result.Artifacts = []apiv1.ArtifactPointer{{
			Path: ref.Path, Digest: ref.Digest, Size: ref.Size, MediaType: cfg.artifactMediaType,
		}}
	}
	return result, nil
}

// committingStubDeterministic is stubDeterministic that first commits a real
// (non-empty) change to the run branch, so a stage feeding an agentic review
// gate produces a diff the reviewer can be invoked on — since #415 an empty
// diff fast-fails before the reviewer. Status/error/outputs/artifacts still
// come from byTask, so it drops into any stub-driven agentic-gate test.
type committingStubDeterministic struct {
	t      *testing.T
	rec    ArtifactRecorder
	byTask map[string]stubTaskResult
}

func (c *committingStubDeterministic) Run(ctx context.Context, env apiv1.InvocationEnvelope, dr apiv1.DeterministicRun) (apiv1.ResultEnvelope, error) {
	c.t.Helper()
	if err := os.WriteFile(filepath.Join(env.Workspace, "impl.txt"), []byte("stub implementation change\n"), 0o644); err != nil {
		return apiv1.ResultEnvelope{}, err
	}
	runGit(c.t, env.Workspace, "add", "-A")
	runGit(c.t, env.Workspace, "commit", "-m", "stub impl change")
	return (&stubDeterministic{rec: c.rec, byTask: c.byTask}).Run(ctx, env, dr)
}

// outputCapturingDeterministic is stubDeterministic plus recording each
// call's full InvocationEnvelope by TaskID, so a test can assert what
// actually arrived in env.Inputs — e.g. #132's InputsFrom output->input
// threading, which stubDeterministic's plain canned-Outputs lookup can't
// observe from the consuming side. (Named distinctly from #107's
// capturingDeterministic below, which records only its last call for a
// different purpose — resume pointer-visibility, not input threading.)
type outputCapturingDeterministic struct {
	rec      ArtifactRecorder
	byTask   map[string]stubTaskResult
	received map[string]apiv1.InvocationEnvelope
}

func (c *outputCapturingDeterministic) Run(_ context.Context, env apiv1.InvocationEnvelope, _ apiv1.DeterministicRun) (apiv1.ResultEnvelope, error) {
	if c.received == nil {
		c.received = map[string]apiv1.InvocationEnvelope{}
	}
	c.received[env.TaskID] = env
	cfg, ok := c.byTask[env.TaskID]
	if !ok {
		return apiv1.ResultEnvelope{}, fmt.Errorf("stub executor: no canned output for %q", env.TaskID)
	}
	result := apiv1.ResultEnvelope{Status: cfg.status, Summary: cfg.summary, Error: cfg.errorInfo, Outputs: cfg.outputs}
	if cfg.artifactName != "" {
		ref, err := c.rec.RecordArtifact(cfg.artifactName, cfg.artifactData)
		if err != nil {
			return apiv1.ResultEnvelope{}, err
		}
		result.Artifacts = []apiv1.ArtifactPointer{{
			Path: ref.Path, Digest: ref.Digest, Size: ref.Size, MediaType: cfg.artifactMediaType,
		}}
	}
	return result, nil
}

// flakyDeterministic fails its first failUntil calls with a dispatch-level Go
// error (not a business ResultStatus), then succeeds — for exercising
// Task.Retry's policy-attempt loop. Safe for the single-goroutine dispatch
// runTask does (no locking needed).
type flakyDeterministic struct {
	failUntil int
	calls     int
}

type infrastructureFlakyDeterministic struct {
	failUntil int
	calls     int
	cause     error
}

type sequencedDeterministic struct {
	failures []error
	calls    int
}

func (s *sequencedDeterministic) Run(_ context.Context, _ apiv1.InvocationEnvelope, _ apiv1.DeterministicRun) (apiv1.ResultEnvelope, error) {
	s.calls++
	if s.calls <= len(s.failures) {
		return apiv1.ResultEnvelope{}, s.failures[s.calls-1]
	}
	return apiv1.ResultEnvelope{Status: apiv1.ResultSuccess, Summary: "eventually succeeded"}, nil
}

func (f *infrastructureFlakyDeterministic) Run(_ context.Context, _ apiv1.InvocationEnvelope, _ apiv1.DeterministicRun) (apiv1.ResultEnvelope, error) {
	f.calls++
	if f.calls <= f.failUntil {
		return apiv1.ResultEnvelope{}, invoke.InfrastructureFailure(fmt.Errorf("provider request: %w", f.cause))
	}
	return apiv1.ResultEnvelope{Status: apiv1.ResultSuccess, Summary: "provider recovered"}, nil
}

func (f *flakyDeterministic) Run(_ context.Context, _ apiv1.InvocationEnvelope, _ apiv1.DeterministicRun) (apiv1.ResultEnvelope, error) {
	f.calls++
	if f.calls <= f.failUntil {
		return apiv1.ResultEnvelope{}, fmt.Errorf("transient failure (call %d)", f.calls)
	}
	return apiv1.ResultEnvelope{Status: apiv1.ResultSuccess, Summary: "eventually succeeded"}, nil
}

// backoffObservedContext reports when runTask obtains Done for its retry wait.
// The walk loop checks Err instead, so this fires only after dispatch fails.
type backoffObservedContext struct {
	context.Context
	entered chan<- struct{}
}

func (c backoffObservedContext) Done() <-chan struct{} {
	select {
	case c.entered <- struct{}{}:
	default:
	}
	return c.Context.Done()
}

// blockingDeterministic signals started once dispatch actually reaches it,
// then blocks until released and signals finished — for proving a
// SIGTERM-style cancellation lets an in-flight attempt finish rather than
// aborting it. started lets the test synchronize "cancel only once the
// attempt is truly in flight" instead of racing the walk loop's own
// between-stages cancellation check.
type blockingDeterministic struct {
	started  chan struct{}
	release  chan struct{}
	finished chan struct{}
}

func (b *blockingDeterministic) Run(_ context.Context, _ apiv1.InvocationEnvelope, _ apiv1.DeterministicRun) (apiv1.ResultEnvelope, error) {
	close(b.started)
	<-b.release
	close(b.finished)
	return apiv1.ResultEnvelope{Status: apiv1.ResultSuccess}, nil
}

// alwaysFailAutomated is an invoke.Automated that always reports "fail",
// regardless of the subject's actual status — for constructing a gate whose
// declared "pass" branch is statically reachable (satisfying
// workflow.Compile's reachability check) but never actually taken at
// runtime, e.g. to exercise a genuine runaway machine against maxSteps.
type alwaysFailAutomated struct{}

func (alwaysFailAutomated) Evaluate(context.Context, apiv1.AutomatedGate, apiv1.InvocationEnvelope) (string, error) {
	return "fail", nil
}

// --- fixture repo: a local bare repo, so the test needs no network access ---

func newFixtureRepo(t *testing.T) string {
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
	runGit(t, "", "clone", "--bare", work, bare)
	return bare
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Run(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out.String())
	}
}

// newHTTPGitRemote serves newFixtureRepo's bare repo over git's dumb-HTTP
// protocol, failing the first `failures` requests with failureStatus (or
// every request, when failures < 0) before serving normally — issue #572's
// worktree-provisioning retry tests need a real transient (or persistent)
// network failure, not a mocked classifier input, to prove the failure
// actually reaches worktree.Create -> internal/runner's dispatchTask
// end-to-end.
func newHTTPGitRemote(t *testing.T, failureStatus, failures int32) string {
	t.Helper()
	repo := newFixtureRepo(t)
	runGit(t, repo, "-c", "safe.bareRepository=all", "update-server-info")

	files := http.FileServer(http.Dir(filepath.Dir(repo)))
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		request := requests.Add(1)
		if failures < 0 || request <= failures {
			if failureStatus == http.StatusUnauthorized {
				w.Header().Set("WWW-Authenticate", `Basic realm="test"`)
			}
			http.Error(w, http.StatusText(int(failureStatus)), int(failureStatus))
			return
		}
		files.ServeHTTP(w, req)
	}))
	t.Cleanup(server.Close)
	return server.URL + "/" + filepath.Base(repo)
}

// newWorktreeProvisioningTestRunner mirrors newTestRunnerWithDeterministic
// but takes repoURL directly (a flaky/failing HTTP git remote, not the
// always-healthy local bare fixture) — issue #572's tests need Worktrees.
// Create's real clone to actually fail, which the fixed-fixtureRepo
// constructor never exercises.
func newWorktreeProvisioningTestRunner(t *testing.T, repoURL string, newDet NewDeterministicFunc) (*Runner, string) {
	t.Helper()
	instanceRoot := t.TempDir()
	wtMgr, err := worktree.NewManager(filepath.Join(instanceRoot, "workcopies"))
	if err != nil {
		t.Fatalf("new worktree manager: %v", err)
	}
	runsDir := filepath.Join(instanceRoot, "runs")
	r, err := New(Config{
		NewDeterministic: newDet,
		Automated:        gate.NewAutomatedEvaluator(),
		Worktrees:        wtMgr,
		RunsDir:          runsDir,
		RepoCloneURL:     func(apiv1.RepoRef) (string, error) { return repoURL, nil },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return r, runsDir
}

// TestRunnerRetriesTransientWorktreeProvisioningAsInfrastructure is issue
// #572's headline acceptance: a worktree remote that fails once with a
// transient 503 then succeeds records an infrastructure attempt followed by
// a successful one, through #613's EXISTING invoke.InfrastructureFailure
// retry path (internal/runner/provider_retry_test.go's own mechanism) —
// dispatchTask's classification is the only new code; runTask's retry loop,
// resume reconstruction, and journaling are untouched.
func TestRunnerRetriesTransientWorktreeProvisioningAsInfrastructure(t *testing.T) {
	repoURL := newHTTPGitRemote(t, http.StatusServiceUnavailable, 1)
	executor := &flakyDeterministic{}
	r, runsDir := newWorktreeProvisioningTestRunner(t, repoURL, func(rec ArtifactRecorder, _ SecretRegistrar) (invoke.Deterministic, error) {
		return executor, nil
	})

	res, err := r.Start(context.Background(), StartInput{
		RunID:   "run-worktree-flaky",
		Machine: fixtureMachine(t),
		Gaggle:  "acme-web",
		Trigger: journal.Trigger{Kind: journal.TriggerManual},
		RepoRef: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web", Branch: "main"},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if res.Phase != journal.PhaseCompleted {
		t.Fatalf("phase = %q, want completed", res.Phase)
	}
	if executor.calls != 1 {
		t.Fatalf("executor called %d times, want 1 (the worktree failure never reached the executor)", executor.calls)
	}

	rd, err := journal.OpenRead(filepath.Join(runsDir, "run-worktree-flaky"))
	if err != nil {
		t.Fatalf("OpenRead: %v", err)
	}
	events, err := rd.Events()
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	var starts, errs []journal.Event
	for _, e := range events {
		if e.Stage != "implement" {
			continue
		}
		switch e.Type {
		case journal.EventStageStarted:
			starts = append(starts, e)
		case journal.EventError:
			errs = append(errs, e)
		}
	}
	if len(starts) != 2 {
		t.Fatalf("stage.started events for implement = %d, want 2: %+v", len(starts), starts)
	}
	if starts[0].AttemptClass != "" || starts[1].AttemptClass != journal.AttemptInfra {
		t.Fatalf("start classes = [%q, %q], want [\"\", infra] (first attempt normative, retry infra-tagged)", starts[0].AttemptClass, starts[1].AttemptClass)
	}
	// The failing attempt's own stage.started/error events are journaled
	// under ITS class (attempt 1 is always normative, "" — AttemptClass
	// marks which attempt a retry belongs to, not whether the FAILURE that
	// triggered it was infrastructure-classified). What proves the
	// classification actually fired is starts[1]'s AttemptClass (asserted
	// above) plus the preserved 503 cause here.
	found := false
	for _, e := range errs {
		if e.Error != nil && e.Error.Code == "executor_error" {
			found = true
			if !strings.Contains(strings.ToLower(e.Error.Message), "503") {
				t.Fatalf("error message = %q, want it to preserve the original 503 cause", e.Error.Message)
			}
		}
	}
	if !found {
		t.Fatal("no executor_error event found for the worktree provisioning failure")
	}
}

// TestRunnerFailsTerminallyAfterPersistentWorktreeProvisioningFailure proves
// a persistent transient-looking outage still fails the run — the
// infrastructure budget (#613's DefaultMaxInfrastructureAttempts) bounds it
// the same way it bounds a persistent provider failure, with the original
// network cause preserved in the terminal error.
func TestRunnerFailsTerminallyAfterPersistentWorktreeProvisioningFailure(t *testing.T) {
	repoURL := newHTTPGitRemote(t, http.StatusServiceUnavailable, -1)
	executor := &flakyDeterministic{}
	r, _ := newWorktreeProvisioningTestRunner(t, repoURL, func(rec ArtifactRecorder, _ SecretRegistrar) (invoke.Deterministic, error) {
		return executor, nil
	})

	res, err := r.Start(context.Background(), StartInput{
		RunID:   "run-worktree-persistent",
		Machine: fixtureMachine(t),
		Gaggle:  "acme-web",
		Trigger: journal.Trigger{Kind: journal.TriggerManual},
		RepoRef: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web", Branch: "main"},
	})
	if err == nil {
		t.Fatal("expected the persistent worktree failure to surface as a dispatch error")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "503") {
		t.Fatalf("terminal error = %q, want the original 503 cause preserved", err)
	}
	if res.Phase != journal.PhaseFailed {
		t.Fatalf("phase = %q, want failed", res.Phase)
	}
	if executor.calls != 0 {
		t.Fatalf("executor called %d times, want 0 (worktree provisioning never succeeded)", executor.calls)
	}
}

// TestRunnerNeverRetriesAuthWorktreeProvisioningFailure is #572's negative
// acceptance criterion: a deterministic (auth) worktree failure fails on the
// FIRST attempt, never retried — retrying can only reproduce the identical
// 401, so treating it as infrastructure would just waste the run's time
// budget.
func TestRunnerNeverRetriesAuthWorktreeProvisioningFailure(t *testing.T) {
	repoURL := newHTTPGitRemote(t, http.StatusUnauthorized, -1)
	executor := &flakyDeterministic{}
	r, runsDir := newWorktreeProvisioningTestRunner(t, repoURL, func(rec ArtifactRecorder, _ SecretRegistrar) (invoke.Deterministic, error) {
		return executor, nil
	})

	_, err := r.Start(context.Background(), StartInput{
		RunID:   "run-worktree-auth",
		Machine: fixtureMachine(t),
		Gaggle:  "acme-web",
		Trigger: journal.Trigger{Kind: journal.TriggerManual},
		RepoRef: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web", Branch: "main"},
	})
	if err == nil {
		t.Fatal("expected the auth worktree failure to surface as a dispatch error")
	}

	rd, rerr := journal.OpenRead(filepath.Join(runsDir, "run-worktree-auth"))
	if rerr != nil {
		t.Fatalf("OpenRead: %v", rerr)
	}
	events, eerr := rd.Events()
	if eerr != nil {
		t.Fatalf("Events: %v", eerr)
	}
	var starts int
	for _, e := range events {
		if e.Stage == "implement" && e.Type == journal.EventStageStarted {
			starts++
		}
	}
	if starts != 1 {
		t.Fatalf("stage.started events for implement = %d, want exactly 1 (no retry for a deterministic auth failure)", starts)
	}
}

// --- fixture workflow: implement -> review (gate) -> pass/fail ---

func fixtureMachine(t *testing.T) *workflow.Machine {
	t.Helper()
	spec := apiv1.WorkflowSpec{
		Gaggle:   "acme-web",
		Triggers: []apiv1.Trigger{{Type: apiv1.TriggerBacklogItem}},
		Start:    "implement",
		Tasks: []apiv1.Task{
			{Name: "implement", Type: apiv1.TaskDeterministic, Goal: "produce a diff", Run: &apiv1.DeterministicRun{Command: []string{"true"}}, Next: "review"},
		},
		Gates: []apiv1.Gate{
			{
				Name:      "review",
				Evaluator: apiv1.EvaluatorAutomated,
				Automated: &apiv1.AutomatedGate{Check: "status-equals"},
				Branches: map[string]string{
					"pass": workflow.TerminalComplete,
					"fail": workflow.TargetAbort,
				},
			},
		},
	}
	m, err := workflow.Compile(workflow.Definition{Name: "fixture", Version: 1, Spec: spec})
	if err != nil {
		t.Fatalf("compile fixture machine: %v", err)
	}
	return m
}

func escalationParkingMachine(t *testing.T) *workflow.Machine {
	t.Helper()
	spec := apiv1.WorkflowSpec{
		Gaggle:   "acme-web",
		Triggers: []apiv1.Trigger{{Type: apiv1.TriggerBacklogItem}},
		Start:    "implement",
		Tasks: []apiv1.Task{
			{Name: "implement", Type: apiv1.TaskDeterministic, Goal: "produce a diff", Run: &apiv1.DeterministicRun{Command: []string{"true"}}, Next: "review"},
			{Name: "park-needs-human", Type: apiv1.TaskDeterministic, Goal: "park the issue", Run: &apiv1.DeterministicRun{Command: []string{"true"}}, Next: workflow.TargetAbort},
			// The escalate branch parks and then terminates @escalate, mirroring
			// the shipped implementation workflow: parking must not downgrade an
			// escalation to an abort, which is what every escalation surface
			// (run exit 3, escalationCause, trace) selects on.
			{Name: "park-escalated", Type: apiv1.TaskDeterministic, Goal: "park the escalated issue", Run: &apiv1.DeterministicRun{Command: []string{"true"}}, Next: workflow.TargetEscalate},
		},
		Gates: []apiv1.Gate{{
			Name:      "review",
			Evaluator: apiv1.EvaluatorAutomated,
			Automated: &apiv1.AutomatedGate{Check: "status-equals"},
			Branches: map[string]string{
				"pass":                  workflow.TerminalComplete,
				"fail":                  "park-needs-human",
				workflow.BranchEscalate: "park-escalated",
			},
		}},
	}
	m, err := workflow.Compile(workflow.Definition{Name: "escalation-parking", Version: 1, Spec: spec})
	if err != nil {
		t.Fatalf("compile escalation parking machine: %v", err)
	}
	return m
}

func newTestRunner(t *testing.T, byTask map[string]stubTaskResult, automated invoke.Automated) (*Runner, string) {
	t.Helper()
	return newTestRunnerWithDeterministic(t, func(rec ArtifactRecorder, _ SecretRegistrar) (invoke.Deterministic, error) {
		return &stubDeterministic{rec: rec, byTask: byTask}, nil
	}, automated)
}

// newTestRunnerWithDeterministic builds a Runner over a fixed deterministic
// executor factory, for tests (retries, drain) that need a stub richer than
// stubTaskResult's canned per-TaskID map.
func newTestRunnerWithDeterministic(t *testing.T, newDet NewDeterministicFunc, automated invoke.Automated) (*Runner, string) {
	t.Helper()
	instanceRoot := t.TempDir()
	wtMgr, err := worktree.NewManager(filepath.Join(instanceRoot, "workcopies"))
	if err != nil {
		t.Fatalf("new worktree manager: %v", err)
	}
	runsDir := filepath.Join(instanceRoot, "runs")
	fixtureRepo := newFixtureRepo(t)

	r, err := New(Config{
		NewDeterministic: newDet,
		Automated:        automated,
		Worktrees:        wtMgr,
		RunsDir:          runsDir,
		RepoCloneURL: func(apiv1.RepoRef) (string, error) {
			return fixtureRepo, nil
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return r, runsDir
}

// retryFixtureMachine is fixtureMachine's single task, but with a declared
// retry policy — a separate spec so the existing single-attempt tests stay
// exactly as they were.
func retryFixtureMachine(t *testing.T, maxAttempts int32) *workflow.Machine {
	t.Helper()
	spec := apiv1.WorkflowSpec{
		Gaggle:   "acme-web",
		Triggers: []apiv1.Trigger{{Type: apiv1.TriggerBacklogItem}},
		Start:    "implement",
		Tasks: []apiv1.Task{
			{
				Name: "implement", Type: apiv1.TaskDeterministic, Goal: "produce a diff",
				Run:   &apiv1.DeterministicRun{Command: []string{"true"}},
				Next:  "review",
				Retry: &apiv1.RetryPolicy{MaxAttempts: maxAttempts},
			},
		},
		Gates: []apiv1.Gate{
			{
				Name:      "review",
				Evaluator: apiv1.EvaluatorAutomated,
				Automated: &apiv1.AutomatedGate{Check: "status-equals"},
				Branches:  map[string]string{"pass": workflow.TerminalComplete, "fail": workflow.TargetAbort},
			},
		},
	}
	m, err := workflow.Compile(workflow.Definition{Name: "retry-fixture", Version: 1, Spec: spec})
	if err != nil {
		t.Fatalf("compile retry fixture machine: %v", err)
	}
	return m
}

// taskReservedNextFixtureMachine is a single deterministic task whose Next is
// a reserved terminal target directly (#123) — unlike retryFixtureMachine's
// abort path, there is no gate mediating it: workflow.Compile admits a
// reserved target as a task's Next exactly as it does for a gate branch
// (compile.go's isTerminal check covers both), so the runner must walk it the
// same way too.
func taskReservedNextFixtureMachine(t *testing.T, next string) *workflow.Machine {
	t.Helper()
	spec := apiv1.WorkflowSpec{
		Gaggle:   "acme-web",
		Triggers: []apiv1.Trigger{{Type: apiv1.TriggerBacklogItem}},
		Start:    "implement",
		Tasks: []apiv1.Task{
			{
				Name: "implement", Type: apiv1.TaskDeterministic, Goal: "produce a diff",
				Run:  &apiv1.DeterministicRun{Command: []string{"true"}},
				Next: next,
			},
		},
	}
	m, err := workflow.Compile(workflow.Definition{Name: "task-reserved-next-fixture", Version: 1, Spec: spec})
	if err != nil {
		t.Fatalf("compile task-reserved-next fixture machine (next=%q): %v", next, err)
	}
	return m
}

// noWorkFixtureMachine is a two-task query-backlog -> curate chain — the
// exact shape backlog-curation.yaml uses (issue #233) — where "curate" is
// compiled into the machine but deliberately has NO canned output in the
// test's byTask map, so if the runner ever dispatches it the stub
// executor's own "no canned output" error surfaces as a hard Start error
// (see stubDeterministic.Run) — a directly observable proof that a
// ResultNoWork query-backlog never reaches it, not just an indirect
// phase-only assertion.
func noWorkFixtureMachine(t *testing.T) *workflow.Machine {
	t.Helper()
	spec := apiv1.WorkflowSpec{
		Gaggle:   "acme-web",
		Triggers: []apiv1.Trigger{{Type: apiv1.TriggerSchedule, Schedule: "@hourly"}},
		Start:    "query-backlog",
		Tasks: []apiv1.Task{
			{
				Name: "query-backlog", Type: apiv1.TaskDeterministic, Goal: "claim eligible items",
				Run: &apiv1.DeterministicRun{Command: []string{"true"}}, Next: "curate",
			},
			{
				Name: "curate", Type: apiv1.TaskDeterministic, Goal: "curate the claimed items",
				Run: &apiv1.DeterministicRun{Command: []string{"true"}},
			},
		},
	}
	m, err := workflow.Compile(workflow.Definition{Name: "no-work-fixture", Version: 1, Spec: spec})
	if err != nil {
		t.Fatalf("compile no-work fixture machine: %v", err)
	}
	return m
}

// TestRunnerNoWorkResultShortCircuitsToCompleted is issue #233's core
// runner-level acceptance: a task reporting ResultNoWork ends the run
// PhaseCompleted immediately, WITHOUT dispatching its declared Next
// ("curate") — an empty-backlog tick must not invoke a downstream agentic
// stage with no subject to act on.
func TestRunnerNoWorkResultShortCircuitsToCompleted(t *testing.T) {
	machine := noWorkFixtureMachine(t)
	runID := "run-no-work"
	// Deliberately no entry for "curate" — see noWorkFixtureMachine's doc.
	byTask := map[string]stubTaskResult{
		runID + ":query-backlog": {status: apiv1.ResultNoWork, summary: "nothing eligible"},
	}
	r, _ := newTestRunner(t, byTask, nil)

	res, err := r.Start(context.Background(), StartInput{
		RunID:   runID,
		Machine: machine,
		Gaggle:  "acme-web",
		Trigger: journal.Trigger{Kind: journal.TriggerManual},
		RepoRef: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web", Branch: "main"},
	})
	if err != nil {
		t.Fatalf("Start: %v (a non-nil error here means curate was dispatched despite no canned output — the short-circuit failed)", err)
	}
	if res.Phase != journal.PhaseCompleted {
		t.Fatalf("phase = %q, want completed", res.Phase)
	}
	if res.FinalState != "query-backlog" {
		t.Fatalf("finalState = %q, want query-backlog (curate must never have become the final state)", res.FinalState)
	}
}

// TestRunnerNoWorkResultIgnoresReservedNextToo proves the short-circuit is
// unconditional on Next, not merely a coincidence of "curate" being a plain
// state name: even when Next names a RESERVED terminal target, ResultNoWork
// still routes to PhaseCompleted specifically (via the ResultNoWork case),
// not whatever phase that reserved target would have produced (@abort would
// otherwise journal PhaseAborted) — the stage's own reported outcome must
// win over a coincidentally-matching Next, since @abort here would mean
// something very different from "no work."
func TestRunnerNoWorkResultIgnoresReservedNextToo(t *testing.T) {
	machine := taskReservedNextFixtureMachine(t, workflow.TargetAbort)
	runID := "run-no-work-reserved-next"
	byTask := map[string]stubTaskResult{
		runID + ":implement": {status: apiv1.ResultNoWork, summary: "nothing to do"},
	}
	r, _ := newTestRunner(t, byTask, nil)

	res, err := r.Start(context.Background(), StartInput{
		RunID:   runID,
		Machine: machine,
		Gaggle:  "acme-web",
		Trigger: journal.Trigger{Kind: journal.TriggerManual},
		RepoRef: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web", Branch: "main"},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if res.Phase != journal.PhaseCompleted {
		t.Fatalf("phase = %q, want completed (ResultNoWork must win over Next=@abort)", res.Phase)
	}
}

// inputsFromFixtureMachine is a minimal open-pr -> ci-poll chain proving
// #132's task-to-task output->input threading: ci-poll declares
// InputsFrom: {"prNumber": "prNumber"}, so it must receive open-pr's
// Outputs["prNumber"] as its own env.Inputs["prNumber"].
func inputsFromFixtureMachine(t *testing.T) *workflow.Machine {
	t.Helper()
	spec := apiv1.WorkflowSpec{
		Gaggle:   "acme-web",
		Triggers: []apiv1.Trigger{{Type: apiv1.TriggerBacklogItem}},
		Start:    "open-pr",
		Tasks: []apiv1.Task{
			{Name: "open-pr", Type: apiv1.TaskDeterministic, Goal: "open the pr", Run: &apiv1.DeterministicRun{Command: []string{"true"}}, Next: "ci-poll"},
			{Name: "ci-poll", Type: apiv1.TaskDeterministic, Goal: "poll ci", Run: &apiv1.DeterministicRun{Command: []string{"true"}},
				InputsFrom: map[string]string{"prNumber": "prNumber"}},
		},
	}
	m, err := workflow.Compile(workflow.Definition{Name: "inputs-from-fixture", Version: 1, Spec: spec})
	if err != nil {
		t.Fatalf("compile inputs-from fixture machine: %v", err)
	}
	return m
}

// TestRunnerThreadsInputsFromUpstreamOutputs proves the #132 prNumber-handoff
// mechanism end to end at the runner level: open-pr's Outputs["prNumber"]
// arrives in ci-poll's env.Inputs["prNumber"] via the declared InputsFrom
// mapping, not blanket propagation.
func TestRunnerThreadsInputsFromUpstreamOutputs(t *testing.T) {
	machine := inputsFromFixtureMachine(t)
	byTask := map[string]stubTaskResult{
		"run-1:open-pr": {status: apiv1.ResultSuccess, outputs: map[string]interface{}{"prNumber": "42"}},
		"run-1:ci-poll": {status: apiv1.ResultSuccess},
	}
	det := &outputCapturingDeterministic{byTask: byTask}
	r, _ := newTestRunnerWithDeterministic(t, func(rec ArtifactRecorder, _ SecretRegistrar) (invoke.Deterministic, error) {
		det.rec = rec
		return det, nil
	}, gate.NewAutomatedEvaluator())

	res, err := r.Start(context.Background(), StartInput{
		RunID:   "run-1",
		Machine: machine,
		Gaggle:  "acme-web",
		Trigger: journal.Trigger{Kind: journal.TriggerManual},
		RepoRef: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web", Branch: "main"},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if res.Phase != journal.PhaseCompleted {
		t.Fatalf("phase = %q, want completed", res.Phase)
	}
	ciPollEnv, ok := det.received["run-1:ci-poll"]
	if !ok {
		t.Fatal("ci-poll never dispatched")
	}
	if got := ciPollEnv.Inputs["prNumber"]; got != "42" {
		t.Fatalf("ci-poll env.Inputs[prNumber] = %v, want \"42\" (threaded from open-pr's Outputs)", got)
	}
}

// TestRunnerInputsFromMissingUpstreamOutputFailsClosed proves a declared
// InputsFrom entry whose referenced upstream output key never showed up
// fails the stage closed rather than silently dispatching with the key
// absent — InputsFrom is a contract, not a best-effort hint.
func TestRunnerInputsFromMissingUpstreamOutputFailsClosed(t *testing.T) {
	machine := inputsFromFixtureMachine(t)
	byTask := map[string]stubTaskResult{
		"run-1:open-pr": {status: apiv1.ResultSuccess}, // no Outputs["prNumber"] set
		"run-1:ci-poll": {status: apiv1.ResultSuccess},
	}
	r, _ := newTestRunner(t, byTask, gate.NewAutomatedEvaluator())

	_, err := r.Start(context.Background(), StartInput{
		RunID:   "run-1",
		Machine: machine,
		Gaggle:  "acme-web",
		Trigger: journal.Trigger{Kind: journal.TriggerManual},
		RepoRef: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web", Branch: "main"},
	})
	if err == nil {
		t.Fatal("expected an error when a declared InputsFrom output is missing upstream")
	}
}

func TestRunnerAdvancesFixtureWorkflowToCompletion(t *testing.T) {
	machine := fixtureMachine(t)
	byTask := map[string]stubTaskResult{
		"run-1:implement": {
			status: apiv1.ResultSuccess, summary: "wrote a diff",
			artifactName: "diff", artifactData: []byte("--- a\n+++ b\n"), artifactMediaType: "text/x-patch",
		},
	}
	r, runsDir := newTestRunner(t, byTask, gate.NewAutomatedEvaluator())

	res, err := r.Start(context.Background(), StartInput{
		RunID:   "run-1",
		Machine: machine,
		Gaggle:  "acme-web",
		Trigger: journal.Trigger{Kind: journal.TriggerManual},
		RepoRef: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web", Branch: "main"},
		Item:    &apiv1.BacklogItem{ID: "42", Provider: apiv1.ProviderGitHub, Title: "Fix bug"},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if res.Phase != journal.PhaseCompleted {
		t.Fatalf("phase = %q, want completed", res.Phase)
	}
	if res.FinalState != "review" {
		t.Fatalf("finalState = %q, want review", res.FinalState)
	}

	// Journal is complete and consistent: verify via the Reader, not just the
	// in-memory Result — this is what a resume replays.
	rd, err := journal.OpenRead(filepath.Join(runsDir, "run-1"))
	if err != nil {
		t.Fatalf("OpenRead: %v", err)
	}
	id, err := rd.Identity()
	if err != nil {
		t.Fatalf("Identity: %v", err)
	}
	if id.WorkflowDigest != machine.Digest() {
		t.Errorf("run.yaml workflowDigest = %q, want %q (WF-016 pin)", id.WorkflowDigest, machine.Digest())
	}
	if len(id.Inputs) != 2 ||
		id.Inputs[0].Name != "item" ||
		id.Inputs[1].Name != journal.PinnedWorkflowGraphInputName {
		t.Errorf("expected the backlog item and workflow graph snapshotted as immutable inputs, got %+v", id.Inputs)
	}
	graphBytes, err := rd.ArtifactBytes(id.Inputs[1].Ref)
	if err != nil {
		t.Fatalf("read pinned workflow graph: %v", err)
	}
	var pinnedGraph workflow.Graph
	if err := json.Unmarshal(graphBytes, &pinnedGraph); err != nil {
		t.Fatalf("parse pinned workflow graph: %v", err)
	}
	if !reflect.DeepEqual(pinnedGraph, machine.Graph()) {
		t.Errorf("pinned workflow graph = %+v, want %+v", pinnedGraph, machine.Graph())
	}

	events, err := rd.Events()
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	var types []journal.EventType
	for _, e := range events {
		types = append(types, e.Type)
	}
	want := []journal.EventType{
		journal.EventRunStarted,
		journal.EventRefTouched, // the run branch, journaled up front (#133)
		journal.EventStageStarted,
		journal.EventArtifactRecorded, // runner-assembled context manifest
		journal.EventArtifactRecorded,
		journal.EventStageFinished,
		journal.EventGateStarted, // recovery marker, excluded from conformance
		journal.EventGateEvaluated,
		journal.EventRunFinished,
	}
	if !eventTypesEqual(types, want) {
		t.Fatalf("event sequence = %v, want %v", types, want)
	}

	st, err := rd.State()
	if err != nil {
		t.Fatalf("State: %v", err)
	}
	if st.Phase != journal.PhaseCompleted {
		t.Errorf("state.json phase = %q, want completed", st.Phase)
	}
	if st.MachineState != "" {
		t.Errorf("state.json machineState = %q, want empty (terminal)", st.MachineState)
	}

	// The artifact the task produced is readable back through its recorded Ref,
	// digest-verified — the same round-trip a downstream stage would do via
	// ArtifactPointer.Resolve (#10).
	var sawDiff bool
	for _, e := range events {
		if e.Type == journal.EventArtifactRecorded && e.Name == "diff" {
			sawDiff = true
			b, err := rd.ArtifactBytes(*e.Ref)
			if err != nil {
				t.Fatalf("ArtifactBytes: %v", err)
			}
			if string(b) != "--- a\n+++ b\n" {
				t.Errorf("artifact bytes = %q", b)
			}
		}
	}
	if !sawDiff {
		t.Fatal("expected the task's diff artifact")
	}
}

func TestRunnerBranchesToAbortOnGateFail(t *testing.T) {
	machine := fixtureMachine(t)
	byTask := map[string]stubTaskResult{
		"run-2:implement": {status: apiv1.ResultFailure, errorInfo: &apiv1.ErrorInfo{Code: "build_failed", Message: "nope"}},
	}
	r, _ := newTestRunner(t, byTask, gate.NewAutomatedEvaluator())

	res, err := r.Start(context.Background(), StartInput{
		RunID:   "run-2",
		Machine: machine,
		Gaggle:  "acme-web",
		Trigger: journal.Trigger{Kind: journal.TriggerManual},
		RepoRef: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web", Branch: "main"},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if res.Phase != journal.PhaseAborted {
		t.Fatalf("phase = %q, want aborted", res.Phase)
	}
}

// TestRunnerAutomatedGateDoesNotProvisionWorktree is #112's regression: an
// automated gate's checks are pure functions over env.Inputs alone
// (internal/gate/automated.go's DefaultChecks), so unlike an agentic
// reviewer gate it never reads or writes a workspace — provisioning one
// anyway wasted a git clone/checkout on every automated-gate evaluation.
// RepoCloneURL is the first thing buildEnvelope calls to provision a
// worktree, so counting its calls across a one-task-one-automated-gate run
// proves whether the gate skipped worktree provisioning: exactly 1 (the
// task's) means it did; 2 would mean the gate still provisioned one too.
func TestRunnerAutomatedGateDoesNotProvisionWorktree(t *testing.T) {
	machine := fixtureMachine(t) // implement (task) -> review (automated gate) -> pass/fail
	byTask := map[string]stubTaskResult{
		"run-automated-gate-no-worktree:implement": {status: apiv1.ResultSuccess, summary: "done"},
	}
	instanceRoot := t.TempDir()
	wtMgr, err := worktree.NewManager(filepath.Join(instanceRoot, "workcopies"))
	if err != nil {
		t.Fatalf("new worktree manager: %v", err)
	}
	fixtureRepo := newFixtureRepo(t)

	var cloneURLCalls int
	r, err := New(Config{
		NewDeterministic: func(rec ArtifactRecorder, _ SecretRegistrar) (invoke.Deterministic, error) {
			return &stubDeterministic{rec: rec, byTask: byTask}, nil
		},
		Automated: gate.NewAutomatedEvaluator(),
		Worktrees: wtMgr,
		RunsDir:   filepath.Join(instanceRoot, "runs"),
		RepoCloneURL: func(apiv1.RepoRef) (string, error) {
			cloneURLCalls++
			return fixtureRepo, nil
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	res, err := r.Start(context.Background(), StartInput{
		RunID:   "run-automated-gate-no-worktree",
		Machine: machine,
		Gaggle:  "acme-web",
		Trigger: journal.Trigger{Kind: journal.TriggerManual},
		RepoRef: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web", Branch: "main"},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if res.Phase != journal.PhaseCompleted {
		t.Fatalf("phase = %q, want completed", res.Phase)
	}
	if cloneURLCalls != 1 {
		t.Fatalf("RepoCloneURL called %d times, want exactly 1 (the task's worktree only) — the automated gate must not provision one (#112)", cloneURLCalls)
	}
}

// TestRunnerWalksTaskReservedNextTargets is the regression test for #123: a
// task's Next can itself be a reserved terminal target (@abort/@escalate),
// admitted by workflow.Compile the same way a gate's branch is, but before
// this fix walk() only special-cased Next == "" and fell through to
// "unknown state" for anything else — a compile-admitted definition crashing
// the local runner while completing fine on the (quarantined) Temporal
// engine, a direct §3.3 conformance violation.
func TestRunnerWalksTaskReservedNextTargets(t *testing.T) {
	cases := []struct {
		name      string
		next      string
		wantPhase journal.RunPhase
	}{
		{"abort", workflow.TargetAbort, journal.PhaseAborted},
		{"escalate", workflow.TargetEscalate, journal.PhaseEscalated},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			machine := taskReservedNextFixtureMachine(t, tc.next)
			runID := "run-task-next-" + tc.name
			byTask := map[string]stubTaskResult{runID + ":implement": {status: apiv1.ResultSuccess, summary: "done"}}
			r, _ := newTestRunner(t, byTask, gate.NewAutomatedEvaluator())

			res, err := r.Start(context.Background(), StartInput{
				RunID:   runID,
				Machine: machine,
				Gaggle:  "acme-web",
				Trigger: journal.Trigger{Kind: journal.TriggerManual},
				RepoRef: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web", Branch: "main"},
			})
			if err != nil {
				t.Fatalf("Start: %v", err)
			}
			if res.Phase != tc.wantPhase {
				t.Fatalf("phase = %q, want %q", res.Phase, tc.wantPhase)
			}
		})
	}
}

// terminalFailMachine is a single task with no Next (terminal) — the exact
// shape #110's ruling targets: "a terminal task's failure journals the run
// as PhaseCompleted" was the bug (run.go:303-305 pre-fix).
func terminalFailMachine(t *testing.T) *workflow.Machine {
	t.Helper()
	spec := apiv1.WorkflowSpec{
		Gaggle:   "acme-web",
		Triggers: []apiv1.Trigger{{Type: apiv1.TriggerBacklogItem}},
		Start:    "implement",
		Tasks: []apiv1.Task{
			{Name: "implement", Type: apiv1.TaskDeterministic, Goal: "produce a diff", Run: &apiv1.DeterministicRun{Command: []string{"true"}}},
		},
	}
	m, err := workflow.Compile(workflow.Definition{Name: "terminal-fail", Version: 1, Spec: spec})
	if err != nil {
		t.Fatalf("compile terminal-fail machine: %v", err)
	}
	return m
}

// chainedNoGateMachine is two tasks back to back with no gate between them —
// "implement" feeds directly into "deploy", never through a gate that could
// branch on a business failure.
func chainedNoGateMachine(t *testing.T) *workflow.Machine {
	t.Helper()
	spec := apiv1.WorkflowSpec{
		Gaggle:   "acme-web",
		Triggers: []apiv1.Trigger{{Type: apiv1.TriggerBacklogItem}},
		Start:    "implement",
		Tasks: []apiv1.Task{
			{Name: "implement", Type: apiv1.TaskDeterministic, Goal: "produce a diff", Run: &apiv1.DeterministicRun{Command: []string{"true"}}, Next: "deploy"},
			{Name: "deploy", Type: apiv1.TaskDeterministic, Goal: "ship it", Run: &apiv1.DeterministicRun{Command: []string{"true"}}},
		},
	}
	m, err := workflow.Compile(workflow.Definition{Name: "chained-no-gate", Version: 1, Spec: spec})
	if err != nil {
		t.Fatalf("compile chained-no-gate machine: %v", err)
	}
	return m
}

// TestRunnerTaskFailureWithNoGateNextFailsRun proves the #110 ruling's core
// fix: a terminal task's business "failure" must journal PhaseFailed, not
// PhaseCompleted — the exact defect the architect review caught (every
// shipped V0 workflow has no gate after its first stage, so every broken run
// used to journal as completed, fail-open).
func TestRunnerTaskFailureWithNoGateNextFailsRun(t *testing.T) {
	machine := terminalFailMachine(t)
	byTask := map[string]stubTaskResult{
		"run-fail-terminal:implement": {status: apiv1.ResultFailure, errorInfo: &apiv1.ErrorInfo{Code: "build_failed", Message: "nope"}},
	}
	r, runsDir := newTestRunner(t, byTask, nil)

	res, err := r.Start(context.Background(), StartInput{
		RunID:   "run-fail-terminal",
		Machine: machine,
		Gaggle:  "acme-web",
		Trigger: journal.Trigger{Kind: journal.TriggerManual},
		RepoRef: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web", Branch: "main"},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if res.Phase != journal.PhaseFailed {
		t.Fatalf("phase = %q, want failed (a terminal task's failure must never journal as completed)", res.Phase)
	}

	rd, err := journal.OpenRead(filepath.Join(runsDir, "run-fail-terminal"))
	if err != nil {
		t.Fatalf("OpenRead: %v", err)
	}
	st, err := rd.State()
	if err != nil {
		t.Fatalf("State: %v", err)
	}
	if st.Phase != journal.PhaseFailed {
		t.Fatalf("state.json phase = %q, want failed", st.Phase)
	}
	events, err := rd.Events()
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	last := events[len(events)-1]
	if last.Type != journal.EventRunFinished || last.Status != string(journal.PhaseFailed) {
		t.Fatalf("last event = %+v, want run.finished status=failed", last)
	}
}

// gateAbsorbedCompleteMachine is implement -> a status-equals gate whose "fail"
// branch terminates-complete (""), the exact shape of merge-review's merge-gate
// (land-outcome "fail" -> ""). params.equals is "blocked" — never matched by an
// ordinary success/failure status — so BOTH a succeeding and a failing feeding
// stage route through the SAME "fail" outcome to complete, mirroring merge-pr:
// its exit-0 refusal (ResultSuccess, no landOutcome) and its error (#003,
// ResultFailure) both land on merge-gate's "fail" -> "". That isolates the one
// variable the fix keys on — the feeding stage's status — while holding the
// gate outcome ("fail") constant, so the run status must diverge on the stage
// status alone, not on the branch taken.
func gateAbsorbedCompleteMachine(t *testing.T) *workflow.Machine {
	t.Helper()
	spec := apiv1.WorkflowSpec{
		Gaggle:   "acme-web",
		Triggers: []apiv1.Trigger{{Type: apiv1.TriggerBacklogItem}},
		Start:    "implement",
		Tasks: []apiv1.Task{
			{Name: "implement", Type: apiv1.TaskDeterministic, Goal: "produce a diff", Run: &apiv1.DeterministicRun{Command: []string{"true"}}, Next: "review"},
		},
		Gates: []apiv1.Gate{
			{
				Name:      "review",
				Evaluator: apiv1.EvaluatorAutomated,
				Automated: &apiv1.AutomatedGate{Check: "status-equals", Params: map[string]string{"equals": "blocked"}},
				Branches: map[string]string{
					"pass": workflow.TargetAbort,
					"fail": workflow.TerminalComplete,
				},
			},
		},
	}
	m, err := workflow.Compile(workflow.Definition{Name: "gate-absorbed-complete", Version: 1, Spec: spec})
	if err != nil {
		t.Fatalf("compile gate-absorbed-complete machine: %v", err)
	}
	return m
}

// TestRunnerGateAbsorbedStageFailureFailsRun is #849's regression: a stage that
// reports ResultFailure and is then routed by its gate to a terminal-complete
// branch must journal PhaseFailed, not PhaseCompleted. This is the exact shape
// that hid merge-review's merge-pr blocker for four rounds — merge-pr errored
// 23/23 times, its gate routed the failure to "", and every run reported
// `completed`, so health metrics read green on a 100%-failing stage.
func TestRunnerGateAbsorbedStageFailureFailsRun(t *testing.T) {
	machine := gateAbsorbedCompleteMachine(t)
	byTask := map[string]stubTaskResult{
		"run-849:implement": {status: apiv1.ResultFailure, errorInfo: &apiv1.ErrorInfo{Code: "merge_failed", Message: "no current pass verdict found in pull request comments"}},
	}
	r, runsDir := newTestRunner(t, byTask, gate.NewAutomatedEvaluator())

	res, err := r.Start(context.Background(), StartInput{
		RunID:   "run-849",
		Machine: machine,
		Gaggle:  "acme-web",
		Trigger: journal.Trigger{Kind: journal.TriggerManual},
		RepoRef: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web", Branch: "main"},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if res.Phase != journal.PhaseFailed {
		t.Fatalf("phase = %q, want failed (a gate must not launder a stage failure into a completed run)", res.Phase)
	}
	if res.FailureStage != "implement" || res.FailureCode != "merge_failed" {
		t.Fatalf("failure attribution = {stage:%q code:%q}, want {implement merge_failed} threaded from the failed stage", res.FailureStage, res.FailureCode)
	}

	rd, err := journal.OpenRead(filepath.Join(runsDir, "run-849"))
	if err != nil {
		t.Fatalf("OpenRead: %v", err)
	}
	st, err := rd.State()
	if err != nil {
		t.Fatalf("State: %v", err)
	}
	if st.Phase != journal.PhaseFailed {
		t.Fatalf("state.json phase = %q, want failed", st.Phase)
	}
}

// TestRunnerGateRoutedSuccessToCompleteStaysCompleted guards the other side of
// #849: a stage that SUCCEEDS but whose gate routes it to a terminal-complete
// branch must stay PhaseCompleted. This is the legitimate-refusal path — a
// designed negative outcome (merge-pr's exit-0 `reasons` refusal, ci-poll's
// status output) is a ResultSuccess, and the fix must not mislabel it as a
// failed run just because the gate routed it to "".
func TestRunnerGateRoutedSuccessToCompleteStaysCompleted(t *testing.T) {
	machine := gateAbsorbedCompleteMachine(t)
	byTask := map[string]stubTaskResult{
		"run-849-ok:implement": {status: apiv1.ResultSuccess, summary: "reviewed, correctly declined to merge"},
	}
	r, _ := newTestRunner(t, byTask, gate.NewAutomatedEvaluator())

	res, err := r.Start(context.Background(), StartInput{
		RunID:   "run-849-ok",
		Machine: machine,
		Gaggle:  "acme-web",
		Trigger: journal.Trigger{Kind: journal.TriggerManual},
		RepoRef: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web", Branch: "main"},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if res.Phase != journal.PhaseCompleted {
		t.Fatalf("phase = %q, want completed (a successful stage routed to a completing branch is a designed outcome, not a failure)", res.Phase)
	}
}

// TestRunnerResultCarriesFailureCauseAtFirstStage is issue #710's core
// acceptance: a business failure at the run's very first (and only) stage
// threads the stage's own errorCode/message onto the returned Result and
// appends a run_failed cause event naming the failing stage — #705's root
// cause (a rate-limited pr-select, structurally identical to this fixture)
// was recorded on stage.finished the whole time; this is what was missing to
// see it at the run's own terminal event.
func TestRunnerResultCarriesFailureCauseAtFirstStage(t *testing.T) {
	machine := terminalFailMachine(t)
	byTask := map[string]stubTaskResult{
		"run-cause-first:implement": {status: apiv1.ResultFailure, errorInfo: &apiv1.ErrorInfo{Code: "github_rate_limited", Message: "list pull requests: status 403, remaining 0"}},
	}
	r, runsDir := newTestRunner(t, byTask, nil)

	res, err := r.Start(context.Background(), StartInput{
		RunID:   "run-cause-first",
		Machine: machine,
		Gaggle:  "acme-web",
		Trigger: journal.Trigger{Kind: journal.TriggerManual},
		RepoRef: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web", Branch: "main"},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if res.Phase != journal.PhaseFailed {
		t.Fatalf("phase = %q, want failed", res.Phase)
	}
	if res.FailureStage != "implement" || res.FailureCode != "github_rate_limited" || res.FailureMessage != "list pull requests: status 403, remaining 0" {
		t.Fatalf("failure cause = stage=%q code=%q message=%q, want implement/github_rate_limited/the stage message",
			res.FailureStage, res.FailureCode, res.FailureMessage)
	}

	rd, err := journal.OpenRead(filepath.Join(runsDir, "run-cause-first"))
	if err != nil {
		t.Fatalf("OpenRead: %v", err)
	}
	events, err := rd.Events()
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	var sawCause bool
	for _, e := range events {
		if e.Type == journal.EventError && e.Stage == "implement" && e.Error != nil && e.Error.Code == "run_failed" {
			sawCause = true
			if !strings.Contains(e.Error.Message, "github_rate_limited") {
				t.Errorf("run_failed message = %q, want it to contain the stage's own code", e.Error.Message)
			}
		}
	}
	if !sawCause {
		t.Fatal("expected a run_failed error event naming stage \"implement\" (#305 pattern)")
	}
}

// TestRunnerResultCarriesFailureCauseAtLastStage covers the AC's second shape
// (a close-out/post-merge-like LATER stage in a chain, not the first): the
// first stage succeeds, the second — a stand-in for a real workflow's later
// deterministic stage (post-merge, close-out) — fails. FailureStage must name
// the actual failing stage ("deploy"), not the run's first stage.
func TestRunnerResultCarriesFailureCauseAtLastStage(t *testing.T) {
	machine := chainedNoGateMachine(t)
	byTask := map[string]stubTaskResult{
		"run-cause-last:implement": {status: apiv1.ResultSuccess},
		"run-cause-last:deploy":    {status: apiv1.ResultFailure, errorInfo: &apiv1.ErrorInfo{Code: "merge_conflict", Message: "post-merge rebase failed"}},
	}
	r, runsDir := newTestRunner(t, byTask, nil)

	res, err := r.Start(context.Background(), StartInput{
		RunID:   "run-cause-last",
		Machine: machine,
		Gaggle:  "acme-web",
		Trigger: journal.Trigger{Kind: journal.TriggerManual},
		RepoRef: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web", Branch: "main"},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if res.Phase != journal.PhaseFailed {
		t.Fatalf("phase = %q, want failed", res.Phase)
	}
	if res.FailureStage != "deploy" || res.FailureCode != "merge_conflict" {
		t.Fatalf("failure cause = stage=%q code=%q, want deploy/merge_conflict", res.FailureStage, res.FailureCode)
	}

	rd, err := journal.OpenRead(filepath.Join(runsDir, "run-cause-last"))
	if err != nil {
		t.Fatalf("OpenRead: %v", err)
	}
	events, err := rd.Events()
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	for _, e := range events {
		if e.Type == journal.EventError && e.Error != nil && e.Error.Code == "run_failed" && e.Stage != "deploy" {
			t.Fatalf("run_failed cause attributed to stage %q, want deploy", e.Stage)
		}
	}
}

// TestRunnerResultOmitsFailureCauseForNonRetryableEscalation proves the
// non-retryable-escalate disposition (#415) — a DIFFERENT PhaseEscalated
// terminal that also derives from ResultFailure — does not populate
// FailureStage/Code/Message: those fields are reserved for a genuine
// PhaseFailed terminal (issue #710's scope), and an escalated run already has
// its own distinct visibility path (the escalation notifier's provider
// comment).
func TestRunnerResultOmitsFailureCauseForNonRetryableEscalation(t *testing.T) {
	machine := fixtureMachine(t)
	byTask := map[string]stubTaskResult{
		"run-no-cause-escalate:implement": {
			status:    apiv1.ResultFailure,
			errorInfo: &apiv1.ErrorInfo{Code: "ISSUE_OVER_SCOPE", Message: "bundles independent changes", Retryable: false},
		},
	}
	r, _ := newTestRunner(t, byTask, nil)

	res, err := r.Start(context.Background(), StartInput{
		RunID:   "run-no-cause-escalate",
		Machine: machine,
		Gaggle:  "acme-web",
		Trigger: journal.Trigger{Kind: journal.TriggerManual},
		RepoRef: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web", Branch: "main"},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if res.Phase != journal.PhaseEscalated {
		t.Fatalf("phase = %q, want escalated", res.Phase)
	}
	if res.FailureStage != "" || res.FailureCode != "" || res.FailureMessage != "" {
		t.Fatalf("failure cause = stage=%q code=%q message=%q, want all empty (escalated, not failed)",
			res.FailureStage, res.FailureCode, res.FailureMessage)
	}
}

// TestFailureCauseFrom pins the (code, message) extraction table: a present
// code+message combines them (matching blockedReason's code-prefixed
// convention), a present message with no code returns it bare, and a missing/
// empty ErrorInfo falls back to a fixed marker rather than an empty string.
func TestFailureCauseFrom(t *testing.T) {
	cases := []struct {
		name        string
		err         *apiv1.ErrorInfo
		wantCode    string
		wantMessage string
	}{
		{"nil", nil, "", "stage reported failure with no error detail"},
		{"empty message", &apiv1.ErrorInfo{Code: "x"}, "", "stage reported failure with no error detail"},
		{"message no code", &apiv1.ErrorInfo{Message: "boom"}, "", "boom"},
		{"code and message", &apiv1.ErrorInfo{Code: "github_rate_limited", Message: "403"}, "github_rate_limited", "403"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, message := failureCauseFrom(tc.err)
			if code != tc.wantCode || message != tc.wantMessage {
				t.Fatalf("failureCauseFrom(%+v) = (%q, %q), want (%q, %q)", tc.err, code, message, tc.wantCode, tc.wantMessage)
			}
		})
	}
}

// TestBoundFailureMessage pins the truncation bound (issue #710: "a bounded
// message") — a short message passes through untouched; an oversized one is
// cut to exactly maxFailureMessageLen runes plus the truncation marker, never
// silently dropped or left unbounded.
func TestBoundFailureMessage(t *testing.T) {
	short := "list pull requests: status 403"
	if got := boundFailureMessage(short); got != short {
		t.Fatalf("boundFailureMessage(short) = %q, want unchanged", got)
	}
	long := strings.Repeat("x", maxFailureMessageLen+250)
	got := boundFailureMessage(long)
	if !strings.HasSuffix(got, "...(truncated)") {
		t.Fatalf("boundFailureMessage(long) = %q, want a truncation marker suffix", got)
	}
	if got == long {
		t.Fatal("boundFailureMessage(long) returned the message unchanged, want it truncated")
	}
}

// TestRunnerTaskFailureWithNonGateNextFailsRunAndSkipsDownstream proves a
// business failure whose Next names another task (not a gate) never
// dispatches that downstream task — "never run downstream stages on
// garbage" (ruling #110) — instead of the pre-fix behavior of advancing
// unconditionally regardless of status.
func TestRunnerTaskFailureWithNonGateNextFailsRunAndSkipsDownstream(t *testing.T) {
	machine := chainedNoGateMachine(t)
	byTask := map[string]stubTaskResult{
		"run-fail-chain:implement": {status: apiv1.ResultFailure, errorInfo: &apiv1.ErrorInfo{Code: "build_failed", Message: "nope"}},
		// Deliberately no canned result for "deploy" — if the runner ever
		// dispatches it, stubDeterministic.Run errors ("no canned output"),
		// which would surface as a different failure than PhaseFailed and
		// fail this test's phase assertion below.
	}
	r, runsDir := newTestRunner(t, byTask, nil)

	res, err := r.Start(context.Background(), StartInput{
		RunID:   "run-fail-chain",
		Machine: machine,
		Gaggle:  "acme-web",
		Trigger: journal.Trigger{Kind: journal.TriggerManual},
		RepoRef: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web", Branch: "main"},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if res.Phase != journal.PhaseFailed {
		t.Fatalf("phase = %q, want failed", res.Phase)
	}
	if res.FinalState != "implement" {
		t.Fatalf("finalState = %q, want implement (the run must stop there, never reach deploy)", res.FinalState)
	}

	rd, err := journal.OpenRead(filepath.Join(runsDir, "run-fail-chain"))
	if err != nil {
		t.Fatalf("OpenRead: %v", err)
	}
	events, err := rd.Events()
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	for _, e := range events {
		if e.Stage == "deploy" {
			t.Fatalf("deploy must never be dispatched after implement's failure, got event %+v", e)
		}
	}
}

// TestRunnerTaskFailureWithGateNextStillBranches proves the ruling's
// preserved case: a business failure whose Next is a gate still advances so
// the gate can branch on it (the shipped reviewer-gate pattern) — the same
// path TestRunnerBranchesToAbortOnGateFail exercises end to end, asserted
// here directly against the gate.evaluated event so a regression that
// terminalizes on ANY failure (over-correcting #110) is caught too.
func TestRunnerTaskFailureWithGateNextStillBranches(t *testing.T) {
	machine := fixtureMachine(t)
	byTask := map[string]stubTaskResult{
		"run-fail-gate:implement": {status: apiv1.ResultFailure, errorInfo: &apiv1.ErrorInfo{Code: "build_failed", Message: "nope"}},
	}
	r, runsDir := newTestRunner(t, byTask, gate.NewAutomatedEvaluator())

	res, err := r.Start(context.Background(), StartInput{
		RunID:   "run-fail-gate",
		Machine: machine,
		Gaggle:  "acme-web",
		Trigger: journal.Trigger{Kind: journal.TriggerManual},
		RepoRef: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web", Branch: "main"},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if res.Phase != journal.PhaseAborted {
		t.Fatalf("phase = %q, want aborted (the gate branches fail->@abort)", res.Phase)
	}

	rd, err := journal.OpenRead(filepath.Join(runsDir, "run-fail-gate"))
	if err != nil {
		t.Fatalf("OpenRead: %v", err)
	}
	events, err := rd.Events()
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	var sawGateEval bool
	for _, e := range events {
		if e.Type == journal.EventGateEvaluated {
			sawGateEval = true
			if e.Verdict != "fail" {
				t.Errorf("gate verdict = %q, want fail", e.Verdict)
			}
		}
	}
	if !sawGateEval {
		t.Fatal("expected the review gate to evaluate the failure, got no gate.evaluated event")
	}
}

// TestRunnerTaskBlockedFinishesEscalated proves the #544 ruling: a "blocked"
// business status ends the run at a canonical terminal phase — escalated —
// with the cause journaled (blocked_by_agent), the Blocked handler invoked
// (with the blockers parsed from the documented outputs.blockedBy scalar
// convention) BEFORE FinalizeTerminal releases the run's claims, and the Next
// gate never evaluated against a blocked result. Pre-fix, this run hung at
// PhaseRunning forever holding its claim for the full lease (#545's 6 live
// occurrences).
func TestRunnerTaskBlockedFinishesEscalated(t *testing.T) {
	machine := fixtureMachine(t)
	byTask := map[string]stubTaskResult{
		"run-blocked:implement": {
			status:    apiv1.ResultBlocked,
			errorInfo: &apiv1.ErrorInfo{Code: "DEPENDENCY_NOT_MET", Message: "issues #441 and #442 must merge first"},
			outputs:   map[string]interface{}{OutputBlockedBy: "#441, 442,441"},
		},
	}
	r, runsDir := newTestRunner(t, byTask, gate.NewAutomatedEvaluator())

	var order []string
	var got BlockedOutcome
	r.cfg.Blocked = func(_ context.Context, o BlockedOutcome) error {
		order = append(order, "blocked")
		got = o
		return nil
	}
	r.cfg.FinalizeTerminal = func(runID string, phase journal.RunPhase) error {
		order = append(order, "finalize")
		if runID != "run-blocked" || phase != journal.PhaseEscalated {
			t.Errorf("FinalizeTerminal got (%q, %q), want (run-blocked, escalated)", runID, phase)
		}
		return nil
	}

	res, err := r.Start(context.Background(), StartInput{
		RunID:   "run-blocked",
		Machine: machine,
		Gaggle:  "acme-web",
		Trigger: journal.Trigger{Kind: journal.TriggerManual},
		RepoRef: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web", Branch: "main"},
		Item:    &apiv1.BacklogItem{ID: "510", Provider: apiv1.ProviderGitHub},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if res.Phase != journal.PhaseEscalated {
		t.Fatalf("phase = %q, want escalated (canonical terminal, #544)", res.Phase)
	}
	if res.FinalState != "implement" {
		t.Fatalf("finalState = %q, want implement", res.FinalState)
	}

	if len(order) != 2 || order[0] != "blocked" || order[1] != "finalize" {
		t.Fatalf("hook order = %v, want [blocked finalize] (handler must see the claims before release)", order)
	}
	want := BlockedOutcome{
		RunID:    "run-blocked",
		Stage:    "implement",
		ItemID:   "510",
		Reason:   "DEPENDENCY_NOT_MET: issues #441 and #442 must merge first",
		Blockers: []string{"441", "442"},
	}
	if got.RunID != want.RunID || got.Stage != want.Stage || got.ItemID != want.ItemID || got.Reason != want.Reason {
		t.Fatalf("BlockedOutcome = %+v, want %+v", got, want)
	}
	if len(got.Blockers) != 2 || got.Blockers[0] != "441" || got.Blockers[1] != "442" {
		t.Fatalf("Blockers = %v, want [441 442] (parsed, deduped, in order)", got.Blockers)
	}

	rd, err := journal.OpenRead(filepath.Join(runsDir, "run-blocked"))
	if err != nil {
		t.Fatalf("OpenRead: %v", err)
	}
	phase, err := rd.Phase()
	if err != nil {
		t.Fatalf("Phase: %v", err)
	}
	if phase != journal.PhaseEscalated {
		t.Fatalf("journaled phase = %q, want escalated (run.finished must be durable)", phase)
	}
	events, err := rd.Events()
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	var sawCause bool
	for _, e := range events {
		if e.Type == journal.EventGateEvaluated {
			t.Fatalf("review must not be evaluated against a blocked result, got gate.evaluated event %+v", e)
		}
		if e.Type == journal.EventError && e.Error != nil && e.Error.Code == "blocked_by_agent" {
			sawCause = true
			if e.Error.Message != want.Reason {
				t.Errorf("blocked_by_agent message = %q, want %q", e.Error.Message, want.Reason)
			}
		}
	}
	if !sawCause {
		t.Fatal("expected a blocked_by_agent error event recording the cause (#305 pattern)")
	}
}

// TestRunnerTaskBlockedHandlerErrorStillTerminal proves the Blocked handler is
// best-effort: its failure is journaled (blocked_handling_failed) but the run
// still reaches escalated — the terminal-cleanup guarantee (I1) must not
// depend on an instance-level notification succeeding.
func TestRunnerTaskBlockedHandlerErrorStillTerminal(t *testing.T) {
	machine := fixtureMachine(t)
	byTask := map[string]stubTaskResult{
		"run-blocked-herr:implement": {status: apiv1.ResultBlocked, summary: "waiting on an external dependency"},
	}
	r, runsDir := newTestRunner(t, byTask, gate.NewAutomatedEvaluator())
	r.cfg.Blocked = func(context.Context, BlockedOutcome) error {
		return errors.New("provider unreachable")
	}

	res, err := r.Start(context.Background(), StartInput{
		RunID:   "run-blocked-herr",
		Machine: machine,
		Gaggle:  "acme-web",
		Trigger: journal.Trigger{Kind: journal.TriggerManual},
		RepoRef: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web", Branch: "main"},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if res.Phase != journal.PhaseEscalated {
		t.Fatalf("phase = %q, want escalated despite handler failure", res.Phase)
	}

	rd, err := journal.OpenRead(filepath.Join(runsDir, "run-blocked-herr"))
	if err != nil {
		t.Fatalf("OpenRead: %v", err)
	}
	events, err := rd.Events()
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	var sawHandlerFailure, sawCause bool
	for _, e := range events {
		if e.Type != journal.EventError || e.Error == nil {
			continue
		}
		switch e.Error.Code {
		case "blocked_handling_failed":
			sawHandlerFailure = true
		case "blocked_by_agent":
			sawCause = true
			if e.Error.Message != "waiting on an external dependency" {
				t.Errorf("blocked_by_agent message = %q, want the summary fallback", e.Error.Message)
			}
		}
	}
	if !sawHandlerFailure {
		t.Fatal("expected a blocked_handling_failed error event for the failed handler")
	}
	if !sawCause {
		t.Fatal("expected the blocked_by_agent cause event")
	}
}

// TestRunnerTaskBlockedWithoutItemPassesEmptyItemID proves a run with no
// StartInput.Item (schedule/backlog-item-triggered implementation runs claim
// their item mid-run) still invokes the Blocked handler, with an empty ItemID
// — the composition-root handler resolves the item from the claim ledger by
// RunID in that case.
func TestRunnerTaskBlockedWithoutItemPassesEmptyItemID(t *testing.T) {
	machine := fixtureMachine(t)
	byTask := map[string]stubTaskResult{
		"run-blocked-noitem:implement": {status: apiv1.ResultBlocked, summary: "blocked"},
	}
	r, _ := newTestRunner(t, byTask, gate.NewAutomatedEvaluator())
	var got *BlockedOutcome
	r.cfg.Blocked = func(_ context.Context, o BlockedOutcome) error {
		got = &o
		return nil
	}

	res, err := r.Start(context.Background(), StartInput{
		RunID:   "run-blocked-noitem",
		Machine: machine,
		Gaggle:  "acme-web",
		Trigger: journal.Trigger{Kind: journal.TriggerSchedule},
		RepoRef: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web", Branch: "main"},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if res.Phase != journal.PhaseEscalated {
		t.Fatalf("phase = %q, want escalated", res.Phase)
	}
	if got == nil {
		t.Fatal("Blocked handler was not invoked")
	}
	if got.ItemID != "" {
		t.Fatalf("ItemID = %q, want empty (no StartInput.Item)", got.ItemID)
	}
}

// TestParseBlockedBy pins the documented outputs.blockedBy convention: lenient
// on separators/#-prefixes/whitespace and a bare JSON number, strict on
// content (digit-only tokens), deduplicated in first-seen order.
func TestParseBlockedBy(t *testing.T) {
	cases := []struct {
		name    string
		outputs map[string]interface{}
		want    []string
	}{
		{"absent key", map[string]interface{}{"other": "441"}, nil},
		{"nil outputs", nil, nil},
		{"plain csv", map[string]interface{}{OutputBlockedBy: "441,442"}, []string{"441", "442"}},
		{"hashes and spaces", map[string]interface{}{OutputBlockedBy: " #441 , #442 ; 443"}, []string{"441", "442", "443"}},
		{"dedup preserves order", map[string]interface{}{OutputBlockedBy: "442,441,442"}, []string{"442", "441"}},
		{"json number", map[string]interface{}{OutputBlockedBy: float64(441)}, []string{"441"}},
		{"non-numeric tokens dropped", map[string]interface{}{OutputBlockedBy: "441, the dashboard epic, GH-442"}, []string{"441"}},
		{"garbage only", map[string]interface{}{OutputBlockedBy: "soon(tm)"}, nil},
		{"boolean value", map[string]interface{}{OutputBlockedBy: true}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := parseBlockedBy(tc.outputs)
			if len(got) != len(tc.want) {
				t.Fatalf("parseBlockedBy = %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("parseBlockedBy = %v, want %v", got, tc.want)
				}
			}
		})
	}
}

// TestRunnerMaxStepsExceededFailsRunClosed proves a runaway machine's
// max-steps abort journals PhaseFailed instead of leaving the run stuck at
// phase=running forever — the daemon auto-resumes every PhaseRunning run on
// restart (cmd/goobers/daemon.go), so an unterminated runaway run would
// re-loop and re-fail identically on every restart (ruling #110 item 6: every
// error return out of walk must first append a terminal event).
func TestRunnerMaxStepsExceededFailsRunClosed(t *testing.T) {
	// compile.go's reachability check requires every state have SOME path to
	// a terminal, so a bare task->task self-loop is rejected at compile time
	// (tasks have only one Next, no branching). A gate's "pass" branch gives
	// the machine a statically reachable terminal; alwaysFailAutomated below
	// makes the gate never actually take it at runtime, so "loop"->"check"
	// cycles forever — the genuine runaway-machine shape maxSteps guards.
	spec := apiv1.WorkflowSpec{
		Gaggle:   "acme-web",
		Triggers: []apiv1.Trigger{{Type: apiv1.TriggerBacklogItem}},
		Start:    "loop",
		Tasks: []apiv1.Task{
			{Name: "loop", Type: apiv1.TaskDeterministic, Goal: "spin", Run: &apiv1.DeterministicRun{Command: []string{"true"}}, Next: "check"},
		},
		Gates: []apiv1.Gate{
			{
				Name:      "check",
				Evaluator: apiv1.EvaluatorAutomated,
				Automated: &apiv1.AutomatedGate{Check: "status-equals"},
				Branches:  map[string]string{"pass": workflow.TerminalComplete, "fail": "loop"},
			},
		},
	}
	machine, err := workflow.Compile(workflow.Definition{Name: "runaway", Version: 1, Spec: spec})
	if err != nil {
		t.Fatalf("compile: %v", err)
	}

	instanceRoot := t.TempDir()
	wtMgr, err := worktree.NewManager(filepath.Join(instanceRoot, "workcopies"))
	if err != nil {
		t.Fatalf("new worktree manager: %v", err)
	}
	runsDir := filepath.Join(instanceRoot, "runs")
	fixtureRepo := newFixtureRepo(t)

	det := &stubDeterministic{byTask: map[string]stubTaskResult{
		"run-runaway:loop": {status: apiv1.ResultSuccess},
	}}

	r, err := New(Config{
		NewDeterministic: func(ArtifactRecorder, SecretRegistrar) (invoke.Deterministic, error) { return det, nil },
		Automated:        alwaysFailAutomated{},
		Worktrees:        wtMgr,
		RunsDir:          runsDir,
		MaxSteps:         3,
		RepoCloneURL:     func(apiv1.RepoRef) (string, error) { return fixtureRepo, nil },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	_, err = r.Start(context.Background(), StartInput{
		RunID:   "run-runaway",
		Machine: machine,
		Gaggle:  "acme-web",
		Trigger: journal.Trigger{Kind: journal.TriggerManual},
		RepoRef: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web", Branch: "main"},
	})
	if err == nil {
		t.Fatal("Start: want an error after exceeding max steps")
	}

	rd, rerr := journal.OpenRead(filepath.Join(runsDir, "run-runaway"))
	if rerr != nil {
		t.Fatalf("OpenRead: %v", rerr)
	}
	st, serr := rd.State()
	if serr != nil {
		t.Fatalf("State: %v", serr)
	}
	if st.Phase != journal.PhaseFailed {
		t.Fatalf("state.json phase = %q, want failed — the run must not be left at phase=running for the daemon to re-resume and re-loop forever", st.Phase)
	}
	events, eerr := rd.Events()
	if eerr != nil {
		t.Fatalf("Events: %v", eerr)
	}
	last := events[len(events)-1]
	if last.Type != journal.EventRunFinished || last.Status != string(journal.PhaseFailed) {
		t.Fatalf("last event = %+v, want run.finished status=failed", last)
	}
}

func TestRunnerRejectsHumanGateBeforeStarting(t *testing.T) {
	spec := apiv1.WorkflowSpec{
		Gaggle:   "acme-web",
		Triggers: []apiv1.Trigger{{Type: apiv1.TriggerBacklogItem}},
		Start:    "approve",
		Gates: []apiv1.Gate{
			{
				Name:      "approve",
				Evaluator: apiv1.EvaluatorHuman,
				Human:     &apiv1.HumanGate{},
				Branches:  map[string]string{"approved": workflow.TerminalComplete, "rejected": workflow.TargetAbort},
			},
		},
	}
	_, err := workflow.Compile(workflow.Definition{Name: "human-gate", Version: 1, Spec: spec})
	const want = "human gates ship with durable pause/resume (#168/#465); until then use an automated gate or remove this block"
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("compile error = %v, want actionable rejection before runner start", err)
	}
}

// TestRunnerRetriesTaskPerPolicy proves Task.Retry's declared policy governs
// dispatch-level (executor Go-error) failures: the task fails twice then
// succeeds on its 3rd attempt, the run still completes, and every attempt is
// journaled distinctly with the first carrying no AttemptClass and the
// retries carrying "policy".
func TestRunnerRetriesTaskPerPolicy(t *testing.T) {
	machine := retryFixtureMachine(t, 3)
	flaky := &flakyDeterministic{failUntil: 2}
	r, runsDir := newTestRunnerWithDeterministic(t, func(ArtifactRecorder, SecretRegistrar) (invoke.Deterministic, error) {
		return flaky, nil
	}, gate.NewAutomatedEvaluator())

	res, err := r.Start(context.Background(), StartInput{
		RunID:   "run-retry",
		Machine: machine,
		Gaggle:  "acme-web",
		Trigger: journal.Trigger{Kind: journal.TriggerManual},
		RepoRef: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web", Branch: "main"},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if res.Phase != journal.PhaseCompleted {
		t.Fatalf("phase = %q, want completed", res.Phase)
	}
	if flaky.calls != 3 {
		t.Fatalf("executor called %d times, want 3 (2 failures + 1 success)", flaky.calls)
	}

	rd, err := journal.OpenRead(filepath.Join(runsDir, "run-retry"))
	if err != nil {
		t.Fatalf("OpenRead: %v", err)
	}
	events, err := rd.Events()
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	var starts []journal.Event
	for _, e := range events {
		if e.Type == journal.EventStageStarted {
			starts = append(starts, e)
		}
	}
	if len(starts) != 3 {
		t.Fatalf("stage.started events = %d, want 3, got %+v", len(starts), starts)
	}
	wantClasses := []journal.AttemptClass{"", journal.AttemptPolicy, journal.AttemptPolicy}
	for i, e := range starts {
		if e.Attempt != i+1 {
			t.Errorf("start[%d].Attempt = %d, want %d", i, e.Attempt, i+1)
		}
		if e.AttemptClass != wantClasses[i] {
			t.Errorf("start[%d].AttemptClass = %q, want %q", i, e.AttemptClass, wantClasses[i])
		}
	}
}

func TestRunnerRetriesInfrastructureFailureAndRecovers(t *testing.T) {
	machine := retryFixtureMachine(t, 1)
	cause := errors.New("status 503: provider unavailable")
	flaky := &infrastructureFlakyDeterministic{failUntil: 1, cause: cause}
	r, runsDir := newTestRunnerWithDeterministic(t, func(ArtifactRecorder, SecretRegistrar) (invoke.Deterministic, error) {
		return flaky, nil
	}, gate.NewAutomatedEvaluator())

	res, err := r.Start(context.Background(), StartInput{
		RunID:   "run-infra-recover",
		Machine: machine,
		Gaggle:  "acme-web",
		Trigger: journal.Trigger{Kind: journal.TriggerManual},
		RepoRef: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web", Branch: "main"},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if res.Phase != journal.PhaseCompleted || flaky.calls != 2 {
		t.Fatalf("result=%+v calls=%d, want completed after 2 attempts", res, flaky.calls)
	}

	rd, err := journal.OpenRead(filepath.Join(runsDir, "run-infra-recover"))
	if err != nil {
		t.Fatal(err)
	}
	events, err := rd.Events()
	if err != nil {
		t.Fatal(err)
	}
	var starts []journal.Event
	for _, event := range events {
		if event.Type == journal.EventStageStarted {
			starts = append(starts, event)
		}
	}
	if len(starts) != 2 {
		t.Fatalf("stage.started events = %d, want 2: %+v", len(starts), starts)
	}
	if starts[0].AttemptClass != "" || starts[1].AttemptClass != journal.AttemptInfra {
		t.Fatalf("attempt classes = [%q %q], want [empty infra]", starts[0].AttemptClass, starts[1].AttemptClass)
	}
}

func TestRunnerInfrastructureRetryIsIndependentOfPolicyAttempts(t *testing.T) {
	machine := retryFixtureMachine(t, 3)
	deterministic := &sequencedDeterministic{failures: []error{
		errors.New("policy dispatch failure"),
		invoke.InfrastructureFailure(errors.New("status 503: provider unavailable")),
	}}
	r, runsDir := newTestRunnerWithDeterministic(t, func(ArtifactRecorder, SecretRegistrar) (invoke.Deterministic, error) {
		return deterministic, nil
	}, gate.NewAutomatedEvaluator())

	res, err := r.Start(context.Background(), StartInput{
		RunID:   "run-mixed-retries",
		Machine: machine,
		Gaggle:  "acme-web",
		Trigger: journal.Trigger{Kind: journal.TriggerManual},
		RepoRef: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web", Branch: "main"},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if res.Phase != journal.PhaseCompleted || deterministic.calls != 3 {
		t.Fatalf("result=%+v calls=%d, want completed after policy and infrastructure retries", res, deterministic.calls)
	}

	rd, err := journal.OpenRead(filepath.Join(runsDir, "run-mixed-retries"))
	if err != nil {
		t.Fatal(err)
	}
	events, err := rd.Events()
	if err != nil {
		t.Fatal(err)
	}
	var classes []journal.AttemptClass
	for _, event := range events {
		if event.Type == journal.EventStageStarted {
			classes = append(classes, event.AttemptClass)
		}
	}
	want := []journal.AttemptClass{"", journal.AttemptPolicy, journal.AttemptInfra}
	if !reflect.DeepEqual(classes, want) {
		t.Fatalf("attempt classes = %q, want %q", classes, want)
	}
}

func TestRunnerBoundsPersistentInfrastructureFailures(t *testing.T) {
	machine := retryFixtureMachine(t, 1)
	cause := errors.New("status 503: provider still unavailable")
	flaky := &infrastructureFlakyDeterministic{failUntil: 100, cause: cause}
	r, runsDir := newTestRunnerWithDeterministic(t, func(ArtifactRecorder, SecretRegistrar) (invoke.Deterministic, error) {
		return flaky, nil
	}, gate.NewAutomatedEvaluator())

	_, err := r.Start(context.Background(), StartInput{
		RunID:   "run-infra-exhaust",
		Machine: machine,
		Gaggle:  "acme-web",
		Trigger: journal.Trigger{Kind: journal.TriggerManual},
		RepoRef: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web", Branch: "main"},
	})
	if err == nil {
		t.Fatal("Start: want persistent infrastructure error")
	}
	if !errors.Is(err, cause) {
		t.Fatalf("error %q does not preserve provider cause %q", err, cause)
	}
	if flaky.calls != 2 {
		t.Fatalf("executor calls = %d, want retry bound 2", flaky.calls)
	}

	rd, openErr := journal.OpenRead(filepath.Join(runsDir, "run-infra-exhaust"))
	if openErr != nil {
		t.Fatal(openErr)
	}
	events, readErr := rd.Events()
	if readErr != nil {
		t.Fatal(readErr)
	}
	var starts, attemptErrors int
	for _, event := range events {
		if event.Type == journal.EventStageStarted {
			starts++
		}
		if event.Type == journal.EventError && event.Error != nil && event.Error.Code == "executor_error" {
			attemptErrors++
			if !strings.Contains(event.Error.Message, cause.Error()) {
				t.Fatalf("journaled attempt error %q does not preserve cause %q", event.Error.Message, cause)
			}
		}
	}
	if starts != 2 || attemptErrors != 2 {
		t.Fatalf("stage.started=%d executor_error=%d, want every one of 2 attempts journaled", starts, attemptErrors)
	}
}

// TestRunnerExhaustsRetriesAndFails proves a stage that never succeeds within
// its declared MaxAttempts fails the run (Start returns an error) rather than
// retrying forever, and every attempt up to the budget is journaled.
func TestRunnerExhaustsRetriesAndFails(t *testing.T) {
	machine := retryFixtureMachine(t, 2)
	flaky := &flakyDeterministic{failUntil: 1000} // always fails within budget
	r, runsDir := newTestRunnerWithDeterministic(t, func(ArtifactRecorder, SecretRegistrar) (invoke.Deterministic, error) {
		return flaky, nil
	}, gate.NewAutomatedEvaluator())

	_, err := r.Start(context.Background(), StartInput{
		RunID:   "run-exhaust",
		Machine: machine,
		Gaggle:  "acme-web",
		Trigger: journal.Trigger{Kind: journal.TriggerManual},
		RepoRef: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web", Branch: "main"},
	})
	if err == nil {
		t.Fatal("Start: want an error after exhausting the retry budget, got nil")
	}
	if flaky.calls != 2 {
		t.Fatalf("executor called %d times, want exactly 2 (the declared budget)", flaky.calls)
	}

	rd, rerr := journal.OpenRead(filepath.Join(runsDir, "run-exhaust"))
	if rerr != nil {
		t.Fatalf("OpenRead: %v", rerr)
	}
	events, eerr := rd.Events()
	if eerr != nil {
		t.Fatalf("Events: %v", eerr)
	}
	var starts, errs, runFailed int
	for _, e := range events {
		if e.Type == journal.EventStageStarted {
			starts++
		}
		if e.Type == journal.EventError {
			errs++
			if e.Error != nil && e.Error.Code == "run_failed" {
				runFailed++
			}
		}
	}
	// 2 stage.started (the retry budget) and 2 per-attempt stage errors, plus the
	// single run-level run_failed cause failTerminal now journals so the walk-level
	// failure is visible in the journal / goobers trace (#305).
	if starts != 2 || errs != 3 || runFailed != 1 {
		t.Fatalf("stage.started=%d error=%d run_failed=%d, want 2, 3, 1", starts, errs, runFailed)
	}
}

// retryFixtureMachineWithBackoff is retryFixtureMachine plus a configurable
// BackoffSeconds, for tests (drain-during-backoff) that need the retry loop's
// idle wait to actually take measurable wall-clock time.
func retryFixtureMachineWithBackoff(t *testing.T, maxAttempts int32, backoff time.Duration) *workflow.Machine {
	t.Helper()
	spec := apiv1.WorkflowSpec{
		Gaggle:   "acme-web",
		Triggers: []apiv1.Trigger{{Type: apiv1.TriggerBacklogItem}},
		Start:    "implement",
		Tasks: []apiv1.Task{
			{
				Name: "implement", Type: apiv1.TaskDeterministic, Goal: "produce a diff",
				Run:   &apiv1.DeterministicRun{Command: []string{"true"}},
				Next:  "review",
				Retry: &apiv1.RetryPolicy{MaxAttempts: maxAttempts, BackoffSeconds: int32(backoff.Seconds())},
			},
		},
		Gates: []apiv1.Gate{
			{
				Name:      "review",
				Evaluator: apiv1.EvaluatorAutomated,
				Automated: &apiv1.AutomatedGate{Check: "status-equals"},
				Branches:  map[string]string{"pass": workflow.TerminalComplete, "fail": workflow.TargetAbort},
			},
		},
	}
	m, err := workflow.Compile(workflow.Definition{Name: "retry-backoff-fixture", Version: 1, Spec: spec})
	if err != nil {
		t.Fatalf("compile retry backoff fixture machine: %v", err)
	}
	return m
}

// TestRunnerRetryBackoffInterruptedByDrainCancellation is #112's regression:
// before the fix, the retry loop's time.Sleep(backoff) between attempts
// ignored ctx entirely, so a SIGTERM mid-retry-storm still had to wait out
// every remaining backoff in full before the run could finish draining. The
// fix waits on the run-level ctx (not attemptCtx, which never cancels — the
// drain contract for an in-flight dispatch) so the wait is interrupted the
// moment a cancellation lands, without changing dispatch itself or the
// number of attempts a task gets: every attempt still runs, just without the
// full idle wait between them once shutdown is already in progress.
func TestRunnerRetryBackoffInterruptedByDrainCancellation(t *testing.T) {
	const backoff = 10 * time.Second
	const maxAttempts = 5 // un-interrupted worst case: 4 waits * 10s = 40s
	machine := retryFixtureMachineWithBackoff(t, maxAttempts, backoff)
	flaky := &flakyDeterministic{failUntil: 1000} // always fails within budget
	r, _ := newTestRunnerWithDeterministic(t, func(ArtifactRecorder, SecretRegistrar) (invoke.Deterministic, error) {
		return flaky, nil
	}, gate.NewAutomatedEvaluator())

	cancelCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	backoffEntered := make(chan struct{}, 1)
	ctx := backoffObservedContext{Context: cancelCtx, entered: backoffEntered}
	type startResult struct {
		res Result
		err error
	}
	done := make(chan startResult, 1)
	go func() {
		res, err := r.Start(ctx, StartInput{
			RunID:   "run-backoff-drain",
			Machine: machine,
			Gaggle:  "acme-web",
			Trigger: journal.Trigger{Kind: journal.TriggerManual},
			RepoRef: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web", Branch: "main"},
		})
		done <- startResult{res, err}
	}()

	const entryBound = 5 * time.Second
	select {
	case <-backoffEntered:
	case got := <-done:
		t.Fatalf("Start returned before entering retry backoff: result=%+v err=%v", got.res, got.err)
	case <-time.After(entryBound):
		t.Fatalf("retry backoff was not entered within %s", entryBound)
	}
	start := time.Now()
	cancel()

	const bound = 8 * time.Second // comfortably under the un-interrupted 40s, comfortably over dispatch/scheduling noise
	select {
	case r := <-done:
		if r.err == nil {
			t.Fatal("Start: expected an error (retries exhausted, the executor never succeeds), got nil")
		}
	case <-time.After(bound):
		t.Fatalf("Start did not return within %s — the backoff sleep ignored ctx cancellation (#112)", bound)
	}
	if elapsed := time.Since(start); elapsed > bound {
		t.Fatalf("Start took %s, want well under the un-interrupted %s (4 * %s backoff) — cancellation should short-circuit the remaining waits", elapsed, 4*backoff, backoff)
	}
	if flaky.calls != maxAttempts {
		t.Fatalf("executor called %d times, want %d (full retry budget still exhausted despite drain — dispatch itself is unaffected)", flaky.calls, maxAttempts)
	}
}

// TestRunnerDrainsInFlightAttemptOnCancellation proves a SIGTERM-style
// cancellation of the run-level context (internal/signals cancels this exact
// context) lets an in-flight stage attempt finish and journal normally,
// pausing only before the NEXT stage — never aborting mid-attempt.
func TestRunnerDrainsInFlightAttemptOnCancellation(t *testing.T) {
	machine := fixtureMachine(t)
	blocker := &blockingDeterministic{started: make(chan struct{}), release: make(chan struct{}), finished: make(chan struct{})}
	r, runsDir := newTestRunnerWithDeterministic(t, func(ArtifactRecorder, SecretRegistrar) (invoke.Deterministic, error) {
		return blocker, nil
	}, gate.NewAutomatedEvaluator())

	ctx, cancel := context.WithCancel(context.Background())
	type startResult struct {
		res Result
		err error
	}
	done := make(chan startResult, 1)
	go func() {
		res, err := r.Start(ctx, StartInput{
			RunID:   "run-drain",
			Machine: machine,
			Gaggle:  "acme-web",
			Trigger: journal.Trigger{Kind: journal.TriggerManual},
			RepoRef: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web", Branch: "main"},
		})
		done <- startResult{res, err}
	}()

	// Wait until dispatch has actually reached the blocking call before
	// cancelling — otherwise cancel() races the walk loop's own between-
	// stages check and could fire before "implement" is ever dispatched.
	select {
	case <-blocker.started:
	case <-time.After(5 * time.Second):
		t.Fatal("blocking executor was never dispatched")
	}

	// Cancel WHILE the (only) task's attempt is blocked mid-dispatch, then
	// release it — proving the cancellation didn't abort the in-flight call.
	cancel()
	select {
	case <-blocker.finished:
		t.Fatal("blocking executor finished before being released — it should still be blocked here")
	case <-time.After(50 * time.Millisecond):
	}
	close(blocker.release)

	select {
	case r := <-done:
		if r.err != nil {
			t.Fatalf("Start: %v", r.err)
		}
		if r.res.Phase != journal.PhaseRunning {
			t.Fatalf("phase = %q, want running (paused, not aborted)", r.res.Phase)
		}
		if r.res.FinalState != "review" {
			t.Fatalf("finalState = %q, want review (paused before the NEXT stage)", r.res.FinalState)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Start did not return after the blocking call was released")
	}

	select {
	case <-blocker.finished:
	default:
		t.Fatal("expected the in-flight attempt to have finished, not been abandoned")
	}

	// The completed attempt is fully journaled despite the cancellation.
	rd, err := journal.OpenRead(filepath.Join(runsDir, "run-drain"))
	if err != nil {
		t.Fatalf("OpenRead: %v", err)
	}
	events, err := rd.Events()
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	var sawFinished bool
	for _, e := range events {
		if e.Type == journal.EventStageFinished && e.Stage == "implement" {
			sawFinished = true
		}
		if e.Type == journal.EventGateEvaluated {
			t.Fatalf("gate should not have been dispatched — the run must pause BEFORE it, got %+v", e)
		}
	}
	if !sawFinished {
		t.Fatal("expected implement's stage.finished to be journaled despite the cancellation")
	}
	st, err := rd.State()
	if err != nil {
		t.Fatalf("State: %v", err)
	}
	if st.MachineState != "review" {
		t.Fatalf("state.json machineState = %q, want review (resume point)", st.MachineState)
	}
}

// simulateCrashMidAttempt hand-builds a run journal exactly to the point a
// real Start would have reached had the process died right after dispatching
// stageName's attempt N: run.started, then stage.started(stageName, attempt),
// with NO matching stage.finished — the crash signature Resume must detect.
// A clean journal.Create/Close (no torn write) is sufficient here since
// torn-write repair is internal/journal's own, already-tested concern
// (TestKill9MidAppendRecovers); this test is about the runner's
// interpretation of "started with no finished", not journal-write durability.
func simulateCrashMidAttempt(t *testing.T, runsDir string, machine *workflow.Machine, runID, stageName string, attempt int) {
	t.Helper()
	jr, err := journal.Create(runsDir, journal.RunIdentity{
		RunID: runID, Workflow: machine.Def.Name, WorkflowVersion: machine.Def.Version,
		WorkflowDigest: machine.Digest(), Gaggle: "acme-web", Trigger: journal.Trigger{Kind: journal.TriggerManual},
	}, nil)
	if err != nil {
		t.Fatalf("simulateCrashMidAttempt: journal.Create: %v", err)
	}
	// Mirror Start's unconditional run-branch ref.touched event (#133) — a
	// real crash always went through Start first, so a faithful simulation
	// must too, or a conformance comparison against a real Start'd run sees
	// a phantom extra event on the clean side only.
	if err := jr.Append(journal.Event{
		Type: journal.EventRefTouched,
		ExternalRef: &journal.ExternalRef{
			Provider: string(apiv1.ProviderGitHub),
			Kind:     "branch",
			ID:       providers.BranchName(machine.Def.Name, runID),
		},
	}); err != nil {
		t.Fatalf("simulateCrashMidAttempt: append ref.touched: %v", err)
	}
	jr.SetMachineState(stageName)
	if err := jr.Append(journal.Event{Type: journal.EventStageStarted, Stage: stageName, Attempt: attempt}); err != nil {
		t.Fatalf("simulateCrashMidAttempt: append stage.started: %v", err)
	}
	if err := recordContextManifest(jr, apiv1.InvocationEnvelope{}, stageName, attempt, ""); err != nil {
		t.Fatalf("simulateCrashMidAttempt: record context manifest: %v", err)
	}
	if err := jr.Close(); err != nil {
		t.Fatalf("simulateCrashMidAttempt: close: %v", err)
	}
}

// TestRunnerResumeRetriesInterruptedAttempt is #17's crash/resume acceptance
// scenario: a run interrupted mid-attempt resumes, journals the interrupted
// attempt as a terminal infra-tagged failure (never silently re-run), and
// continues the SAME attempt count against the task's own retry budget.
func TestRunnerResumeRetriesInterruptedAttempt(t *testing.T) {
	machine := retryFixtureMachine(t, 3)
	instanceRoot := t.TempDir()
	wtMgr, err := worktree.NewManager(filepath.Join(instanceRoot, "workcopies"))
	if err != nil {
		t.Fatalf("new worktree manager: %v", err)
	}
	runsDir := filepath.Join(instanceRoot, "runs")
	fixtureRepo := newFixtureRepo(t)

	simulateCrashMidAttempt(t, runsDir, machine, "run-crash", "implement", 1)

	det := &flakyDeterministic{failUntil: 0} // succeeds immediately once dispatched
	r, err := New(Config{
		NewDeterministic: func(ArtifactRecorder, SecretRegistrar) (invoke.Deterministic, error) { return det, nil },
		Automated:        gate.NewAutomatedEvaluator(),
		Worktrees:        wtMgr,
		RunsDir:          runsDir,
		RepoCloneURL:     func(apiv1.RepoRef) (string, error) { return fixtureRepo, nil },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	res, err := r.Resume(context.Background(), ResumeInput{
		RunID:   "run-crash",
		Machine: machine,
		RepoRef: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web", Branch: "main"},
	})
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if res.Phase != journal.PhaseCompleted {
		t.Fatalf("phase = %q, want completed", res.Phase)
	}
	if det.calls != 1 {
		t.Fatalf("executor called %d times after resume, want exactly 1 (resume continues at attempt 2, which succeeds immediately)", det.calls)
	}

	rd, err := journal.OpenRead(filepath.Join(runsDir, "run-crash"))
	if err != nil {
		t.Fatalf("OpenRead: %v", err)
	}
	events, err := rd.Events()
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	var stageEvents []journal.Event
	for _, e := range events {
		if e.Stage == "implement" {
			stageEvents = append(stageEvents, e)
		}
	}
	wantTypes := []journal.EventType{
		journal.EventStageStarted, // attempt 1, pre-crash
		journal.EventArtifactRecorded,
		journal.EventStageFinished, // attempt 1, infra, journaled by Resume
		journal.EventStageStarted,  // attempt 2, the crash-driven continuation
		journal.EventArtifactRecorded,
		journal.EventStageFinished, // attempt 2, the crash-driven continuation, success
	}
	if len(stageEvents) != len(wantTypes) {
		t.Fatalf("implement-stage events = %d, want %d: %+v", len(stageEvents), len(wantTypes), stageEvents)
	}
	for i, e := range stageEvents {
		if e.Type != wantTypes[i] {
			t.Errorf("event[%d].Type = %q, want %q", i, e.Type, wantTypes[i])
		}
	}
	if stageEvents[2].Attempt != 1 || stageEvents[2].AttemptClass != journal.AttemptInfra || stageEvents[2].Status != string(apiv1.ResultFailure) {
		t.Errorf("interrupted-attempt event = %+v, want attempt=1 class=infra status=failure", stageEvents[2])
	}
	// #111: the continuation dispatched right after an interrupted attempt is
	// driven by the CRASH, not Task.Retry — it must be tagged "infra", not
	// "policy" (which would wrongly make it conformance-normative, §3.3).
	if stageEvents[3].Attempt != 2 || stageEvents[3].AttemptClass != journal.AttemptInfra {
		t.Errorf("resumed-attempt stage.started = %+v, want attempt=2 class=infra", stageEvents[3])
	}
	if stageEvents[4].Attempt != 2 || stageEvents[4].AttemptClass != journal.AttemptInfra || stageEvents[4].Type != journal.EventArtifactRecorded {
		t.Errorf("resumed-attempt context artifact = %+v, want attempt=2 class=infra artifact.recorded", stageEvents[4])
	}
	if stageEvents[5].Attempt != 2 || stageEvents[5].AttemptClass != journal.AttemptInfra || stageEvents[5].Status != string(apiv1.ResultSuccess) {
		t.Errorf("resumed-attempt stage.finished = %+v, want attempt=2 class=infra status=success", stageEvents[5])
	}

	// Every post-crash event for "implement" (the interrupted marker AND the
	// crash-driven continuation's own started/context/finished) is excluded
	// from the conformance set (§3.3) — confirm IsConformanceNormative agrees
	// for all four, not just the interrupted marker.
	for i := 2; i <= 5; i++ {
		if stageEvents[i].IsConformanceNormative() {
			t.Errorf("event[%d] = %+v must be excluded from conformance (§3.3) — only the original attempt=1 events may be normative for a crashed stage", i, stageEvents[i])
		}
	}

	// #111's conformance-seed assertion: a crash+resume run's normative
	// event set must not gain phantom policy-retry events a crash-free run
	// of the identical workflow never produces. Build a clean run of the
	// SAME machine (succeeding on its first attempt, no crash) and confirm
	// its normative set is exactly the crash run's normative set PLUS the
	// one event a crash can never journal — "implement"'s own
	// stage.finished(attempt=1) — since the interrupted attempt's true
	// result is genuinely unknowable, not because of any policy-tagging bug.
	cleanByTask := map[string]stubTaskResult{"run-clean:implement": {status: apiv1.ResultSuccess}}
	cleanRunner, cleanRunsDir := newTestRunner(t, cleanByTask, gate.NewAutomatedEvaluator())
	cleanRes, cerr := cleanRunner.Start(context.Background(), StartInput{
		RunID:   "run-clean",
		Machine: machine,
		Gaggle:  "acme-web",
		Trigger: journal.Trigger{Kind: journal.TriggerManual},
		RepoRef: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web", Branch: "main"},
	})
	if cerr != nil {
		t.Fatalf("clean Start: %v", cerr)
	}
	if cleanRes.Phase != journal.PhaseCompleted {
		t.Fatalf("clean run phase = %q, want completed", cleanRes.Phase)
	}
	cleanRd, cerr := journal.OpenRead(filepath.Join(cleanRunsDir, "run-clean"))
	if cerr != nil {
		t.Fatalf("OpenRead (clean): %v", cerr)
	}
	cleanEvents, cerr := cleanRd.Events()
	if cerr != nil {
		t.Fatalf("Events (clean): %v", cerr)
	}

	normativeTypes := func(evs []journal.Event) []journal.EventType {
		var out []journal.EventType
		for _, e := range evs {
			if e.IsConformanceNormative() {
				out = append(out, e.Type)
			}
		}
		return out
	}
	crashNormative := normativeTypes(events)
	cleanNormative := normativeTypes(cleanEvents)
	// The clean run has exactly one extra normative event vs the crash run:
	// "implement"'s stage.finished(attempt=1) — remove it before comparing.
	removed := false
	var cleanNormativeMinusStageFinished []journal.EventType
	for _, ty := range cleanNormative {
		if !removed && ty == journal.EventStageFinished {
			removed = true
			continue
		}
		cleanNormativeMinusStageFinished = append(cleanNormativeMinusStageFinished, ty)
	}
	if !removed {
		t.Fatal("clean run's normative set unexpectedly has no stage.finished to remove for comparison")
	}
	if !eventTypesEqual(crashNormative, cleanNormativeMinusStageFinished) {
		t.Fatalf("crash-run normative types = %v, want clean-run normative types minus one stage.finished = %v (a crash must not add phantom normative events beyond the one it structurally cannot produce)", crashNormative, cleanNormativeMinusStageFinished)
	}
}

// TestRunnerResumeFailsWhenInterruptedAttemptExhaustsBudget proves a crash
// during a task's LAST allowed attempt does not grant it a bonus attempt —
// Resume must fail closed, not silently extend the retry budget.
func TestRunnerResumeFailsWhenInterruptedAttemptExhaustsBudget(t *testing.T) {
	machine := retryFixtureMachine(t, 1) // MaxAttempts=1: no retries allowed at all
	instanceRoot := t.TempDir()
	wtMgr, err := worktree.NewManager(filepath.Join(instanceRoot, "workcopies"))
	if err != nil {
		t.Fatalf("new worktree manager: %v", err)
	}
	runsDir := filepath.Join(instanceRoot, "runs")
	fixtureRepo := newFixtureRepo(t)

	simulateCrashMidAttempt(t, runsDir, machine, "run-crash-exhausted", "implement", 1)

	det := &flakyDeterministic{}
	r, err := New(Config{
		NewDeterministic: func(ArtifactRecorder, SecretRegistrar) (invoke.Deterministic, error) { return det, nil },
		Automated:        gate.NewAutomatedEvaluator(),
		Worktrees:        wtMgr,
		RunsDir:          runsDir,
		RepoCloneURL:     func(apiv1.RepoRef) (string, error) { return fixtureRepo, nil },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	_, err = r.Resume(context.Background(), ResumeInput{
		RunID:   "run-crash-exhausted",
		Machine: machine,
		RepoRef: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web", Branch: "main"},
	})
	if err == nil {
		t.Fatal("Resume: want an error — the interrupted attempt already consumed the entire 1-attempt budget")
	}
	if det.calls != 0 {
		t.Fatalf("executor called %d times, want 0 — must not dispatch beyond the exhausted budget", det.calls)
	}

	// The exhaustion must reach a terminal journal state (ruling #110's
	// failTerminal), not leave the run at phase=running — that's what makes
	// a SECOND Resume (TestDoubleResumeAfterExhaustedBudgetDoesNotGrantFreshBudget,
	// #109) short-circuit instead of granting a fresh attempt budget.
	rd, rerr := journal.OpenRead(filepath.Join(runsDir, "run-crash-exhausted"))
	if rerr != nil {
		t.Fatalf("OpenRead: %v", rerr)
	}
	st, serr := rd.State()
	if serr != nil {
		t.Fatalf("State: %v", serr)
	}
	if st.Phase != journal.PhaseFailed {
		t.Fatalf("state.json phase = %q, want failed", st.Phase)
	}
}

// TestDoubleResumeAfterExhaustedBudgetDoesNotGrantFreshBudget is #109's
// acceptance scenario: Resume #1 of a crash on a task's LAST allowed attempt
// must not leave the run resumable again with a fresh budget. Before #110's
// failTerminal fix, Resume #1's "no attempts left" error propagated with no
// terminal journal write, so Resume #2 read a still-"running" state.json,
// found interruptedAttempt back at 0 (the infra-tagged marker Resume #1 DID
// manage to journal made started==finished again), and re-dispatched
// "implement" from attempt 1 with its full budget restored — exactly what a
// crash-loop-of-restarts (cmd/goobers/daemon.go auto-resumes every
// PhaseRunning run) would repeat forever. #110's failTerminal closes this by
// journaling PhaseFailed before Resume #1 returns its error, so Resume #2
// short-circuits at the terminal-phase check instead of re-entering the walk
// at all.
func TestDoubleResumeAfterExhaustedBudgetDoesNotGrantFreshBudget(t *testing.T) {
	machine := retryFixtureMachine(t, 1) // MaxAttempts=1: no retries allowed at all
	instanceRoot := t.TempDir()
	wtMgr, err := worktree.NewManager(filepath.Join(instanceRoot, "workcopies"))
	if err != nil {
		t.Fatalf("new worktree manager: %v", err)
	}
	runsDir := filepath.Join(instanceRoot, "runs")
	fixtureRepo := newFixtureRepo(t)

	simulateCrashMidAttempt(t, runsDir, machine, "run-double-resume", "implement", 1)

	det := &flakyDeterministic{}
	r, err := New(Config{
		NewDeterministic: func(ArtifactRecorder, SecretRegistrar) (invoke.Deterministic, error) { return det, nil },
		Automated:        gate.NewAutomatedEvaluator(),
		Worktrees:        wtMgr,
		RunsDir:          runsDir,
		RepoCloneURL:     func(apiv1.RepoRef) (string, error) { return fixtureRepo, nil },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// Resume #1: the interrupted attempt already consumed the entire budget,
	// so this errors (TestRunnerResumeFailsWhenInterruptedAttemptExhaustsBudget
	// covers this in isolation) — but must still journal a terminal phase.
	if _, err := r.Resume(context.Background(), ResumeInput{
		RunID:   "run-double-resume",
		Machine: machine,
		RepoRef: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web", Branch: "main"},
	}); err == nil {
		t.Fatal("Resume #1: want an error — the interrupted attempt already exhausted its budget")
	}

	// Resume #2 (the daemon-restart-retries-forever scenario): must NOT
	// re-dispatch "implement" with a fresh budget — it should short-circuit
	// on the terminal phase Resume #1 already journaled.
	res2, err2 := r.Resume(context.Background(), ResumeInput{
		RunID:   "run-double-resume",
		Machine: machine,
		RepoRef: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web", Branch: "main"},
	})
	if err2 != nil {
		t.Fatalf("Resume #2: %v", err2)
	}
	if res2.Phase != journal.PhaseFailed {
		t.Fatalf("Resume #2 phase = %q, want failed (idempotent terminal short-circuit, not a fresh attempt)", res2.Phase)
	}
	if det.calls != 0 {
		t.Fatalf("executor called %d times across both resumes, want 0 — a crash on the last attempt must never grant a fresh budget on a later resume", det.calls)
	}
}

// TestRunnerResumeAlreadyTerminalIsIdempotent proves Resume on an
// already-finished run just reports its phase, without re-dispatching
// anything.
func TestRunnerResumeAlreadyTerminalIsIdempotent(t *testing.T) {
	machine := fixtureMachine(t)
	byTask := map[string]stubTaskResult{
		"run-done:implement": {status: apiv1.ResultSuccess},
	}
	r, _ := newTestRunner(t, byTask, gate.NewAutomatedEvaluator())

	res, err := r.Start(context.Background(), StartInput{
		RunID:   "run-done",
		Machine: machine,
		Gaggle:  "acme-web",
		Trigger: journal.Trigger{Kind: journal.TriggerManual},
		RepoRef: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web", Branch: "main"},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if res.Phase != journal.PhaseCompleted {
		t.Fatalf("phase = %q, want completed", res.Phase)
	}

	res2, err := r.Resume(context.Background(), ResumeInput{
		RunID:   "run-done",
		Machine: machine,
		RepoRef: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web", Branch: "main"},
	})
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if res2.Phase != journal.PhaseCompleted {
		t.Fatalf("Resume phase = %q, want completed (idempotent, no re-dispatch)", res2.Phase)
	}
}

// TestRunnerResumeRestoresGateRepassCounter is #89's acceptance scenario: a
// crash mid repass-loop must not grant the gate extra passes beyond its
// budget. The fixture's "review" gate loops "fail" back to "implement"
// (rather than aborting), so a broken seed would let the resumed run take a
// second full pass before escalating instead of escalating immediately.
func TestRunnerResumeRestoresGateRepassCounter(t *testing.T) {
	spec := apiv1.WorkflowSpec{
		Gaggle:   "acme-web",
		Triggers: []apiv1.Trigger{{Type: apiv1.TriggerBacklogItem}},
		Start:    "implement",
		Tasks: []apiv1.Task{
			{Name: "implement", Type: apiv1.TaskDeterministic, Goal: "produce a diff", Run: &apiv1.DeterministicRun{Command: []string{"true"}}, Next: "review"},
		},
		Gates: []apiv1.Gate{
			{
				Name:      "review",
				Evaluator: apiv1.EvaluatorAutomated,
				Automated: &apiv1.AutomatedGate{Check: "status-equals"},
				Branches:  map[string]string{"pass": workflow.TerminalComplete, "fail": "implement"},
			},
		},
	}
	machine, err := workflow.Compile(workflow.Definition{Name: "repass-loop", Version: 1, Spec: spec})
	if err != nil {
		t.Fatalf("compile: %v", err)
	}

	byTask := map[string]stubTaskResult{
		"run-repass:implement": {status: apiv1.ResultFailure, errorInfo: &apiv1.ErrorInfo{Code: "x", Message: "always fails"}},
	}
	r, runsDir := newTestRunner(t, byTask, gate.NewAutomatedEvaluator())
	r.cfg.MaxRepasses = 1 // a 2nd non-pass evaluation must escalate

	// Hand-build a journal at the point a real Start would have reached had
	// the process died right after journaling "review"'s first fail (attempt
	// 1, not yet escalated) and looping back to "implement" for a 2nd pass —
	// mirroring simulateCrashMidAttempt's pattern for task attempts.
	jr, err := journal.Create(runsDir, journal.RunIdentity{
		RunID: "run-repass", Workflow: machine.Def.Name, WorkflowVersion: machine.Def.Version,
		WorkflowDigest: machine.Digest(), Gaggle: "acme-web", Trigger: journal.Trigger{Kind: journal.TriggerManual},
	}, nil)
	if err != nil {
		t.Fatalf("journal.Create: %v", err)
	}
	jr.SetMachineState("implement")
	if err := jr.Append(journal.Event{
		Type: journal.EventGateEvaluated, Gate: "review", Verdict: "fail", Target: "implement",
		Runner: map[string]any{"repassAttempt": 1, "escalated": false},
	}); err != nil {
		t.Fatalf("append gate.evaluated: %v", err)
	}
	if err := jr.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	res, err := r.Resume(context.Background(), ResumeInput{
		RunID:   "run-repass",
		Machine: machine,
		RepoRef: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web", Branch: "main"},
	})
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if res.Phase != journal.PhaseEscalated {
		t.Fatalf("phase = %q, want escalated (seeded repass count should exhaust the budget on the very next evaluation)", res.Phase)
	}

	rd, err := journal.OpenRead(filepath.Join(runsDir, "run-repass"))
	if err != nil {
		t.Fatalf("OpenRead: %v", err)
	}
	events, err := rd.Events()
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	gateEvals := 0
	for _, e := range events {
		if e.Type == journal.EventGateEvaluated {
			gateEvals++
		}
	}
	if gateEvals != 2 {
		t.Fatalf("gate.evaluated count = %d, want 2 (1 pre-crash + exactly 1 post-resume before escalating) — a broken repass seed would allow extra passes before escalating", gateEvals)
	}
}

func TestRunnerResumeEscalatesAfterRepeatedInterruptedGateEvaluations(t *testing.T) {
	spec := apiv1.WorkflowSpec{
		Gaggle:   "acme-web",
		Triggers: []apiv1.Trigger{{Type: apiv1.TriggerBacklogItem}},
		Start:    "implement",
		Tasks: []apiv1.Task{
			{Name: "implement", Type: apiv1.TaskDeterministic, Goal: "produce a diff", Run: &apiv1.DeterministicRun{Command: []string{"true"}}, Next: "review"},
		},
		Gates: []apiv1.Gate{
			{
				Name:      "review",
				Evaluator: apiv1.EvaluatorAgentic,
				Agentic:   &apiv1.AgenticGate{Goober: "reviewer"},
				Branches: map[string]string{
					string(apiv1.VerdictPass):         workflow.TerminalComplete,
					string(apiv1.VerdictNeedsChanges): "implement",
					string(apiv1.VerdictFail):         workflow.TargetAbort,
				},
			},
		},
	}
	machine, err := workflow.Compile(workflow.Definition{Name: "interrupted-gate-loop", Version: 1, Spec: spec})
	if err != nil {
		t.Fatalf("compile: %v", err)
	}

	r, runsDir := newTestRunner(t, map[string]stubTaskResult{}, gate.NewAutomatedEvaluator())
	r.cfg.MaxRepasses = 1
	var preparationCalls, executorCalls int
	r.cfg.RepoCloneURL = func(apiv1.RepoRef) (string, error) {
		preparationCalls++
		return "", errors.New("agentic gate preparation must not run after recovery exhausted the budget")
	}
	reviewer := &alwaysNeedsChangesReviewer{}
	r.cfg.NewAgentic = func(string, ArtifactRecorder, SecretRegistrar) (invoke.Goober, error) {
		executorCalls++
		return reviewer, nil
	}
	jr, err := journal.Create(runsDir, journal.RunIdentity{
		RunID: "run-interrupted-gate", Workflow: machine.Def.Name, WorkflowVersion: machine.Def.Version,
		WorkflowDigest: machine.Digest(), Gaggle: "acme-web", Trigger: journal.Trigger{Kind: journal.TriggerManual},
	}, nil)
	if err != nil {
		t.Fatalf("journal.Create: %v", err)
	}
	jr.SetMachineState("review")
	if err := jr.Append(journal.Event{Type: journal.EventStageStarted, Stage: "implement", Attempt: 1}); err != nil {
		t.Fatalf("append stage.started: %v", err)
	}
	if err := jr.Append(journal.Event{Type: journal.EventStageFinished, Stage: "implement", Attempt: 1, Status: string(apiv1.ResultFailure)}); err != nil {
		t.Fatalf("append stage.finished: %v", err)
	}
	for attempt := 1; attempt <= 2; attempt++ {
		if err := jr.Append(journal.Event{
			Type: journal.EventGateStarted, Gate: "review",
			Runner: map[string]any{"repassAttempt": attempt},
		}); err != nil {
			t.Fatalf("append gate.started attempt %d: %v", attempt, err)
		}
	}
	if err := jr.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	res, err := r.Resume(context.Background(), ResumeInput{
		RunID:   "run-interrupted-gate",
		Machine: machine,
		RepoRef: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web", Branch: "main"},
	})
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if res.Phase != journal.PhaseEscalated {
		t.Fatalf("phase = %q, want escalated after interrupted attempts exhausted the budget", res.Phase)
	}
	if preparationCalls != 0 || executorCalls != 0 || reviewer.calls != 0 {
		t.Fatalf("agentic work after resume: preparations=%d executor constructions=%d reviewer calls=%d, want all zero", preparationCalls, executorCalls, reviewer.calls)
	}

	rd, err := journal.OpenRead(filepath.Join(runsDir, "run-interrupted-gate"))
	if err != nil {
		t.Fatalf("OpenRead: %v", err)
	}
	events, err := rd.Events()
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	var starts, verdicts int
	var verdict journal.Event
	for _, e := range events {
		switch e.Type {
		case journal.EventGateStarted:
			starts++
		case journal.EventGateEvaluated:
			verdicts++
			verdict = e
		}
	}
	if starts != 2 || verdicts != 1 {
		t.Fatalf("gate events: starts=%d verdicts=%d, want 2 interrupted starts and 1 synthesized verdict", starts, verdicts)
	}
	if verdict.Target != workflow.TargetEscalate || verdict.Verdict != gate.OutcomeFail {
		t.Fatalf("recovery verdict = %+v, want fail -> %s", verdict, workflow.TargetEscalate)
	}
	if interrupted, _ := verdict.Runner["interrupted"].(bool); !interrupted {
		t.Fatalf("recovery verdict Runner[interrupted] = %v, want true", verdict.Runner["interrupted"])
	}
	if attempt := int(verdict.Runner["repassAttempt"].(float64)); attempt != 2 {
		t.Fatalf("recovery verdict repassAttempt = %d, want 2", attempt)
	}
}

// TestGateDiffSeedReconstructsFromJournal is #316's resume-side counterpart
// to TestRunnerResumeRestoresGateRepassCounter above: gateDiffSeed must
// reconstruct Evaluator.LastDiffDigest from a run's journaled gate.evaluated
// events exactly the way gateRepassSeed reconstructs Attempts, or a crash
// mid-repass-loop would silently forget the prior attempt's digest and let a
// resumed run re-invoke the reviewer for a diff it already judged.
func TestGateDiffSeedReconstructsFromJournal(t *testing.T) {
	events := []journal.Event{
		{Type: journal.EventGateEvaluated, Gate: "review", Verdict: "fail", Target: "implement",
			Runner: map[string]any{"repassAttempt": 1.0, "escalated": false, "duplicateDiff": false, "diffDigest": "sha256:aaaa"}},
		// A later evaluation of a DIFFERENT gate must not clobber "review"'s
		// entry, and one with no diffDigest at all (e.g. an automated gate)
		// must leave the prior seed for its own name untouched.
		{Type: journal.EventGateEvaluated, Gate: "autogate", Verdict: "pass", Target: "",
			Runner: map[string]any{"repassAttempt": 0.0, "escalated": false}},
		// "review" evaluated again — the LAST event per gate wins.
		{Type: journal.EventGateEvaluated, Gate: "review", Verdict: "fail", Target: "implement",
			Runner: map[string]any{"repassAttempt": 2.0, "escalated": false, "duplicateDiff": false, "diffDigest": "sha256:bbbb"}},
		{Type: journal.EventStageStarted, Stage: "implement"},
	}
	seed := gateDiffSeed(events)
	if got := seed["review"]; got != "sha256:bbbb" {
		t.Fatalf("seed[review] = %q, want sha256:bbbb (the last journaled digest for that gate)", got)
	}
	if _, ok := seed["autogate"]; ok {
		t.Fatalf("seed[autogate] = %q, want absent (that event carried no diffDigest)", seed["autogate"])
	}
}

// TestGateDiffSeedNilForNoGateEvents proves the nil-safe zero value a fresh
// run needs: a journal with no gate.evaluated events at all (or none
// carrying a diffDigest) yields a nil map, matching Evaluator.LastDiffDigest's
// own nil-safe contract.
func TestGateDiffSeedNilForNoGateEvents(t *testing.T) {
	if seed := gateDiffSeed(nil); seed != nil {
		t.Fatalf("gateDiffSeed(nil) = %v, want nil", seed)
	}
	events := []journal.Event{{Type: journal.EventStageStarted, Stage: "implement"}}
	if seed := gateDiffSeed(events); seed != nil {
		t.Fatalf("gateDiffSeed with no gate.evaluated events = %v, want nil", seed)
	}
}

// TestReconstructPointersIncludesVerdictOnRepassRoute is issue #412's
// resume-side counterpart to TestRunnerRepassReceivesReviewerVerdictAsContext:
// a crash right after a gate journals a repass verdict — before the repass
// stage's own dispatch — must not lose that verdict on resume. reconstructPointers
// must include it exactly as the live path would (walk's own append right
// after evaluateGate), and must exclude terminal-routed evaluations (abort/
// escalate/complete never dispatch anything downstream to hand it to).
func TestReconstructPointersIncludesVerdictOnRepassRoute(t *testing.T) {
	events := []journal.Event{
		{Type: journal.EventStageFinished, Stage: "implement", Attempt: 1,
			Artifacts: []journal.Ref{{Path: "artifacts/sha256/aa", Digest: "sha256:aaaa", Size: 3}}},
		{Type: journal.EventGateEvaluated, Gate: "review", Verdict: "needs-changes", Target: "implement",
			Name: "verdict/review-1.json", Ref: &journal.Ref{Path: "artifacts/sha256/bb", Digest: "sha256:bbbb", Size: 42}},
		// A terminal-routed evaluation (no downstream dispatch) must NOT
		// contribute a pointer — there's nothing to hand it to.
		{Type: journal.EventGateEvaluated, Gate: "ci-gate", Verdict: "fail", Target: workflow.TargetAbort,
			Name: "verdict/ci-gate-1.json", Ref: &journal.Ref{Path: "artifacts/sha256/cc", Digest: "sha256:cccc", Size: 9}},
		// An automated gate's event (no Ref — nothing was journaled as an
		// artifact) must be a no-op too, same as the live path's nil check.
		{Type: journal.EventGateEvaluated, Gate: "autogate", Verdict: "fail", Target: "implement"},
	}
	got := reconstructPointers(events)

	var stagePtr, verdictPtr *apiv1.ContextPointer
	for i := range got {
		switch got[i].Name {
		case "implement.artifact[0]":
			stagePtr = &got[i]
		case "review.verdict":
			verdictPtr = &got[i]
		}
	}
	if stagePtr == nil {
		t.Fatalf("reconstructPointers = %+v, want implement's own stage.finished artifact preserved", got)
	}
	if verdictPtr == nil {
		t.Fatalf("reconstructPointers = %+v, want a review.verdict pointer for the repass-routed gate.evaluated event", got)
	}
	if verdictPtr.Artifact == nil || verdictPtr.Artifact.Digest != "sha256:bbbb" {
		t.Fatalf("verdictPtr.Artifact = %+v, want digest sha256:bbbb (the journaled Ref)", verdictPtr.Artifact)
	}
	for _, p := range got {
		if p.Name == "ci-gate.verdict" {
			t.Fatalf("reconstructPointers = %+v, want no pointer for a terminal-routed (@abort) evaluation", got)
		}
	}
	if len(got) != 2 {
		t.Fatalf("reconstructPointers returned %d pointers, want exactly 2 (implement's artifact + review's verdict)", len(got))
	}
}

// countingDeterministic records how many times it was dispatched — for
// proving a stage was NOT re-dispatched (#107): an already-finished stage
// resumed at must never re-invoke its executor.
type countingDeterministic struct{ calls int }

func (c *countingDeterministic) Run(_ context.Context, _ apiv1.InvocationEnvelope, _ apiv1.DeterministicRun) (apiv1.ResultEnvelope, error) {
	c.calls++
	return apiv1.ResultEnvelope{Status: apiv1.ResultSuccess}, nil
}

// capturingDeterministic records the invocation envelope of its last call —
// for asserting a downstream stage's ContextPointers after a resume (#107's
// pointer-visibility scenario).
type capturingDeterministic struct {
	calls   int
	lastEnv apiv1.InvocationEnvelope
}

func (c *capturingDeterministic) Run(_ context.Context, env apiv1.InvocationEnvelope, _ apiv1.DeterministicRun) (apiv1.ResultEnvelope, error) {
	c.calls++
	c.lastEnv = env
	return apiv1.ResultEnvelope{Status: apiv1.ResultSuccess}, nil
}

// chainedThroughGateMachine is implement -> review (gate, pass->deploy) ->
// deploy — for proving a resumed run's ContextPointers still carry an
// artifact a stage produced before the crash (#107), once it flows through
// a gate rather than directly to the next task.
func chainedThroughGateMachine(t *testing.T) *workflow.Machine {
	t.Helper()
	spec := apiv1.WorkflowSpec{
		Gaggle:   "acme-web",
		Triggers: []apiv1.Trigger{{Type: apiv1.TriggerBacklogItem}},
		Start:    "implement",
		Tasks: []apiv1.Task{
			{Name: "implement", Type: apiv1.TaskDeterministic, Goal: "produce a diff", Run: &apiv1.DeterministicRun{Command: []string{"true"}}, Next: "review"},
			{Name: "deploy", Type: apiv1.TaskDeterministic, Goal: "ship it", Run: &apiv1.DeterministicRun{Command: []string{"true"}}},
		},
		Gates: []apiv1.Gate{
			{
				Name:      "review",
				Evaluator: apiv1.EvaluatorAutomated,
				Automated: &apiv1.AutomatedGate{Check: "status-equals"},
				Branches:  map[string]string{"pass": "deploy", "fail": workflow.TargetAbort},
			},
		},
	}
	m, err := workflow.Compile(workflow.Definition{Name: "chained-through-gate", Version: 1, Spec: spec})
	if err != nil {
		t.Fatalf("compile chained-through-gate machine: %v", err)
	}
	return m
}

// newTestRunnerEnv builds the worktree manager, runs directory, and fixture
// repo a hand-built-journal resume test needs, without a byTask stub map
// (the crash-simulation tests dispatch through custom invoke.Deterministic
// implementations instead of stubDeterministic).
func newTestRunnerEnv(t *testing.T) (runsDir, fixtureRepo string, wtMgr *worktree.Manager) {
	t.Helper()
	instanceRoot := t.TempDir()
	mgr, err := worktree.NewManager(filepath.Join(instanceRoot, "workcopies"))
	if err != nil {
		t.Fatalf("new worktree manager: %v", err)
	}
	return filepath.Join(instanceRoot, "runs"), newFixtureRepo(t), mgr
}

// TestRunnerResumeAdvancesPastFinishedTaskWithoutRedispatch is #107's core
// acceptance scenario: a crash right after a task's stage.finished is
// journaled, before the machine advances past it (state.json's
// machineState still names the task — walk's SetMachineState timing).
// Resume's only pre-#107 guard, interruptedAttempt, reports "not
// interrupted" here (started == finished) — the exact gap that used to
// re-dispatch the completed stage from attempt 1, re-running its side
// effects.
func TestRunnerResumeAdvancesPastFinishedTaskWithoutRedispatch(t *testing.T) {
	machine := fixtureMachine(t)
	runsDir, fixtureRepo, wtMgr := newTestRunnerEnv(t)

	jr, err := journal.Create(runsDir, journal.RunIdentity{
		RunID: "run-finished-crash", Workflow: machine.Def.Name, WorkflowVersion: machine.Def.Version,
		WorkflowDigest: machine.Digest(), Gaggle: "acme-web", Trigger: journal.Trigger{Kind: journal.TriggerManual},
	}, nil)
	if err != nil {
		t.Fatalf("journal.Create: %v", err)
	}
	jr.SetMachineState("implement")
	if err := jr.Append(journal.Event{Type: journal.EventStageStarted, Stage: "implement", Attempt: 1}); err != nil {
		t.Fatalf("append stage.started: %v", err)
	}
	if err := jr.Append(journal.Event{Type: journal.EventStageFinished, Stage: "implement", Attempt: 1, Status: string(apiv1.ResultSuccess)}); err != nil {
		t.Fatalf("append stage.finished: %v", err)
	}
	// No further events: the crash lands here, before walk's next loop
	// iteration ever reassigns state to "review".
	if err := jr.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	counting := &countingDeterministic{}
	r, err := New(Config{
		NewDeterministic: func(ArtifactRecorder, SecretRegistrar) (invoke.Deterministic, error) { return counting, nil },
		Automated:        gate.NewAutomatedEvaluator(),
		Worktrees:        wtMgr,
		RunsDir:          runsDir,
		RepoCloneURL:     func(apiv1.RepoRef) (string, error) { return fixtureRepo, nil },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	res, err := r.Resume(context.Background(), ResumeInput{
		RunID:   "run-finished-crash",
		Machine: machine,
		RepoRef: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web", Branch: "main"},
	})
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if res.Phase != journal.PhaseCompleted {
		t.Fatalf("phase = %q, want completed", res.Phase)
	}
	if counting.calls != 0 {
		t.Fatalf("implement was re-dispatched %d times after resume, want 0 — an already-finished stage must never re-run its side effects", counting.calls)
	}

	rd, err := journal.OpenRead(filepath.Join(runsDir, "run-finished-crash"))
	if err != nil {
		t.Fatalf("OpenRead: %v", err)
	}
	events, err := rd.Events()
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	var starts int
	for _, e := range events {
		if e.Type == journal.EventStageStarted && e.Stage == "implement" {
			starts++
		}
	}
	if starts != 1 {
		t.Fatalf("stage.started(implement) count = %d, want 1 (the pre-crash one only — no re-dispatch)", starts)
	}
}

func TestRunnerResumePreservesSuccessfulInfrastructureRetry(t *testing.T) {
	machine := chainedThroughGateMachine(t)
	runsDir, fixtureRepo, wtMgr := newTestRunnerEnv(t)

	jr, err := journal.Create(runsDir, journal.RunIdentity{
		RunID: "run-infra-retry-crash", Workflow: machine.Def.Name, WorkflowVersion: machine.Def.Version,
		WorkflowDigest: machine.Digest(), Gaggle: "acme-web", Trigger: journal.Trigger{Kind: journal.TriggerManual},
	}, nil)
	if err != nil {
		t.Fatalf("journal.Create: %v", err)
	}
	jr.SetMachineState("implement")
	if err := jr.Append(journal.Event{Type: journal.EventStageStarted, Stage: "implement", Attempt: 1}); err != nil {
		t.Fatalf("append initial stage.started: %v", err)
	}
	if err := jr.Append(journal.Event{
		Type: journal.EventError, Stage: "implement", Attempt: 1,
		Error: &journal.ErrorDetail{Code: "executor_error", Message: "status 503: provider unavailable"},
	}); err != nil {
		t.Fatalf("append infrastructure error: %v", err)
	}
	if err := jr.Append(journal.Event{
		Type: journal.EventStageStarted, Stage: "implement", Attempt: 2, AttemptClass: journal.AttemptInfra,
	}); err != nil {
		t.Fatalf("append retry stage.started: %v", err)
	}
	artRef, err := jr.RecordArtifact("diff", []byte("--- a\n+++ b\n"))
	if err != nil {
		t.Fatalf("RecordArtifact: %v", err)
	}
	if err := jr.Append(journal.Event{
		Type: journal.EventStageFinished, Stage: "implement", Attempt: 2, AttemptClass: journal.AttemptInfra,
		Status: string(apiv1.ResultSuccess), Outputs: map[string]any{"recovered": true},
		Artifacts: []journal.Ref{{Path: artRef.Path, Digest: artRef.Digest, Size: artRef.Size}},
	}); err != nil {
		t.Fatalf("append retry stage.finished: %v", err)
	}
	if err := jr.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	capturing := &capturingDeterministic{}
	r, err := New(Config{
		NewDeterministic: func(ArtifactRecorder, SecretRegistrar) (invoke.Deterministic, error) { return capturing, nil },
		Automated:        gate.NewAutomatedEvaluator(),
		Worktrees:        wtMgr,
		RunsDir:          runsDir,
		RepoCloneURL:     func(apiv1.RepoRef) (string, error) { return fixtureRepo, nil },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	res, err := r.Resume(context.Background(), ResumeInput{
		RunID:   "run-infra-retry-crash",
		Machine: machine,
		RepoRef: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web", Branch: "main"},
	})
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if res.Phase != journal.PhaseCompleted {
		t.Fatalf("phase = %q, want completed", res.Phase)
	}
	if capturing.calls != 1 {
		t.Fatalf("executor calls = %d, want only downstream deploy; successful infrastructure retry must not redispatch", capturing.calls)
	}

	var found bool
	for _, cp := range capturing.lastEnv.ContextPointers {
		if cp.Name == "implement.artifact[0]" && cp.Artifact != nil && cp.Artifact.Digest == artRef.Digest {
			found = true
		}
	}
	if !found {
		t.Fatalf("deploy's ContextPointers = %+v, want artifact from successful infrastructure retry", capturing.lastEnv.ContextPointers)
	}
}

// TestRunnerResumeReconstructsUpstreamPointersForDownstreamStage is #107's
// pointer-visibility scenario: a stage that finished (and produced an
// artifact) before the crash must still be visible to a downstream stage as
// a ContextPointer after resume, even though resume advances past it
// without re-dispatching it (so there is no live in-process
// `pointers = append(pointers, produced...)` to rebuild it from).
func TestRunnerResumeReconstructsUpstreamPointersForDownstreamStage(t *testing.T) {
	machine := chainedThroughGateMachine(t)
	runsDir, fixtureRepo, wtMgr := newTestRunnerEnv(t)

	jr, err := journal.Create(runsDir, journal.RunIdentity{
		RunID: "run-ptr-crash", Workflow: machine.Def.Name, WorkflowVersion: machine.Def.Version,
		WorkflowDigest: machine.Digest(), Gaggle: "acme-web", Trigger: journal.Trigger{Kind: journal.TriggerManual},
	}, nil)
	if err != nil {
		t.Fatalf("journal.Create: %v", err)
	}
	jr.SetMachineState("implement")
	if err := jr.Append(journal.Event{Type: journal.EventStageStarted, Stage: "implement", Attempt: 1}); err != nil {
		t.Fatalf("append stage.started: %v", err)
	}
	artRef, err := jr.RecordArtifact("diff", []byte("--- a\n+++ b\n"))
	if err != nil {
		t.Fatalf("RecordArtifact: %v", err)
	}
	if err := jr.Append(journal.Event{
		Type: journal.EventStageFinished, Stage: "implement", Attempt: 1, Status: string(apiv1.ResultSuccess),
		Artifacts: []journal.Ref{{Path: artRef.Path, Digest: artRef.Digest, Size: artRef.Size}},
	}); err != nil {
		t.Fatalf("append stage.finished: %v", err)
	}
	if err := jr.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	capturing := &capturingDeterministic{}
	r, err := New(Config{
		NewDeterministic: func(ArtifactRecorder, SecretRegistrar) (invoke.Deterministic, error) { return capturing, nil },
		Automated:        gate.NewAutomatedEvaluator(),
		Worktrees:        wtMgr,
		RunsDir:          runsDir,
		RepoCloneURL:     func(apiv1.RepoRef) (string, error) { return fixtureRepo, nil },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	res, err := r.Resume(context.Background(), ResumeInput{
		RunID:   "run-ptr-crash",
		Machine: machine,
		RepoRef: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web", Branch: "main"},
	})
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if res.Phase != journal.PhaseCompleted {
		t.Fatalf("phase = %q, want completed", res.Phase)
	}
	if capturing.calls != 1 {
		t.Fatalf("deploy dispatched %d times, want exactly 1", capturing.calls)
	}

	var found bool
	for _, cp := range capturing.lastEnv.ContextPointers {
		if cp.Name == "implement.artifact[0]" && cp.Artifact != nil && cp.Artifact.Digest == artRef.Digest {
			found = true
		}
	}
	if !found {
		t.Fatalf("deploy's ContextPointers = %+v, want implement.artifact[0] pointing at digest %q (the pre-crash artifact, reconstructed from the journal)", capturing.lastEnv.ContextPointers, artRef.Digest)
	}
}

// TestRunnerResumeAtGateEvaluatesRealSubject is #108's core acceptance
// scenario: a crash after a task finishes but before its downstream gate
// ever evaluates leaves state.json pointing at the gate with no
// gate.evaluated event at all. Resume must reconstruct the finished task's
// REAL result as the gate's subject (status/outputs/artifacts, now
// journaled on stage.finished for exactly this) — not evaluate against a
// zero-value ResultEnvelope, which would make an automated status-equals
// check read status="" and always fail closed, wrongly aborting a run whose
// subject stage actually succeeded.
func TestRunnerResumeAtGateEvaluatesRealSubject(t *testing.T) {
	machine := fixtureMachine(t)
	runsDir, fixtureRepo, wtMgr := newTestRunnerEnv(t)

	jr, err := journal.Create(runsDir, journal.RunIdentity{
		RunID: "run-gate-crash", Workflow: machine.Def.Name, WorkflowVersion: machine.Def.Version,
		WorkflowDigest: machine.Digest(), Gaggle: "acme-web", Trigger: journal.Trigger{Kind: journal.TriggerManual},
	}, nil)
	if err != nil {
		t.Fatalf("journal.Create: %v", err)
	}
	jr.SetMachineState("implement")
	if err := jr.Append(journal.Event{Type: journal.EventStageStarted, Stage: "implement", Attempt: 1}); err != nil {
		t.Fatalf("append stage.started: %v", err)
	}
	if err := jr.Append(journal.Event{Type: journal.EventStageFinished, Stage: "implement", Attempt: 1, Status: string(apiv1.ResultSuccess)}); err != nil {
		t.Fatalf("append stage.finished: %v", err)
	}
	// walk's next loop iteration reaches the gate and checkpoints there
	// (SetMachineState("review")) before crashing — no gate.evaluated event
	// exists yet.
	jr.SetMachineState("review")
	if err := jr.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// Deliberately no canned output for "implement" — if resume ever
	// re-dispatches it (a #107 regression), stubDeterministic errors and
	// this test's phase assertion fails.
	det := &stubDeterministic{byTask: map[string]stubTaskResult{}}
	r, err := New(Config{
		NewDeterministic: func(ArtifactRecorder, SecretRegistrar) (invoke.Deterministic, error) { return det, nil },
		Automated:        gate.NewAutomatedEvaluator(),
		Worktrees:        wtMgr,
		RunsDir:          runsDir,
		RepoCloneURL:     func(apiv1.RepoRef) (string, error) { return fixtureRepo, nil },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	res, err := r.Resume(context.Background(), ResumeInput{
		RunID:   "run-gate-crash",
		Machine: machine,
		RepoRef: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web", Branch: "main"},
	})
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if res.Phase != journal.PhaseCompleted {
		t.Fatalf("phase = %q, want completed — the automated gate must see the real success status reconstructed from the journal, not a zero-value subject that always evaluates to fail", res.Phase)
	}

	rd, err := journal.OpenRead(filepath.Join(runsDir, "run-gate-crash"))
	if err != nil {
		t.Fatalf("OpenRead: %v", err)
	}
	events, err := rd.Events()
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	for _, e := range events {
		if e.Type == journal.EventGateEvaluated && e.Verdict != gate.OutcomePass {
			t.Fatalf("gate.evaluated verdict = %q, want %q", e.Verdict, gate.OutcomePass)
		}
	}
}

// TestRunnerResumeRefusesEmptyWorkflowDigest proves the #112 hardening fix:
// a run journal with no pinned WorkflowDigest (a corrupted or pre-WF-016
// run.yaml — Start always pins one, so this should never happen in
// practice) is refused, not silently resumed under whatever Machine the
// caller happens to pass — WF-016's whole point is that a changed
// definition is refused, not reinterpreted, and an unpinned run is
// indistinguishable from "we don't know if the definition changed."
// Since #520 a refusal reports the canonical PhaseFailed terminal instead
// of a bare error — resume_refusal_test.go covers that contract in depth;
// here it's enough that the run was not walked.
func TestRunnerResumeRefusesEmptyWorkflowDigest(t *testing.T) {
	machine := fixtureMachine(t)
	runsDir, fixtureRepo, wtMgr := newTestRunnerEnv(t)

	jr, err := journal.Create(runsDir, journal.RunIdentity{
		RunID: "run-no-digest", Workflow: machine.Def.Name, WorkflowVersion: machine.Def.Version,
		// WorkflowDigest deliberately left empty.
		Gaggle: "acme-web", Trigger: journal.Trigger{Kind: journal.TriggerManual},
	}, nil)
	if err != nil {
		t.Fatalf("journal.Create: %v", err)
	}
	jr.SetMachineState("implement")
	if err := jr.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	det := &countingDeterministic{}
	r, err := New(Config{
		NewDeterministic: func(ArtifactRecorder, SecretRegistrar) (invoke.Deterministic, error) { return det, nil },
		Automated:        gate.NewAutomatedEvaluator(),
		Worktrees:        wtMgr,
		RunsDir:          runsDir,
		RepoCloneURL:     func(apiv1.RepoRef) (string, error) { return fixtureRepo, nil },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	res, err := r.Resume(context.Background(), ResumeInput{
		RunID:   "run-no-digest",
		Machine: machine,
		RepoRef: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web", Branch: "main"},
	})
	if err != nil {
		t.Fatalf("Resume: %v — a refusal is handled terminally (#520), not surfaced as an error", err)
	}
	if res.Phase != journal.PhaseFailed {
		t.Fatalf("phase = %q, want failed — an unpinned run must be refused terminally, not silently resumed (WF-016)", res.Phase)
	}
	if det.calls != 0 {
		t.Fatalf("executor dispatched %d times, want 0 — a refused run must never be walked", det.calls)
	}
}

// blockingCommenter blocks inside UpdateWorkItem until released, recording
// the ctx it was called with (checked AFTER release, once the caller has
// had a chance to cancel it) — for proving NotifyEscalated survives a
// SIGTERM-style cancellation of the run-level context (#112) instead of
// racing it.
type blockingCommenter struct {
	called  chan struct{}
	release chan struct{}
	ctxErr  error
}

func (c *blockingCommenter) UpdateWorkItem(ctx context.Context, _ providers.UpdateWorkItemRequest) (providers.WorkItem, error) {
	close(c.called)
	<-c.release
	c.ctxErr = ctx.Err()
	return providers.WorkItem{}, nil
}

type recordingCommenter struct {
	requests []providers.UpdateWorkItemRequest
	err      error
}

func (c *recordingCommenter) UpdateWorkItem(_ context.Context, req providers.UpdateWorkItemRequest) (providers.WorkItem, error) {
	c.requests = append(c.requests, req)
	return providers.WorkItem{}, c.err
}

type fixedOutcomeAutomated string

func (a fixedOutcomeAutomated) Evaluate(context.Context, apiv1.AutomatedGate, apiv1.InvocationEnvelope) (string, error) {
	return string(a), nil
}

type fixedVerdictReviewer struct {
	verdict apiv1.Verdict
}

func (r *fixedVerdictReviewer) Invoke(context.Context, apiv1.InvocationEnvelope) (apiv1.ResultEnvelope, error) {
	return apiv1.ResultEnvelope{Status: apiv1.ResultSuccess}, nil
}

func (r *fixedVerdictReviewer) Review(context.Context, apiv1.InvocationEnvelope) (apiv1.Verdict, error) {
	return r.verdict, nil
}

func terminalCIGateMachine(t *testing.T) *workflow.Machine {
	t.Helper()
	spec := apiv1.WorkflowSpec{
		Gaggle:   "acme-web",
		Triggers: []apiv1.Trigger{{Type: apiv1.TriggerBacklogItem}},
		Start:    "ci-poll",
		Tasks: []apiv1.Task{
			{Name: "ci-poll", Type: apiv1.TaskDeterministic, Goal: "poll CI", Run: &apiv1.DeterministicRun{Command: []string{"true"}}, Next: "ci-gate"},
		},
		Gates: []apiv1.Gate{
			{
				Name:      "ci-gate",
				Evaluator: apiv1.EvaluatorAutomated,
				Automated: &apiv1.AutomatedGate{Check: "ci-status"},
				Branches: map[string]string{
					gate.OutcomePass:    workflow.TerminalComplete,
					gate.OutcomeFail:    workflow.TargetAbort,
					gate.OutcomeTimeout: workflow.TargetEscalate,
				},
			},
		},
	}
	m, err := workflow.Compile(workflow.Definition{Name: "terminal-ci-gate", Version: 1, Spec: spec})
	if err != nil {
		t.Fatalf("compile terminal CI gate machine: %v", err)
	}
	return m
}

func TestRunnerNotifiesExplicitGateEscalationOnce(t *testing.T) {
	commenter := &recordingCommenter{}
	r, _ := newTestRunner(t, map[string]stubTaskResult{
		"run-ci-timeout:ci-poll": {status: apiv1.ResultSuccess},
	}, fixedOutcomeAutomated(gate.OutcomeTimeout))
	r.cfg.Escalation = &gate.EscalationNotifier{
		Poster:     commenter,
		Repository: providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "acme", Name: "web"},
	}

	res, err := r.Start(context.Background(), StartInput{
		RunID:   "run-ci-timeout",
		Machine: terminalCIGateMachine(t),
		Gaggle:  "acme-web",
		Trigger: journal.Trigger{Kind: journal.TriggerManual},
		RepoRef: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web", Branch: "main"},
		Item:    &apiv1.BacklogItem{ID: "42", Provider: apiv1.ProviderGitHub, Title: "Fix CI"},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if res.Phase != journal.PhaseEscalated {
		t.Fatalf("phase = %q, want escalated", res.Phase)
	}
	if len(commenter.requests) != 1 {
		t.Fatalf("notification calls = %d, want 1", len(commenter.requests))
	}
	comment := commenter.requests[0].Comment
	for _, want := range []string{"ci-gate", gate.OutcomeTimeout, workflow.TargetEscalate} {
		if !strings.Contains(comment, want) {
			t.Fatalf("comment = %q, want it to contain %q", comment, want)
		}
	}
}

func TestRunnerTerminalGateNotificationFailureIsBestEffort(t *testing.T) {
	commenter := &recordingCommenter{err: fmt.Errorf("provider unavailable")}
	r, runsDir := newTestRunner(t, map[string]stubTaskResult{
		"run-notify-error:ci-poll": {status: apiv1.ResultSuccess},
	}, fixedOutcomeAutomated(gate.OutcomeTimeout))
	r.cfg.Escalation = &gate.EscalationNotifier{
		Poster:     commenter,
		Repository: providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "acme", Name: "web"},
	}

	res, err := r.Start(context.Background(), StartInput{
		RunID:   "run-notify-error",
		Machine: terminalCIGateMachine(t),
		Gaggle:  "acme-web",
		Trigger: journal.Trigger{Kind: journal.TriggerManual},
		RepoRef: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web", Branch: "main"},
		Item:    &apiv1.BacklogItem{ID: "42", Provider: apiv1.ProviderGitHub, Title: "Fix CI"},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if res.Phase != journal.PhaseEscalated {
		t.Fatalf("phase = %q, want escalated despite notification error", res.Phase)
	}

	rd, err := journal.OpenRead(filepath.Join(runsDir, "run-notify-error"))
	if err != nil {
		t.Fatalf("OpenRead: %v", err)
	}
	events, err := rd.Events()
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	for _, event := range events {
		if event.Type == journal.EventError && event.Error != nil && event.Error.Code == "gate_terminal_notification_failed" {
			return
		}
	}
	t.Fatal("notification failure was not recorded in the run journal")
}

func TestRunnerSkipsTerminalGateNotificationWithoutDrivingItem(t *testing.T) {
	commenter := &recordingCommenter{}
	r, _ := newTestRunner(t, map[string]stubTaskResult{
		"run-scheduled-timeout:ci-poll": {status: apiv1.ResultSuccess},
	}, fixedOutcomeAutomated(gate.OutcomeTimeout))
	r.cfg.Escalation = &gate.EscalationNotifier{Poster: commenter}

	res, err := r.Start(context.Background(), StartInput{
		RunID:   "run-scheduled-timeout",
		Machine: terminalCIGateMachine(t),
		Gaggle:  "acme-web",
		Trigger: journal.Trigger{Kind: journal.TriggerSchedule},
		RepoRef: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web", Branch: "main"},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if res.Phase != journal.PhaseEscalated {
		t.Fatalf("phase = %q, want escalated", res.Phase)
	}
	if len(commenter.requests) != 0 {
		t.Fatalf("notification calls = %d, want 0 without a driving item", len(commenter.requests))
	}
}

// TestRunnerNotifiesTerminalGateEscalationViaClaimLedgerFallback is #796's core
// acceptance scenario: a scheduled run (as `implementation` always fires) starts
// with no Item snapshot — it self-selects its backlog item mid-run — yet its
// escalation must still comment on the driving issue. Before the fix
// notifyTerminalGate no-oped on the nil Item and posted nothing; now it resolves
// the driving item id(s) via the configured ClaimedItems resolver (the same
// claim-ledger fallback buildBlockedHandler already uses).
func TestRunnerNotifiesTerminalGateEscalationViaClaimLedgerFallback(t *testing.T) {
	commenter := &recordingCommenter{}
	r, _ := newTestRunner(t, map[string]stubTaskResult{
		"run-scheduled-claimed:ci-poll": {status: apiv1.ResultSuccess},
	}, fixedOutcomeAutomated(gate.OutcomeTimeout))
	r.cfg.Escalation = &gate.EscalationNotifier{
		Poster:     commenter,
		Repository: providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "acme", Name: "web"},
	}
	var resolvedRunID string
	r.cfg.ClaimedItems = func(runID string) ([]string, error) {
		resolvedRunID = runID
		return []string{"57"}, nil
	}

	res, err := r.Start(context.Background(), StartInput{
		RunID:   "run-scheduled-claimed",
		Machine: terminalCIGateMachine(t),
		Gaggle:  "acme-web",
		Trigger: journal.Trigger{Kind: journal.TriggerSchedule},
		RepoRef: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web", Branch: "main"},
		// Item deliberately nil — a scheduled implementation dispatch.
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if res.Phase != journal.PhaseEscalated {
		t.Fatalf("phase = %q, want escalated", res.Phase)
	}
	if resolvedRunID != "run-scheduled-claimed" {
		t.Fatalf("ClaimedItems resolver called with run id %q, want %q", resolvedRunID, "run-scheduled-claimed")
	}
	if len(commenter.requests) != 1 {
		t.Fatalf("notification calls = %d, want 1 (resolved from the claim ledger)", len(commenter.requests))
	}
	if got := commenter.requests[0].ID; got != "57" {
		t.Fatalf("comment posted on item %q, want the claim-ledger-resolved %q", got, "57")
	}
}

// TestRunnerTerminalGateEscalationFanOutsToEveryClaimedItem covers the fan-out
// implementation case: a run that claims more than one backlog item comments on
// each, mirroring buildBlockedHandler's per-item loop.
func TestRunnerTerminalGateEscalationFanOutsToEveryClaimedItem(t *testing.T) {
	commenter := &recordingCommenter{}
	r, _ := newTestRunner(t, map[string]stubTaskResult{
		"run-multi-claimed:ci-poll": {status: apiv1.ResultSuccess},
	}, fixedOutcomeAutomated(gate.OutcomeTimeout))
	r.cfg.Escalation = &gate.EscalationNotifier{
		Poster:     commenter,
		Repository: providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "acme", Name: "web"},
	}
	r.cfg.ClaimedItems = func(string) ([]string, error) { return []string{"57", "58"}, nil }

	res, err := r.Start(context.Background(), StartInput{
		RunID:   "run-multi-claimed",
		Machine: terminalCIGateMachine(t),
		Gaggle:  "acme-web",
		Trigger: journal.Trigger{Kind: journal.TriggerSchedule},
		RepoRef: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web", Branch: "main"},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if res.Phase != journal.PhaseEscalated {
		t.Fatalf("phase = %q, want escalated", res.Phase)
	}
	var ids []string
	for _, req := range commenter.requests {
		ids = append(ids, req.ID)
	}
	if len(ids) != 2 || ids[0] != "57" || ids[1] != "58" {
		t.Fatalf("commented on items %v, want [57 58]", ids)
	}
}

// TestRunnerTerminalGateItemResolutionFailureIsBestEffort proves a claim-ledger
// resolver failure never gates the run's terminal transition: the failure is
// journaled (gate_terminal_item_resolution_failed) and swallowed, exactly like a
// NotifyEscalated provider error, and no comment is posted.
func TestRunnerTerminalGateItemResolutionFailureIsBestEffort(t *testing.T) {
	commenter := &recordingCommenter{}
	r, runsDir := newTestRunner(t, map[string]stubTaskResult{
		"run-resolve-error:ci-poll": {status: apiv1.ResultSuccess},
	}, fixedOutcomeAutomated(gate.OutcomeTimeout))
	r.cfg.Escalation = &gate.EscalationNotifier{
		Poster:     commenter,
		Repository: providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "acme", Name: "web"},
	}
	r.cfg.ClaimedItems = func(string) ([]string, error) { return nil, fmt.Errorf("claim ledger unreadable") }

	res, err := r.Start(context.Background(), StartInput{
		RunID:   "run-resolve-error",
		Machine: terminalCIGateMachine(t),
		Gaggle:  "acme-web",
		Trigger: journal.Trigger{Kind: journal.TriggerSchedule},
		RepoRef: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web", Branch: "main"},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if res.Phase != journal.PhaseEscalated {
		t.Fatalf("phase = %q, want escalated despite resolution error", res.Phase)
	}
	if len(commenter.requests) != 0 {
		t.Fatalf("notification calls = %d, want 0 when item resolution fails", len(commenter.requests))
	}

	rd, err := journal.OpenRead(filepath.Join(runsDir, "run-resolve-error"))
	if err != nil {
		t.Fatalf("OpenRead: %v", err)
	}
	events, err := rd.Events()
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	for _, event := range events {
		if event.Type == journal.EventError && event.Error != nil && event.Error.Code == "gate_terminal_item_resolution_failed" {
			return
		}
	}
	t.Fatal("item resolution failure was not recorded in the run journal")
}

func TestRunnerNotifiesGateAbortWithReviewerRationale(t *testing.T) {
	commenter := &recordingCommenter{}
	r, _ := newTestRunnerWithDeterministic(t, func(ArtifactRecorder, SecretRegistrar) (invoke.Deterministic, error) {
		return &committingDeterministic{t: t}, nil
	}, nil)
	reviewer := &fixedVerdictReviewer{verdict: apiv1.Verdict{
		Decision:  apiv1.VerdictFail,
		Rationale: "the implementation violates the fail-closed contract",
	}}
	r.cfg.NewAgentic = func(string, ArtifactRecorder, SecretRegistrar) (invoke.Goober, error) {
		return reviewer, nil
	}
	r.cfg.Escalation = &gate.EscalationNotifier{
		Poster:     commenter,
		Repository: providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "acme", Name: "web"},
	}

	res, err := r.Start(context.Background(), StartInput{
		RunID:   "run-review-abort",
		Machine: agenticGateMachine(t),
		Gaggle:  "acme-web",
		Trigger: journal.Trigger{Kind: journal.TriggerManual},
		RepoRef: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web", Branch: "main"},
		Item:    &apiv1.BacklogItem{ID: "42", Provider: apiv1.ProviderGitHub, Title: "Fix bug"},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if res.Phase != journal.PhaseAborted {
		t.Fatalf("phase = %q, want aborted", res.Phase)
	}
	if len(commenter.requests) != 1 {
		t.Fatalf("notification calls = %d, want 1", len(commenter.requests))
	}
	comment := commenter.requests[0].Comment
	for _, want := range []string{"review", workflow.TargetAbort, reviewer.verdict.Rationale} {
		if !strings.Contains(comment, want) {
			t.Fatalf("comment = %q, want it to contain %q", comment, want)
		}
	}
}

// repassLoopMachine is implement (always fails) -> review (automated gate,
// fail loops back to implement) — the same shape
// TestRunnerResumeRestoresGateRepassCounter uses, factored out here so this
// file's escalation test can drive it fresh via Start instead of a
// hand-built journal.
func repassLoopMachine(t *testing.T) *workflow.Machine {
	t.Helper()
	spec := apiv1.WorkflowSpec{
		Gaggle:   "acme-web",
		Triggers: []apiv1.Trigger{{Type: apiv1.TriggerBacklogItem}},
		Start:    "implement",
		Tasks: []apiv1.Task{
			{Name: "implement", Type: apiv1.TaskDeterministic, Goal: "produce a diff", Run: &apiv1.DeterministicRun{Command: []string{"true"}}, Next: "review"},
		},
		Gates: []apiv1.Gate{
			{
				Name:      "review",
				Evaluator: apiv1.EvaluatorAutomated,
				Automated: &apiv1.AutomatedGate{Check: "status-equals"},
				Branches:  map[string]string{"pass": workflow.TerminalComplete, "fail": "implement"},
			},
		},
	}
	m, err := workflow.Compile(workflow.Definition{Name: "repass-loop-escalate", Version: 1, Spec: spec})
	if err != nil {
		t.Fatalf("compile repass-loop machine: %v", err)
	}
	return m
}

// TestRunnerNotifyEscalatedSurvivesCancellation proves the #112 hardening
// fix: NotifyEscalated now runs on context.WithoutCancel, the same drain
// contract as every other post-decision effect (runTask's dispatch,
// evaluateGate) — a SIGTERM mid-notification must let it finish instead of
// racing it, so the escalation comment reliably posts exactly once instead
// of risking an aborted post that a later resume's re-evaluation would then
// duplicate.
func TestRunnerNotifyEscalatedSurvivesCancellation(t *testing.T) {
	machine := repassLoopMachine(t)
	byTask := map[string]stubTaskResult{
		"run-escalate:implement": {status: apiv1.ResultFailure, errorInfo: &apiv1.ErrorInfo{Code: "x", Message: "always fails"}},
	}
	runsDir, fixtureRepo, wtMgr := newTestRunnerEnv(t)

	commenter := &blockingCommenter{called: make(chan struct{}), release: make(chan struct{})}
	r, err := New(Config{
		NewDeterministic: func(rec ArtifactRecorder, _ SecretRegistrar) (invoke.Deterministic, error) {
			return &stubDeterministic{rec: rec, byTask: byTask}, nil
		},
		Automated:    gate.NewAutomatedEvaluator(),
		MaxRepasses:  0, // escalate on the very first non-pass evaluation
		Escalation:   &gate.EscalationNotifier{Poster: commenter, Repository: providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "acme", Name: "web"}},
		Worktrees:    wtMgr,
		RunsDir:      runsDir,
		RepoCloneURL: func(apiv1.RepoRef) (string, error) { return fixtureRepo, nil },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	type startResult struct {
		res Result
		err error
	}
	done := make(chan startResult, 1)
	go func() {
		res, err := r.Start(ctx, StartInput{
			RunID:   "run-escalate",
			Machine: machine,
			Gaggle:  "acme-web",
			Trigger: journal.Trigger{Kind: journal.TriggerManual},
			RepoRef: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web", Branch: "main"},
			Item:    &apiv1.BacklogItem{ID: "42", Provider: apiv1.ProviderGitHub, Title: "Fix bug"},
		})
		done <- startResult{res, err}
	}()

	select {
	case <-commenter.called:
	case <-time.After(5 * time.Second):
		t.Fatal("NotifyEscalated's UpdateWorkItem was never dispatched")
	}

	// Cancel WHILE the notification is blocked mid-flight, then release it —
	// proving the cancellation didn't abort the in-flight call.
	cancel()
	select {
	case <-done:
		t.Fatal("Start returned before the blocking notification was released")
	case <-time.After(50 * time.Millisecond):
	}
	close(commenter.release)

	select {
	case r := <-done:
		if r.err != nil {
			t.Fatalf("Start: %v", r.err)
		}
		if r.res.Phase != journal.PhaseEscalated {
			t.Fatalf("phase = %q, want escalated", r.res.Phase)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Start did not return after the blocking notification was released")
	}

	if commenter.ctxErr != nil {
		t.Fatalf("UpdateWorkItem's ctx.Err() = %v, want nil — the run-level cancellation must not have propagated to the escalation notification", commenter.ctxErr)
	}
}

func TestRunnerAutomaticEscalationDoesNotDoubleNotify(t *testing.T) {
	commenter := &recordingCommenter{}
	r, _ := newTestRunner(t, map[string]stubTaskResult{
		"run-auto-escalate:implement": {status: apiv1.ResultFailure, errorInfo: &apiv1.ErrorInfo{Code: "x", Message: "always fails"}},
	}, gate.NewAutomatedEvaluator())
	r.cfg.MaxRepasses = 1
	r.cfg.Escalation = &gate.EscalationNotifier{
		Poster:     commenter,
		Repository: providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "acme", Name: "web"},
	}

	res, err := r.Start(context.Background(), StartInput{
		RunID:   "run-auto-escalate",
		Machine: repassLoopMachine(t),
		Gaggle:  "acme-web",
		Trigger: journal.Trigger{Kind: journal.TriggerManual},
		RepoRef: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web", Branch: "main"},
		Item:    &apiv1.BacklogItem{ID: "42", Provider: apiv1.ProviderGitHub, Title: "Fix bug"},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if res.Phase != journal.PhaseEscalated {
		t.Fatalf("phase = %q, want escalated", res.Phase)
	}
	if len(commenter.requests) != 1 {
		t.Fatalf("notification calls = %d, want exactly 1", len(commenter.requests))
	}
	if !strings.Contains(commenter.requests[0].Comment, "repass budget exhausted") {
		t.Fatalf("comment = %q, want automatic escalation reason", commenter.requests[0].Comment)
	}
}

// TestRunnerEmitsRunTaskAndGateSpans is issue #126's runner-level acceptance:
// when Config.Telemetry is set, Start opens a run span before walking and a
// task/gate span for each stage dispatched, in walk order. Before this fix,
// Config had no Telemetry field at all and none of these were ever called.
func TestRunnerEmitsRunTaskAndGateSpans(t *testing.T) {
	machine := fixtureMachine(t)
	byTask := map[string]stubTaskResult{
		"run-span:implement": {status: apiv1.ResultSuccess},
	}
	instanceRoot := t.TempDir()
	wtMgr, err := worktree.NewManager(filepath.Join(instanceRoot, "workcopies"))
	if err != nil {
		t.Fatalf("new worktree manager: %v", err)
	}
	runsDir := filepath.Join(instanceRoot, "runs")
	fixtureRepo := newFixtureRepo(t)

	spans := &fakeSpanStarter{}
	r, err := New(Config{
		NewDeterministic: func(rec ArtifactRecorder, _ SecretRegistrar) (invoke.Deterministic, error) {
			return &stubDeterministic{rec: rec, byTask: byTask}, nil
		},
		Automated:    gate.NewAutomatedEvaluator(),
		Worktrees:    wtMgr,
		RunsDir:      runsDir,
		RepoCloneURL: func(apiv1.RepoRef) (string, error) { return fixtureRepo, nil },
		Telemetry:    spans,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	res, err := r.Start(context.Background(), StartInput{
		RunID:   "run-span",
		Machine: machine,
		Gaggle:  "acme-web",
		Trigger: journal.Trigger{Kind: journal.TriggerManual},
		RepoRef: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web", Branch: "main"},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if res.Phase != journal.PhaseCompleted {
		t.Fatalf("phase = %q, want completed", res.Phase)
	}

	want := []string{"run:run-span", "task:implement", "gate:review"}
	if !reflect.DeepEqual(spans.calls, want) {
		t.Fatalf("span calls = %v, want %v", spans.calls, want)
	}
}

func eventTypesEqual(got, want []journal.EventType) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

// --- #294: an agentic gate sources its reviewer goober's capabilities ---

// capturingReviewer is a fake invoke.Goober (agentic reviewer) that records the
// invocation envelope it was handed and returns a fixed pass verdict — so a
// test can assert exactly which capabilities the runner sourced into an agentic
// gate's envelope (#294). An agentic gate has no stage-level capabilities of
// its own, so this is the only observation point for the sourcing behavior.
type capturingReviewer struct {
	gotCaps     []string
	gotPointers []apiv1.ContextPointer
	called      bool
}

func (c *capturingReviewer) Invoke(context.Context, apiv1.InvocationEnvelope) (apiv1.ResultEnvelope, error) {
	return apiv1.ResultEnvelope{Status: apiv1.ResultSuccess}, nil
}

func (c *capturingReviewer) Review(_ context.Context, env apiv1.InvocationEnvelope) (apiv1.Verdict, error) {
	c.called = true
	c.gotCaps = env.Capabilities
	c.gotPointers = env.ContextPointers
	return apiv1.Verdict{Decision: apiv1.VerdictPass}, nil
}

// committingDeterministic is a deterministic stub that commits a real change on
// the run branch in its worktree — so a downstream agentic gate's runner-produced
// diff evidence (#301) is non-empty.
type committingDeterministic struct{ t *testing.T }

func (c *committingDeterministic) Run(_ context.Context, env apiv1.InvocationEnvelope, _ apiv1.DeterministicRun) (apiv1.ResultEnvelope, error) {
	c.t.Helper()
	if err := os.WriteFile(filepath.Join(env.Workspace, "impl.txt"), []byte("real implementation change\n"), 0o644); err != nil {
		return apiv1.ResultEnvelope{}, err
	}
	runGit(c.t, env.Workspace, "add", "-A")
	runGit(c.t, env.Workspace, "commit", "-m", "impl change")
	return apiv1.ResultEnvelope{Status: apiv1.ResultSuccess, Summary: "implemented"}, nil
}

// agenticGateMachine mirrors fixtureMachine but with an AGENTIC reviewer gate
// (goober "reviewer") instead of an automated one, for exercising #294's
// gate-envelope capability sourcing.
func agenticGateMachine(t *testing.T) *workflow.Machine {
	t.Helper()
	spec := apiv1.WorkflowSpec{
		Gaggle:   "acme-web",
		Triggers: []apiv1.Trigger{{Type: apiv1.TriggerBacklogItem}},
		Start:    "implement",
		Tasks: []apiv1.Task{
			{Name: "implement", Type: apiv1.TaskDeterministic, Goal: "produce a diff", Run: &apiv1.DeterministicRun{Command: []string{"true"}}, Next: "review"},
		},
		Gates: []apiv1.Gate{
			{
				Name:      "review",
				Evaluator: apiv1.EvaluatorAgentic,
				Agentic:   &apiv1.AgenticGate{Goober: "reviewer"},
				Branches: map[string]string{
					"pass":          workflow.TerminalComplete,
					"needs-changes": "implement",
					"fail":          workflow.TargetAbort,
				},
			},
		},
	}
	m, err := workflow.Compile(workflow.Definition{Name: "agentic-gate-fixture", Version: 1, Spec: spec})
	if err != nil {
		t.Fatalf("compile agentic gate machine: %v", err)
	}
	return m
}

// agenticImplementGateMachine is agenticGateMachine but with an AGENTIC
// implement stage (goober "coder"), for #415's empty-diff fast-fail, which
// fires only when the subject feeding the review gate is agentic — an agent
// that was supposed to commit work but produced nothing.
func agenticImplementGateMachine(t *testing.T) *workflow.Machine {
	t.Helper()
	spec := apiv1.WorkflowSpec{
		Gaggle:   "acme-web",
		Triggers: []apiv1.Trigger{{Type: apiv1.TriggerBacklogItem}},
		Start:    "implement",
		Tasks: []apiv1.Task{
			{Name: "implement", Type: apiv1.TaskAgentic, Goober: "coder", Goal: "produce a diff", Next: "review"},
		},
		Gates: []apiv1.Gate{
			{
				Name:      "review",
				Evaluator: apiv1.EvaluatorAgentic,
				Agentic:   &apiv1.AgenticGate{Goober: "reviewer"},
				Branches: map[string]string{
					"pass":          workflow.TerminalComplete,
					"needs-changes": "implement",
					"fail":          workflow.TargetAbort,
				},
			},
		},
	}
	m, err := workflow.Compile(workflow.Definition{Name: "agentic-implement-fixture", Version: 1, Spec: spec})
	if err != nil {
		t.Fatalf("compile agentic implement machine: %v", err)
	}
	return m
}

// newAgenticGateRunner mirrors newTestRunnerWithDeterministic but wires a fake
// agentic reviewer and a GateGooberCapabilities map, for #294's gate-envelope
// capability sourcing.
func newAgenticGateRunner(t *testing.T, byTask map[string]stubTaskResult, reviewer invoke.Goober, gateCaps map[string][]string) *Runner {
	t.Helper()
	instanceRoot := t.TempDir()
	wtMgr, err := worktree.NewManager(filepath.Join(instanceRoot, "workcopies"))
	if err != nil {
		t.Fatalf("new worktree manager: %v", err)
	}
	fixtureRepo := newFixtureRepo(t)
	r, err := New(Config{
		// Commit a real change on the implement stage so the agentic review
		// gate sees a non-empty diff — since #415 an empty diff fast-fails
		// before the reviewer runs, which would defeat a test that needs the
		// reviewer invoked (e.g. #294 gate-capability sourcing).
		NewDeterministic: func(rec ArtifactRecorder, _ SecretRegistrar) (invoke.Deterministic, error) {
			return &committingStubDeterministic{t: t, rec: rec, byTask: byTask}, nil
		},
		NewAgentic: func(string, ArtifactRecorder, SecretRegistrar) (invoke.Goober, error) {
			return reviewer, nil
		},
		GateGooberCapabilities: gateCaps,
		Worktrees:              wtMgr,
		RunsDir:                filepath.Join(instanceRoot, "runs"),
		RepoCloneURL:           func(apiv1.RepoRef) (string, error) { return fixtureRepo, nil },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return r
}

// TestRunnerAgenticGateSourcesGooberCapabilities is #294: an agentic gate
// (AgenticGate carries no stage-level capabilities) must source them from its
// reviewer goober's definition into the gate envelope — the only path by which
// the reviewer subprocess can be handed agent:model. A goober absent from the
// map sources nothing (fail-closed, no silent default).
func TestRunnerAgenticGateSourcesGooberCapabilities(t *testing.T) {
	byTask := map[string]stubTaskResult{"run-agentic-gate:implement": {status: apiv1.ResultSuccess}}
	start := func(t *testing.T, r *Runner) Result {
		t.Helper()
		res, err := r.Start(context.Background(), StartInput{
			RunID:   "run-agentic-gate",
			Machine: agenticGateMachine(t),
			Gaggle:  "acme-web",
			RepoRef: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web", Branch: "main"},
		})
		if err != nil {
			t.Fatalf("Start: %v", err)
		}
		return res
	}

	t.Run("sources the reviewer goober's declared capabilities", func(t *testing.T) {
		reviewer := &capturingReviewer{}
		r := newAgenticGateRunner(t, byTask, reviewer, map[string][]string{"reviewer": {"agent:model"}})
		if res := start(t, r); res.Phase != journal.PhaseCompleted {
			t.Fatalf("phase = %q, want completed", res.Phase)
		}
		if !reviewer.called {
			t.Fatal("reviewer was never invoked")
		}
		if got := reviewer.gotCaps; len(got) != 1 || got[0] != "agent:model" {
			t.Fatalf("gate envelope capabilities = %v, want [agent:model]", got)
		}
	})

	t.Run("no mapping means no capabilities (fail-closed)", func(t *testing.T) {
		reviewer := &capturingReviewer{}
		r := newAgenticGateRunner(t, byTask, reviewer, nil)
		_ = start(t, r)
		if !reviewer.called {
			t.Fatal("reviewer was never invoked")
		}
		if len(reviewer.gotCaps) != 0 {
			t.Fatalf("gate envelope capabilities = %v, want none (fail-closed, no silent default)", reviewer.gotCaps)
		}
	})
}

// TestRunnerAgenticGateAttachesReviewerDiffEvidence is #301: before invoking an
// agentic reviewer gate, the runner computes a diff of the run branch and
// attaches it as a digested evidence context pointer — so the reviewer judges
// the actual committed change, not a model-self-reported artifact.
func TestRunnerAgenticGateAttachesReviewerDiffEvidence(t *testing.T) {
	reviewer := &capturingReviewer{}
	instanceRoot := t.TempDir()
	wtMgr, err := worktree.NewManager(filepath.Join(instanceRoot, "workcopies"))
	if err != nil {
		t.Fatalf("new worktree manager: %v", err)
	}
	fixtureRepo := newFixtureRepo(t)
	r, err := New(Config{
		NewDeterministic: func(ArtifactRecorder, SecretRegistrar) (invoke.Deterministic, error) {
			return &committingDeterministic{t: t}, nil
		},
		NewAgentic: func(string, ArtifactRecorder, SecretRegistrar) (invoke.Goober, error) {
			return reviewer, nil
		},
		Worktrees:    wtMgr,
		RunsDir:      filepath.Join(instanceRoot, "runs"),
		RepoCloneURL: func(apiv1.RepoRef) (string, error) { return fixtureRepo, nil },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	res, err := r.Start(context.Background(), StartInput{
		RunID:   "run-diff-evidence",
		Machine: agenticGateMachine(t),
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
	if diffPtr == nil {
		t.Fatalf("reviewer got no runner-produced diff evidence pointer; pointers = %+v", reviewer.gotPointers)
	}
	if diffPtr.Artifact == nil || diffPtr.Artifact.Digest == "" {
		t.Fatalf("diff evidence pointer has no digested artifact: %+v", diffPtr)
	}
}

// alwaysNeedsChangesReviewer always requests changes and counts how many
// times it was actually invoked (#316's key observation point: a detected
// duplicate diff must skip the reviewer call entirely, not merely override
// its result afterward).
type alwaysNeedsChangesReviewer struct{ calls int }

func (r *alwaysNeedsChangesReviewer) Invoke(context.Context, apiv1.InvocationEnvelope) (apiv1.ResultEnvelope, error) {
	return apiv1.ResultEnvelope{Status: apiv1.ResultSuccess}, nil
}

func (r *alwaysNeedsChangesReviewer) Review(context.Context, apiv1.InvocationEnvelope) (apiv1.Verdict, error) {
	r.calls++
	return apiv1.Verdict{Decision: apiv1.VerdictNeedsChanges, Summary: "please fix X"}, nil
}

// repeatingDeterministic commits the exact same file content on every
// invocation, using --allow-empty once the tree is already unchanged from
// the prior attempt — reproducing #316's non-convergent-implementer failure
// mode (byte-identical diffs attempt after attempt) without a real stuck
// model.
type repeatingDeterministic struct{ t *testing.T }

func (c *repeatingDeterministic) Run(_ context.Context, env apiv1.InvocationEnvelope, _ apiv1.DeterministicRun) (apiv1.ResultEnvelope, error) {
	c.t.Helper()
	if err := os.WriteFile(filepath.Join(env.Workspace, "impl.txt"), []byte("real implementation change\n"), 0o644); err != nil {
		return apiv1.ResultEnvelope{}, err
	}
	runGit(c.t, env.Workspace, "add", "-A")
	runGit(c.t, env.Workspace, "commit", "--allow-empty", "-m", "impl change")
	return apiv1.ResultEnvelope{Status: apiv1.ResultSuccess, Summary: "implemented"}, nil
}

// TestRunnerEscalatesOnDuplicateRepassDiff is issue #316's end-to-end
// acceptance, driven through a real Start(): an implementer stuck producing
// the exact same diff attempt after attempt must escalate on the very first
// repeat, not after burning the full repass budget (MaxRepasses:3 here, so
// escalating after only 2 gate evaluations can only be explained by the
// duplicate-diff detection), and the reviewer must not be re-invoked for a
// diff it already judged.
func TestRunnerEscalatesOnDuplicateRepassDiff(t *testing.T) {
	reviewer := &alwaysNeedsChangesReviewer{}
	instanceRoot := t.TempDir()
	wtMgr, err := worktree.NewManager(filepath.Join(instanceRoot, "workcopies"))
	if err != nil {
		t.Fatalf("new worktree manager: %v", err)
	}
	fixtureRepo := newFixtureRepo(t)
	runsDir := filepath.Join(instanceRoot, "runs")
	r, err := New(Config{
		NewDeterministic: func(ArtifactRecorder, SecretRegistrar) (invoke.Deterministic, error) {
			return &repeatingDeterministic{t: t}, nil
		},
		NewAgentic: func(string, ArtifactRecorder, SecretRegistrar) (invoke.Goober, error) {
			return reviewer, nil
		},
		Worktrees:    wtMgr,
		RunsDir:      runsDir,
		RepoCloneURL: func(apiv1.RepoRef) (string, error) { return fixtureRepo, nil },
		MaxRepasses:  3,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	res, err := r.Start(context.Background(), StartInput{
		RunID:   "run-duplicate-diff",
		Machine: agenticGateMachine(t),
		Gaggle:  "acme-web",
		RepoRef: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web", Branch: "main"},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if res.Phase != journal.PhaseEscalated {
		t.Fatalf("phase = %q, want escalated (2nd identical-diff repass must escalate immediately)", res.Phase)
	}
	if reviewer.calls != 1 {
		t.Fatalf("reviewer.calls = %d, want 1 (must not be re-invoked on a detected duplicate diff)", reviewer.calls)
	}

	rd, err := journal.OpenRead(filepath.Join(runsDir, "run-duplicate-diff"))
	if err != nil {
		t.Fatalf("OpenRead: %v", err)
	}
	events, err := rd.Events()
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	var gateEvents []journal.Event
	for _, e := range events {
		if e.Type == journal.EventGateEvaluated {
			gateEvents = append(gateEvents, e)
		}
	}
	if len(gateEvents) != 2 {
		t.Fatalf("journaled gate.evaluated events = %d, want 2 (1st real review, 2nd detected duplicate)", len(gateEvents))
	}
	if dup, _ := gateEvents[1].Runner["duplicateDiff"].(bool); !dup {
		t.Fatalf("2nd gate.evaluated event Runner[duplicateDiff] = %v, want true", gateEvents[1].Runner["duplicateDiff"])
	}
	if gateEvents[1].Target != workflow.TargetEscalate {
		t.Fatalf("2nd gate.evaluated event target = %q, want %q", gateEvents[1].Target, workflow.TargetEscalate)
	}
}

// verdictAwareDeterministic records each invocation's InvocationEnvelope and
// produces genuinely different content each attempt (the attempt count baked
// into the file) — so successive repasses never trip the #316 identical-diff
// guard on their own, isolating this fixture's actual subject (issue #412:
// does the repass dispatch receive the reviewer's prior verdict as a
// ContextPointer) from #316's guard.
type verdictAwareDeterministic struct {
	t       *testing.T
	calls   int
	gotEnvs []apiv1.InvocationEnvelope
}

func (c *verdictAwareDeterministic) Run(_ context.Context, env apiv1.InvocationEnvelope, _ apiv1.DeterministicRun) (apiv1.ResultEnvelope, error) {
	c.t.Helper()
	c.calls++
	c.gotEnvs = append(c.gotEnvs, env)
	content := fmt.Sprintf("attempt %d content\n", c.calls)
	if err := os.WriteFile(filepath.Join(env.Workspace, "impl.txt"), []byte(content), 0o644); err != nil {
		return apiv1.ResultEnvelope{}, err
	}
	runGit(c.t, env.Workspace, "add", "-A")
	runGit(c.t, env.Workspace, "commit", "-m", fmt.Sprintf("impl change %d", c.calls))
	return apiv1.ResultEnvelope{Status: apiv1.ResultSuccess, Summary: "implemented"}, nil
}

// needsChangesThenPassReviewer requests changes on the first review and
// approves the second — simulating a repass that successfully engages with
// feedback and converges, the scenario #412 exists to make possible.
type needsChangesThenPassReviewer struct{ calls int }

func (r *needsChangesThenPassReviewer) Invoke(context.Context, apiv1.InvocationEnvelope) (apiv1.ResultEnvelope, error) {
	return apiv1.ResultEnvelope{Status: apiv1.ResultSuccess}, nil
}

func (r *needsChangesThenPassReviewer) Review(context.Context, apiv1.InvocationEnvelope) (apiv1.Verdict, error) {
	r.calls++
	if r.calls == 1 {
		return apiv1.Verdict{Decision: apiv1.VerdictNeedsChanges, Summary: "please fix X", Rationale: "specific, addressable finding"}, nil
	}
	return apiv1.Verdict{Decision: apiv1.VerdictPass, Summary: "looks good now"}, nil
}

// TestRunnerRepassReceivesReviewerVerdictAsContext is issue #412's end-to-end
// acceptance, driven through a real Start(): a repass dispatch back to
// "implement" must carry the gate's just-recorded verdict as a
// ContextPointer, resolving to the reviewer's actual decision/summary — not
// a placeholder — and a repass that engages with it (produces new content)
// must converge to completed rather than trip the #316 identical-diff guard.
func TestRunnerRepassReceivesReviewerVerdictAsContext(t *testing.T) {
	deterministic := &verdictAwareDeterministic{t: t}
	reviewer := &needsChangesThenPassReviewer{}
	instanceRoot := t.TempDir()
	wtMgr, err := worktree.NewManager(filepath.Join(instanceRoot, "workcopies"))
	if err != nil {
		t.Fatalf("new worktree manager: %v", err)
	}
	fixtureRepo := newFixtureRepo(t)
	runsDir := filepath.Join(instanceRoot, "runs")
	r, err := New(Config{
		NewDeterministic: func(ArtifactRecorder, SecretRegistrar) (invoke.Deterministic, error) {
			return deterministic, nil
		},
		NewAgentic: func(string, ArtifactRecorder, SecretRegistrar) (invoke.Goober, error) {
			return reviewer, nil
		},
		Worktrees:    wtMgr,
		RunsDir:      runsDir,
		RepoCloneURL: func(apiv1.RepoRef) (string, error) { return fixtureRepo, nil },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	res, err := r.Start(context.Background(), StartInput{
		RunID:   "run-verdict-context",
		Machine: agenticGateMachine(t),
		Gaggle:  "acme-web",
		RepoRef: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web", Branch: "main"},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	// The #316 identical-diff guard must NOT fire: the repass received the
	// verdict and produced genuinely new content, so the run converges.
	if res.Phase != journal.PhaseCompleted {
		t.Fatalf("phase = %q, want completed (a repass that engages with the verdict should converge, not escalate)", res.Phase)
	}
	if deterministic.calls != 2 {
		t.Fatalf("implement calls = %d, want 2 (1 initial + 1 repass)", deterministic.calls)
	}
	if reviewer.calls != 2 {
		t.Fatalf("reviewer calls = %d, want 2", reviewer.calls)
	}

	initialEnv, repassEnv := deterministic.gotEnvs[0], deterministic.gotEnvs[1]
	for _, cp := range initialEnv.ContextPointers {
		if cp.Name == "review.verdict" {
			t.Fatalf("initial implement envelope unexpectedly carries a review.verdict pointer (no gate has evaluated yet): %+v", cp)
		}
	}

	var verdictPtr *apiv1.ContextPointer
	for i := range repassEnv.ContextPointers {
		if repassEnv.ContextPointers[i].Name == "review.verdict" {
			verdictPtr = &repassEnv.ContextPointers[i]
		}
	}
	if verdictPtr == nil {
		t.Fatalf("repass envelope has no review.verdict ContextPointer; got %+v", repassEnv.ContextPointers)
	}
	if verdictPtr.Artifact == nil || verdictPtr.Artifact.Digest == "" {
		t.Fatalf("review.verdict pointer has no digested artifact: %+v", verdictPtr)
	}

	// The pointer must resolve to the ACTUAL verdict content (not a
	// placeholder) — read it back the same way materializeContext would.
	data, err := verdictPtr.Artifact.Resolve(filepath.Join(runsDir, "run-verdict-context"))
	if err != nil {
		t.Fatalf("resolve verdict artifact: %v", err)
	}
	var verdict apiv1.Verdict
	if err := json.Unmarshal(data, &verdict); err != nil {
		t.Fatalf("unmarshal verdict artifact: %v", err)
	}
	if verdict.Decision != apiv1.VerdictNeedsChanges || verdict.Summary != "please fix X" {
		t.Fatalf("resolved verdict = %+v, want the reviewer's actual 1st verdict", verdict)
	}

	rd, err := journal.OpenRead(filepath.Join(runsDir, "run-verdict-context"))
	if err != nil {
		t.Fatalf("OpenRead: %v", err)
	}
	events, err := rd.Events()
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	var manifests []contextManifest
	for _, event := range events {
		if event.Type != journal.EventArtifactRecorded || event.Name != "context/implement-attempt-1.json" {
			continue
		}
		if event.Stage != "implement" || event.Attempt != 1 || event.Ref == nil || event.Ref.Digest == "" {
			t.Fatalf("context manifest event is not a digested implement-stage artifact: %+v", event)
		}
		manifestData, err := rd.ArtifactBytes(*event.Ref)
		if err != nil {
			t.Fatalf("read context manifest artifact: %v", err)
		}
		var manifest contextManifest
		if err := json.Unmarshal(manifestData, &manifest); err != nil {
			t.Fatalf("unmarshal context manifest: %v", err)
		}
		manifests = append(manifests, manifest)
	}
	if len(manifests) != 2 {
		t.Fatalf("implement context manifests = %d, want 2 (initial + repass)", len(manifests))
	}
	for _, cp := range manifests[0].ContextPointers {
		if cp.Name == "review.verdict" {
			t.Fatalf("initial context manifest unexpectedly carries review.verdict: %+v", manifests[0])
		}
	}
	var manifestVerdict *apiv1.ContextPointer
	for i := range manifests[1].ContextPointers {
		if manifests[1].ContextPointers[i].Name == "review.verdict" {
			manifestVerdict = &manifests[1].ContextPointers[i]
		}
	}
	if manifestVerdict == nil {
		t.Fatalf("repass context manifest has no review.verdict pointer: %+v", manifests[1])
	}
	if !reflect.DeepEqual(*manifestVerdict, *verdictPtr) {
		t.Fatalf("manifest review.verdict = %+v, want the pointer actually supplied to the repass = %+v", *manifestVerdict, *verdictPtr)
	}
}

// committingFailingDeterministic commits a real (non-empty) diff and then
// returns status:failure with a configurable, non-retryable error code — the
// #415 shape: an implementer that did work but concludes the issue can't be
// completed. The committed diff keeps the control subtest's failure off the
// #415 empty-diff fast-fail path, so an unrecognized code genuinely reaches
// the reviewer rather than being fast-failed on an empty branch.
type committingFailingDeterministic struct {
	t    *testing.T
	code string
}

func (c *committingFailingDeterministic) Run(_ context.Context, env apiv1.InvocationEnvelope, _ apiv1.DeterministicRun) (apiv1.ResultEnvelope, error) {
	c.t.Helper()
	if err := os.WriteFile(filepath.Join(env.Workspace, "impl.txt"), []byte("partial work before giving up\n"), 0o644); err != nil {
		return apiv1.ResultEnvelope{}, err
	}
	runGit(c.t, env.Workspace, "add", "-A")
	runGit(c.t, env.Workspace, "commit", "-m", "partial")
	return apiv1.ResultEnvelope{
		Status:  apiv1.ResultFailure,
		Summary: "issue cannot be completed as a single change",
		Error:   &apiv1.ErrorInfo{Code: c.code, Message: "needs a human / decomposition", Retryable: false},
	}, nil
}

// TestRunnerEscalatesNonRetryableFailureDisposition is issue #415's end-to-end
// acceptance, driven through a real Start(): an implement stage that returns
// status:failure with error.retryable==false AND a recognized escalate code
// (ISSUE_OVER_SCOPE) terminates `escalated` on the FIRST attempt — bypassing
// the review gate and the repass loop entirely (the reviewer is never
// invoked). The control case proves the route is keyed on the recognized code,
// not merely on retryable==false: an unrecognized code still routes into the
// gate, where the reviewer evaluates it. Without the fix, the escalate-code
// failure routes into the review gate (needs-changes → implement) and
// terminates `aborted` only after the repass budget exhausts — inverted from
// the intended `escalated`.
func TestRunnerEscalatesNonRetryableFailureDisposition(t *testing.T) {
	start := func(t *testing.T, code string) (Result, *capturingReviewer) {
		t.Helper()
		reviewer := &capturingReviewer{}
		instanceRoot := t.TempDir()
		wtMgr, err := worktree.NewManager(filepath.Join(instanceRoot, "workcopies"))
		if err != nil {
			t.Fatalf("new worktree manager: %v", err)
		}
		fixtureRepo := newFixtureRepo(t)
		r, err := New(Config{
			NewDeterministic: func(ArtifactRecorder, SecretRegistrar) (invoke.Deterministic, error) {
				return &committingFailingDeterministic{t: t, code: code}, nil
			},
			NewAgentic: func(string, ArtifactRecorder, SecretRegistrar) (invoke.Goober, error) {
				return reviewer, nil
			},
			Worktrees:    wtMgr,
			RunsDir:      filepath.Join(instanceRoot, "runs"),
			RepoCloneURL: func(apiv1.RepoRef) (string, error) { return fixtureRepo, nil },
		})
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		res, err := r.Start(context.Background(), StartInput{
			RunID:   "run-escalate-disposition",
			Machine: agenticGateMachine(t),
			Gaggle:  "acme-web",
			RepoRef: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web", Branch: "main"},
		})
		if err != nil {
			t.Fatalf("Start: %v", err)
		}
		return res, reviewer
	}

	t.Run("recognized escalate code routes straight to @escalate, bypassing the gate", func(t *testing.T) {
		res, reviewer := start(t, "ISSUE_OVER_SCOPE")
		if res.Phase != journal.PhaseEscalated {
			t.Fatalf("phase = %q, want escalated (a non-retryable escalate-code failure escalates on attempt 1)", res.Phase)
		}
		if reviewer.called {
			t.Fatal("reviewer was invoked — the non-retryable escalation must bypass the review gate and its repass loop entirely")
		}
	})

	t.Run("control: an unrecognized code still routes into the gate", func(t *testing.T) {
		// retryable==false but the code is not a recognized escalate code, so
		// the ordinary failure route applies: into the review gate. The commit
		// gives it a non-empty diff, so the reviewer actually evaluates it
		// (here: pass → complete). Proves the escalation is keyed on the code.
		res, reviewer := start(t, "SOME_OTHER_FAILURE")
		if res.Phase != journal.PhaseCompleted {
			t.Fatalf("phase = %q, want completed (unrecognized code → into the gate → reviewer passes)", res.Phase)
		}
		if !reviewer.called {
			t.Fatal("reviewer was not invoked — an unrecognized failure code must still route into the gate, not escalate")
		}
	})
}

func TestRunnerRoutesNonRetryableFailureThroughGateEscalationBranch(t *testing.T) {
	const runID = "run-park-nonretryable"
	const summary = "The issue bundles independent changes and needs decomposition."
	byTask := map[string]stubTaskResult{
		runID + ":implement": {
			status:  apiv1.ResultFailure,
			summary: summary,
			errorInfo: &apiv1.ErrorInfo{
				Code: "NEEDS_DECOMPOSITION", Message: "implementation cannot continue", Retryable: false,
			},
		},
		runID + ":park-escalated": {status: apiv1.ResultSuccess},
	}
	r, runsDir := newTestRunner(t, byTask, nil)

	res, err := r.Start(context.Background(), StartInput{
		RunID:   runID,
		Machine: escalationParkingMachine(t),
		Gaggle:  "acme-web",
		RepoRef: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web", Branch: "main"},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if res.Phase != journal.PhaseEscalated || res.FinalState != "park-escalated" {
		t.Fatalf("result = %+v, want escalated after park-escalated", res)
	}

	rd, err := journal.OpenRead(filepath.Join(runsDir, runID))
	if err != nil {
		t.Fatalf("OpenRead: %v", err)
	}
	events, err := rd.Events()
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	var finished []string
	for _, event := range events {
		if event.Type == journal.EventGateEvaluated {
			t.Fatalf("non-retryable disposition evaluated review gate: %+v", event)
		}
		if event.Type != journal.EventStageFinished {
			continue
		}
		finished = append(finished, event.Stage)
		if event.Stage == "implement" && (event.Error == nil || event.Error.Message != summary) {
			t.Fatalf("implement error = %+v, want retained summary %q", event.Error, summary)
		}
	}
	if !reflect.DeepEqual(finished, []string{"implement", "park-escalated"}) {
		t.Fatalf("finished stages = %v, want implement then park-escalated", finished)
	}
}

// TestRunnerFastFailsEmptyDiffFromAgenticStage is #415's reviewer sibling,
// driven end to end: an AGENTIC implement stage that returns success but
// commits nothing (an empty diff) reaching an agentic review gate fast-`fail`s
// on review-1 — terminal `aborted` via the gate's fail branch — without ever
// invoking the reviewer. Exercises the runner wiring the gate-package unit test
// can't: evaluateGate detecting the empty diff from recordReviewerDiff's nil
// pointer AND confirming the subject stage is agentic before passing
// emptyDiff=true.
func TestRunnerFastFailsEmptyDiffFromAgenticStage(t *testing.T) {
	coder := &capturingReviewer{}    // its Invoke returns success, commits nothing
	reviewer := &capturingReviewer{} // must never be called on an empty diff
	instanceRoot := t.TempDir()
	wtMgr, err := worktree.NewManager(filepath.Join(instanceRoot, "workcopies"))
	if err != nil {
		t.Fatalf("new worktree manager: %v", err)
	}
	fixtureRepo := newFixtureRepo(t)
	r, err := New(Config{
		NewDeterministic: func(ArtifactRecorder, SecretRegistrar) (invoke.Deterministic, error) {
			return &stubDeterministic{}, nil
		},
		NewAgentic: func(gooberName string, _ ArtifactRecorder, _ SecretRegistrar) (invoke.Goober, error) {
			if gooberName == "reviewer" {
				return reviewer, nil
			}
			return coder, nil
		},
		Worktrees:    wtMgr,
		RunsDir:      filepath.Join(instanceRoot, "runs"),
		RepoCloneURL: func(apiv1.RepoRef) (string, error) { return fixtureRepo, nil },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	res, err := r.Start(context.Background(), StartInput{
		RunID:   "run-empty-agentic-diff",
		Machine: agenticImplementGateMachine(t),
		Gaggle:  "acme-web",
		RepoRef: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web", Branch: "main"},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if res.Phase != journal.PhaseAborted {
		t.Fatalf("phase = %q, want aborted (an agentic stage's empty diff fast-fails to the gate's fail→@abort branch on review-1)", res.Phase)
	}
	if reviewer.called {
		t.Fatal("reviewer was invoked — an agentic stage's empty diff must fast-fail on review-1 without a reviewer call")
	}
}

// TestRunnerDeterministicSubjectEmptyDiffStillReviews is #415's collision guard
// for merge-review: an agentic review gate fed by a DETERMINISTIC subject stage
// that commits nothing (its reviewer judges PRs/outputs, not a run-branch diff)
// must NOT fast-fail on the empty diff — the reviewer still runs against its
// real evidence. Without the agentic-subject scoping, the blanket empty-diff
// fast-fail would bypass merge-review's reviewer and feed it a bogus fail.
func TestRunnerDeterministicSubjectEmptyDiffStillReviews(t *testing.T) {
	runID := "run-det-subject-empty-diff"
	// A deterministic implement returns success but commits nothing → empty diff.
	byTask := map[string]stubTaskResult{runID + ":implement": {status: apiv1.ResultSuccess, summary: "no run-branch commit"}}
	reviewer := &capturingReviewer{} // returns pass → the run completes
	instanceRoot := t.TempDir()
	wtMgr, err := worktree.NewManager(filepath.Join(instanceRoot, "workcopies"))
	if err != nil {
		t.Fatalf("new worktree manager: %v", err)
	}
	fixtureRepo := newFixtureRepo(t)
	r, err := New(Config{
		NewDeterministic: func(rec ArtifactRecorder, _ SecretRegistrar) (invoke.Deterministic, error) {
			return &stubDeterministic{rec: rec, byTask: byTask}, nil
		},
		NewAgentic: func(string, ArtifactRecorder, SecretRegistrar) (invoke.Goober, error) {
			return reviewer, nil
		},
		Worktrees:    wtMgr,
		RunsDir:      filepath.Join(instanceRoot, "runs"),
		RepoCloneURL: func(apiv1.RepoRef) (string, error) { return fixtureRepo, nil },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	res, err := r.Start(context.Background(), StartInput{
		RunID:   runID,
		Machine: agenticGateMachine(t), // deterministic implement → agentic review
		Gaggle:  "acme-web",
		RepoRef: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web", Branch: "main"},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if res.Phase != journal.PhaseCompleted {
		t.Fatalf("phase = %q, want completed (a deterministic subject's empty diff must NOT fast-fail — the reviewer still runs and passes)", res.Phase)
	}
	if !reviewer.called {
		t.Fatal("reviewer was NOT invoked — a deterministic subject's empty diff must still reach the reviewer (the merge-review case)")
	}
}

// TestRunnerSkipsReviewerOnCachedVerdictOutput is issue #523's end-to-end
// wiring proof: when the subject stage feeding an agentic review gate
// emits a "cachedVerdictJson" output (merge-review's gather-sibling-context,
// having already found a digest-matched prior verdict on the selected PR's
// own comment thread), evaluateGate must decode it, set
// gate.Evaluator.CachedVerdict, and never invoke the reviewer goober at
// all — the actual mechanism the fetch-level cache test (cmd/goobers) and
// the gate-level unit test (internal/gate) can each only prove one half of.
// The journaled gate.evaluated event must carry the cached verdict's own
// Digest/SourceRunID unchanged, so a human or `goobers trace` reading THIS
// run's journal can still find which run originally produced it.
func TestRunnerSkipsReviewerOnCachedVerdictOutput(t *testing.T) {
	runID := "run-cached-verdict"
	cached := apiv1.Verdict{
		Decision: apiv1.VerdictPass, Summary: "reused from a prior run",
		Digest: "sha256:cache-hit-digest", SourceRunID: "run-original-producer",
	}
	cachedJSON, err := json.Marshal(cached)
	if err != nil {
		t.Fatalf("marshal cached verdict: %v", err)
	}
	byTask := map[string]stubTaskResult{
		runID + ":implement": {
			status: apiv1.ResultSuccess, summary: "gathered sibling context",
			outputs: map[string]interface{}{"cachedVerdictJson": string(cachedJSON)},
		},
	}
	reviewer := &capturingReviewer{}
	instanceRoot := t.TempDir()
	wtMgr, err := worktree.NewManager(filepath.Join(instanceRoot, "workcopies"))
	if err != nil {
		t.Fatalf("new worktree manager: %v", err)
	}
	fixtureRepo := newFixtureRepo(t)
	r, err := New(Config{
		NewDeterministic: func(rec ArtifactRecorder, _ SecretRegistrar) (invoke.Deterministic, error) {
			return &stubDeterministic{rec: rec, byTask: byTask}, nil
		},
		NewAgentic: func(string, ArtifactRecorder, SecretRegistrar) (invoke.Goober, error) {
			return reviewer, nil
		},
		Worktrees:    wtMgr,
		RunsDir:      filepath.Join(instanceRoot, "runs"),
		RepoCloneURL: func(apiv1.RepoRef) (string, error) { return fixtureRepo, nil },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	res, err := r.Start(context.Background(), StartInput{
		RunID:   runID,
		Machine: agenticGateMachine(t),
		Gaggle:  "acme-web",
		RepoRef: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web", Branch: "main"},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if res.Phase != journal.PhaseCompleted {
		t.Fatalf("phase = %q, want completed (the cached verdict's own pass decision resolves the gate's pass branch)", res.Phase)
	}
	if reviewer.called {
		t.Fatal("reviewer WAS invoked — a cachedVerdictJson output must short-circuit the reviewer call entirely (#523)")
	}

	rd, err := journal.OpenRead(filepath.Join(instanceRoot, "runs", runID))
	if err != nil {
		t.Fatalf("OpenRead: %v", err)
	}
	events, err := rd.Events()
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	var gateEvent *journal.Event
	for i := range events {
		if events[i].Type == journal.EventGateEvaluated {
			gateEvent = &events[i]
		}
	}
	if gateEvent == nil {
		t.Fatal("no gate.evaluated event journaled")
	}
	if hit, _ := gateEvent.Runner["verdictCacheHit"].(bool); !hit {
		t.Fatalf("gate.evaluated Runner[verdictCacheHit] = %v, want true", gateEvent.Runner["verdictCacheHit"])
	}
	if gateEvent.Ref == nil {
		t.Fatal("gate.evaluated has no journaled verdict artifact")
	}
	data, err := rd.ArtifactBytes(*gateEvent.Ref)
	if err != nil {
		t.Fatalf("read verdict artifact: %v", err)
	}
	var journaled apiv1.Verdict
	if err := json.Unmarshal(data, &journaled); err != nil {
		t.Fatalf("unmarshal verdict artifact: %v", err)
	}
	if journaled.Digest != cached.Digest || journaled.SourceRunID != cached.SourceRunID {
		t.Fatalf("journaled verdict = %+v, want the cached verdict's Digest/SourceRunID preserved unchanged (%+v)", journaled, cached)
	}
}

// capturingAutomated records the envelope it evaluates, so a test can assert an
// automated gate never receives runner-produced reviewer-diff evidence (#301).
type capturingAutomated struct{ gotPointers []apiv1.ContextPointer }

func (c *capturingAutomated) Evaluate(_ context.Context, _ apiv1.AutomatedGate, env apiv1.InvocationEnvelope) (string, error) {
	c.gotPointers = env.ContextPointers
	return "pass", nil
}

// TestRunnerAutomatedGateGetsNoDiffEvidence pins #301's agentic-only guard: the
// runner-produced diff evidence is attached ONLY for an agentic reviewer gate.
// An automated gate (which runs no goober and needs no evidence) never receives
// it — even directly after a stage that committed a real change. Same seam
// discipline as #294's credential guard.
func TestRunnerAutomatedGateGetsNoDiffEvidence(t *testing.T) {
	automated := &capturingAutomated{}
	instanceRoot := t.TempDir()
	wtMgr, err := worktree.NewManager(filepath.Join(instanceRoot, "workcopies"))
	if err != nil {
		t.Fatalf("new worktree manager: %v", err)
	}
	fixtureRepo := newFixtureRepo(t)
	r, err := New(Config{
		NewDeterministic: func(ArtifactRecorder, SecretRegistrar) (invoke.Deterministic, error) {
			return &committingDeterministic{t: t}, nil
		},
		Automated:    automated,
		Worktrees:    wtMgr,
		RunsDir:      filepath.Join(instanceRoot, "runs"),
		RepoCloneURL: func(apiv1.RepoRef) (string, error) { return fixtureRepo, nil },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	res, err := r.Start(context.Background(), StartInput{
		RunID:   "run-automated-nodiff",
		Machine: fixtureMachine(t),
		Gaggle:  "acme-web",
		RepoRef: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web", Branch: "main"},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if res.Phase != journal.PhaseCompleted {
		t.Fatalf("phase = %q, want completed", res.Phase)
	}
	for _, p := range automated.gotPointers {
		if p.Name == "review.diff" {
			t.Fatalf("automated gate must not receive runner diff evidence, got pointer %q", p.Name)
		}
	}
}

// erroringAutomated is an invoke.Automated whose evaluation returns a Go error
// (not a business outcome), driving the runner's gate-eval error path into
// failTerminal — for #305's journaled-cause guard.
type erroringAutomated struct{}

func (erroringAutomated) Evaluate(context.Context, apiv1.AutomatedGate, apiv1.InvocationEnvelope) (string, error) {
	return "", fmt.Errorf("boom: evaluator exploded")
}

// TestRunnerFailTerminalJournalsCause is #305: a walk-level failure (here a gate
// evaluator that errors) must journal an error event carrying the actual cause,
// not just the bare run.finished{failed} marker — otherwise the run dies with
// zero explanation reachable via the journal or `goobers trace`.
func TestRunnerFailTerminalJournalsCause(t *testing.T) {
	byTask := map[string]stubTaskResult{"run-failterm:implement": {status: apiv1.ResultSuccess}}
	r, runsDir := newTestRunner(t, byTask, erroringAutomated{})

	res, err := r.Start(context.Background(), StartInput{
		RunID:   "run-failterm",
		Machine: fixtureMachine(t),
		Gaggle:  "acme-web",
		RepoRef: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web", Branch: "main"},
	})
	if err == nil {
		t.Fatal("expected the gate-eval error to surface up the call stack")
	}
	if res.Phase != journal.PhaseFailed {
		t.Fatalf("phase = %q, want failed", res.Phase)
	}

	rd, oerr := journal.OpenRead(filepath.Join(runsDir, "run-failterm"))
	if oerr != nil {
		t.Fatalf("OpenRead: %v", oerr)
	}
	events, eerr := rd.Events()
	if eerr != nil {
		t.Fatalf("Events: %v", eerr)
	}
	var found bool
	for _, e := range events {
		if e.Type == journal.EventError && e.Error != nil && e.Error.Code == "run_failed" &&
			strings.Contains(e.Error.Message, "boom: evaluator exploded") {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected a run_failed error event carrying the cause, got events: %+v", events)
	}
}

func TestDefaultRepoCloneURL(t *testing.T) {
	tests := []struct {
		name    string
		ref     apiv1.RepoRef
		want    string
		wantErr string
	}{
		{
			name: "github",
			ref:  apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web"},
			want: "https://github.com/acme/web.git",
		},
		{
			name: "azure devops",
			ref:  apiv1.RepoRef{Provider: apiv1.ProviderADO, Owner: "acme/widgets", Name: "web"},
			want: "https://dev.azure.com/acme/widgets/_git/web",
		},
		{
			name: "azure devops escapes path segments",
			ref:  apiv1.RepoRef{Provider: apiv1.ProviderADO, Owner: "acme/widgets project", Name: "web app"},
			want: "https://dev.azure.com/acme/widgets%20project/_git/web%20app",
		},
		{
			name:    "unknown provider",
			ref:     apiv1.RepoRef{Provider: "unknown", Owner: "acme", Name: "web"},
			wantErr: `runner: unsupported repo provider "unknown"`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := defaultRepoCloneURL(tt.ref)
			if tt.wantErr != "" {
				if err == nil || err.Error() != tt.wantErr {
					t.Fatalf("defaultRepoCloneURL() error = %v, want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("defaultRepoCloneURL() error = %v", err)
			}
			if got != tt.want {
				t.Fatalf("defaultRepoCloneURL() = %q, want %q", got, tt.want)
			}
		})
	}
}
