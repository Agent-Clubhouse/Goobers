// Package engine_test hosts the cross-runner workspace-provisioning fixture
// (#2878). It is an EXTERNAL test package on purpose: the fixture drives the
// engine through the PRODUCTION provisioner, workerhost.WorktreeWorkspaces,
// which imports internal/engine and therefore cannot be reached from an
// in-package test. Faking the provisioner would prove only that a fake
// reports what it was told to; the classification under test reads git's own
// exit code and output, so the failure has to be a real one.
package engine_test

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"go.temporal.io/sdk/testsuite"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/engine"
	"github.com/goobers/goobers/internal/gate"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/runner"
	"github.com/goobers/goobers/internal/temporaltest"
	"github.com/goobers/goobers/internal/testgit"
	"github.com/goobers/goobers/internal/workerhost"
	wf "github.com/goobers/goobers/internal/workflow"
	"github.com/goobers/goobers/internal/worktree"
)

// newProvisionParityWorkspaces is the engine side's provisioner: the real
// worker-side one, pointed at the fixture remote.
func newProvisionParityWorkspaces(mgr *worktree.Manager, scratchDir, repoURL string) engine.WorkspaceProvisioner {
	return &workerhost.WorktreeWorkspaces{
		Manager:    mgr,
		ScratchDir: scratchDir,
		CloneURL:   func(apiv1.RepoRef) (string, error) { return repoURL, nil },
	}
}

// provisionParityGaggle and provisionParityRepo are the shared fixture
// identity both sides walk.
const provisionParityGaggle = "acme-web"

var provisionParityRepo = apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web"}

// countingDeterministic records how many times a stage actually reached the
// executor. A provisioning failure must never reach it: the workspace is
// built before the stage is dispatched, so a nonzero count on a run that
// failed to provision would mean the driver dispatched a partial envelope.
type countingDeterministic struct {
	mu    sync.Mutex
	calls int
}

func (c *countingDeterministic) Run(context.Context, apiv1.InvocationEnvelope, apiv1.DeterministicRun) (apiv1.ResultEnvelope, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls++
	return apiv1.ResultEnvelope{Status: apiv1.ResultSuccess}, nil
}

func (c *countingDeterministic) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls
}

// provisionParitySpec is a single repo-workspace stage — the smallest fixture
// that provisions a working copy at all, which is the only thing this row
// compares.
func provisionParitySpec() apiv1.WorkflowSpec {
	return apiv1.WorkflowSpec{
		Gaggle: provisionParityGaggle,
		Start:  "implement",
		Tasks: []apiv1.Task{{
			Name: "implement",
			Type: apiv1.TaskDeterministic,
			Goal: "provision a workspace",
			Run:  &apiv1.DeterministicRun{Command: []string{"true"}},
		}},
	}
}

// newProvisionParityRemote serves a bare fixture repository over git's dumb
// HTTP protocol, answering the first `failures` requests with failureStatus
// (every request when failures < 0) before serving normally.
//
// A real remote, not a hand-built error: both drivers classify on the typed
// git command error — exit code plus git's own combined output — so an
// injected lookalike would only assert that the fixture matches the fixture.
// This is internal/runner's own #572 recipe (newHTTPGitRemote), reused here
// so the two sides of the comparison fail for identical reasons.
func newProvisionParityRemote(t *testing.T, failureStatus int, failures int32) string {
	t.Helper()
	bare := newProvisionParityRepo(t)
	runProvisionParityGit(t, bare, "-c", "safe.bareRepository=all", "update-server-info")

	files := http.FileServer(http.Dir(filepath.Dir(bare)))
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if request := requests.Add(1); failures < 0 || request <= failures {
			if failureStatus == http.StatusUnauthorized {
				w.Header().Set("WWW-Authenticate", `Basic realm="test"`)
			}
			http.Error(w, http.StatusText(failureStatus), failureStatus)
			return
		}
		files.ServeHTTP(w, req)
	}))
	t.Cleanup(server.Close)
	return server.URL + "/" + filepath.Base(bare)
}

func newProvisionParityRepo(t *testing.T) string {
	t.Helper()
	work := t.TempDir()
	bare := filepath.Join(t.TempDir(), "fixture.git")
	runProvisionParityGit(t, work, "init", "--initial-branch=main")
	runProvisionParityGit(t, work, "config", "user.email", "test@example.com")
	runProvisionParityGit(t, work, "config", "user.name", "test")
	if err := os.WriteFile(filepath.Join(work, "README.md"), []byte("fixture\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runProvisionParityGit(t, work, "add", "README.md")
	runProvisionParityGit(t, work, "commit", "-m", "initial")
	runProvisionParityGit(t, "", "clone", "--bare", work, bare)
	return bare
}

func runProvisionParityGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := testgit.Command(args...)
	if dir != "" {
		cmd.Dir = dir
	}
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Run(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out.String())
	}
}

// provisionParityOutcome is one driver's observation of the fixture: whether
// the walk survived, what it said if it did not, how many attempts the stage
// was given, the class each attempt carried, and how many times the executor
// was actually reached.
type provisionParityOutcome struct {
	driver       string
	err          error
	attempts     []journal.AttemptClass
	errorCodes   []string
	errorMessage string
	execCalls    int
}

func (o provisionParityOutcome) String() string {
	return fmt.Sprintf("%s{err=%v attempts=%s execCalls=%d}", o.driver, o.err, formatAttemptClasses(o.attempts), o.execCalls)
}

// formatAttemptClasses quotes each class so the NORMATIVE empty one (attempt 1
// always carries "") is visible rather than rendering as a gap in the slice.
func formatAttemptClasses(classes []journal.AttemptClass) string {
	parts := make([]string, 0, len(classes))
	for _, c := range classes {
		parts = append(parts, fmt.Sprintf("%q", c))
	}
	return "[" + strings.Join(parts, " ") + "]"
}

// runProvisionParityRunner walks the fixture through the local runner against
// repoURL.
func runProvisionParityRunner(t *testing.T, runID, repoURL, baseBranch string) provisionParityOutcome {
	t.Helper()
	exec := &countingDeterministic{}
	instanceRoot := t.TempDir()
	wtMgr, err := worktree.NewManager(filepath.Join(instanceRoot, "workcopies"))
	if err != nil {
		t.Fatalf("new worktree manager: %v", err)
	}
	runsDir := filepath.Join(instanceRoot, "runs")
	r, err := runner.New(runner.Config{
		NewDeterministic: func(runner.ArtifactRecorder, runner.SecretRegistrar) (invoke.Deterministic, error) {
			return exec, nil
		},
		Automated:    gate.NewAutomatedEvaluator(),
		Worktrees:    wtMgr,
		RunsDir:      runsDir,
		ScratchDir:   filepath.Join(instanceRoot, "scratch"),
		RepoCloneURL: func(apiv1.RepoRef) (string, error) { return repoURL, nil },
	})
	if err != nil {
		t.Fatalf("runner.New: %v", err)
	}
	machine, err := wf.Compile(wf.Definition{Name: "provision-parity", Version: 1, Spec: provisionParitySpec()})
	if err != nil {
		t.Fatalf("compile fixture: %v", err)
	}
	repoRef := provisionParityRepo
	repoRef.Branch = baseBranch
	_, startErr := r.Start(context.Background(), runner.StartInput{
		RunID:   runID,
		Machine: machine,
		Gaggle:  provisionParityGaggle,
		Trigger: journal.Trigger{Kind: journal.TriggerManual},
		RepoRef: repoRef,
	})
	out := provisionParityOutcome{driver: "runner", err: startErr, execCalls: exec.count()}
	readProvisionParityJournal(t, filepath.Join(runsDir, runID), &out)
	return out
}

// runProvisionParityEngine walks the same fixture through the engine, with the
// production worker-side provisioner (workerhost.WorktreeWorkspaces) over the
// same kind of worktree manager the runner uses.
func runProvisionParityEngine(t *testing.T, runID, repoURL, baseBranch string) provisionParityOutcome {
	t.Helper()
	exec := &countingDeterministic{}
	instanceRoot := t.TempDir()
	wtMgr, err := worktree.NewManager(filepath.Join(instanceRoot, "workcopies"))
	if err != nil {
		t.Fatalf("new worktree manager: %v", err)
	}
	repoRef := provisionParityRepo
	repoRef.Branch = baseBranch
	preview := true
	in := engine.RunInput{
		RunID:                  runID,
		Gaggle:                 provisionParityGaggle,
		WorkflowName:           "provision-parity",
		Version:                1,
		PreviewFeaturesEnabled: &preview,
		Spec:                   provisionParitySpec(),
		RepoRef:                repoRef,
		TriggerKind:            string(journal.TriggerManual),
	}
	var ts testsuite.WorkflowTestSuite
	env := temporaltest.NewWorkflowEnvironment(&ts)
	env.RegisterActivity(&engine.Activities{
		Det:  exec,
		Auto: gate.NewAutomatedEvaluator(),
		Workspaces: newProvisionParityWorkspaces(
			wtMgr, filepath.Join(instanceRoot, "scratch"), repoURL),
	})
	env.ExecuteWorkflow(engine.Run, in)
	out := provisionParityOutcome{driver: "engine", err: env.GetWorkflowError(), execCalls: exec.count()}
	readProvisionParityJournal(t, projectProvisionParityJournal(t, env), &out)
	return out
}

// projectProvisionParityJournal projects the engine run's history into a
// journal directory, the same path every other engine fixture reads back.
func projectProvisionParityJournal(t *testing.T, env *testsuite.TestWorkflowEnvironment) string {
	t.Helper()
	val, err := env.QueryWorkflow(engine.JournalQuery)
	if err != nil {
		t.Fatalf("query journal projection: %v", err)
	}
	var proj engine.JournalProjection
	if err := val.Get(&proj); err != nil {
		t.Fatalf("decode journal projection: %v", err)
	}
	dir, err := engine.ProjectRun(filepath.Join(t.TempDir(), "runs"), proj)
	if err != nil {
		t.Fatalf("ProjectRun: %v", err)
	}
	return dir
}

// readProvisionParityJournal fills in the stage's attempt classes and error
// records from a projected run directory.
func readProvisionParityJournal(t *testing.T, dir string, out *provisionParityOutcome) {
	t.Helper()
	rd, err := journal.OpenRead(dir)
	if err != nil {
		t.Fatalf("%s: OpenRead: %v", out.driver, err)
	}
	events, err := rd.Events()
	if err != nil {
		t.Fatalf("%s: Events: %v", out.driver, err)
	}
	for _, e := range events {
		if e.Stage != "implement" {
			continue
		}
		switch e.Type {
		case journal.EventStageStarted:
			out.attempts = append(out.attempts, e.AttemptClass)
		case journal.EventError:
			if e.Error == nil {
				continue
			}
			out.errorCodes = append(out.errorCodes, e.Error.Code)
			out.errorMessage += " " + e.Error.Message
		}
	}
}

// TestCrossRunnerTransientProvisioningIsAnInfrastructureAttempt is #2878's
// headline acceptance, and #572's on the engine side: a remote that fails one
// request with a transient 503 and then serves normally must cost an
// INFRASTRUCTURE attempt and then complete — on both drivers, for the same
// fixture. The engine used to charge such a failure to the run's own work
// budget, so a blip in the worker's network ended the run the local runner
// would have recovered.
func TestCrossRunnerTransientProvisioningIsAnInfrastructureAttempt(t *testing.T) {
	for _, side := range provisionParitySides() {
		t.Run(side.name, func(t *testing.T) {
			remote := newProvisionParityRemote(t, http.StatusServiceUnavailable, 1)
			got := side.run(t, "run-provision-transient-"+side.name, remote, "")
			if got.err != nil {
				t.Fatalf("%s: walk failed: %v", got, got.err)
			}
			requireTransientPremise(t, got, "503")
			want := []journal.AttemptClass{"", journal.AttemptInfra}
			if len(got.attempts) != len(want) || got.attempts[0] != want[0] || got.attempts[1] != want[1] {
				t.Fatalf("%s: attempt classes = %s, want %s (first normative, retry infra-classed)",
					got, formatAttemptClasses(got.attempts), formatAttemptClasses(want))
			}
			if got.execCalls != 1 {
				t.Fatalf("%s: executor called %d times, want 1 — the failed provisioning never reached the stage", got, got.execCalls)
			}
		})
	}
}

// TestCrossRunnerPersistentProvisioningStopsAtTheInfrastructureBound is the
// terminal half: a remote that never recovers must stop at the SHARED
// infrastructure bound (runner.DefaultMaxInfrastructureAttempts) rather than
// retrying forever or failing on the first blip, and must surface the
// original cause. Both drivers spend the same budget, so the same outage
// costs the same wall clock wherever the run happens to execute.
func TestCrossRunnerPersistentProvisioningStopsAtTheInfrastructureBound(t *testing.T) {
	for _, side := range provisionParitySides() {
		t.Run(side.name, func(t *testing.T) {
			remote := newProvisionParityRemote(t, http.StatusServiceUnavailable, -1)
			got := side.run(t, "run-provision-persistent-"+side.name, remote, "")
			if got.err == nil {
				t.Fatalf("%s: expected the persistent outage to end the walk", got)
			}
			if !strings.Contains(got.err.Error(), "503") {
				t.Errorf("%s: terminal error = %q, want the original 503 cause preserved", got, got.err)
			}
			requireTransientPremise(t, got, "503")
			if len(got.attempts) != int(runner.DefaultMaxInfrastructureAttempts) {
				t.Errorf("%s: attempts = %d (%s), want %d (the shared infrastructure bound)",
					got, len(got.attempts), formatAttemptClasses(got.attempts), runner.DefaultMaxInfrastructureAttempts)
			}
			if got.execCalls != 0 {
				t.Errorf("%s: executor called %d times, want 0 — provisioning never succeeded", got, got.execCalls)
			}
		})
	}
}

// TestCrossRunnerDeterministicProvisioningIsNotRetried is the negative
// acceptance: an authentication failure and a base ref that does not exist
// are deterministic, so retrying can only reproduce them. Both drivers must
// fail on the FIRST attempt rather than spending the infrastructure budget on
// a wall.
func TestCrossRunnerDeterministicProvisioningIsNotRetried(t *testing.T) {
	cases := []struct {
		name       string
		status     int
		failures   int32
		baseBranch string
	}{
		{name: "auth", status: http.StatusUnauthorized, failures: -1},
		{name: "bad-ref", status: http.StatusOK, failures: 0, baseBranch: "no-such-branch"},
	}
	for _, tc := range cases {
		for _, side := range provisionParitySides() {
			t.Run(tc.name+"/"+side.name, func(t *testing.T) {
				remote := newProvisionParityRemote(t, tc.status, tc.failures)
				got := side.run(t, "run-provision-"+tc.name+"-"+side.name, remote, tc.baseBranch)
				if got.err == nil {
					t.Fatalf("%s: expected the deterministic failure to end the walk", got)
				}
				if len(got.attempts) != 1 {
					t.Errorf("%s: attempts = %d (%s), want 1 — a deterministic provisioning failure is never retried",
						got, len(got.attempts), formatAttemptClasses(got.attempts))
				}
				if got.execCalls != 0 {
					t.Errorf("%s: executor called %d times, want 0", got, got.execCalls)
				}
			})
		}
	}
}

// requireTransientPremise is the row's anti-vacuity assertion: the walk really
// did record a failed provisioning attempt carrying the remote's own cause. A
// fixture whose remote quietly stopped failing would otherwise satisfy every
// assertion above by never failing at all.
func requireTransientPremise(t *testing.T, got provisionParityOutcome, cause string) {
	t.Helper()
	if len(got.errorCodes) == 0 {
		t.Fatalf("%s: no error event recorded for the provisioning failure", got)
	}
	if !strings.Contains(got.errorMessage, cause) {
		t.Fatalf("%s: error events = %q, want the original %s cause preserved", got, got.errorMessage, cause)
	}
}

// provisionParitySides is the pair of drivers every case above walks. Keeping
// them in one list is what makes each case a PARITY case rather than two
// tests that happen to be adjacent: a driver added here is asserted by every
// row, and a row cannot silently cover only one side.
func provisionParitySides() []struct {
	name string
	run  func(t *testing.T, runID, repoURL, baseBranch string) provisionParityOutcome
} {
	return []struct {
		name string
		run  func(t *testing.T, runID, repoURL, baseBranch string) provisionParityOutcome
	}{
		{name: "runner", run: runProvisionParityRunner},
		{name: "engine", run: runProvisionParityEngine},
	}
}
