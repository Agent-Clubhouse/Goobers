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
	"retry-artifact-inputs-and-gates": "sha256:196737967b344cfcc26b2f614ca02b8a134b3f05a48d6873bf83a67216a6dc4e",
	"human-gate":                      "sha256:ad2946487d76cac39e2246f74dd1e6211ee7ea4ce9ecf7ad7f88f851b26f6acd",
	"stage-boundary-cancellation":     "sha256:1d425cf39d17e8d5172b09345d07c70b6e487209db06c819ec82cc32271df81d",
}

func TestLocalJournalEquivalenceBaseline(t *testing.T) {
	for _, scenario := range localCorpus() {
		t.Run(scenario.name, func(t *testing.T) {
			var baseline canonicalRun
			for repetition := 0; repetition < 3; repetition++ {
				got := runScenario(t, scenario)
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
}

type corpusScenario struct {
	name    string
	runID   string
	fixture string
	run     func(*testing.T, context.Context, *runner.Runner, *workflow.Machine, string) runner.Result
	script  deterministicConfig
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
	r, runsDir := newCorpusRunner(t, newDeterministicScript(scenario.script), cancel)
	machine := loadMachine(t, scenario.fixture)
	result := scenario.run(t, ctx, r, machine, scenario.runID)
	if result.Phase == "" {
		t.Fatal("runner returned an empty phase")
	}
	reader, err := journal.OpenRead(filepath.Join(runsDir, scenario.runID))
	if err != nil {
		t.Fatal(err)
	}
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
				typed[key] = "<masked>"
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
	failFirst   bool
	artifact    []byte
	cancelAfter string
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

type passingGoober struct{}

func (passingGoober) Invoke(context.Context, apiv1.InvocationEnvelope) (apiv1.ResultEnvelope, error) {
	return apiv1.ResultEnvelope{Status: apiv1.ResultSuccess}, nil
}

func (passingGoober) Review(context.Context, apiv1.InvocationEnvelope) (apiv1.Verdict, error) {
	return apiv1.Verdict{Decision: apiv1.VerdictPass, Rationale: "stable approval"}, nil
}

type passingAutomated struct{}

func (passingAutomated) Evaluate(context.Context, apiv1.AutomatedGate, apiv1.InvocationEnvelope) (string, error) {
	return "pass", nil
}

func newCorpusRunner(t *testing.T, script *deterministicScript, cancel context.CancelFunc) (*runner.Runner, string) {
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
			return passingGoober{}, nil
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

func startToTerminal(t *testing.T, ctx context.Context, r *runner.Runner, machine *workflow.Machine, runID string) runner.Result {
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

func startAndApproveHumanGate(t *testing.T, ctx context.Context, r *runner.Runner, machine *workflow.Machine, runID string) runner.Result {
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

func startToCancellationBoundary(t *testing.T, ctx context.Context, r *runner.Runner, machine *workflow.Machine, runID string) runner.Result {
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
