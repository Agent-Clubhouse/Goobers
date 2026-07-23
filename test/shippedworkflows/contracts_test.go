package shippedworkflows

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"

	"sigs.k8s.io/yaml"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/credentials"
	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/internal/gate"
	"github.com/goobers/goobers/internal/harness"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/runner"
	"github.com/goobers/goobers/internal/workflow"
	"github.com/goobers/goobers/internal/worktree"
	"github.com/goobers/goobers/providers"
)

const (
	contractHelperMode       = "GOOBERS_SHIPPED_CONTRACT_HELPER"
	contractHelperPayload    = "GOOBERS_SHIPPED_CONTRACT_PAYLOAD"
	contractHelperResultFile = "GOOBERS_SHIPPED_CONTRACT_RESULT_FILE"
	contractHelperExitCode   = "GOOBERS_SHIPPED_CONTRACT_EXIT_CODE"
	contractNumber           = float64(73)
)

type scenarioKind string

const (
	scenarioHappy      scenarioKind = "happy"
	scenarioFailure    scenarioKind = "failure"
	scenarioEscalation scenarioKind = "escalation"
)

type contractScenario struct {
	kind               scenarioKind
	wantPhase          journal.RunPhase
	wantSequence       []string
	wantGateFailure    bool
	wantGateEscalation bool
}

type workflowContract struct {
	scenarios []contractScenario
}

var shippedContracts = map[string]workflowContract{
	"backlog-curation": {
		scenarios: []contractScenario{
			scenario(scenarioHappy, journal.PhaseCompleted, "task:query-backlog", "task:curate", "task:release-claim"),
			scenario(scenarioFailure, journal.PhaseFailed, "task:query-backlog"),
			scenario(scenarioEscalation, journal.PhaseEscalated, "task:query-backlog"),
		},
	},
	"implementation": {
		scenarios: []contractScenario{
			scenario(scenarioHappy, journal.PhaseCompleted,
				"task:query-backlog", "task:implement", "gate:review=pass",
				"task:local-ci", "gate:local-gate=pass", "task:push-branch",
				"task:open-pr", "gate:open-pr-gate=pass", "task:ci-poll",
				"gate:ci-gate=pass", "task:close-out"),
			withGateFailure(scenario(scenarioFailure, journal.PhaseAborted,
				"task:query-backlog", "task:implement", "gate:review=fail",
				"task:park-needs-human")),
			withGateEscalation(scenario(scenarioEscalation, journal.PhaseEscalated,
				"task:query-backlog", "task:implement", "gate:review=needs-changes",
				"task:implement", "gate:review=needs-changes", "task:park-escalated")),
		},
	},
	"merge-review": {
		scenarios: []contractScenario{
			scenario(scenarioHappy, journal.PhaseCompleted,
				"task:reconcile-post-merge", "task:pr-select", "task:gather-sibling-context",
				"gate:review=pass", "task:apply-verdict", "gate:published-verdict=pass",
				"task:merge-pr", "gate:merge-gate=merged", "task:post-merge"),
			withGateFailure(scenario(scenarioFailure, journal.PhaseCompleted,
				"task:reconcile-post-merge", "task:pr-select", "task:gather-sibling-context",
				"gate:review=fail", "task:apply-verdict", "gate:published-verdict=fail")),
			scenario(scenarioEscalation, journal.PhaseEscalated,
				"task:reconcile-post-merge", "task:pr-select"),
		},
	},
	"pr-remediation": {
		scenarios: []contractScenario{
			scenario(scenarioHappy, journal.PhaseCompleted,
				"task:update-behind-pr", "gate:update-behind-gate=pass"),
			withGateFailure(scenario(scenarioFailure, journal.PhaseCompleted,
				"task:update-behind-pr", "gate:update-behind-gate=fail",
				"task:gather-pr-context", "task:rebase-pr", "gate:rebase-gate=pass")),
			withGateEscalation(scenario(scenarioEscalation, journal.PhaseEscalated,
				"task:update-behind-pr", "gate:update-behind-gate=fail",
				"task:gather-pr-context", "task:rebase-pr", "gate:rebase-gate=fail",
				"task:remediation-checkpoint", "gate:checkpoint-gate=pass",
				"task:gather-sibling-context",
				"task:implement", "gate:review=needs-changes",
				"task:implement", "gate:review=needs-changes", "task:park-escalated")),
		},
	},
	"tutor": {
		scenarios: []contractScenario{
			scenario(scenarioHappy, journal.PhaseCompleted,
				"task:gather-signals", "task:analyze", "task:draft-change",
				"task:validate-config", "gate:config-valid=pass",
				"task:push-branch", "task:open-pr"),
			withGateFailure(scenario(scenarioFailure, journal.PhaseAborted,
				"task:gather-signals", "task:analyze", "task:draft-change",
				"task:validate-config", "gate:config-valid=fail")),
			scenario(scenarioEscalation, journal.PhaseEscalated, "task:gather-signals"),
		},
	},
	"work-nomination": {
		scenarios: []contractScenario{
			scenario(scenarioHappy, journal.PhaseCompleted, "task:gather-signals", "task:nominate"),
			scenario(scenarioFailure, journal.PhaseFailed, "task:gather-signals"),
			scenario(scenarioEscalation, journal.PhaseEscalated, "task:gather-signals"),
		},
	},
}

func scenario(kind scenarioKind, phase journal.RunPhase, sequence ...string) contractScenario {
	return contractScenario{kind: kind, wantPhase: phase, wantSequence: sequence}
}

func withGateFailure(s contractScenario) contractScenario {
	s.wantGateFailure = true
	return s
}

func withGateEscalation(s contractScenario) contractScenario {
	s.wantGateFailure = true
	s.wantGateEscalation = true
	return s
}

func TestShippedWorkflowCommandHelper(t *testing.T) {
	if os.Getenv(contractHelperMode) == "" {
		return
	}
	if resultFile := os.Getenv(contractHelperResultFile); resultFile != "" {
		if err := os.WriteFile(resultFile, []byte(os.Getenv(contractHelperPayload)), 0o644); err != nil {
			_, _ = fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
	}
	exitCode, err := strconv.Atoi(os.Getenv(contractHelperExitCode))
	if err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	os.Exit(exitCode)
}

func TestShippedWorkflowContracts(t *testing.T) {
	root := repositoryRoot(t)
	selfhost := filepath.Join(root, "selfhost")
	set, report, err := instance.LoadConfigDir(selfhost)
	if err != nil {
		t.Fatalf("load selfhost config: %v\n%v", err, report)
	}
	allowPreview := set.Manifest != nil && workflow.PreviewFeaturesEnabled(set.Manifest.Annotations)

	discovered := discoverWorkflowDefinitions(t, selfhost)
	loaded := make(map[string]apiv1.Workflow, len(set.Workflows))
	for _, definition := range set.Workflows {
		loaded[definition.Name] = definition
		source, ok := set.WorkflowSource(definition.Spec.Gaggle, definition.Name)
		if !ok {
			t.Fatalf("loaded workflow %q has no source path", definition.Name)
		}
		if got := filepath.ToSlash(source); got != discovered[definition.Name] {
			t.Fatalf("workflow %q source = %q, discovered %q", definition.Name, got, discovered[definition.Name])
		}
	}
	assertContractCoverage(t, discovered, loaded)

	names := make([]string, 0, len(loaded))
	for name := range loaded {
		names = append(names, name)
	}
	sort.Strings(names)
	gateCapabilities := gooberCapabilities(set.Goobers)

	for _, name := range names {
		definition := loaded[name]
		contract := shippedContracts[name]
		t.Run(name, func(t *testing.T) {
			for _, contractScenario := range contract.scenarios {
				contractScenario := contractScenario
				t.Run(string(contractScenario.kind), func(t *testing.T) {
					machine, err := workflow.Compile(workflow.Definition{
						Name: definition.Name, Version: 1, Spec: definition.Spec,
					}, workflow.WithPreviewFeatures(allowPreview))
					if err != nil {
						t.Fatalf("compile shipped workflow: %v", err)
					}

					script := newScenarioScript(definition.Name, contractScenario.kind)
					localRunner, runsDir := newContractRunner(t, script, gateCapabilities)
					runID := "contract-" + definition.Name + "-" + string(contractScenario.kind)
					_, err = localRunner.Start(context.Background(), runner.StartInput{
						RunID:   runID,
						Machine: machine,
						Gaggle:  definition.Spec.Gaggle,
						Trigger: journal.Trigger{Kind: journal.TriggerManual},
						RepoRef: apiv1.RepoRef{
							Provider: apiv1.ProviderGitHub,
							Owner:    "fixture",
							Name:     "repository",
							Branch:   "main",
						},
					})
					if err != nil {
						t.Fatalf("run shipped workflow: %v", err)
					}

					reader, err := journal.OpenRead(filepath.Join(runsDir, runID))
					if err != nil {
						t.Fatalf("open run journal: %v", err)
					}
					events, err := reader.Events()
					if err != nil {
						t.Fatalf("read run events: %v", err)
					}
					assertJournalScenario(t, events, contractScenario)
					state, err := reader.State()
					if err != nil {
						t.Fatalf("read run state: %v", err)
					}
					if state.Phase != contractScenario.wantPhase {
						t.Fatalf("journal phase = %q, want %q", state.Phase, contractScenario.wantPhase)
					}
					if definition.Name == "merge-review" && contractScenario.kind == scenarioHappy {
						assertNumericInputThreading(t, events)
					}
				})
			}
		})
	}
}

func repositoryRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve test source path")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
}

var yamlDocumentSeparator = regexp.MustCompile(`(?m)^---\s*$`)

func discoverWorkflowDefinitions(t *testing.T, root string) map[string]string {
	t.Helper()
	discovered := map[string]string{}
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		ext := strings.ToLower(filepath.Ext(path))
		if ext != ".yaml" && ext != ".yml" {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, document := range yamlDocumentSeparator.Split(string(data), -1) {
			var metadata struct {
				Kind     string `json:"kind"`
				Metadata struct {
					Name string `json:"name"`
				} `json:"metadata"`
			}
			if err := yaml.Unmarshal([]byte(document), &metadata); err != nil || metadata.Kind != "Workflow" {
				continue
			}
			rel, err := filepath.Rel(root, path)
			if err != nil {
				return err
			}
			if previous, exists := discovered[metadata.Metadata.Name]; exists {
				return fmt.Errorf("duplicate shipped workflow %q in %s and %s", metadata.Metadata.Name, previous, rel)
			}
			discovered[metadata.Metadata.Name] = filepath.ToSlash(rel)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("discover shipped workflows: %v", err)
	}
	return discovered
}

func assertContractCoverage(t *testing.T, discovered map[string]string, loaded map[string]apiv1.Workflow) {
	t.Helper()
	for name, path := range discovered {
		if _, ok := loaded[name]; !ok {
			t.Errorf("discovered workflow %q (%s) is not loaded by the production config loader", name, path)
		}
		if _, ok := shippedContracts[name]; !ok {
			t.Errorf("discovered workflow %q (%s) has no contract scenarios", name, path)
		}
	}
	for name, contract := range shippedContracts {
		if _, ok := discovered[name]; !ok {
			t.Errorf("contract scenarios name removed or undiscoverable workflow %q", name)
		}
		seen := map[scenarioKind]bool{}
		for _, scenario := range contract.scenarios {
			if seen[scenario.kind] {
				t.Errorf("workflow %q repeats %q scenario", name, scenario.kind)
			}
			seen[scenario.kind] = true
		}
		for _, required := range []scenarioKind{scenarioHappy, scenarioFailure, scenarioEscalation} {
			if !seen[required] {
				t.Errorf("workflow %q has no %q contract scenario", name, required)
			}
		}
	}
	if len(discovered) != len(loaded) || len(discovered) != len(shippedContracts) {
		t.Fatalf("workflow coverage counts: discovered=%d loaded=%d scripted=%d", len(discovered), len(loaded), len(shippedContracts))
	}
}

func gooberCapabilities(goobers []apiv1.Goober) map[string][]string {
	out := make(map[string][]string, len(goobers))
	for _, goober := range goobers {
		out[goober.Name] = append([]string(nil), goober.Spec.Capabilities...)
	}
	return out
}

func assertJournalScenario(t *testing.T, events []journal.Event, contract contractScenario) {
	t.Helper()
	var (
		sequence      []string
		terminalPhase string
		gateFailed    bool
		gateEscalated bool
	)
	for _, event := range events {
		switch event.Type {
		case journal.EventStageStarted:
			sequence = append(sequence, "task:"+event.Stage)
		case journal.EventGateEvaluated:
			sequence = append(sequence, "gate:"+event.Gate+"="+event.Verdict)
			if event.Verdict != gate.OutcomePass {
				gateFailed = true
			}
			gateEscalated = gateEscalated || event.Escalated
		case journal.EventRunFinished:
			terminalPhase = event.Status
		}
	}
	if !reflect.DeepEqual(sequence, contract.wantSequence) {
		t.Fatalf("journal execution sequence:\n got: %v\nwant: %v", sequence, contract.wantSequence)
	}
	if terminalPhase != string(contract.wantPhase) {
		t.Errorf("journal terminal status = %q, want %q", terminalPhase, contract.wantPhase)
	}
	if contract.wantGateFailure && !gateFailed {
		t.Error("journal has no non-pass gate verdict")
	}
	if contract.wantGateEscalation && !gateEscalated {
		t.Error("journal has no escalated gate verdict")
	}
}

func assertNumericInputThreading(t *testing.T, events []journal.Event) {
	t.Helper()
	var source, destination any
	for _, event := range events {
		if event.Type != journal.EventStageFinished {
			continue
		}
		switch event.Stage {
		case "pr-select":
			source = event.Outputs["number"]
		case "gather-sibling-context":
			destination = event.Outputs["contractThreadedNumber"]
		}
	}
	sourceNumber, sourceOK := source.(float64)
	destinationNumber, destinationOK := destination.(float64)
	if !sourceOK || !destinationOK || sourceNumber != contractNumber || destinationNumber != contractNumber {
		t.Fatalf("journal numeric threading pr-select -> gather-sibling-context = source(%T %v), destination(%T %v), want float64(%v) at both ends",
			source, source, destination, destination, contractNumber)
	}
}

type scenarioScript struct {
	workflow string
	scenario scenarioKind
	mu       sync.Mutex
	calls    map[string]int
}

func newScenarioScript(workflowName string, scenario scenarioKind) *scenarioScript {
	return &scenarioScript{workflow: workflowName, scenario: scenario, calls: map[string]int{}}
}

func (s *scenarioScript) nextCall(key string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls[key]++
	return s.calls[key]
}

type stageScript struct {
	outputs  map[string]any
	exitCode int
}

func (s *scenarioScript) deterministic(stage string, inputs map[string]any) stageScript {
	if failure, ok := s.scriptedFailure(stage); ok {
		return failure
	}

	outputs := map[string]any{}
	switch s.workflow {
	case "backlog-curation":
		if stage == "query-backlog" {
			outputs["claimed-items"] = "fixture"
		}
	case "implementation":
		switch stage {
		case "query-backlog":
			outputs["claimed-item"] = "fixture"
		case "open-pr":
			outputs["pull-request-url"] = "https://example.test/pull/73"
			outputs["prNumber"] = "73"
			outputs["opened"] = true
		}
	case "merge-review":
		switch stage {
		case "pr-select":
			outputs["number"] = contractNumber
			outputs["head"] = "goobers/implementation/fixture"
			outputs["base"] = "main"
		case "gather-sibling-context":
			outputs["selectedNumber"] = inputs["selectedNumber"]
			outputs["selectedHeadSha"] = "head-sha"
			outputs["selectedBaseSha"] = "base-sha"
			outputs["reviewDigest"] = "sha256:review"
			outputs["overlappingSiblingsCsv"] = ""
			outputs["contractThreadedNumber"] = inputs["selectedNumber"]
		case "elect-lander":
			outputs["elected"] = true
			outputs["selectedNumber"] = inputs["selectedNumber"]
			outputs["selectedHeadSha"] = inputs["selectedHeadSha"]
			outputs["selectedBaseSha"] = inputs["selectedBaseSha"]
			outputs["reviewDigest"] = inputs["reviewDigest"]
			outputs["overlappingSiblingsCsv"] = inputs["overlappingSiblings"]
		case "apply-verdict":
			outputs["selectedNumber"] = inputs["selectedNumber"]
			outputs["selectedHeadSha"] = inputs["selectedHeadSha"]
			outputs["selectedBaseSha"] = inputs["selectedBaseSha"]
			outputs["decision"] = "pass"
			if s.scenario == scenarioFailure {
				outputs["decision"] = "fail"
			}
			outputs["verdictAuthor"] = "fixture-reviewer"
		case "merge-pr":
			outputs["selectedNumber"] = inputs["pullNumber"]
			outputs["selectedHeadSha"] = inputs["headSha"]
			outputs["merged"] = true
			outputs["reason"] = ""
			outputs["landOutcome"] = "merged"
		case "queue-watch":
			outputs["selectedNumber"] = inputs["pullNumber"]
			outputs["queueOutcome"] = "merged"
		}
	case "pr-remediation":
		switch stage {
		case "update-behind-pr":
			outputs["selectedNumber"] = contractNumber
			outputs["needsFullRemediation"] = s.scenario != scenarioHappy
		case "gather-pr-context":
			outputs["selectedNumber"] = contractNumber
			outputs["head"] = "goobers/implementation/fixture"
			outputs["base"] = "main"
			outputs["isBehindBase"] = true
			outputs["hasSubstantiveFindings"] = true
			outputs["hasFailingCI"] = false
		case "rebase-pr":
			outputs["selectedNumber"] = inputs["selectedNumber"]
			outputs["head"] = inputs["head"]
			outputs["needsAgent"] = s.scenario == scenarioEscalation
		case "remediation-checkpoint":
			outputs["continueRemediation"] = true
			outputs["selectedNumber"] = inputs["selectedNumber"]
			outputs["head"] = "goobers/implementation/fixture"
			outputs["headSha"] = "head-sha"
		}
	case "work-nomination":
		if stage == "gather-signals" {
			outputs["candidate-findings"] = "fixture"
		}
	}
	return stageScript{outputs: outputs}
}

func (s *scenarioScript) scriptedFailure(stage string) (stageScript, bool) {
	var (
		failingStage string
		code         = "CONTRACT_FAILURE"
	)
	switch s.workflow {
	case "backlog-curation":
		failingStage = "query-backlog"
	case "work-nomination", "tutor":
		failingStage = "gather-signals"
		if s.workflow == "tutor" && s.scenario == scenarioFailure {
			failingStage = "validate-config"
		}
	case "merge-review":
		if s.scenario == scenarioEscalation {
			failingStage = "pr-select"
		}
	}
	if s.scenario == scenarioEscalation && failingStage != "" {
		code = "ISSUE_OVER_SCOPE"
	}
	if stage != failingStage || s.scenario == scenarioHappy {
		return stageScript{}, false
	}
	return stageScript{
		outputs: map[string]any{
			executor.OutputErrorCode:      code,
			executor.OutputErrorMessage:   "scripted contract outcome",
			executor.OutputErrorRetryable: false,
		},
		exitCode: 1,
	}, true
}

func (s *scenarioScript) harnessAct(_ context.Context, request harness.RunRequest) error {
	stage := stageName(request.Envelope.TaskID)
	call := s.nextCall(string(request.Mode) + ":" + stage)
	switch request.Mode {
	case harness.ModeInvoke:
		result := apiv1.ResultEnvelope{
			Status:  apiv1.ResultSuccess,
			Summary: "scripted fake-harness completion",
		}
		if err := commitAgentChange(request.Workspace, stage, call); err != nil {
			return err
		}
		return harness.WriteCompletion(request.Workspace, request.CompletionPath, result)
	case harness.ModeReview:
		decision := apiv1.VerdictPass
		switch {
		case s.scenario == scenarioFailure && (s.workflow == "implementation" || s.workflow == "merge-review"):
			decision = apiv1.VerdictFail
		case s.scenario == scenarioEscalation && (s.workflow == "implementation" || s.workflow == "pr-remediation"):
			decision = apiv1.VerdictNeedsChanges
		}
		return harness.WriteCompletion(request.Workspace, request.CompletionPath, apiv1.Verdict{
			Decision:  decision,
			Rationale: "scripted fake-harness verdict",
		})
	default:
		return fmt.Errorf("unsupported harness mode %q", request.Mode)
	}
}

type scriptedDeterministic struct {
	shell      *executor.ShellExecutor
	builtins   *executor.TaskExecutor
	executable string
	script     *scenarioScript
}

var _ invoke.Deterministic = (*scriptedDeterministic)(nil)

type contractPRPoller struct{}

func (contractPRPoller) PollPullRequest(context.Context, providers.PullRequestPollRequest) (providers.PullRequestPollResult, error) {
	return providers.PullRequestPollResult{CheckState: providers.CheckStatePassing}, nil
}

// Run preserves built-in dispatch and substitutes only external shell command
// effects. ShellExecutor still harvests each workflow's declared result file.
func (d *scriptedDeterministic) Run(ctx context.Context, env apiv1.InvocationEnvelope, run apiv1.DeterministicRun) (apiv1.ResultEnvelope, error) {
	if kind, _ := env.Inputs[executor.InputKind].(string); kind == executor.KindCIPoll {
		return d.builtins.Run(ctx, env, run)
	}

	stage := stageName(env.TaskID)
	script := d.script.deterministic(stage, env.Inputs)
	payload, err := json.Marshal(script.outputs)
	if err != nil {
		return apiv1.ResultEnvelope{}, fmt.Errorf("marshal scripted result for %s: %w", stage, err)
	}

	resultFile := ""
	if value, ok := env.Inputs[executor.InputResultFile]; ok {
		var valid bool
		resultFile, valid = value.(string)
		if !valid {
			return apiv1.ResultEnvelope{}, fmt.Errorf("%s input for %s is %T, want string", executor.InputResultFile, stage, value)
		}
	}
	run.Command = []string{d.executable, "-test.run=^TestShippedWorkflowCommandHelper$"}
	run.Env = map[string]string{
		contractHelperMode:       "1",
		contractHelperPayload:    string(payload),
		contractHelperResultFile: resultFile,
		contractHelperExitCode:   strconv.Itoa(script.exitCode),
	}
	run.Network = ""
	return d.shell.Run(ctx, env, run)
}

func stageName(taskID string) string {
	if index := strings.LastIndex(taskID, ":"); index >= 0 {
		return taskID[index+1:]
	}
	return taskID
}

func newContractRunner(t *testing.T, script *scenarioScript, gateCapabilities map[string][]string) (*runner.Runner, string) {
	t.Helper()
	instanceRoot := t.TempDir()
	manager, err := worktree.NewManager(filepath.Join(instanceRoot, "workcopies"))
	if err != nil {
		t.Fatalf("create worktree manager: %v", err)
	}
	runsDir := filepath.Join(instanceRoot, "runs")
	repository := newFixtureRepository(t)
	resolver, err := credentials.NewResolver(nil)
	if err != nil {
		t.Fatalf("create credential resolver: %v", err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatalf("resolve test executable: %v", err)
	}

	localRunner, err := runner.New(runner.Config{
		NewDeterministic: func(rec runner.ArtifactRecorder, registrar runner.SecretRegistrar) (invoke.Deterministic, error) {
			injector, err := credentials.NewInjector(resolver, nil, registrar)
			if err != nil {
				return nil, err
			}
			shell, err := executor.NewShellExecutor(injector, rec)
			if err != nil {
				return nil, err
			}
			ciPoll, err := executor.NewCIPollExecutor(contractPRPoller{}, rec)
			if err != nil {
				return nil, err
			}
			builtins, err := executor.NewTaskExecutor(shell, ciPoll)
			if err != nil {
				return nil, err
			}
			return &scriptedDeterministic{
				shell: shell, builtins: builtins, executable: executable, script: script,
			}, nil
		},
		NewAgentic: func(_ string, rec runner.ArtifactRecorder, registrar runner.SecretRegistrar) (invoke.Goober, error) {
			injector, err := credentials.NewInjector(resolver, nil, registrar)
			if err != nil {
				return nil, err
			}
			spanRecorder, ok := rec.(harness.SpanRecorder)
			if !ok {
				return nil, fmt.Errorf("journal recorder %T does not record spans", rec)
			}
			runDir, ok := rec.(interface{ Dir() string })
			if !ok {
				return nil, fmt.Errorf("journal recorder %T does not expose its directory", rec)
			}
			registryScrubber, ok := registrar.(journal.Scrubber)
			if !ok {
				return nil, fmt.Errorf("secret registrar %T is not a journal scrubber", registrar)
			}
			adapter := &harness.FakeAdapter{
				Act:        script.harnessAct,
				Transcript: []byte("scripted shipped-workflow contract harness\n"),
			}
			return harness.NewExecutor(
				adapter,
				injector,
				spanRecorder,
				rec,
				harness.NewContextResolver(runDir, runsDir),
				journal.Chain(registryScrubber, journal.NewPatternScrubber()),
				"shipped-workflow contract fixture",
			)
		},
		Automated:              gate.NewAutomatedEvaluator(),
		MaxRepasses:            1,
		GateGooberCapabilities: gateCapabilities,
		Worktrees:              manager,
		ScratchDir:             filepath.Join(instanceRoot, "scratch"),
		RunsDir:                runsDir,
		RepoCloneURL: func(apiv1.RepoRef) (string, error) {
			return repository, nil
		},
	})
	if err != nil {
		t.Fatalf("create local runner: %v", err)
	}
	return localRunner, runsDir
}

func newFixtureRepository(t *testing.T) string {
	t.Helper()
	work := t.TempDir()
	bare := filepath.Join(t.TempDir(), "fixture.git")
	runGit(t, work, "init", "--initial-branch=main")
	runGit(t, work, "config", "user.email", "contract@example.test")
	runGit(t, work, "config", "user.name", "contract")
	if err := os.WriteFile(filepath.Join(work, "README.md"), []byte("fixture\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, work, "add", "README.md")
	runGit(t, work, "commit", "-m", "fixture")
	runGit(t, "", "clone", "--bare", work, bare)
	return bare
}

func commitAgentChange(workspace, stage string, call int) error {
	name := strings.NewReplacer("/", "-", ":", "-").Replace(stage)
	path := filepath.Join(workspace, fmt.Sprintf("contract-%s-%d.txt", name, call))
	if err := os.WriteFile(path, []byte(fmt.Sprintf("%s %d\n", stage, call)), 0o644); err != nil {
		return err
	}
	if err := runGitCommand(workspace, "add", "-A"); err != nil {
		return err
	}
	return runGitCommand(
		workspace,
		"-c", "user.email=contract@example.test",
		"-c", "user.name=contract",
		"commit", "-m", fmt.Sprintf("contract %s %d", stage, call),
	)
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	if err := runGitCommand(dir, args...); err != nil {
		t.Fatal(err)
	}
}

func runGitCommand(dir string, args ...string) error {
	command := exec.Command("git", args...)
	if dir != "" {
		command.Dir = dir
	}
	command.Env = append(os.Environ(),
		"GIT_CONFIG_COUNT=2",
		"GIT_CONFIG_KEY_0=core.autocrlf",
		"GIT_CONFIG_VALUE_0=false",
		"GIT_CONFIG_KEY_1=core.safecrlf",
		"GIT_CONFIG_VALUE_1=false",
	)
	var output bytes.Buffer
	command.Stdout = &output
	command.Stderr = &output
	if err := command.Run(); err != nil {
		return fmt.Errorf("git %v: %w\n%s", args, err, output.String())
	}
	return nil
}
