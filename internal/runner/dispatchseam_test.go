package runner

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/blobstore"
	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/internal/gate"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/testgit"
	"github.com/goobers/goobers/internal/workflow"
	"github.com/goobers/goobers/internal/workspacedelta"
	"github.com/goobers/goobers/internal/worktree"
)

// fakeStageDispatcher is the seam double: it records every request, answers
// Dispatch/Await with canned (or computed) results, and reports whatever
// Describe state the test scripted.
type fakeStageDispatcher struct {
	mu         sync.Mutex
	dispatches []StageDispatchRequest
	awaits     []StageDispatchRequest
	describes  []string

	onDispatch  func(req StageDispatchRequest) (StageDispatchResult, error)
	onAwait     func(req StageDispatchRequest) (StageDispatchResult, error)
	describe    StageAttemptState
	describeErr error
}

func (f *fakeStageDispatcher) Dispatch(_ context.Context, req StageDispatchRequest) (StageDispatchResult, error) {
	f.mu.Lock()
	f.dispatches = append(f.dispatches, req)
	f.mu.Unlock()
	if f.onDispatch != nil {
		return f.onDispatch(req)
	}
	return StageDispatchResult{Result: apiv1.ResultEnvelope{Status: apiv1.ResultSuccess}}, nil
}

func (f *fakeStageDispatcher) Describe(_ context.Context, runID, stage string, attempt int) (StageAttemptState, error) {
	f.mu.Lock()
	f.describes = append(f.describes, fmt.Sprintf("%s/%s/%d", runID, stage, attempt))
	f.mu.Unlock()
	return f.describe, f.describeErr
}

func (f *fakeStageDispatcher) Await(_ context.Context, req StageDispatchRequest) (StageDispatchResult, error) {
	f.mu.Lock()
	f.awaits = append(f.awaits, req)
	f.mu.Unlock()
	if f.onAwait != nil {
		return f.onAwait(req)
	}
	return StageDispatchResult{Result: apiv1.ResultEnvelope{Status: apiv1.ResultSuccess}}, nil
}

func (f *fakeStageDispatcher) snapshot() (dispatches, awaits []StageDispatchRequest) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]StageDispatchRequest(nil), f.dispatches...), append([]StageDispatchRequest(nil), f.awaits...)
}

// podPin is a non-self placement for stage, shaped like a real
// bootstrap.PinStagePlacements answer.
func podPin(stage string) journal.PinnedPlacement {
	return journal.PinnedPlacement{
		Stage: stage, Queue: "goobers-dispatch.acme-web.linux-image",
		Eligible: []journal.PinnedRunner{{
			Name: "linux-image", OS: "linux", HostKind: "image",
			Host: "ghcr.io/goobers/goobers-base:test", CPU: "2", Memory: "4Gi",
		}},
		CPU: "500m", Memory: "1Gi",
	}
}

func selfPin(stage string) journal.PinnedPlacement {
	return journal.PinnedPlacement{Stage: stage, Self: true}
}

// podProvenance is what a settled pod attempt reports.
func podProvenance() journal.Placement {
	queued := time.Date(2026, 8, 29, 10, 0, 0, 0, time.UTC)
	started := queued.Add(3 * time.Second)
	return journal.Placement{
		Runner: "linux-image", Node: "aks-linux-0001", Pod: "goobers-stage-build-1", OS: "linux",
		Image: "ghcr.io/goobers/goobers-base:test", QueuedAt: &queued, PodStartedAt: &started,
	}
}

// seamFixtureMachine: query (self, deterministic) -> build (placed,
// deterministic, scratch, inputsFrom query) -> review gate -> complete.
func seamFixtureMachine(t *testing.T, build apiv1.Task) *workflow.Machine {
	t.Helper()
	build.Name = "build"
	build.Type = apiv1.TaskDeterministic
	build.Next = "review"
	spec := apiv1.WorkflowSpec{
		Gaggle:   "acme-web",
		Triggers: []apiv1.Trigger{{Type: apiv1.TriggerBacklogItem}},
		Start:    "query",
		Tasks: []apiv1.Task{
			{Name: "query", Type: apiv1.TaskDeterministic, Goal: "select", Run: &apiv1.DeterministicRun{Command: []string{"true"}}, Next: "build"},
			build,
		},
		Gates: []apiv1.Gate{{
			Name:      "review",
			Evaluator: apiv1.EvaluatorAutomated,
			Automated: &apiv1.AutomatedGate{Check: "status-equals"},
			Branches: map[string]string{
				"pass": workflow.TerminalComplete,
				"fail": workflow.TargetAbort,
			},
		}},
	}
	m, err := workflow.Compile(workflow.Definition{Name: "seam-fixture", Version: 1, Spec: spec}, workflow.WithPreviewFeatures(true))
	if err != nil {
		t.Fatalf("compile seam fixture machine: %v", err)
	}
	return m
}

// newSeamRunner builds a Runner whose deterministic executor is the canned
// stub and whose stage dispatcher is the fake. No ScratchDir on purpose: a
// scratch-workspace stage that reached the self arm would fail on it, which
// is how the tests prove a placed stage never provisions a workspace.
func newSeamRunner(t *testing.T, byTask map[string]stubTaskResult, seam StageDispatcher, mutate func(*Config)) (*Runner, string) {
	t.Helper()
	instanceRoot := t.TempDir()
	wtMgr, err := worktree.NewManager(filepath.Join(instanceRoot, "workcopies"))
	if err != nil {
		t.Fatalf("new worktree manager: %v", err)
	}
	store, err := blobstore.NewDir(filepath.Join(instanceRoot, "blobstore"))
	if err != nil {
		t.Fatalf("new blob store: %v", err)
	}
	runsDir := filepath.Join(instanceRoot, "runs")
	fixtureRepo := newFixtureRepo(t)
	cfg := Config{
		NewDeterministic: func(rec ArtifactRecorder, _ SecretRegistrar) (invoke.Deterministic, error) {
			return &stubDeterministic{rec: rec, byTask: byTask}, nil
		},
		Automated:       gate.NewAutomatedEvaluator(),
		Worktrees:       wtMgr,
		RunsDir:         runsDir,
		RepoCloneURL:    func(apiv1.RepoRef) (string, error) { return fixtureRepo, nil },
		StageDispatcher: seam,
		WorkspaceDeltas: store,
	}
	if mutate != nil {
		mutate(&cfg)
	}
	r, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return r, runsDir
}

func stageFinished(events []journal.Event, stage string) []journal.Event {
	var out []journal.Event
	for _, ev := range events {
		if ev.Type == journal.EventStageFinished && ev.Stage == stage {
			out = append(out, ev)
		}
	}
	return out
}

var repoRefMain = apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web", Branch: "main"}

// TestRunnerNilPlacementsLeaveJournalsByteIdentical is the zero-declaration
// invariance guard (goobernetes-architecture.md §11 item 1) for the seam: a
// Runner WITH a stage dispatcher configured, started with nil Placements,
// writes exactly the run.yaml and events.jsonl a Runner without one writes —
// and never consults the seam.
func TestRunnerNilPlacementsLeaveJournalsByteIdentical(t *testing.T) {
	machine := seamFixtureMachine(t, apiv1.Task{Goal: "build", Run: &apiv1.DeterministicRun{Command: []string{"true"}}})
	byTask := func(runID string) map[string]stubTaskResult {
		return map[string]stubTaskResult{
			runID + ":query": {status: apiv1.ResultSuccess, outputs: map[string]interface{}{"items": "42"}},
			runID + ":build": {status: apiv1.ResultSuccess, outputs: map[string]interface{}{"done": "yes"}},
		}
	}
	fake := &fakeStageDispatcher{}
	withSeam, seamRuns := newSeamRunner(t, byTask("run-a"), fake, nil)
	withoutSeam, plainRuns := newSeamRunner(t, byTask("run-b"), nil, func(cfg *Config) { cfg.WorkspaceDeltas = nil })

	start := func(r *Runner, runID string) {
		t.Helper()
		res, err := r.Start(context.Background(), StartInput{
			RunID: runID, Machine: machine, Gaggle: "acme-web",
			Trigger: journal.Trigger{Kind: journal.TriggerManual}, RepoRef: repoRefMain,
		})
		if err != nil || res.Phase != journal.PhaseCompleted {
			t.Fatalf("Start %s: phase=%s err=%v", runID, res.Phase, err)
		}
	}
	start(withSeam, "run-a")
	start(withoutSeam, "run-b")
	if d, a := fake.snapshot(); len(d) != 0 || len(a) != 0 {
		t.Fatalf("seam consulted for an unplaced run: dispatches=%d awaits=%d", len(d), len(a))
	}

	normalize := func(b []byte, runID string) string {
		s := strings.ReplaceAll(string(b), runID, "RUN")
		s = regexp.MustCompile(`"time":"[^"]+"`).ReplaceAllString(s, `"time":"T"`)
		s = regexp.MustCompile(`startedAt: .*`).ReplaceAllString(s, `startedAt: T`)
		return s
	}
	for _, name := range []string{"run.yaml", "events.jsonl"} {
		a, err := os.ReadFile(filepath.Join(seamRuns, "run-a", name))
		if err != nil {
			t.Fatal(err)
		}
		b, err := os.ReadFile(filepath.Join(plainRuns, "run-b", name))
		if err != nil {
			t.Fatal(err)
		}
		if got, want := normalize(a, "run-a"), normalize(b, "run-b"); got != want {
			t.Fatalf("%s differs between seam-configured and plain runner:\n--- seam\n%s\n--- plain\n%s", name, got, want)
		}
		if strings.Contains(string(a), "placements") {
			t.Fatalf("%s mentions placements for a run that pinned none:\n%s", name, a)
		}
	}
}

// TestRunnerRoutesPinnedStageThroughSeamWithResolvedInputs: a stage pinned to
// a non-self runner reaches the seam AFTER the runner's own input resolution
// (inputsFrom from the upstream stage, backlog-query defaulting) and BEFORE
// any workspace is provisioned — the stage declares a scratch workspace on a
// Runner with no ScratchDir, which the self arm cannot provision — with the
// pinned placement, the attempt number, the run branch and base, and the
// open journal handle. The self arm's executor is never invoked for it, the
// seam's provenance is journaled as runner.placement, and the seam's result
// is the stage's outcome.
func TestRunnerRoutesPinnedStageThroughSeamWithResolvedInputs(t *testing.T) {
	const runID = "run-placed"
	machine := seamFixtureMachine(t, apiv1.Task{
		Goal:         "build",
		Run:          &apiv1.DeterministicRun{Command: []string{"goobers", "backlog-query", "--read-only"}, Workspace: apiv1.WorkspaceScratch},
		Capabilities: []string{"github:issues:read"},
		Inputs:       map[string]string{"static": "yes"},
		InputsFrom:   map[string]string{"count": "items"},
	})
	fake := &fakeStageDispatcher{onDispatch: func(StageDispatchRequest) (StageDispatchResult, error) {
		return StageDispatchResult{
			Result: apiv1.ResultEnvelope{
				Status: apiv1.ResultSuccess, Summary: "built in a pod",
				Outputs: map[string]interface{}{"artifact": "pod-1"},
			},
			Mutations: []MutationFact{{Provider: "github", Kind: "pr", ID: "7", Operation: "open"}},
			Placement: podProvenance(),
		}, nil
	}}
	r, runsDir := newSeamRunner(t, map[string]stubTaskResult{
		runID + ":query": {status: apiv1.ResultSuccess, outputs: map[string]interface{}{"items": "42"}},
		// No canned result for build: the stub would fail loudly if the
		// self arm ever reached it.
	}, fake, func(cfg *Config) {
		cfg.RunnersDeclared = true
		cfg.BacklogQueryRequireLabels = "goobers:cloud"
	})

	res, err := r.Start(context.Background(), StartInput{
		RunID: runID, Machine: machine, Gaggle: "acme-web",
		Trigger: journal.Trigger{Kind: journal.TriggerManual}, RepoRef: repoRefMain,
		Placements: []journal.PinnedPlacement{selfPin("query"), podPin("build")},
	})
	if err != nil || res.Phase != journal.PhaseCompleted {
		t.Fatalf("Start: phase=%s err=%v", res.Phase, err)
	}

	dispatches, _ := fake.snapshot()
	if len(dispatches) != 1 {
		t.Fatalf("seam dispatches = %d, want 1", len(dispatches))
	}
	req := dispatches[0]
	if req.Task.Name != "build" || req.Attempt != 1 || req.Envelope.Attempt != 1 || req.AttemptClass != "" {
		t.Fatalf("request identity = task %q attempt %d/%d class %q", req.Task.Name, req.Attempt, req.Envelope.Attempt, req.AttemptClass)
	}
	if !reflect.DeepEqual(req.Placement, podPin("build")) {
		t.Fatalf("request placement = %+v, want the pin", req.Placement)
	}
	if got := req.Envelope.Inputs; got["count"] != "42" || got["static"] != "yes" || got["requireLabels"] != "goobers:cloud" {
		t.Fatalf("request inputs = %v; want inputsFrom, static and requireLabels defaulting all resolved", got)
	}
	if req.Envelope.Workspace != "" || req.Envelope.TaskID != runID+":build" || req.Envelope.RunID != runID {
		t.Fatalf("request envelope = workspace %q task %q run %q; want no workspace", req.Envelope.Workspace, req.Envelope.TaskID, req.Envelope.RunID)
	}
	if req.Run == nil || req.Run.Workspace != apiv1.WorkspaceScratch || req.Workspace != "" {
		t.Fatalf("request run/workspace = %+v / %q", req.Run, req.Workspace)
	}
	if req.BaseBranch != "main" || req.WorkspaceBranch != "goobers/seam-fixture/"+runID {
		t.Fatalf("request branches = base %q branch %q", req.BaseBranch, req.WorkspaceBranch)
	}
	if req.Journal == nil || req.Journal.Dir() != filepath.Join(runsDir, runID) {
		t.Fatalf("request journal handle = %v; want the run's open handle", req.Journal)
	}
	if req.WorkspaceDelta != "" {
		t.Fatalf("scratch stage carried a workspace delta %q", req.WorkspaceDelta)
	}

	events := readRunEvents(t, runsDir, runID)
	finished := stageFinished(events, "build")
	if len(finished) != 1 || finished[0].Status != string(apiv1.ResultSuccess) || finished[0].Outputs["artifact"] != "pod-1" {
		t.Fatalf("build stage.finished = %+v", finished)
	}
	var placements []journal.Placement
	var touched []journal.Event
	for _, ev := range events {
		if ev.Stage != "build" {
			continue
		}
		if p, ok := journal.PlacementFromEvent(ev); ok {
			placements = append(placements, p)
		}
		if ev.Type == journal.EventRefTouched {
			touched = append(touched, ev)
		}
	}
	if len(placements) != 1 {
		t.Fatalf("build placement events = %+v, want exactly the pod's provenance", placements)
	}
	if want := podProvenance(); placements[0].Runner != want.Runner || placements[0].Pod != want.Pod || placements[0].Image != want.Image ||
		placements[0].Node != want.Node || placements[0].QueuedAt == nil || !placements[0].QueuedAt.Equal(*want.QueuedAt) {
		t.Fatalf("build placement = %+v, want %+v", placements[0], want)
	}
	if len(touched) != 1 || touched[0].ExternalRef == nil || touched[0].ExternalRef.ID != "7" || touched[0].Runner["operation"] != "open" {
		t.Fatalf("mutation projection = %+v, want one ref.touched for pr 7", touched)
	}
	// query stayed on the self arm: its placement is the self runner.
	for _, ev := range events {
		if p, ok := journal.PlacementFromEvent(ev); ok && ev.Stage == "query" && p.Runner != journal.PlacementRunnerSelf {
			t.Fatalf("query placement = %+v, want self", p)
		}
	}
	// run.yaml pinned both placements, self:true / self:false.
	rd, err := journal.OpenRead(filepath.Join(runsDir, runID))
	if err != nil {
		t.Fatal(err)
	}
	id, err := rd.Identity()
	if err != nil {
		t.Fatal(err)
	}
	if len(id.Placements) != 2 || id.Placements[0].Stage != "query" || !id.Placements[0].Self ||
		id.Placements[1].Stage != "build" || id.Placements[1].Self || id.Placements[1].Queue != podPin("build").Queue {
		t.Fatalf("run.yaml placements = %+v", id.Placements)
	}
	if id.Driver != "" {
		t.Fatalf("run.yaml driver = %q, want empty (runner-driven)", id.Driver)
	}
}

// TestRunnerRefusesInstanceRootStagesBeforeSeam: the shared refusal list
// (executor.StageRequiresInstanceRoot, plus a non-shell kind) is applied to a
// placed stage BEFORE the seam is consulted, as a journaled stage failure
// carrying the shared code; the deterministic-shape and instance-config
// guards fail the stage with a plain error. In every case the seam sees
// nothing.
func TestRunnerRefusesInstanceRootStagesBeforeSeam(t *testing.T) {
	cases := []struct {
		name     string
		build    apiv1.Task
		wantCode string // journaled stage.finished code; "" means a walk error
		wantErr  string
	}{
		{
			name:     "ledger command",
			build:    apiv1.Task{Goal: "claim", Run: &apiv1.DeterministicRun{Command: []string{"goobers", "pr-claim"}, Workspace: apiv1.WorkspaceScratch}, Capabilities: []string{"github:pr:write"}, PolicyActions: []string{"release-pr-claim"}},
			wantCode: executor.StageRequiresInstanceRootCode,
		},
		{
			name:     "backlog-query claim mode",
			build:    apiv1.Task{Goal: "claim", Run: &apiv1.DeterministicRun{Command: []string{"goobers", "backlog-query", "--claim"}, Workspace: apiv1.WorkspaceScratch}, Capabilities: []string{"github:issues:write"}, PolicyActions: []string{"claim-backlog-items"}},
			wantCode: executor.StageRequiresInstanceRootCode,
		},
		{
			name:     "built-in kind",
			build:    apiv1.Task{Goal: "poll", Run: &apiv1.DeterministicRun{Command: []string{"true"}, Workspace: apiv1.WorkspaceScratch}, Inputs: map[string]string{executor.InputKind: "ci-poll"}, Capabilities: []string{"provider:pr:write"}},
			wantCode: executor.StageRequiresInstanceRootCode,
		},
		{
			name:    "instance config",
			build:   apiv1.Task{Goal: "query", Run: &apiv1.DeterministicRun{Command: []string{"goobers", "telemetry-query"}, Workspace: apiv1.WorkspaceScratch}, Capabilities: []string{"telemetry:read"}},
			wantErr: "instance config directory",
		},
		{
			name:    "empty command",
			build:   apiv1.Task{Goal: "noop", Run: &apiv1.DeterministicRun{Workspace: apiv1.WorkspaceScratch}},
			wantErr: "no command or script",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			runID := "run-refuse-" + strings.ReplaceAll(tc.name, " ", "-")
			machine := seamFixtureMachine(t, tc.build)
			fake := &fakeStageDispatcher{}
			r, runsDir := newSeamRunner(t, map[string]stubTaskResult{
				runID + ":query": {status: apiv1.ResultSuccess, outputs: map[string]interface{}{"items": "42"}},
			}, fake, nil)
			res, err := r.Start(context.Background(), StartInput{
				RunID: runID, Machine: machine, Gaggle: "acme-web",
				Trigger: journal.Trigger{Kind: journal.TriggerManual}, RepoRef: repoRefMain,
				Placements: []journal.PinnedPlacement{podPin("build")},
			})
			if d, a := fake.snapshot(); len(d) != 0 || len(a) != 0 {
				t.Fatalf("seam consulted for a refused stage: %d dispatches, %d awaits", len(d), len(a))
			}
			events := readRunEvents(t, runsDir, runID)
			finished := stageFinished(events, "build")
			if tc.wantCode != "" {
				// A JOURNALED failure: the review gate's fail branch (abort)
				// routes it, exactly as it would a real executor failure.
				if err != nil || res.Phase != journal.PhaseAborted {
					t.Fatalf("Start: phase=%s err=%v; want the gate's fail branch to route a journaled stage failure", res.Phase, err)
				}
				if len(finished) != 1 || finished[0].Status != string(apiv1.ResultFailure) || finished[0].Error == nil || finished[0].Error.Code != tc.wantCode {
					t.Fatalf("build stage.finished = %+v, want a failure with code %q", finished, tc.wantCode)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("Start err = %v, want %q", err, tc.wantErr)
			}
			if len(finished) != 0 {
				t.Fatalf("a refused-by-shape stage journaled a result: %+v", finished)
			}
		})
	}
}

// TestRunnerRefusesRoutedPinWithoutSeam: a run that pins a stage to a
// non-self runner on a Runner with no StageDispatcher fails closed, with a
// named error, BEFORE any stage executes — never falls through to the self
// arm (decision 003 ruling 7).
func TestRunnerRefusesRoutedPinWithoutSeam(t *testing.T) {
	const runID = "run-no-seam"
	machine := seamFixtureMachine(t, apiv1.Task{Goal: "build", Run: &apiv1.DeterministicRun{Command: []string{"true"}}})
	calls := 0
	r, runsDir := newSeamRunner(t, nil, nil, func(cfg *Config) {
		cfg.WorkspaceDeltas = nil
		cfg.NewDeterministic = func(ArtifactRecorder, SecretRegistrar) (invoke.Deterministic, error) {
			calls++
			return &stubDeterministic{}, nil
		}
	})
	res, err := r.Start(context.Background(), StartInput{
		RunID: runID, Machine: machine, Gaggle: "acme-web",
		Trigger: journal.Trigger{Kind: journal.TriggerManual}, RepoRef: repoRefMain,
		Placements: []journal.PinnedPlacement{selfPin("query"), podPin("build")},
	})
	if !errors.Is(err, ErrStageDispatcherUnavailable) || res.Phase != journal.PhaseFailed || res.FailureStage != stageDispatchPreflightState {
		t.Fatalf("Start: phase=%s stage=%q err=%v; want a %s failure naming %v", res.Phase, res.FailureStage, err, stageDispatchPreflightState, ErrStageDispatcherUnavailable)
	}
	if !strings.Contains(res.FailureMessage, ErrStageDispatcherUnavailable.Error()) {
		t.Fatalf("failure message %q does not name %v", res.FailureMessage, ErrStageDispatcherUnavailable)
	}
	if calls != 0 {
		t.Fatalf("a stage executor was built %d times; want none before the refusal", calls)
	}
	if got := stageFinished(readRunEvents(t, runsDir, runID), "query"); len(got) != 0 {
		t.Fatalf("query executed before the refusal: %+v", got)
	}

	// All-self pins need no seam.
	ok, okRuns := newSeamRunner(t, map[string]stubTaskResult{
		"run-all-self:query": {status: apiv1.ResultSuccess, outputs: map[string]interface{}{"items": "1"}},
		"run-all-self:build": {status: apiv1.ResultSuccess, outputs: map[string]interface{}{"done": "yes"}},
	}, nil, func(cfg *Config) { cfg.WorkspaceDeltas = nil })
	res, err = ok.Start(context.Background(), StartInput{
		RunID: "run-all-self", Machine: machine, Gaggle: "acme-web",
		Trigger: journal.Trigger{Kind: journal.TriggerManual}, RepoRef: repoRefMain,
		Placements: []journal.PinnedPlacement{selfPin("query"), selfPin("build")},
	})
	if err != nil || res.Phase != journal.PhaseCompleted {
		t.Fatalf("all-self Start: phase=%s err=%v", res.Phase, err)
	}
	rd, err := journal.OpenRead(filepath.Join(okRuns, "run-all-self"))
	if err != nil {
		t.Fatal(err)
	}
	id, err := rd.Identity()
	if err != nil {
		t.Fatal(err)
	}
	if len(id.Placements) != 2 || !id.Placements[0].Self || !id.Placements[1].Self {
		t.Fatalf("all-self run.yaml placements = %+v", id.Placements)
	}
}

// TestSelfPinnedCapabilitiesFilter: the #735 toolchain preflight verifies
// only the tokens a stage still executing on this host declares.
func TestSelfPinnedCapabilitiesFilter(t *testing.T) {
	tasks := []apiv1.Task{
		{Name: "query", RunsOn: &apiv1.RunsOn{Capabilities: []string{"git"}}},
		{Name: "build", RunsOn: &apiv1.RunsOn{Capabilities: []string{"go@1.26", "git"}}, RequiredCapabilities: []string{"make"}},
		{Name: "publish", RunsOn: &apiv1.RunsOn{Capabilities: []string{"dotnet@8"}}},
	}
	required := []string{"dotnet@8", "gcc", "git", "go@1.26", "make"}
	if got := selfPinnedCapabilities(required, tasks, nil); !reflect.DeepEqual(got, required) {
		t.Fatalf("no pins: %v, want identity %v", got, required)
	}
	allSelf := []journal.PinnedPlacement{selfPin("query"), selfPin("build"), selfPin("publish")}
	if got := selfPinnedCapabilities(required, tasks, allSelf); !reflect.DeepEqual(got, required) {
		t.Fatalf("all self: %v, want identity %v", got, required)
	}
	pinned := []journal.PinnedPlacement{selfPin("query"), podPin("build"), podPin("publish")}
	// go@1.26 and make are declared only by pod-pinned stages; git is shared
	// with self-pinned query; gcc is nobody's (the gaggle floor) and stays.
	if got, want := selfPinnedCapabilities(required, tasks, pinned), []string{"gcc", "git"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("filtered = %v, want %v", got, want)
	}
}

// TestRunnerToolchainPreflightSkipsPodOnlyToolchains: end to end, a run whose
// only stage needing go@1.26 is pod-pinned passes preflight on a host whose
// verifier would refuse that token.
func TestRunnerToolchainPreflightSkipsPodOnlyToolchains(t *testing.T) {
	const runID = "run-toolchain"
	machine := seamFixtureMachine(t, apiv1.Task{
		Goal: "build", Run: &apiv1.DeterministicRun{Command: []string{"true"}, Workspace: apiv1.WorkspaceScratch},
		RequiredCapabilities: []string{"go@1.26"},
	})
	var verified [][]string
	fake := &fakeStageDispatcher{onDispatch: func(StageDispatchRequest) (StageDispatchResult, error) {
		return StageDispatchResult{Result: apiv1.ResultEnvelope{Status: apiv1.ResultSuccess, Outputs: map[string]interface{}{"done": "yes"}}}, nil
	}}
	r, _ := newSeamRunner(t, map[string]stubTaskResult{
		runID + ":query": {status: apiv1.ResultSuccess, outputs: map[string]interface{}{"items": "1"}},
	}, fake, func(cfg *Config) {
		cfg.ToolchainVerifier = toolchainVerifierFunc(func(_ context.Context, required []string) error {
			verified = append(verified, required)
			for _, token := range required {
				if token == "go@1.26" {
					return errors.New("go@1.26 is not installed on this host")
				}
			}
			return nil
		})
	})
	res, err := r.Start(context.Background(), StartInput{
		RunID: runID, Machine: machine, Gaggle: "acme-web",
		Trigger: journal.Trigger{Kind: journal.TriggerManual}, RepoRef: repoRefMain,
		RequiredCapabilities: []string{"git", "go@1.26"},
		Placements:           []journal.PinnedPlacement{selfPin("query"), podPin("build")},
	})
	if err != nil || res.Phase != journal.PhaseCompleted {
		t.Fatalf("Start: phase=%s err=%v (verified %v)", res.Phase, err, verified)
	}
	if !reflect.DeepEqual(verified, [][]string{{"git"}}) {
		t.Fatalf("verifier saw %v, want only [[git]]", verified)
	}
}

type toolchainVerifierFunc func(context.Context, []string) error

func (f toolchainVerifierFunc) Verify(ctx context.Context, required []string) error {
	return f(ctx, required)
}

// interruptedPlacedRun hand-builds the journal of a run whose placed
// implement stage was in flight (stage.started, no stage.finished) when the
// daemon died.
func interruptedPlacedRun(t *testing.T, runsDir string, machine *workflow.Machine, runID string) {
	t.Helper()
	jr, err := journal.Create(runsDir, journal.RunIdentity{
		RunID: runID, Workflow: machine.Def.Name, WorkflowVersion: machine.Def.Version,
		WorkflowDigest: machine.Digest(), Gaggle: "acme-web", Trigger: journal.Trigger{Kind: journal.TriggerManual},
		Placements: []journal.PinnedPlacement{podPin("implement")},
	}, nil)
	if err != nil {
		t.Fatalf("journal.Create: %v", err)
	}
	jr.SetMachineState("implement")
	if err := jr.Append(journal.Event{Type: journal.EventStageStarted, Stage: "implement", Attempt: 1}); err != nil {
		t.Fatalf("append stage.started: %v", err)
	}
	if err := jr.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
}

// TestRunnerResumeAdoptsOrAwaitsPlacedAttempt is decision 003 ruling 6: an
// interrupted placed attempt N is settled through the seam's Describe.
// Completed and Running adopt Await's result as attempt N's outcome (no
// attempt N+1, no Dispatch); Failed/TimedOut/NotFound take today's
// interrupted path (attempt N journaled interrupted, N+1 dispatched on the
// infrastructure budget); an unreadable state fails the resume closed.
func TestRunnerResumeAdoptsOrAwaitsPlacedAttempt(t *testing.T) {
	adopted := StageDispatchResult{
		Result:    apiv1.ResultEnvelope{Status: apiv1.ResultSuccess, Summary: "settled in the pod", Outputs: map[string]interface{}{"pr": "11"}},
		Placement: podProvenance(),
	}
	cases := []struct {
		name        string
		state       StageAttemptState
		describeErr error
		awaitDelay  time.Duration
		wantAdopt   bool
		wantErr     bool
	}{
		{name: "completed", state: StageAttemptCompleted, wantAdopt: true},
		{name: "running", state: StageAttemptRunning, awaitDelay: 100 * time.Millisecond, wantAdopt: true},
		{name: "failed", state: StageAttemptFailed},
		{name: "timed-out", state: StageAttemptTimedOut},
		{name: "not-found", state: StageAttemptNotFound},
		{name: "unreadable", describeErr: errors.New("temporal unreachable"), wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			runID := "run-adopt-" + tc.name
			machine := fixtureMachine(t)
			fake := &fakeStageDispatcher{describe: tc.state, describeErr: tc.describeErr}
			fake.onAwait = func(StageDispatchRequest) (StageDispatchResult, error) {
				time.Sleep(tc.awaitDelay)
				return adopted, nil
			}
			fake.onDispatch = func(StageDispatchRequest) (StageDispatchResult, error) {
				return StageDispatchResult{Result: apiv1.ResultEnvelope{Status: apiv1.ResultSuccess, Outputs: map[string]interface{}{"pr": "12"}}, Placement: podProvenance()}, nil
			}
			r, runsDir := newSeamRunner(t, nil, fake, nil)
			interruptedPlacedRun(t, runsDir, machine, runID)

			res, err := r.Resume(context.Background(), ResumeInput{RunID: runID, Machine: machine, RepoRef: repoRefMain, RecoveryReason: "daemon_restart"})
			dispatches, awaits := fake.snapshot()
			if len(fake.describes) != 1 || fake.describes[0] != runID+"/implement/1" {
				t.Fatalf("describes = %v, want exactly %s/implement/1", fake.describes, runID)
			}
			if tc.wantErr {
				if err == nil || !strings.Contains(err.Error(), "temporal unreachable") {
					t.Fatalf("Resume err = %v, want the describe failure", err)
				}
				if len(dispatches) != 0 || len(awaits) != 0 {
					t.Fatalf("an unreadable attempt was re-dispatched or awaited: %d/%d", len(dispatches), len(awaits))
				}
				return
			}
			if err != nil || res.Phase != journal.PhaseCompleted {
				t.Fatalf("Resume: phase=%s err=%v", res.Phase, err)
			}
			events := readRunEvents(t, runsDir, runID)
			finished := stageFinished(events, "implement")
			if tc.wantAdopt {
				if len(dispatches) != 0 || len(awaits) != 1 || awaits[0].Attempt != 1 || awaits[0].Envelope.Attempt != 1 || awaits[0].Task.Name != "implement" {
					t.Fatalf("adoption: dispatches=%d awaits=%+v; want one await of attempt 1 and no dispatch", len(dispatches), awaits)
				}
				if len(finished) != 1 || finished[0].Attempt != 1 || finished[0].Status != string(apiv1.ResultSuccess) || finished[0].Outputs["pr"] != "11" {
					t.Fatalf("adopted stage.finished = %+v, want attempt 1 success carrying the pod's outputs", finished)
				}
				var placed bool
				for _, ev := range events {
					if p, ok := journal.PlacementFromEvent(ev); ok && ev.Stage == "implement" && ev.Attempt == 1 && p.Pod == podProvenance().Pod {
						placed = true
					}
				}
				if !placed {
					t.Fatalf("adopted attempt journaled no runner.placement from the seam's provenance")
				}
				return
			}
			// Today's interrupted path: attempt 1 closed as interrupted, attempt 2
			// dispatched fresh on the infrastructure budget.
			if len(awaits) != 0 || len(dispatches) != 1 || dispatches[0].Attempt != 2 || dispatches[0].AttemptClass != journal.AttemptInfra {
				t.Fatalf("N+1 path: awaits=%d dispatches=%+v; want one infra dispatch of attempt 2", len(awaits), dispatches)
			}
			if len(finished) != 2 || finished[0].Attempt != 1 || finished[0].Error == nil || finished[0].Error.Code != interruptedAttemptErrorCode ||
				finished[1].Attempt != 2 || finished[1].Status != string(apiv1.ResultSuccess) || finished[1].Outputs["pr"] != "12" {
				t.Fatalf("N+1 stage.finished = %+v", finished)
			}
		})
	}
}

// TestRunnerResumeRefusesRoutedPinWithoutSeam: a daemon rolled back after a
// run was pinned refuses the resume rather than re-running the placed stage
// on the self arm.
func TestRunnerResumeRefusesRoutedPinWithoutSeam(t *testing.T) {
	const runID = "run-adopt-no-seam"
	machine := fixtureMachine(t)
	calls := 0
	r, runsDir := newSeamRunner(t, nil, nil, func(cfg *Config) {
		cfg.WorkspaceDeltas = nil
		cfg.NewDeterministic = func(ArtifactRecorder, SecretRegistrar) (invoke.Deterministic, error) {
			calls++
			return &stubDeterministic{}, nil
		}
	})
	interruptedPlacedRun(t, runsDir, machine, runID)
	res, err := r.Resume(context.Background(), ResumeInput{RunID: runID, Machine: machine, RepoRef: repoRefMain})
	if err != nil || res.Phase != journal.PhaseFailed || res.FailureCode != "resume_refused_stage_dispatcher_unavailable" {
		t.Fatalf("Resume: phase=%s code=%q err=%v", res.Phase, res.FailureCode, err)
	}
	if calls != 0 {
		t.Fatalf("the self arm was reached %d times for a placed stage", calls)
	}
}

// gitOut runs git in dir and returns trimmed stdout.
func gitOut(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := testgit.Command(args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v in %s: %v\n%s", args, dir, err, out)
	}
	return strings.TrimSpace(string(out))
}

// testGit adapts testgit to the workspacedelta.Git seam for the simulated
// pod side of the continuity round trip.
type testGit struct{}

func (testGit) Run(ctx context.Context, dir string, args ...string) error {
	cmd := testgit.CommandContext(ctx, args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			return fmt.Errorf("git %v: %w: %s", args, err, out)
		}
		return fmt.Errorf("git %v: %w: %s", args, err, out)
	}
	return nil
}

func (testGit) Output(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := testgit.CommandContext(ctx, args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git %v: %w", args, err)
	}
	return strings.TrimSpace(string(out)), nil
}

// TestRunnerCarriesRunBranchThroughSeam is the continuity round trip
// (decision 003 ruling 5) against a real temp mirror: a self stage commits A
// on the run branch; the placed stage is handed a bundle of base..A from the
// daemon mirror (present in the blob store); the simulated pod builds on A,
// commits B and surrenders a bundle; the runner applies it to the mirror
// fast-forward-only and journals it; the NEXT self stage's worktree is at B.
func TestRunnerCarriesRunBranchThroughSeam(t *testing.T) {
	const runID = "run-continuity"
	spec := apiv1.WorkflowSpec{
		Gaggle:   "acme-web",
		Triggers: []apiv1.Trigger{{Type: apiv1.TriggerBacklogItem}},
		Start:    "prepare",
		Tasks: []apiv1.Task{
			{Name: "prepare", Type: apiv1.TaskDeterministic, Goal: "commit A", Run: &apiv1.DeterministicRun{Command: []string{"true"}}, Next: "build"},
			{Name: "build", Type: apiv1.TaskDeterministic, Goal: "commit B in a pod", Run: &apiv1.DeterministicRun{Command: []string{"true"}, Workspace: apiv1.WorkspaceRepo}, Next: "verify"},
			{Name: "verify", Type: apiv1.TaskDeterministic, Goal: "read HEAD", Run: &apiv1.DeterministicRun{Command: []string{"true"}}, Next: "review"},
		},
		Gates: []apiv1.Gate{{
			Name: "review", Evaluator: apiv1.EvaluatorAutomated, Automated: &apiv1.AutomatedGate{Check: "status-equals"},
			Branches: map[string]string{"pass": workflow.TerminalComplete, "fail": workflow.TargetAbort},
		}},
	}
	machine, err := workflow.Compile(workflow.Definition{Name: "continuity", Version: 1, Spec: spec}, workflow.WithPreviewFeatures(true))
	if err != nil {
		t.Fatalf("compile: %v", err)
	}

	instanceRoot := t.TempDir()
	wtMgr, err := worktree.NewManager(filepath.Join(instanceRoot, "workcopies"))
	if err != nil {
		t.Fatal(err)
	}
	store, err := blobstore.NewDir(filepath.Join(instanceRoot, "blobstore"))
	if err != nil {
		t.Fatal(err)
	}
	fixtureRepo := newFixtureRepo(t)
	runsDir := filepath.Join(instanceRoot, "runs")
	ctx := context.Background()

	var commitA, commitB string
	var handedDelta string
	fake := &fakeStageDispatcher{}
	fake.onDispatch = func(req StageDispatchRequest) (StageDispatchResult, error) {
		handedDelta = req.WorkspaceDelta
		if req.WorkspaceDelta == "" {
			return StageDispatchResult{}, errors.New("no delta handed to the pod")
		}
		data, err := store.Get(ctx, req.WorkspaceDelta)
		if err != nil {
			return StageDispatchResult{}, fmt.Errorf("pod: get delta: %w", err)
		}
		bundle, err := workspacedelta.Load(data, req.WorkspaceDelta)
		if err != nil {
			return StageDispatchResult{}, err
		}
		// The pod: a fresh clone of the repository at base, the delta
		// fetched and checked out, one more commit, a bundle of base..B.
		pod := filepath.Join(t.TempDir(), "pod")
		runGit(t, "", "clone", "--quiet", fixtureRepo, pod)
		tip, err := workspacedelta.Fetch(ctx, testGit{}, pod, bundle)
		if err != nil {
			return StageDispatchResult{}, err
		}
		if tip != commitA {
			return StageDispatchResult{}, fmt.Errorf("pod fetched %s, want A %s", tip, commitA)
		}
		runGit(t, pod, "checkout", "--quiet", "--detach", tip)
		if err := os.WriteFile(filepath.Join(pod, "pod.txt"), []byte("built in a pod\n"), 0o644); err != nil {
			return StageDispatchResult{}, err
		}
		runGit(t, pod, "add", "pod.txt")
		runGit(t, pod, "-c", "user.email=pod@example.com", "-c", "user.name=pod", "commit", "--quiet", "-m", "B")
		commitB = gitOut(t, pod, "rev-parse", "HEAD")
		out, err := workspacedelta.Create(ctx, testGit{}, pod, "refs/remotes/origin/"+req.BaseBranch, "HEAD")
		if err != nil {
			return StageDispatchResult{}, err
		}
		if err := store.Put(ctx, out.Digest, out.Data); err != nil {
			return StageDispatchResult{}, err
		}
		return StageDispatchResult{
			Result:             apiv1.ResultEnvelope{Status: apiv1.ResultSuccess, Outputs: map[string]interface{}{"built": "B"}},
			Placement:          podProvenance(),
			WorkspaceDelta:     out.Digest,
			WorkspaceDeltaBase: out.Base,
			WorkspaceDeltaTip:  out.Tip,
		}, nil
	}

	det := deterministicFunc(func(_ context.Context, env apiv1.InvocationEnvelope, _ apiv1.DeterministicRun) (apiv1.ResultEnvelope, error) {
		switch env.TaskID {
		case runID + ":prepare":
			if err := os.WriteFile(filepath.Join(env.Workspace, "a.txt"), []byte("A\n"), 0o644); err != nil {
				return apiv1.ResultEnvelope{}, err
			}
			runGit(t, env.Workspace, "add", "a.txt")
			runGit(t, env.Workspace, "-c", "user.email=self@example.com", "-c", "user.name=self", "commit", "--quiet", "-m", "A")
			commitA = gitOut(t, env.Workspace, "rev-parse", "HEAD")
			return apiv1.ResultEnvelope{Status: apiv1.ResultSuccess, Outputs: map[string]interface{}{"commit": commitA}}, nil
		case runID + ":verify":
			head := gitOut(t, env.Workspace, "rev-parse", "HEAD")
			_, statErr := os.Stat(filepath.Join(env.Workspace, "pod.txt"))
			return apiv1.ResultEnvelope{Status: apiv1.ResultSuccess, Outputs: map[string]interface{}{"head": head, "podFile": statErr == nil}}, nil
		}
		return apiv1.ResultEnvelope{}, fmt.Errorf("self arm reached for %q", env.TaskID)
	})
	r, err := New(Config{
		NewDeterministic: func(ArtifactRecorder, SecretRegistrar) (invoke.Deterministic, error) { return det, nil },
		Automated:        gate.NewAutomatedEvaluator(),
		Worktrees:        wtMgr,
		RunsDir:          runsDir,
		RepoCloneURL:     func(apiv1.RepoRef) (string, error) { return fixtureRepo, nil },
		StageDispatcher:  fake,
		WorkspaceDeltas:  store,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	res, err := r.Start(ctx, StartInput{
		RunID: runID, Machine: machine, Gaggle: "acme-web",
		Trigger: journal.Trigger{Kind: journal.TriggerManual}, RepoRef: repoRefMain,
		Placements: []journal.PinnedPlacement{selfPin("prepare"), podPin("build"), selfPin("verify")},
	})
	if err != nil || res.Phase != journal.PhaseCompleted {
		t.Fatalf("Start: phase=%s err=%v", res.Phase, err)
	}
	if commitA == "" || commitB == "" {
		t.Fatalf("commits A=%q B=%q were not both made", commitA, commitB)
	}
	// Dispatch: the pod was handed base..A from the mirror, stored in the store.
	if has, err := store.Has(ctx, handedDelta); err != nil || !has {
		t.Fatalf("handed delta %q present in the store: %v, %v", handedDelta, has, err)
	}
	// Surrender: the mirror's run branch is at B, and the next self stage saw it.
	branch := "goobers/continuity/" + runID
	mirror, err := wtMgr.WorkingCopy(ctx, fixtureRepo)
	if err != nil {
		t.Fatal(err)
	}
	if got := gitOut(t, mirror, "rev-parse", "refs/heads/"+branch); got != commitB {
		t.Fatalf("mirror %s = %s, want B %s", branch, got, commitB)
	}
	events := readRunEvents(t, runsDir, runID)
	verify := stageFinished(events, "verify")
	if len(verify) != 1 || verify[0].Outputs["head"] != commitB || verify[0].Outputs["podFile"] != true {
		t.Fatalf("verify saw %+v, want HEAD=B %s with the pod's file", verify, commitB)
	}
	var applied bool
	for _, ev := range events {
		if ev.Type == journal.EventRunnerAnnotation && ev.Stage == "build" && ev.Runner["kind"] == RunnerAnnotationWorkspaceDelta {
			applied = true
			if ev.Runner["outcome"] != "fast-forward" || ev.Runner["after"] != commitB || ev.Runner["before"] != commitA || ev.Runner["tip"] != commitB {
				t.Fatalf("workspace.delta annotation = %v, want fast-forward %s -> %s", ev.Runner, commitA, commitB)
			}
		}
	}
	if !applied {
		t.Fatal("no workspace.delta annotation journaled for the placed stage")
	}
}

type deterministicFunc func(context.Context, apiv1.InvocationEnvelope, apiv1.DeterministicRun) (apiv1.ResultEnvelope, error)

func (f deterministicFunc) Run(ctx context.Context, env apiv1.InvocationEnvelope, run apiv1.DeterministicRun) (apiv1.ResultEnvelope, error) {
	return f(ctx, env, run)
}

// TestRunnerRefusesDivergedSurrenderedDelta: a surrendered delta that
// diverges from the mirror's run branch fails the stage closed with a named
// code rather than being forced onto the mirror.
func TestRunnerRefusesDivergedSurrenderedDelta(t *testing.T) {
	const runID = "run-diverged"
	machine := seamFixtureMachine(t, apiv1.Task{Goal: "build", Run: &apiv1.DeterministicRun{Command: []string{"true"}, Workspace: apiv1.WorkspaceRepo}})
	instanceRoot := t.TempDir()
	wtMgr, err := worktree.NewManager(filepath.Join(instanceRoot, "workcopies"))
	if err != nil {
		t.Fatal(err)
	}
	store, err := blobstore.NewDir(filepath.Join(instanceRoot, "blobstore"))
	if err != nil {
		t.Fatal(err)
	}
	fixtureRepo := newFixtureRepo(t)
	ctx := context.Background()
	branch := "goobers/seam-fixture/" + runID
	fake := &fakeStageDispatcher{}
	fake.onDispatch = func(req StageDispatchRequest) (StageDispatchResult, error) {
		// The pod commits P on top of base while, "meanwhile", the mirror's run
		// branch gains an unrelated commit M — the two diverge.
		pod := filepath.Join(t.TempDir(), "pod")
		runGit(t, "", "clone", "--quiet", fixtureRepo, pod)
		if err := os.WriteFile(filepath.Join(pod, "pod.txt"), []byte("P\n"), 0o644); err != nil {
			return StageDispatchResult{}, err
		}
		runGit(t, pod, "add", "pod.txt")
		runGit(t, pod, "-c", "user.email=pod@example.com", "-c", "user.name=pod", "commit", "--quiet", "-m", "P")
		out, err := workspacedelta.Create(ctx, testGit{}, pod, "refs/remotes/origin/main", "HEAD")
		if err != nil {
			return StageDispatchResult{}, err
		}
		if err := store.Put(ctx, out.Digest, out.Data); err != nil {
			return StageDispatchResult{}, err
		}
		wt, err := wtMgr.Create(ctx, worktree.CreateOptions{RepoURL: fixtureRepo, RunID: runID + "-meanwhile", OwnerRunID: runID, BaseRef: "main", Branch: branch})
		if err != nil {
			return StageDispatchResult{}, err
		}
		if err := os.WriteFile(filepath.Join(wt.Path, "m.txt"), []byte("M\n"), 0o644); err != nil {
			return StageDispatchResult{}, err
		}
		runGit(t, wt.Path, "add", "m.txt")
		runGit(t, wt.Path, "-c", "user.email=m@example.com", "-c", "user.name=m", "commit", "--quiet", "-m", "M")
		if err := wt.Remove(ctx, worktree.RemoveOptions{}); err != nil {
			return StageDispatchResult{}, err
		}
		return StageDispatchResult{
			Result:         apiv1.ResultEnvelope{Status: apiv1.ResultSuccess},
			Placement:      podProvenance(),
			WorkspaceDelta: out.Digest, WorkspaceDeltaBase: out.Base, WorkspaceDeltaTip: out.Tip,
		}, nil
	}
	r, err := New(Config{
		NewDeterministic: func(rec ArtifactRecorder, _ SecretRegistrar) (invoke.Deterministic, error) {
			return &stubDeterministic{rec: rec, byTask: map[string]stubTaskResult{runID + ":query": {status: apiv1.ResultSuccess, outputs: map[string]interface{}{"items": "1"}}}}, nil
		},
		Automated: gate.NewAutomatedEvaluator(), Worktrees: wtMgr, RunsDir: filepath.Join(instanceRoot, "runs"),
		RepoCloneURL:    func(apiv1.RepoRef) (string, error) { return fixtureRepo, nil },
		StageDispatcher: fake, WorkspaceDeltas: store,
	})
	if err != nil {
		t.Fatal(err)
	}
	res, err := r.Start(ctx, StartInput{
		RunID: runID, Machine: machine, Gaggle: "acme-web",
		Trigger: journal.Trigger{Kind: journal.TriggerManual}, RepoRef: repoRefMain,
		Placements: []journal.PinnedPlacement{selfPin("query"), podPin("build")},
	})
	if err == nil || res.Phase != journal.PhaseFailed {
		t.Fatalf("Start: phase=%s err=%v; want the run to fail closed", res.Phase, err)
	}
	if res.FailureStage != "build" || !strings.Contains(res.FailureMessage, "diverged") || !strings.Contains(err.Error(), "refusing to overwrite history") {
		t.Fatalf("failure = %s: %s (err %v)", res.FailureStage, res.FailureMessage, err)
	}
	var coded bool
	for _, ev := range readRunEvents(t, filepath.Join(instanceRoot, "runs"), runID) {
		if ev.Type == journal.EventError && ev.Stage == "build" && ev.Runner[stageErrorCodeKey] == errCodeWorkspaceDeltaApply {
			coded = true
		}
	}
	if !coded {
		t.Fatalf("no error event carrying %s", errCodeWorkspaceDeltaApply)
	}
}

// TestRunnerResumeAdoptsPlacedAttemptInConcurrentBranch is ruling 6 for the
// concurrent-parallel path (runParallelBranch): a branch stage pinned to a
// runner, in flight when the daemon died, is settled through Describe/Await
// on resume — adopted when Completed, re-dispatched as attempt 2 on the
// infrastructure budget when NotFound — while the other branches start fresh.
func TestRunnerResumeAdoptsPlacedAttemptInConcurrentBranch(t *testing.T) {
	for _, tc := range []struct {
		name      string
		state     StageAttemptState
		wantAdopt bool
	}{
		{name: "completed", state: StageAttemptCompleted, wantAdopt: true},
		{name: "not-found", state: StageAttemptNotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runID := "run-parallel-adopt-" + tc.name
			machine := parallelRunnerMachine(t, 2, apiv1.WorkspaceScratch)
			fake := &fakeStageDispatcher{describe: tc.state}
			fake.onAwait = func(StageDispatchRequest) (StageDispatchResult, error) {
				return StageDispatchResult{Result: apiv1.ResultEnvelope{Status: apiv1.ResultSuccess, Outputs: map[string]interface{}{"lens": "adopted"}}, Placement: podProvenance()}, nil
			}
			fake.onDispatch = func(StageDispatchRequest) (StageDispatchResult, error) {
				return StageDispatchResult{Result: apiv1.ResultEnvelope{Status: apiv1.ResultSuccess, Outputs: map[string]interface{}{"lens": "redispatched"}}, Placement: podProvenance()}, nil
			}
			r, runsDir := newSeamRunner(t, map[string]stubTaskResult{
				runID + ":lens-b":  {status: apiv1.ResultSuccess, outputs: map[string]interface{}{"lens": "b"}},
				runID + ":lens-c":  {status: apiv1.ResultSuccess, outputs: map[string]interface{}{"lens": "c"}},
				runID + ":collate": {status: apiv1.ResultSuccess},
			}, fake, func(cfg *Config) { cfg.ScratchDir = t.TempDir() })

			jr, err := journal.Create(runsDir, journal.RunIdentity{
				RunID: runID, Workflow: machine.Def.Name, WorkflowVersion: machine.Def.Version,
				WorkflowDigest: machine.Digest(), Gaggle: "demo", Trigger: journal.Trigger{Kind: journal.TriggerManual},
				Placements: []journal.PinnedPlacement{podPin("lens-a"), selfPin("lens-b"), selfPin("lens-c"), selfPin("collate")},
			}, nil)
			if err != nil {
				t.Fatalf("journal.Create: %v", err)
			}
			jr.SetMachineState("fan")
			for _, event := range []journal.Event{
				{Type: journal.EventParallelStarted, Parallel: "fan", Completeness: []journal.BranchOutcome{
					{Branch: 1, Name: "a"}, {Branch: 2, Name: "b"}, {Branch: 3, Name: "c"},
				}},
				{Type: journal.EventBranchStarted, Parallel: "fan", Branch: 1, BranchName: "a", Stage: "lens-a"},
				{Type: journal.EventStageStarted, Stage: "lens-a", Branch: 1, Attempt: 1},
			} {
				if err := jr.Append(event); err != nil {
					t.Fatalf("append %s: %v", event.Type, err)
				}
			}
			if err := jr.Close(); err != nil {
				t.Fatalf("close journal: %v", err)
			}

			res, err := r.Resume(context.Background(), ResumeInput{RunID: runID, Machine: machine, RecoveryReason: "daemon_restart"})
			if err != nil || res.Phase != journal.PhaseCompleted {
				t.Fatalf("Resume: phase=%s err=%v", res.Phase, err)
			}
			dispatches, awaits := fake.snapshot()
			finished := stageFinished(readRunEvents(t, runsDir, runID), "lens-a")
			if tc.wantAdopt {
				if len(dispatches) != 0 || len(awaits) != 1 || awaits[0].Attempt != 1 {
					t.Fatalf("adoption: dispatches=%d awaits=%+v", len(dispatches), awaits)
				}
				if len(finished) != 1 || finished[0].Attempt != 1 || finished[0].Branch != 1 || finished[0].Outputs["lens"] != "adopted" {
					t.Fatalf("lens-a stage.finished = %+v, want attempt 1 on branch 1 carrying the adopted outputs", finished)
				}
				return
			}
			if len(awaits) != 0 || len(dispatches) != 1 || dispatches[0].Attempt != 2 || dispatches[0].AttemptClass != journal.AttemptInfra {
				t.Fatalf("N+1: awaits=%d dispatches=%+v", len(awaits), dispatches)
			}
			if len(finished) != 2 || finished[0].Error == nil || finished[0].Error.Code != interruptedAttemptErrorCode || finished[1].Attempt != 2 || finished[1].Outputs["lens"] != "redispatched" {
				t.Fatalf("N+1 lens-a stage.finished = %+v", finished)
			}
		})
	}
}
