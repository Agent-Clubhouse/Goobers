package journalequivalence

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"

	"sigs.k8s.io/yaml"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/readmodel"
	"github.com/goobers/goobers/internal/runner"
	"github.com/goobers/goobers/internal/testgit"
	"github.com/goobers/goobers/internal/workflow"
	"github.com/goobers/goobers/internal/worktree"
)

var localBaselineDigests = map[string]string{
	"retry-artifact-inputs-and-gates": "sha256:367154a223e1cb782e85eb51754dd98890876dd9683ca69dc0b5e0f345a95c3b",
	"human-gate":                      "sha256:4ade0f52fc3ab78b2202dbdfb128b91dcdae2310124709df5575c4bae95489bf",
	"stage-boundary-cancellation":     "sha256:69ad5302bca5ab6e5806c546193bf842f8ebea0122f642b0e7a44ecccd3e56e6",
	"gate-fail-rerun":                 "sha256:fc5e7bfff6eefcdf64bca32e0ac76b97e68776a6b2f5c0036a470a6375cb0dc5",
	"terminal-resume":                 "sha256:bcfc16278dbce9969be24de7dfd240c87e3960109399e9b6c79c923396ac1066",
	"infrastructure-retry":            "sha256:4020c3810b2d63ea427856a7952211490e4f134d71f87bfc2cccbccd4d612dbc",
}

func TestLocalJournalEquivalenceBaseline(t *testing.T) {
	for _, scenario := range localCorpus() {
		t.Run(scenario.name, func(t *testing.T) {
			var baseline canonicalRun
			for repetition := 0; repetition < 3; repetition++ {
				got := runScenario(t, scenario)
				if scenario.name == "infrastructure-retry" {
					assertDomainInfrastructureError(t, got)
				}
				if repetition == 0 {
					baseline = got
					if digest := canonicalDigest(t, got); digest != localBaselineDigests[scenario.name] {
						t.Fatalf("local baseline digest = %s, want %s", digest, localBaselineDigests[scenario.name])
					}

					continue
				}

				if !reflect.DeepEqual(got, baseline) {
					t.Fatalf("repetition %d differs from local baseline:\nfirst: %+v\n got: %+v", repetition+1, baseline, got)
				}
			}
		})
	}
}

func TestCanonicalizerMasksOnlyAllowedVolatility(t *testing.T) {
	const runID = "11111111111111111111111111111111"
	value := map[string]any{
		"time": "volatile", "runId": runID, "host": "runner-a", "durationMs": 42,
		"name": "artifact-" + runID, "status": "completed", "format": "json",
		"finishedAt": nil, "latestErrorAt": "", "startedAt": "0001-01-01T00:00:00Z",
	}
	maskVolatile(value, runID)
	for _, key := range []string{"time", "runId", "host", "durationMs"} {
		if value[key] != "<masked>" {
			t.Fatalf("%s = %v, want masked", key, value[key])
		}
	}
	if value["name"] != "artifact-<run-id>" || value["status"] != "completed" || value["format"] != "json" {
		t.Fatalf("semantic fields changed: %+v", value)
	}
	if value["finishedAt"] != nil || value["latestErrorAt"] != "" ||
		value["startedAt"] != "0001-01-01T00:00:00Z" {
		t.Fatalf("unset timestamp presence changed: %+v", value)
	}
	unset := canonicalJSON(t, map[string]any{"finishedAt": nil}, runID)
	zero := canonicalJSON(t, map[string]any{"finishedAt": "0001-01-01T00:00:00Z"}, runID)
	set := canonicalJSON(t, map[string]any{"finishedAt": "2026-10-23T00:00:00Z"}, runID)
	if bytes.Equal(unset, set) || bytes.Equal(zero, set) {
		t.Fatalf("unset timestamp canonicalized like set: null=%s zero=%s set=%s", unset, zero, set)
	}
}

type corpusScenario struct {
	name    string
	runID   string
	fixture string
	run     func(*testing.T, context.Context, *runner.Runner, *workflow.Machine, string, string) runner.Result
	script  deterministicConfig
	goober  gooberConfig
	expect  []journal.EventType
}

func localCorpus() []corpusScenario {
	return []corpusScenario{
		{
			name: "retry-artifact-inputs-and-gates", runID: strings.Repeat("1", 32),
			fixture: "retry-gates.yaml",
			script: deterministicConfig{
				failFirst: true, artifact: []byte(`{"result":"stable"}`),
			},
			run: startToTerminal,
			expect: []journal.EventType{
				journal.EventStageStarted, journal.EventError, journal.EventArtifactRecorded,
				journal.EventStageFinished, journal.EventGateStarted, journal.EventGateEvaluated,
				journal.EventReviewerStarted, journal.EventReviewerFinished, journal.EventRunFinished,
			},
		},
		{
			name: "human-gate", runID: strings.Repeat("2", 32),
			fixture: "human-gate.yaml",
			run:     startAndApproveHumanGate,
			expect:  []journal.EventType{journal.EventGatePaused, journal.EventGateEvaluated, journal.EventRunFinished},
		},
		{
			name: "stage-boundary-cancellation", runID: strings.Repeat("3", 32),
			fixture: "cancellation.yaml",
			script:  deterministicConfig{cancelAfter: "first"},
			run:     startToCancellationBoundary,
			expect:  []journal.EventType{journal.EventStageStarted, journal.EventStageFinished},
		},
		{
			name: "gate-fail-rerun", runID: strings.Repeat("4", 32),
			fixture: "gate-rerun.yaml", run: startAndRerunGate,
			goober: gooberConfig{failReviewWithoutAddendum: true},
			expect: []journal.EventType{
				journal.EventGateEvaluated, journal.EventStageRerunRequested, journal.EventRunFinished,
			},
		},
		{
			name: "terminal-resume", runID: strings.Repeat("5", 32),
			fixture: "terminal-resume.yaml", run: startAndResumeTerminal,
			script: deterministicConfig{businessFailFirst: true},
			expect: []journal.EventType{
				journal.EventRunFinished, journal.EventRunResumed, journal.EventStageStarted,
			},
		},
		{
			name: "infrastructure-retry", runID: strings.Repeat("6", 32),
			fixture: "infra-retry.yaml", run: startToTerminal,
			script: deterministicConfig{infraFailFirst: true},
			expect: []journal.EventType{journal.EventError, journal.EventStageStarted, journal.EventRunFinished},
		},
	}
}

type canonicalRun struct {
	Events          []string
	ArtifactDigests []string
	Projection      string
}

func canonicalDigest(t *testing.T, run canonicalRun) string {
	t.Helper()
	data, err := json.Marshal(run)
	if err != nil {
		t.Fatal(err)
	}
	return fmt.Sprintf("sha256:%x", sha256.Sum256(data))
}

func runScenario(t *testing.T, scenario corpusScenario) canonicalRun {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r, runsDir := newCorpusRunner(t, newDeterministicScript(scenario.script), scenario.goober, cancel)
	machine := loadMachine(t, scenario.fixture)
	result := scenario.run(t, ctx, r, machine, runsDir, scenario.runID)
	if result.Phase == "" {
		t.Fatal("runner returned an empty phase")
	}
	reader, err := journal.OpenRead(filepath.Join(runsDir, scenario.runID))
	if err != nil {
		t.Fatal(err)
	}
	return canonicalReader(t, scenario, reader)
}

func canonicalReader(t *testing.T, scenario corpusScenario, reader journal.RunReader) canonicalRun {
	t.Helper()
	identity, err := reader.Identity()
	if err != nil {
		t.Fatal(err)
	}
	events, err := reader.Events()
	if err != nil {
		t.Fatal(err)
	}
	if err := journal.MonotonicSeq(events); err != nil {
		t.Fatal(err)
	}
	assertEventCoverage(t, scenario.expect, events)
	if scenario.name == "infrastructure-retry" {
		assertAttemptClass(t, events, journal.AttemptInfra)
	}
	projection, err := readmodel.ProjectRunFromJournal(reader, identity, events)
	if err != nil {
		t.Fatal(err)
	}

	canonicalEvents := make([]string, 0, len(events))
	var digests []string
	for _, event := range events {
		canonicalEvents = append(canonicalEvents, string(canonicalJSON(t, event, scenario.runID)))
		if event.Ref != nil && event.Ref.Digest != "" {
			digests = append(digests, event.Ref.Digest)
		}
		for _, artifact := range event.Artifacts {
			if artifact.Digest != "" {
				digests = append(digests, artifact.Digest)
			}
		}
	}
	slices.Sort(digests)
	digests = slices.Compact(digests)
	return canonicalRun{
		Events: canonicalEvents, ArtifactDigests: digests,
		Projection: string(canonicalJSON(t, projection, scenario.runID)),
	}
}

func assertAttemptClass(t *testing.T, events []journal.Event, class journal.AttemptClass) {
	t.Helper()
	for _, event := range events {
		if event.Type == journal.EventStageStarted && event.AttemptClass == class {
			return
		}
	}
	t.Errorf("corpus does not emit a stage.started with attempt class %s", class)
}

func assertEventCoverage(t *testing.T, expected []journal.EventType, events []journal.Event) {
	t.Helper()
	seen := make(map[journal.EventType]bool, len(events))
	for _, event := range events {
		seen[event.Type] = true
	}
	for _, eventType := range expected {
		if !seen[eventType] {
			t.Errorf("corpus does not emit %s", eventType)
		}
	}
}

func canonicalJSON(t *testing.T, value any, runID string) json.RawMessage {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	var decoded any
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}
	maskVolatile(decoded, runID)
	data, err = json.Marshal(decoded)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func maskVolatile(value any, runID string) {
	switch typed := value.(type) {
	case []any:
		for index, item := range typed {
			if text, ok := item.(string); ok {
				typed[index] = strings.ReplaceAll(text, runID, "<run-id>")
				continue
			}
			maskVolatile(item, runID)
		}
	case map[string]any:
		for key, item := range typed {
			if volatileField(key) {
				if item != nil && item != "" && item != "0001-01-01T00:00:00Z" {
					typed[key] = "<masked>"
				}
				continue
			}
			if text, ok := item.(string); ok {
				typed[key] = strings.ReplaceAll(text, runID, "<run-id>")
				continue
			}
			maskVolatile(item, runID)
		}
	}
}

func volatileField(key string) bool {
	switch strings.ToLower(key) {
	case "time", "startedat", "finishedat", "completedat", "requestedat", "expiresat",
		"acknowledgedat", "retryat", "lastheartbeatat", "latesterrorat", "reviewat",
		"queuedat", "podstartedat", "updatedat", "occurredat",
		"lastactivity", "duration", "durationms",
		"elapsed", "elapsedms", "runid", "host", "hostname", "hostidentity", "pid", "processid":
		return true
	default:
		return false
	}
}

type deterministicScript struct {
	mu     sync.Mutex
	calls  map[string]int
	config deterministicConfig
}

type deterministicConfig struct {
	failFirst         bool
	infraFailFirst    bool
	businessFailFirst bool
	artifact          []byte
	cancelAfter       string
}

func newDeterministicScript(config deterministicConfig) *deterministicScript {
	return &deterministicScript{calls: map[string]int{}, config: config}
}

func (s *deterministicScript) next(task string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls[task]++
	return s.calls[task]
}

type scriptedDeterministic struct {
	rec    runner.ArtifactRecorder
	script *deterministicScript
	cancel context.CancelFunc
}

func (s *scriptedDeterministic) Run(_ context.Context, env apiv1.InvocationEnvelope, _ apiv1.DeterministicRun) (apiv1.ResultEnvelope, error) {
	call := s.script.next(env.TaskID)
	if s.script.config.failFirst && call == 1 {
		return apiv1.ResultEnvelope{}, errors.New("fixture policy retry")
	}
	if s.script.config.infraFailFirst && call == 1 {
		return apiv1.ResultEnvelope{}, invoke.InfrastructureFailure(errors.New("fixture infrastructure retry"))
	}
	if s.script.config.businessFailFirst && call == 1 {
		return apiv1.ResultEnvelope{
			Status: apiv1.ResultFailure,
			Error:  &apiv1.ErrorInfo{Code: "fixture_failure", Message: "resume required"},
		}, nil
	}
	result := apiv1.ResultEnvelope{
		Status: apiv1.ResultSuccess, Summary: "stable result",
		Outputs: map[string]any{"input": env.Inputs["fixture"]},
	}
	if len(s.script.config.artifact) > 0 {
		ref, err := s.rec.RecordArtifact("result.json", s.script.config.artifact)
		if err != nil {
			return apiv1.ResultEnvelope{}, err
		}
		result.Artifacts = []apiv1.ArtifactPointer{{
			Path: ref.Path, Digest: ref.Digest, Size: ref.Size, Integrity: ref.Integrity,
			MediaType: "application/json",
		}}
	}
	if err := commitFixtureChange(env.Workspace, env.TaskID, call); err != nil {
		return apiv1.ResultEnvelope{}, err
	}
	if s.script.config.cancelAfter != "" && strings.HasSuffix(env.TaskID, ":"+s.script.config.cancelAfter) {
		s.cancel()
	}
	return result, nil
}

type gooberConfig struct {
	failReviewWithoutAddendum bool
}

type scriptedGoober struct {
	config gooberConfig
}

func (scriptedGoober) Invoke(context.Context, apiv1.InvocationEnvelope) (apiv1.ResultEnvelope, error) {
	return apiv1.ResultEnvelope{Status: apiv1.ResultSuccess}, nil
}

func (g scriptedGoober) Review(_ context.Context, env apiv1.InvocationEnvelope) (apiv1.Verdict, error) {
	if g.config.failReviewWithoutAddendum && env.InstructionAddendum == "" {
		return apiv1.Verdict{Decision: apiv1.VerdictFail, Rationale: "operator guidance required"}, nil
	}
	return apiv1.Verdict{Decision: apiv1.VerdictPass, Rationale: "stable approval"}, nil
}

type passingAutomated struct{}

func (passingAutomated) Evaluate(context.Context, apiv1.AutomatedGate, apiv1.InvocationEnvelope) (string, error) {
	return "pass", nil
}

func newCorpusRunner(t *testing.T, script *deterministicScript, goober gooberConfig, cancel context.CancelFunc) (*runner.Runner, string) {
	t.Helper()
	root := t.TempDir()
	runsDir := filepath.Join(root, "runs")
	manager, err := worktree.NewManager(filepath.Join(root, "workcopies"))
	if err != nil {
		t.Fatal(err)
	}
	repository := fixtureRepository(t)
	r, err := runner.New(runner.Config{
		NewDeterministic: func(rec runner.ArtifactRecorder, _ runner.SecretRegistrar) (invoke.Deterministic, error) {
			return &scriptedDeterministic{rec: rec, script: script, cancel: cancel}, nil
		},
		NewAgentic: func(string, runner.ArtifactRecorder, runner.SecretRegistrar) (invoke.Goober, error) {
			return scriptedGoober{config: goober}, nil
		},
		Automated: passingAutomated{},
		Worktrees: manager,
		RunsDir:   runsDir,
		RepoCloneURL: func(apiv1.RepoRef) (string, error) {
			return repository, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return r, runsDir
}

func loadMachine(t *testing.T, fixture string) *workflow.Machine {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", fixture))
	if err != nil {
		t.Fatal(err)
	}
	var document apiv1.Workflow
	if err := yaml.UnmarshalStrict(data, &document); err != nil {
		t.Fatal(err)
	}
	machine, err := workflow.Compile(workflow.Definition{
		Name: document.Name, Version: 1, DSLVersion: document.DSLVersion, Spec: document.Spec,
	}, workflow.WithPreviewFeatures(true))
	if err != nil {
		t.Fatal(err)
	}
	return machine
}

func startToTerminal(t *testing.T, ctx context.Context, r *runner.Runner, machine *workflow.Machine, _ string, runID string) runner.Result {
	t.Helper()
	result, err := r.Start(ctx, startInput(runID, machine))
	if err != nil {
		t.Fatal(err)
	}
	if result.Phase != journal.PhaseCompleted {
		t.Fatalf("phase = %s", result.Phase)
	}
	return result
}

func startAndApproveHumanGate(t *testing.T, ctx context.Context, r *runner.Runner, machine *workflow.Machine, _ string, runID string) runner.Result {
	t.Helper()
	result, err := r.Start(ctx, startInput(runID, machine))
	if err != nil {
		t.Fatal(err)
	}
	if result.Phase != journal.PhaseRunning || result.FinalState != "approval" {
		t.Fatalf("pause result = %+v", result)
	}
	result, err = r.Resume(context.Background(), runner.ResumeInput{
		RunID: runID, Machine: machine, RepoRef: fixtureRepoRef(),
		HumanDecision: &runner.HumanGateDecision{
			Gate: "approval", PauseSeq: 2, Decision: "pass", Actor: "operator:fixture",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func startAndRerunGate(t *testing.T, ctx context.Context, r *runner.Runner, machine *workflow.Machine, runsDir, runID string) runner.Result {
	t.Helper()
	result, err := r.Start(ctx, startInput(runID, machine))
	if err != nil {
		t.Fatal(err)
	}
	if result.Phase != journal.PhaseEscalated {
		t.Fatalf("initial rerun phase = %s", result.Phase)
	}
	result, err = r.RerunStage(context.Background(), runner.RerunStageInput{
		RunID: runID, Machine: machine, RepoRef: fixtureRepoRef(), Stage: "review",
		Actor: "operator:fixture", InstructionAddendum: "accept the deterministic fixture",
		ExpectedTerminalSeq: terminalSequence(t, runsDir, runID),
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Phase != journal.PhaseCompleted {
		t.Fatalf("rerun phase = %s", result.Phase)
	}
	return result
}

func startAndResumeTerminal(t *testing.T, ctx context.Context, r *runner.Runner, machine *workflow.Machine, runsDir, runID string) runner.Result {
	t.Helper()
	result, err := r.Start(ctx, startInput(runID, machine))
	if err != nil {
		t.Fatal(err)
	}
	if result.Phase != journal.PhaseFailed {
		t.Fatalf("initial resume phase = %s", result.Phase)
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	result, err = r.ResumeFromTerminal(cancelled, runner.ResumeFromTerminalInput{
		RunID: runID, Machine: machine, RepoRef: fixtureRepoRef(), Target: "recover",
		Actor: "operator:fixture", Action: "override", Rationale: "retry deterministic fixture",
		ExpectedTerminalSeq: terminalSequence(t, runsDir, runID),
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Phase != journal.PhaseRunning || result.FinalState != "recover" {
		t.Fatalf("resume intervention = %+v", result)
	}
	result, err = r.Resume(context.Background(), runner.ResumeInput{
		RunID: runID, Machine: machine, RepoRef: fixtureRepoRef(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Phase != journal.PhaseCompleted {
		t.Fatalf("resumed phase = %s", result.Phase)
	}
	return result
}

func startToCancellationBoundary(t *testing.T, ctx context.Context, r *runner.Runner, machine *workflow.Machine, _ string, runID string) runner.Result {
	t.Helper()
	result, err := r.Start(ctx, startInput(runID, machine))
	if err != nil {
		t.Fatal(err)
	}
	if result.Phase != journal.PhaseRunning || result.FinalState != "second" {
		t.Fatalf("cancellation boundary result = %+v", result)
	}
	return result
}

func terminalSequence(t *testing.T, runsDir, runID string) uint64 {
	t.Helper()
	reader, err := journal.OpenRead(filepath.Join(runsDir, runID))
	if err != nil {
		t.Fatal(err)
	}
	events, err := reader.Events()
	if err != nil {
		t.Fatal(err)
	}
	for index := len(events) - 1; index >= 0; index-- {
		if events[index].Type == journal.EventRunFinished {
			return events[index].Seq
		}
	}
	t.Fatalf("run %s has no terminal event", runID)
	return 0
}

func startInput(runID string, machine *workflow.Machine) runner.StartInput {
	return runner.StartInput{
		RunID: runID, Machine: machine, Gaggle: "equivalence",
		Trigger: journal.Trigger{Kind: journal.TriggerManual}, RepoRef: fixtureRepoRef(),
	}
}

func fixtureRepoRef() apiv1.RepoRef {
	return apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "fixture", Name: "repository", Branch: "main"}
}

func fixtureRepository(t *testing.T) string {
	t.Helper()
	work := t.TempDir()
	bare := filepath.Join(t.TempDir(), "fixture.git")
	runGit(t, work, "init", "--initial-branch=main")
	runGit(t, work, "config", "user.email", "fixture@example.com")
	runGit(t, work, "config", "user.name", "fixture")
	if err := os.WriteFile(filepath.Join(work, "README.md"), []byte("fixture\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, work, "add", ".")
	runGit(t, work, "commit", "-m", "initial")
	runGit(t, "", "clone", "--bare", work, bare)
	return bare
}

func commitFixtureChange(workspace, task string, call int) error {
	path := filepath.Join(workspace, task+".txt")
	if err := os.WriteFile(path, []byte(fmt.Sprintf("%s-%d\n", task, call)), 0o644); err != nil {
		return err
	}
	cmd := testgit.Command("add", ".")
	cmd.Dir = workspace
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("git add: %w: %s", err, output)
	}
	cmd = testgit.Command("-c", "user.email=fixture@example.com", "-c", "user.name=fixture", "commit", "-m", task)
	cmd.Dir = workspace
	if output, err := cmd.CombinedOutput(); err != nil && !bytes.Contains(output, []byte("nothing to commit")) {
		return fmt.Errorf("git commit: %w: %s", err, output)
	}
	return nil
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := testgit.Command(args...)
	cmd.Dir = dir
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, output)
	}
}

// Infrastructure failures retain the invoke marker and underlying cause, not
// unary errors.Join wrappers from successful cleanup or mutation reporting.
func assertDomainInfrastructureError(t *testing.T, run canonicalRun) {
	t.Helper()
	for _, raw := range run.Events {
		var event struct {
			Type   journal.EventType
			Error  *journal.ErrorDetail
			Runner map[string]any
		}
		if err := json.Unmarshal([]byte(raw), &event); err != nil {
			t.Fatal(err)
		}
		if event.Type != journal.EventError {
			continue
		}
		if event.Error == nil || event.Error.Message != "fixture infrastructure retry" || len(event.Error.Causes) != 2 || event.Runner["errorClass"] != "infra" || event.Runner["errorCode"] != "infra.failure" {
			t.Fatalf("domain error drift: %+v, runner=%v", event.Error, event.Runner)
		}
		return
	}
	t.Fatal("infrastructure retry recorded no error")
}
