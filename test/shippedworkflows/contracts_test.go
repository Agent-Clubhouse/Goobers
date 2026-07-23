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
	skipShippedContracts     = "GOOBERS_SKIP_SHIPPED_WORKFLOW_CONTRACTS"
	contractNumber           = float64(73)
)

type terminalScenario struct {
	name           string
	steps          []workflow.GraphEdge
	gateOutcomes   map[string][]string
	escalationTask string
	escalationGate string
	wantPhase      journal.RunPhase
}

type valueHandoffContract struct {
	workflow       string
	producer       string
	producerOutput string
	consumer       string
	consumerInput  string
	consumerOutput string
	expectedValue  any
}

var requiredValueHandoffs = []valueHandoffContract{{
	workflow:       "merge-review",
	producer:       "pr-select",
	producerOutput: "number",
	consumer:       "gather-sibling-context",
	consumerInput:  "selectedNumber",
	consumerOutput: "selectedNumber",
	expectedValue:  contractNumber,
}}

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
	if os.Getenv(skipShippedContracts) != "" {
		t.Skip("covered by the dedicated shipped-workflows CI tier")
	}
	root := repositoryRoot(t)
	configs := []struct {
		name string
		path string
	}{
		{name: "selfhost", path: filepath.Join(root, "selfhost")},
		{name: "config-examples", path: filepath.Join(root, "config-examples")},
	}
	for _, config := range configs {
		config := config
		t.Run(config.name, func(t *testing.T) {
			set, report, err := instance.LoadConfigDir(config.path)
			if err != nil {
				t.Fatalf("load shipped config: %v\n%v", err, report)
			}
			discovered := discoverWorkflowDefinitions(t, config.path)
			loaded := make(map[string]apiv1.Workflow, len(set.Workflows))
			for _, definition := range set.Workflows {
				key := shippedWorkflowKey(definition.Spec.Gaggle, definition.Name)
				loaded[key] = definition
				source, ok := set.WorkflowSource(definition.Spec.Gaggle, definition.Name)
				if !ok {
					t.Fatalf("workflow %q has no source path", key)
				}
				if got := filepath.ToSlash(source); got != discovered[key] {
					t.Fatalf("workflow %q source = %q, discovered %q", key, got, discovered[key])
				}
			}
			assertDiscoveryCoverage(t, discovered, loaded)

			keys := make([]string, 0, len(loaded))
			for key := range loaded {
				keys = append(keys, key)
			}
			sort.Strings(keys)
			allowPreview := set.Manifest != nil && workflow.PreviewFeaturesEnabled(set.Manifest.Annotations)
			gateCapabilities := gooberCapabilities(set.Goobers)
			for _, key := range keys {
				definition := loaded[key]
				source := discovered[key]
				t.Run(strings.ReplaceAll(key, "/", "_"), func(t *testing.T) {
					def := workflow.Definition{
						Name: definition.Name, Version: 1, DSLVersion: definition.DSLVersion, Spec: definition.Spec,
					}
					assertStaticStageContracts(t, source, def)
					machine, err := workflow.Compile(def, workflow.WithPreviewFeatures(allowPreview))
					if err != nil {
						t.Fatalf("%s: workflow %q compile contract: %v", source, key, err)
					}
					scenarios := terminalScenarios(t, machine)
					for _, scenario := range scenarios {
						scenario := scenario
						t.Run(scenario.name, func(t *testing.T) {
							t.Parallel()
							script := newScenarioScript(definition, scenario)
							localRunner, runsDir := newContractRunner(t, script, gateCapabilities)
							runID := contractRunID(config.name, key, scenario.name)
							_, runErr := localRunner.Start(context.Background(), runner.StartInput{
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
							if runErr != nil {
								t.Fatalf("%s: workflow %q terminal path %q: %v", source, key, scenario.name, runErr)
							}

							runDir := filepath.Join(runsDir, runID)
							reader, err := journal.OpenRead(runDir)
							if err != nil {
								t.Fatalf("%s: workflow %q open journal: %v", source, key, err)
							}
							events, err := reader.Events()
							if err != nil {
								t.Fatalf("%s: workflow %q read journal events: %v", source, key, err)
							}
							assertJournalScenario(t, definition, events, scenario)
							assertRequiredValueHandoffs(t, definition.Name, events)
							state, err := reader.State()
							if err != nil {
								t.Fatalf("%s: workflow %q read journal state: %v", source, key, err)
							}
							if state.Phase != scenario.wantPhase {
								t.Fatalf("%s: workflow %q terminal path %q phase = %q, want %q",
									source, key, scenario.name, state.Phase, scenario.wantPhase)
							}
						})
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
				Spec struct {
					Gaggle string `json:"gaggle"`
				} `json:"spec"`
			}
			if err := yaml.Unmarshal([]byte(document), &metadata); err != nil || metadata.Kind != "Workflow" {
				continue
			}
			rel, err := filepath.Rel(root, path)
			if err != nil {
				return err
			}
			key := shippedWorkflowKey(metadata.Spec.Gaggle, metadata.Metadata.Name)
			if previous, exists := discovered[key]; exists {
				return fmt.Errorf("duplicate shipped workflow %q in %s and %s", key, previous, rel)
			}
			discovered[key] = filepath.ToSlash(rel)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("discover shipped workflows: %v", err)
	}
	return discovered
}

func shippedWorkflowKey(gaggle, name string) string {
	return gaggle + "/" + name
}

func assertDiscoveryCoverage(t *testing.T, discovered map[string]string, loaded map[string]apiv1.Workflow) {
	t.Helper()
	for key, path := range discovered {
		if _, ok := loaded[key]; !ok {
			t.Errorf("discovered workflow %q (%s) is not loaded by the production config loader", key, path)
		}
	}
	for key := range loaded {
		if _, ok := discovered[key]; !ok {
			t.Errorf("loaded workflow %q is not discovered from shipped YAML", key)
		}
	}
	if len(discovered) != len(loaded) {
		t.Fatalf("workflow discovery coverage: discovered=%d loaded=%d", len(discovered), len(loaded))
	}
}

func assertStaticStageContracts(t *testing.T, source string, def workflow.Definition) {
	t.Helper()
	checks := []struct {
		name     string
		problems []string
	}{
		{name: "handoff", problems: workflow.CheckStageContracts(def)},
		{name: "output", problems: workflow.CheckStageContractWarnings(def)},
		{name: "required-input", problems: workflow.CheckStageRequiredInputs(def)},
		{name: "gate-output", problems: gateOutputContractProblems(def)},
		{name: "required-value-handoff", problems: requiredValueHandoffProblems(def)},
	}
	for _, check := range checks {
		if len(check.problems) > 0 {
			t.Fatalf("%s: workflow %q %s contract: %s",
				source, def.Name, check.name, strings.Join(check.problems, "; "))
		}
	}
}

func gateOutputContractProblems(def workflow.Definition) []string {
	gates := make(map[string]apiv1.Gate, len(def.Spec.Gates))
	for _, gateDefinition := range def.Spec.Gates {
		gates[gateDefinition.Name] = gateDefinition
	}
	var problems []string
	for _, task := range def.Spec.Tasks {
		gateDefinition, ok := gates[task.Next]
		if !ok || gateDefinition.Automated == nil {
			continue
		}
		key := automatedGateOutputKey(*gateDefinition.Automated)
		if key == "" || contains(task.ExpectedOutputs, key) {
			continue
		}
		problems = append(problems, fmt.Sprintf(
			"task %q feeds gate %q check %q from output %q, but expectedOutputs does not declare it; the wired fake emits only declared outputs, so this path cannot produce the requested gate outcome",
			task.Name, gateDefinition.Name, gateDefinition.Automated.Check, key,
		))
	}
	return problems
}

func requiredValueHandoffProblems(def workflow.Definition) []string {
	tasks := make(map[string]apiv1.Task, len(def.Spec.Tasks))
	for _, task := range def.Spec.Tasks {
		tasks[task.Name] = task
	}
	var problems []string
	for _, contract := range requiredValueHandoffs {
		if contract.workflow != def.Name {
			continue
		}
		consumer, ok := tasks[contract.consumer]
		if !ok {
			problems = append(problems, fmt.Sprintf(
				"required value handoff %s.%s -> %s.%s has no consumer task %q",
				contract.producer, contract.producerOutput, contract.consumer, contract.consumerInput, contract.consumer,
			))
			continue
		}
		output, ok := consumer.InputsFrom[contract.consumerInput]
		if !ok {
			problems = append(problems, fmt.Sprintf(
				"required value handoff %s.%s -> %s.%s is missing inputsFrom mapping %q: %q",
				contract.producer, contract.producerOutput, contract.consumer, contract.consumerInput,
				contract.consumerInput, contract.producerOutput,
			))
		} else if output != contract.producerOutput {
			problems = append(problems, fmt.Sprintf(
				"required value handoff %s.%s -> %s.%s maps output %q, want %q",
				contract.producer, contract.producerOutput, contract.consumer, contract.consumerInput,
				output, contract.producerOutput,
			))
		}
		if !contains(consumer.ExpectedOutputs, contract.consumerOutput) {
			problems = append(problems, fmt.Sprintf(
				"required value handoff %s.%s -> %s.%s cannot be observed: task %q does not declare expected output %q",
				contract.producer, contract.producerOutput, contract.consumer, contract.consumerInput,
				contract.consumer, contract.consumerOutput,
			))
		}
	}
	return problems
}

func automatedGateOutputKey(automated apiv1.AutomatedGate) string {
	switch automated.Check {
	case "output-equals", "output-not-equals", "output-numeric-gte", "output-numeric-lte", "output-numeric-lt", "output-matches":
		return automated.Params["key"]
	case "ci-status":
		return executor.OutputCIStatus
	case "queue-outcome":
		return "queueOutcome"
	case "land-outcome":
		// landOutcome is conditional and intentionally omitted from merge-pr's
		// exhaustive postconditions; the runner check handles its vocabulary.
		return ""
	default:
		return ""
	}
}

func contains(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

func TestGateOutputContractNamesMissingExpectedOutput(t *testing.T) {
	t.Parallel()
	def := workflow.Definition{Name: "l1-regression", Spec: apiv1.WorkflowSpec{
		Start: "apply-verdict",
		Tasks: []apiv1.Task{{
			Name: "apply-verdict", ExpectedOutputs: []string{"selectedNumber"}, Next: "published-verdict",
		}},
		Gates: []apiv1.Gate{{
			Name: "published-verdict",
			Automated: &apiv1.AutomatedGate{
				Check:  "output-equals",
				Params: map[string]string{"key": "decision", "equals": "pass"},
			},
		}},
	}}
	problems := gateOutputContractProblems(def)
	if len(problems) != 1 ||
		!strings.Contains(problems[0], `task "apply-verdict"`) ||
		!strings.Contains(problems[0], `gate "published-verdict"`) ||
		!strings.Contains(problems[0], `output "decision"`) {
		t.Fatalf("gate output problems = %v, want producer, consumer, and missing output", problems)
	}
}

func TestRequiredValueHandoffNamesMissingOrMisthreadedMapping(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		inputsFrom map[string]string
		want       string
	}{
		{name: "missing", want: `is missing inputsFrom mapping "selectedNumber": "number"`},
		{
			name:       "misthreaded",
			inputsFrom: map[string]string{"selectedNumber": "head"},
			want:       `maps output "head", want "number"`,
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			def := workflow.Definition{Name: "merge-review", Spec: apiv1.WorkflowSpec{
				Tasks: []apiv1.Task{{
					Name:            "gather-sibling-context",
					InputsFrom:      test.inputsFrom,
					ExpectedOutputs: []string{"selectedNumber"},
				}},
			}}
			problems := requiredValueHandoffProblems(def)
			if len(problems) != 1 || !strings.Contains(problems[0], test.want) {
				t.Fatalf("required value handoff problems = %v, want one containing %q", problems, test.want)
			}
		})
	}
}

func terminalScenarios(t *testing.T, machine *workflow.Machine) []terminalScenario {
	t.Helper()
	graph := machine.Graph()
	outgoing := make(map[string][]workflow.GraphEdge, len(graph.Nodes))
	for _, edge := range graph.Edges {
		outgoing[edge.Source] = append(outgoing[edge.Source], edge)
	}

	var scenarios []terminalScenario
	seenPaths := map[string]bool{}
	// Build a start-to-terminal run around every edge, then deduplicate identical
	// runs. This covers new branches automatically without a workflow allowlist.
	for _, selected := range graph.Edges {
		path, ok := pathToState(graph.Start, selected.Source, outgoing)
		if !ok {
			t.Fatalf("workflow %q edge %s -> %s is unreachable",
				graph.Name, selected.Source, selected.Target)
		}
		path = append(path, selected)
		if selected.Terminal == "" {
			suffix, ok := pathToTerminal(selected.Target, outgoing)
			if !ok {
				t.Fatalf("workflow %q edge %s -> %s cannot reach a terminal",
					graph.Name, selected.Source, selected.Target)
			}
			path = append(path, suffix...)
		}
		signature := pathSignature(path)
		if seenPaths[signature] {
			continue
		}
		seenPaths[signature] = true
		terminal := path[len(path)-1]
		scenario := terminalScenario{
			name: fmt.Sprintf("%02d_%s_%s", len(scenarios)+1,
				contractToken(selected.Source), contractToken(selected.Outcome)),
			steps:        path,
			gateOutcomes: map[string][]string{},
			wantPhase:    phaseForTerminal(terminal.Terminal),
		}
		for stepIndex, edge := range path {
			if edge.Outcome == "" {
				continue
			}
			scenario.gateOutcomes[edge.Source] = append(scenario.gateOutcomes[edge.Source], edge.Outcome)
			if edge.Outcome != workflow.BranchEscalate {
				continue
			}
			if scenario.escalationGate != "" {
				t.Fatalf("workflow %q terminal path %q crosses multiple escalation control branches", graph.Name, scenario.name)
			}
			if stepIndex == 0 {
				t.Fatalf("workflow %q escalation gate %q has no preceding task", graph.Name, edge.Source)
			}
			preceding := path[stepIndex-1].Source
			if _, ok := machine.Task(preceding); !ok {
				t.Fatalf("workflow %q escalation gate %q is preceded by %q, not a task", graph.Name, edge.Source, preceding)
			}
			scenario.escalationTask = preceding
			scenario.escalationGate = edge.Source
		}
		scenarios = append(scenarios, scenario)
	}
	for _, edge := range graph.Edges {
		covered := false
		for _, scenario := range scenarios {
			if containsEdge(scenario.steps, edge) {
				covered = true
				break
			}
		}
		if !covered {
			t.Fatalf("workflow %q edge %s --%s--> %s has no executable terminal scenario",
				graph.Name, edge.Source, edge.Outcome, edge.Target)
		}
	}
	return scenarios
}

func pathToTerminal(start string, outgoing map[string][]workflow.GraphEdge) ([]workflow.GraphEdge, bool) {
	var walk func(string, map[string]bool) ([]workflow.GraphEdge, bool)
	walk = func(state string, seen map[string]bool) ([]workflow.GraphEdge, bool) {
		// Graph edges put pass first; depth-first selection exits repass loops
		// through a successful suffix before trying another non-pass verdict.
		for _, edge := range outgoing[state] {
			if edge.Terminal != "" {
				return []workflow.GraphEdge{edge}, true
			}
			if seen[edge.Target] {
				continue
			}
			nextSeen := make(map[string]bool, len(seen)+1)
			for visited := range seen {
				nextSeen[visited] = true
			}
			nextSeen[edge.Target] = true
			suffix, ok := walk(edge.Target, nextSeen)
			if ok {
				return append([]workflow.GraphEdge{edge}, suffix...), true
			}
		}
		return nil, false
	}
	return walk(start, map[string]bool{start: true})
}

func pathSignature(path []workflow.GraphEdge) string {
	var parts []string
	for _, edge := range path {
		parts = append(parts, edge.Source+"|"+edge.Outcome+"|"+edge.Target)
	}
	return strings.Join(parts, "->")
}

func containsEdge(edges []workflow.GraphEdge, wanted workflow.GraphEdge) bool {
	for _, edge := range edges {
		if edge == wanted {
			return true
		}
	}
	return false
}

func pathToState(start, target string, outgoing map[string][]workflow.GraphEdge) ([]workflow.GraphEdge, bool) {
	type candidate struct {
		state string
		path  []workflow.GraphEdge
		seen  map[string]bool
	}
	queue := []candidate{{state: start, seen: map[string]bool{start: true}}}
	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]
		if current.state == target {
			return current.path, true
		}
		for _, edge := range outgoing[current.state] {
			if edge.Terminal != "" || current.seen[edge.Target] {
				continue
			}
			seen := make(map[string]bool, len(current.seen)+1)
			for state := range current.seen {
				seen[state] = true
			}
			seen[edge.Target] = true
			path := append(append([]workflow.GraphEdge(nil), current.path...), edge)
			queue = append(queue, candidate{state: edge.Target, path: path, seen: seen})
		}
	}
	return nil, false
}

func phaseForTerminal(terminal workflow.GraphTerminal) journal.RunPhase {
	switch terminal {
	case workflow.GraphTerminalComplete:
		return journal.PhaseCompleted
	case workflow.GraphTerminalAbort:
		return journal.PhaseAborted
	case workflow.GraphTerminalEscalate:
		return journal.PhaseEscalated
	default:
		panic(fmt.Sprintf("unknown graph terminal %q", terminal))
	}
}

func contractToken(value string) string {
	if value == "" {
		return "next"
	}
	return strings.NewReplacer("@", "", "/", "-", ":", "-", " ", "-").Replace(value)
}

func contractRunID(_, _, _ string) string {
	return "wired-contract"
}

func gooberCapabilities(goobers []apiv1.Goober) map[string][]string {
	out := make(map[string][]string, len(goobers))
	for _, goober := range goobers {
		out[goober.Name] = append([]string(nil), goober.Spec.Capabilities...)
	}
	return out
}

func assertJournalScenario(t *testing.T, definition apiv1.Workflow, events []journal.Event, scenario terminalScenario) {
	t.Helper()
	tasks := make(map[string]apiv1.Task, len(definition.Spec.Tasks))
	for _, task := range definition.Spec.Tasks {
		tasks[task.Name] = task
	}
	var sequence []string
	var terminalEvents int
	for index, event := range events {
		if event.Schema != journal.EventSchema {
			t.Errorf("journal event %d schema = %q, want %q", event.Seq, event.Schema, journal.EventSchema)
		}
		if event.Seq != uint64(index+1) {
			t.Errorf("journal event index %d seq = %d, want %d", index, event.Seq, index+1)
		}
		if event.Type == "" {
			t.Errorf("journal event %d has no type", event.Seq)
		}
		switch event.Type {
		case journal.EventStageStarted:
			sequence = append(sequence, "task:"+event.Stage)
		case journal.EventGateEvaluated:
			sequence = append(sequence, "gate:"+event.Gate+"="+event.Verdict)
		case journal.EventStageFinished:
			if event.Status != string(apiv1.ResultSuccess) {
				continue
			}
			task := tasks[event.Stage]
			for _, expected := range task.ExpectedOutputs {
				if _, ok := event.Outputs[expected]; !ok {
					t.Errorf("task %q succeeded without expected output %q", event.Stage, expected)
				}
			}
		case journal.EventRunFinished:
			terminalEvents++
			if event.Status != string(scenario.wantPhase) {
				t.Errorf("journal terminal status = %q, want %q", event.Status, scenario.wantPhase)
			}
		}
	}
	if terminalEvents != 1 {
		t.Fatalf("journal has %d terminal events, want 1", terminalEvents)
	}
	var expected []string
	for _, edge := range scenario.steps {
		if edge.Source == scenario.escalationGate {
			continue
		}
		if _, ok := tasks[edge.Source]; ok {
			expected = append(expected, "task:"+edge.Source)
			continue
		}
		expected = append(expected, "gate:"+edge.Source+"="+edge.Outcome)
	}
	if !reflect.DeepEqual(sequence, expected) {
		t.Fatalf("journal execution sequence:\n got: %v\nwant: %v", sequence, expected)
	}
}

func assertRequiredValueHandoffs(t *testing.T, workflowName string, events []journal.Event) {
	t.Helper()
	for _, contract := range requiredValueHandoffs {
		if contract.workflow != workflowName {
			continue
		}
		var source, destination any
		var sourceFound, destinationFound bool
		for _, event := range events {
			if event.Type != journal.EventStageFinished {
				continue
			}
			switch event.Stage {
			case contract.producer:
				source, sourceFound = event.Outputs[contract.producerOutput]
			case contract.consumer:
				destination, destinationFound = event.Outputs[contract.consumerOutput]
			}
		}
		if !sourceFound || !destinationFound ||
			!reflect.DeepEqual(source, contract.expectedValue) ||
			!reflect.DeepEqual(destination, source) {
			t.Fatalf(
				"required value handoff %s.%s -> %s.%s = source(%T %v), destination(%T %v), want %T(%v) at both ends",
				contract.producer, contract.producerOutput, contract.consumer, contract.consumerInput,
				source, source, destination, destination, contract.expectedValue, contract.expectedValue,
			)
		}
	}
}

type scenarioScript struct {
	definition apiv1.Workflow
	scenario   terminalScenario
	tasks      map[string]apiv1.Task
	gates      map[string]apiv1.Gate
	mu         sync.Mutex
	calls      map[string]int
	gateCalls  map[string]int
	ciOutcome  string
}

func newScenarioScript(definition apiv1.Workflow, scenario terminalScenario) *scenarioScript {
	script := &scenarioScript{
		definition: definition,
		scenario:   scenario,
		tasks:      make(map[string]apiv1.Task, len(definition.Spec.Tasks)),
		gates:      make(map[string]apiv1.Gate, len(definition.Spec.Gates)),
		calls:      map[string]int{},
		gateCalls:  map[string]int{},
	}
	for _, task := range definition.Spec.Tasks {
		script.tasks[task.Name] = task
	}
	for _, gateDefinition := range definition.Spec.Gates {
		script.gates[gateDefinition.Name] = gateDefinition
	}
	return script
}

func (s *scenarioScript) nextCall(key string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls[key]++
	return s.calls[key]
}

func (s *scenarioScript) nextGateOutcome(gateName string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	outcomes, ok := s.scenario.gateOutcomes[gateName]
	if !ok {
		return "", false
	}
	index := s.gateCalls[gateName]
	if index >= len(outcomes) {
		return "", false
	}
	s.gateCalls[gateName]++
	return outcomes[index], true
}

func (s *scenarioScript) setCIOutcome(outcome string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ciOutcome = outcome
}

func (s *scenarioScript) currentCIOutcome() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ciOutcome
}

type stageScript struct {
	outputs  map[string]any
	exitCode int
}

func (s *scenarioScript) deterministic(stage string, inputs map[string]any) (stageScript, error) {
	task, ok := s.tasks[stage]
	if !ok {
		return stageScript{}, fmt.Errorf("workflow %q invoked unknown task %q", s.definition.Name, stage)
	}
	if err := validateThreadedInputs(task, inputs); err != nil {
		return stageScript{}, err
	}
	outputs := expectedOutputs(task)
	if err := s.threadRequiredValueHandoffs(task, inputs, outputs); err != nil {
		return stageScript{}, err
	}
	if stage == s.scenario.escalationTask {
		return failedStage(outputs, "ISSUE_OVER_SCOPE"), nil
	}
	if gateDefinition, desired, ok := s.desiredGateAfter(task); ok {
		if gateDefinition.Automated == nil {
			return stageScript{}, fmt.Errorf("task %q routes to malformed automated gate %q", task.Name, gateDefinition.Name)
		}
		if gateDefinition.Automated.Check == "status-equals" && desired == gate.OutcomeFail {
			return failedStage(outputs, "CONTRACT_FAILURE"), nil
		}
		if err := shapeGateOutputs(outputs, *gateDefinition.Automated, desired); err != nil {
			return stageScript{}, fmt.Errorf("task %q -> gate %q: %w", task.Name, gateDefinition.Name, err)
		}
	}
	return stageScript{outputs: outputs}, nil
}

func validateThreadedInputs(task apiv1.Task, inputs map[string]any) error {
	for inputKey, outputKey := range task.InputsFrom {
		if _, ok := inputs[inputKey]; !ok {
			return fmt.Errorf("task %q inputsFrom %q did not resolve upstream output %q", task.Name, inputKey, outputKey)
		}
	}
	return nil
}

func (s *scenarioScript) threadRequiredValueHandoffs(task apiv1.Task, inputs, outputs map[string]any) error {
	for _, contract := range requiredValueHandoffs {
		if contract.workflow != s.definition.Name || contract.consumer != task.Name {
			continue
		}
		value, ok := inputs[contract.consumerInput]
		if !ok {
			return fmt.Errorf(
				"required value handoff %s.%s -> %s.%s did not resolve input %q",
				contract.producer, contract.producerOutput, contract.consumer, contract.consumerInput,
				contract.consumerInput,
			)
		}
		if _, ok := outputs[contract.consumerOutput]; !ok {
			return fmt.Errorf(
				"required value handoff %s.%s -> %s.%s cannot emit undeclared output %q",
				contract.producer, contract.producerOutput, contract.consumer, contract.consumerInput,
				contract.consumerOutput,
			)
		}
		outputs[contract.consumerOutput] = value
	}
	return nil
}

func expectedOutputs(task apiv1.Task) map[string]any {
	outputs := make(map[string]any, len(task.ExpectedOutputs))
	for _, key := range task.ExpectedOutputs {
		outputs[key] = contractOutputValue(key)
	}
	return outputs
}

func contractOutputValue(key string) any {
	switch key {
	case "number", "selectedNumber", "todoCount":
		return contractNumber
	case "opened", "elected", "merged", "needsAgent", "needsFullRemediation", "continueRemediation":
		return true
	case "prNumber":
		return "73"
	case "decision":
		return "pass"
	case "ciStatus":
		return providers.CheckStatePassing
	case "landOutcome", "queueOutcome":
		return gate.OutcomeMerged
	default:
		return "contract-" + key
	}
}

func failedStage(outputs map[string]any, code string) stageScript {
	outputs[executor.OutputErrorCode] = code
	outputs[executor.OutputErrorMessage] = "scripted wired-workflow contract outcome"
	outputs[executor.OutputErrorRetryable] = false
	return stageScript{
		outputs:  outputs,
		exitCode: 1,
	}
}

func (s *scenarioScript) desiredGateAfter(task apiv1.Task) (apiv1.Gate, string, bool) {
	gateDefinition, ok := s.gates[task.Next]
	if !ok || gateDefinition.Evaluator != apiv1.EvaluatorAutomated {
		return apiv1.Gate{}, "", false
	}
	desired, ok := s.nextGateOutcome(gateDefinition.Name)
	return gateDefinition, desired, ok
}

func shapeGateOutputs(outputs map[string]any, automated apiv1.AutomatedGate, desired string) error {
	setDeclared := func(key string, value any) {
		// Never invent a promised output: omitting expectedOutputs must make the
		// real gate take the wrong path and fail the scenario precisely.
		if _, ok := outputs[key]; ok {
			outputs[key] = value
		}
	}
	pass := desired == gate.OutcomePass
	switch automated.Check {
	case "status-equals", "ci-status":
		return nil
	case "output-equals":
		key := automated.Params["key"]
		value := automated.Params["equals"]
		if pass {
			setDeclared(key, contractScalar(value))
		} else {
			setDeclared(key, contractMismatch(value))
		}
	case "output-not-equals":
		key := automated.Params["key"]
		value := automated.Params["equals"]
		if pass {
			setDeclared(key, contractMismatch(value))
		} else {
			setDeclared(key, contractScalar(value))
		}
	case "output-numeric-gte":
		threshold, err := strconv.ParseFloat(automated.Params["threshold"], 64)
		if err != nil {
			return err
		}
		if pass {
			setDeclared(automated.Params["key"], threshold)
		} else {
			setDeclared(automated.Params["key"], threshold-1)
		}
	case "output-numeric-lte":
		threshold, err := strconv.ParseFloat(automated.Params["threshold"], 64)
		if err != nil {
			return err
		}
		if pass {
			setDeclared(automated.Params["key"], threshold)
		} else {
			setDeclared(automated.Params["key"], threshold+1)
		}
	case "output-numeric-lt":
		threshold, err := strconv.ParseFloat(automated.Params["threshold"], 64)
		if err != nil {
			return err
		}
		if pass {
			setDeclared(automated.Params["key"], threshold-1)
		} else {
			setDeclared(automated.Params["key"], threshold)
		}
	case "output-matches":
		value := strings.Trim(automated.Params["pattern"], "^$")
		value = strings.ReplaceAll(value, `\.`, ".")
		if pass {
			setDeclared(automated.Params["key"], value)
		} else {
			setDeclared(automated.Params["key"], "contract-non-match")
		}
	case "land-outcome":
		// landOutcome is conditional and intentionally non-exhaustive in
		// expectedOutputs, unlike the ordinary declared handoff keys above.
		outputs["landOutcome"] = desired
	case "queue-outcome":
		setDeclared("queueOutcome", desired)
	default:
		return fmt.Errorf("unsupported automated check %q", automated.Check)
	}
	return nil
}

func contractScalar(value string) any {
	switch value {
	case "true":
		return true
	case "false":
		return false
	default:
		return value
	}
}

func contractMismatch(value string) any {
	if value == "true" {
		return false
	}
	if value == "false" {
		return true
	}
	return "contract-not-" + value
}

func (s *scenarioScript) harnessAct(_ context.Context, request harness.RunRequest) error {
	stage := stageName(request.Envelope.TaskID)
	call := s.nextCall(string(request.Mode) + ":" + stage)
	switch request.Mode {
	case harness.ModeInvoke:
		task, ok := s.tasks[stage]
		if !ok {
			return fmt.Errorf("workflow %q invoked unknown agentic task %q", s.definition.Name, stage)
		}
		if err := validateThreadedInputs(task, request.Envelope.Inputs); err != nil {
			return err
		}
		result := apiv1.ResultEnvelope{
			Status:  apiv1.ResultSuccess,
			Summary: "scripted fake-harness completion",
			Outputs: expectedOutputs(task),
		}
		if err := s.threadRequiredValueHandoffs(task, request.Envelope.Inputs, result.Outputs); err != nil {
			return err
		}
		if stage == s.scenario.escalationTask {
			result.Status = apiv1.ResultFailure
			result.Error = &apiv1.ErrorInfo{
				Code: "ISSUE_OVER_SCOPE", Message: "scripted wired-workflow escalation", Retryable: false,
			}
		} else if gateDefinition, desired, ok := s.desiredGateAfter(task); ok {
			if gateDefinition.Automated.Check == "status-equals" && desired == gate.OutcomeFail {
				result.Status = apiv1.ResultFailure
				result.Error = &apiv1.ErrorInfo{
					Code: "CONTRACT_FAILURE", Message: "scripted wired-workflow failure", Retryable: false,
				}
			}
			if err := shapeGateOutputs(result.Outputs, *gateDefinition.Automated, desired); err != nil {
				return fmt.Errorf("task %q -> gate %q: %w", task.Name, gateDefinition.Name, err)
			}
		}
		if err := commitAgentChange(request.Workspace, stage, call); err != nil {
			return err
		}
		return harness.WriteCompletion(request.Workspace, request.CompletionPath, result)
	case harness.ModeReview:
		outcome, ok := s.nextGateOutcome(stage)
		if !ok {
			return fmt.Errorf("workflow %q unexpectedly evaluated unscripted gate %q", s.definition.Name, stage)
		}
		var decision apiv1.VerdictDecision
		switch outcome {
		case string(apiv1.VerdictPass):
			decision = apiv1.VerdictPass
		case string(apiv1.VerdictFail):
			decision = apiv1.VerdictFail
		case string(apiv1.VerdictNeedsChanges):
			decision = apiv1.VerdictNeedsChanges
		default:
			return fmt.Errorf("workflow %q gate %q cannot return scripted outcome %q through the harness", s.definition.Name, stage, outcome)
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

type contractPRPoller struct {
	script *scenarioScript
}

func (p contractPRPoller) PollPullRequest(context.Context, providers.PullRequestPollRequest) (providers.PullRequestPollResult, error) {
	state := providers.CheckStatePassing
	if p.script.currentCIOutcome() == gate.OutcomeFail {
		state = providers.CheckStateFailing
	}
	return providers.PullRequestPollResult{CheckState: state}, nil
}

// Run preserves built-in dispatch and substitutes only external shell command
// effects. ShellExecutor still harvests each workflow's declared result file.
func (d *scriptedDeterministic) Run(ctx context.Context, env apiv1.InvocationEnvelope, run apiv1.DeterministicRun) (apiv1.ResultEnvelope, error) {
	stage := stageName(env.TaskID)
	if stage == d.script.scenario.escalationTask {
		return apiv1.ResultEnvelope{
			Status:  apiv1.ResultFailure,
			Summary: "scripted wired-workflow escalation",
			Error: &apiv1.ErrorInfo{
				Code: "ISSUE_OVER_SCOPE", Message: "scripted wired-workflow escalation", Retryable: false,
			},
		}, nil
	}
	if kind, _ := env.Inputs[executor.InputKind].(string); kind == executor.KindCIPoll {
		task := d.script.tasks[stage]
		gateDefinition, ok := d.script.gates[task.Next]
		if !ok {
			return apiv1.ResultEnvelope{}, fmt.Errorf("CI-poll task %q does not feed a gate", stage)
		}
		outcome, ok := d.script.nextGateOutcome(gateDefinition.Name)
		if !ok {
			return apiv1.ResultEnvelope{}, fmt.Errorf("CI-poll task %q has no scripted outcome for gate %q", stage, gateDefinition.Name)
		}
		if outcome == gate.OutcomeTimeout {
			return apiv1.ResultEnvelope{
				Status:  apiv1.ResultFailure,
				Summary: "scripted CI timeout",
				Outputs: map[string]any{executor.OutputCIStatus: executor.CIStatusTimeout},
				Error: &apiv1.ErrorInfo{
					Code: "poll_timeout", Message: "scripted CI timeout", Retryable: true,
				},
			}, nil
		}
		d.script.setCIOutcome(outcome)
		return d.builtins.Run(ctx, env, run)
	}

	script, err := d.script.deterministic(stage, env.Inputs)
	if err != nil {
		return apiv1.ResultEnvelope{}, err
	}
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
			ciPoll, err := executor.NewCIPollExecutor(contractPRPoller{script: script}, rec)
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
