package workflowupgrade

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/pmezard/go-difflib/difflib"
	"gopkg.in/yaml.v3"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/api/validate"
	"github.com/goobers/goobers/internal/dslmigrate"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/workflow"
)

type fixtureDocument struct {
	Scenarios []fixtureScenario `json:"scenarios"`
}

type fixtureScenario struct {
	Name            string             `json:"name"`
	Scope           fixtureScope       `json:"scope"`
	Target          fixtureRelease     `json:"target"`
	Configs         fixtureConfigs     `json:"configs"`
	Workflows       []fixtureWorkflow  `json:"workflows"`
	Drift           []fixtureDrift     `json:"drift"`
	Migrations      []fixtureMigration `json:"migrations"`
	WriteReadiness  string             `json:"writeReadiness"`
	ReadinessReason string             `json:"readinessReason"`
}

type fixtureScope struct {
	CurrentBinary, TargetBinary, ContractSource, ConfigSource, CanonicalRoot string
}

type fixtureConfigs struct {
	Current, Canonical, Proposed []string
}

type fixtureRelease struct {
	Ref, Commit, Source, Confidence string
}

type fixtureWorkflow struct {
	Identity, CurrentDSL, TargetDSL, TargetVersionLevel string
	Features                                            []fixtureFeature
	CurrentGraph, TargetGraph, ProposedGraph            []string
	Diagnostics                                         []string
}

type fixtureFeature struct {
	Name, TargetLevel, Change, Source, Confidence string
	Breaking                                      bool
}

type fixtureDrift struct {
	Workflow, Kind, Path, Detail, Source, Confidence   string
	Plan, ExpectedDiff, Validation, Dependency, Review string
}

type fixtureMigration struct {
	Workflow, From, To, Tool, ExpectedDiff, Validation, Dependency, Review string
	Available                                                              bool
}

type recommendation struct {
	Class, Text, Source, Confidence string
}

func TestAdvisorFixtures(t *testing.T) {
	document := loadFixtures(t)
	seen := map[string]bool{}
	for _, scenario := range document.Scenarios {
		t.Run(scenario.Name, func(t *testing.T) {
			seen[scenario.Name] = true
			got := renderAdvisory(scenario)
			want, err := os.ReadFile(filepath.Join("testdata", scenario.Name+".golden.md"))
			if err != nil {
				t.Fatal(err)
			}
			if got != string(want) {
				t.Fatalf("advisory mismatch\n%s", unifiedDiff("golden", "actual", string(want), got))
			}
			for _, field := range []string{
				"Current binary:", "Target binary:", "Exact target ref:",
				"Contract source:", "Config source:", "Canonical root:",
				"Validation diagnostics:", "## Write readiness",
				"`" + scenario.WriteReadiness + "`",
			} {
				if !strings.Contains(got, field) {
					t.Errorf("advisory is missing required report field %q", field)
				}
			}
			for _, workflowFixture := range scenario.Workflows {
				for _, feature := range workflowFixture.Features {
					registered, ok := workflow.LookupFeature(workflow.FeatureID(feature.Name))
					if !ok {
						t.Errorf("feature %q does not exist in the release registry", feature.Name)
						continue
					}
					level, ok := featureLevelAtDSLVersion(registered, workflowFixture.TargetDSL)
					if !ok {
						t.Errorf("feature %q has no support entry for target DSL %s", feature.Name, workflowFixture.TargetDSL)
					} else if level != feature.TargetLevel {
						t.Errorf("feature %q target level = %q, target DSL %s registry says %q",
							feature.Name, feature.TargetLevel, workflowFixture.TargetDSL, level)
					}
				}
				for _, item := range recommendationsFor(workflowFixture, scenario.Drift, scenario.Target) {
					if item.Source == "" || item.Confidence == "" {
						t.Errorf("recommendation lacks provenance: %+v", item)
					}
					if !strings.Contains(item.Source, scenario.Target.Ref) ||
						!strings.Contains(item.Source, scenario.Target.Commit) {
						t.Errorf("recommendation lacks exact target ref provenance: %+v", item)
					}
				}
			}
			for _, migration := range scenario.Migrations {
				if migration.Available && !strings.Contains(migration.Tool, "goobers fix --to") {
					t.Errorf("available migration %s -> %s does not delegate to goobers fix", migration.From, migration.To)
				}
				if migration.ExpectedDiff == "" || migration.Validation == "" {
					t.Errorf("migration %s -> %s lacks an expected diff or validation command", migration.From, migration.To)
				}
			}
			for _, difference := range scenario.Drift {
				if difference.Plan != "" && (difference.ExpectedDiff == "" || difference.Validation == "") {
					t.Errorf("planned drift %s lacks an expected diff or validation command", difference.Path)
				}
			}
		})
	}
	for _, name := range []string{
		"pre-stable-breaking-change",
		"one-version-migration",
		"custom-drift",
		"already-current",
	} {
		if !seen[name] {
			t.Errorf("fixture suite is missing %q", name)
		}
	}
}

func TestRecommendationsClassifyFeatureLifecycleEvidence(t *testing.T) {
	target := fixtureRelease{
		Ref:        "refs/tags/v2.0.0",
		Commit:     "2222222222222222222222222222222222222222",
		Source:     "fixture target registry",
		Confidence: "high",
	}
	for _, test := range []struct {
		name, level, change, wantClass string
		breaking                       bool
	}{
		{
			name:      "removed",
			level:     string(workflow.SupportRemoved),
			change:    "replace it",
			wantClass: "required compatibility change",
		},
		{
			name:      "deprecated",
			level:     string(workflow.SupportDeprecated),
			change:    "use its replacement",
			wantClass: "recommended canonical workflow improvement",
		},
		{
			name:      "changed",
			level:     string(workflow.SupportGA),
			change:    "preserve the old default explicitly",
			breaking:  true,
			wantClass: "required compatibility change",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			items := recommendationsFor(fixtureWorkflow{
				Features: []fixtureFeature{{
					Name:        "fixture.feature",
					TargetLevel: test.level,
					Change:      test.change,
					Source:      "fixture lifecycle evidence",
					Confidence:  "high",
					Breaking:    test.breaking,
				}},
			}, nil, target)
			if len(items) != 1 || items[0].Class != test.wantClass {
				t.Fatalf("recommendations = %+v, want one %q item", items, test.wantClass)
			}
		})
	}
}

func TestFixtureConfigsExerciseAdvisorBehavior(t *testing.T) {
	document := loadFixtures(t)
	for _, scenario := range document.Scenarios {
		t.Run(scenario.Name, func(t *testing.T) {
			current := loadFixtureConfig(t, scenario, scenario.Configs.Current, true)
			canonical := loadFixtureConfig(t, scenario, scenario.Configs.Canonical, false)
			proposed := loadFixtureConfig(t, scenario, scenario.Configs.Proposed, false)

			for _, expected := range scenario.Workflows {
				currentWorkflow, ok := current[expected.Identity]
				if !ok {
					t.Fatalf("current config is missing workflow %q", expected.Identity)
				}
				if currentWorkflow.document.DSLVersion != expected.CurrentDSL {
					t.Errorf("%s current dslVersion = %q, want %q",
						expected.Identity, currentWorkflow.document.DSLVersion, expected.CurrentDSL)
				}
				if !reflect.DeepEqual(currentWorkflow.graph, expected.CurrentGraph) {
					t.Errorf("%s current graph = %v, want %v",
						expected.Identity, currentWorkflow.graph, expected.CurrentGraph)
				}

				proposedWorkflow, ok := proposed[expected.Identity]
				if !ok {
					t.Fatalf("proposed config is missing workflow %q", expected.Identity)
				}
				if proposedWorkflow.document.DSLVersion != expected.TargetDSL {
					t.Errorf("%s proposed dslVersion = %q, want %q",
						expected.Identity, proposedWorkflow.document.DSLVersion, expected.TargetDSL)
				}
				if !reflect.DeepEqual(proposedWorkflow.graph, expected.ProposedGraph) {
					t.Errorf("%s proposed graph = %v, want %v",
						expected.Identity, proposedWorkflow.graph, expected.ProposedGraph)
				}

				canonicalWorkflow, hasCanonical := canonical[expected.Identity]
				if expected.TargetGraph == nil {
					if hasCanonical {
						t.Errorf("custom workflow %q unexpectedly has a canonical peer", expected.Identity)
					}
				} else if !hasCanonical {
					t.Errorf("canonical config is missing workflow %q", expected.Identity)
				} else if !reflect.DeepEqual(canonicalWorkflow.graph, expected.TargetGraph) {
					t.Errorf("%s canonical graph = %v, want %v",
						expected.Identity, canonicalWorkflow.graph, expected.TargetGraph)
				}

				for _, feature := range expected.Features {
					if !currentWorkflow.features[feature.Name] && !proposedWorkflow.features[feature.Name] {
						t.Errorf("workflow %q fixtures do not exercise reported feature %q", expected.Identity, feature.Name)
					}
				}
				if got, want := tuningFromWorkflow(proposedWorkflow.document), tuningFromWorkflow(currentWorkflow.document); !reflect.DeepEqual(got, want) {
					t.Errorf("%s operational tuning changed: got %+v, want %+v", expected.Identity, got, want)
				}
			}

			assertDriftBehavior(t, scenario, current, canonical, proposed)
			if scenario.Name == "already-current" {
				if !reflect.DeepEqual(configDocuments(current), configDocuments(canonical)) ||
					!reflect.DeepEqual(configDocuments(current), configDocuments(proposed)) {
					t.Fatal("already-current fixtures differ")
				}
			}
		})
	}
}

func TestFixturePlansDecomposeVersionJumpsAndPreserveCustomDrift(t *testing.T) {
	document := loadFixtures(t)
	var preStable, custom fixtureScenario
	for _, scenario := range document.Scenarios {
		switch scenario.Name {
		case "pre-stable-breaking-change":
			preStable = scenario
		case "custom-drift":
			custom = scenario
		}
	}
	if len(preStable.Migrations) != 2 ||
		preStable.Migrations[0].From != "0.7" || preStable.Migrations[0].To != "1.0" ||
		preStable.Migrations[1].From != "1.0" || preStable.Migrations[1].To != "1.4" {
		t.Fatalf("pre-stable migration path is not adjacent: %+v", preStable.Migrations)
	}
	report := renderAdvisory(custom)
	for _, class := range []string{
		"recommended canonical workflow improvement",
		"local operational tuning",
		"user customization requiring human judgment",
	} {
		if !strings.Contains(report, "**"+class+"**") {
			t.Errorf("custom drift report is missing class %q", class)
		}
	}
	if strings.Contains(report, "copy canonical workflow") {
		t.Fatal("custom workflow report proposes wholesale replacement")
	}
	if !strings.Contains(report, "no same-identity canonical workflow exists") {
		t.Fatal("custom workflow report does not preserve the unmatched workflow")
	}
}

func TestFixtureGraphsCoverStartKindsAndParallelTopology(t *testing.T) {
	data := readFixture(t, "parallel-state-graph.yaml")
	var document apiv1.Workflow
	if err := yaml.Unmarshal(data, &document); err != nil {
		t.Fatal(err)
	}
	otherGaggle := document
	otherGaggle.Spec.Gaggle = "other"
	byIdentity := map[string]apiv1.Workflow{
		workflowIdentity(document):    document,
		workflowIdentity(otherGaggle): otherGaggle,
	}
	if len(byIdentity) != 2 {
		t.Fatal("same-name workflows in different gaggles collide")
	}
	definition := workflow.Definition{
		Name:       document.Name,
		Version:    1,
		DSLVersion: document.DSLVersion,
		Spec:       document.Spec,
	}
	machine, err := workflow.Compile(definition, workflow.WithPreviewFeatures(true))
	if err != nil {
		t.Fatal(err)
	}
	wantCompiled := []string{
		"start: query",
		"query[deterministic] -> analyze",
		"security[deterministic] -> implement",
		"performance[deterministic] -> implement",
		"implement[agentic] -> review",
		"review[gate](pass -> done, fail -> abort, timeout -> escalate)",
		"analyze[parallel](branch security -> security, branch performance -> performance, branch-failed -> abort)",
	}
	if got := graphLines(machine.Graph()); !reflect.DeepEqual(got, wantCompiled) {
		t.Errorf("compiled graph = %v, want %v", got, wantCompiled)
	}
	wantDocument := []string{
		"start: query",
		"query[deterministic] -> analyze",
		"security[deterministic] -> @join",
		"performance[deterministic] -> @join",
		"implement[agentic] -> review",
		"review[gate](pass -> done, fail -> abort, timeout -> escalate)",
		"analyze[parallel](branch security -> security, branch performance -> performance, join -> implement, branch-failed -> abort)",
	}
	if got := graphLinesFromDocument(document); !reflect.DeepEqual(got, wantDocument) {
		t.Errorf("document graph = %v, want %v", got, wantDocument)
	}
}

func TestMechanicalUpgradeMatchesGoldenAndTargetInterpreter(t *testing.T) {
	before := readFixture(t, "one-version.before.yaml")
	result, err := dslmigrate.Migrate(before, "2.0")
	if err != nil {
		t.Fatal(err)
	}
	after := readFixture(t, "one-version.after.yaml")
	if result.After != string(after) {
		t.Fatalf("migrated workflow mismatch\n%s", unifiedDiff("golden", "actual", string(after), result.After))
	}
	diff := unifiedDiff(
		"a/gaggles/example/workflows/default-implement.yaml",
		"b/gaggles/example/workflows/default-implement.yaml",
		result.Before,
		result.After,
	)
	wantDiff := readFixture(t, "one-version.diff.golden")
	if diff != string(wantDiff) {
		t.Fatalf("migration diff mismatch\n%s", unifiedDiff("golden", "actual", string(wantDiff), diff))
	}

	if got, want := workflowTuning(t, []byte(result.After)), workflowTuning(t, before); !reflect.DeepEqual(got, want) {
		t.Fatalf("operational tuning changed: got %+v, want %+v", got, want)
	}

	root := filepath.Join(t.TempDir(), "instance")
	if _, err := instance.Init(root); err != nil {
		t.Fatal(err)
	}
	workflowPath := filepath.Join(root, "config", "gaggles", "example", "workflows", "default-implement.yaml")
	if err := os.WriteFile(workflowPath, []byte(result.After), 0o644); err != nil {
		t.Fatal(err)
	}
	set, report, err := instance.LoadConfigDir(filepath.Join(root, "config"))
	if err != nil {
		t.Fatalf("target interpreter rejected migrated workflow: %v\n%+v", err, report)
	}
	if len(set.Workflows) != 1 || set.Workflows[0].DSLVersion != "2.0" {
		t.Fatalf("loaded workflows = %+v, want one workflow at dslVersion 2.0", set.Workflows)
	}
}

func TestUpgradeSkillMatchesFixtureContract(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "skills", "goobers-workflow-upgrade", "SKILL.md"))
	if err != nil {
		t.Fatal(err)
	}
	skill := string(data)
	assertInOrder(t, skill,
		"## 1. Establish the upgrade boundary",
		"## 2. Collect release-matched evidence",
		"## 3. Classify differences",
		"## 4. Produce a per-workflow state-graph diff",
		"## 5. Build the ordered upgrade plan",
		"## Required advisory report",
		"## 6. Explicit write path",
	)
	for _, directive := range []string{
		"`kind: git` source is the committed configured ref",
		"authoring worktree",
		"`<spec.gaggle>/<metadata.name>` identity used by `config diff`",
		"`features --used` returns an instance-wide union",
		"`config diff` classifies these paths as",
		"delegate the mechanical transform to",
		"one adjacent compatibility edge",
		"Every recommendation must carry source provenance",
		"Never copy an entire canonical",
		"validate --strict",
		"no target validation warning",
		"require a human to commit it to the configured ref",
		"Do not commit, push, deploy",
	} {
		if !strings.Contains(skill, directive) {
			t.Errorf("skill is missing fixture-backed directive %q", directive)
		}
	}
}

func loadFixtures(t *testing.T) fixtureDocument {
	t.Helper()
	data := readFixture(t, "advisor-fixtures.json")
	var document fixtureDocument
	if err := json.Unmarshal(data, &document); err != nil {
		t.Fatal(err)
	}
	for i := range document.Scenarios {
		scenario := &document.Scenarios[i]
		hydrateScenarioGraphs(t, scenario)
	}
	return document
}

func hydrateScenarioGraphs(t *testing.T, scenario *fixtureScenario) {
	t.Helper()
	current := fixtureGraphs(t, scenario.Configs.Current)
	canonical := fixtureGraphs(t, scenario.Configs.Canonical)
	proposed := fixtureGraphs(t, scenario.Configs.Proposed)
	for i := range scenario.Workflows {
		candidate := &scenario.Workflows[i]
		var ok bool
		if candidate.CurrentGraph, ok = current[candidate.Identity]; !ok {
			t.Fatalf("current fixtures are missing workflow %q", candidate.Identity)
		}
		candidate.TargetGraph = canonical[candidate.Identity]
		if candidate.ProposedGraph, ok = proposed[candidate.Identity]; !ok {
			t.Fatalf("proposed fixtures are missing workflow %q", candidate.Identity)
		}
	}
}

func fixtureGraphs(t *testing.T, fixtures []string) map[string][]string {
	t.Helper()
	graphs := make(map[string][]string, len(fixtures))
	for _, name := range fixtures {
		var document apiv1.Workflow
		if err := yaml.Unmarshal(readFixture(t, name), &document); err != nil {
			t.Fatalf("parse fixture %s: %v", name, err)
		}
		graphs[workflowIdentity(document)] = graphLinesFromDocument(document)
	}
	return graphs
}

func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func renderAdvisory(scenario fixtureScenario) string {
	var out strings.Builder
	fmt.Fprintf(&out, "# Upgrade advisory: %s\n\n", scenario.Name)
	out.WriteString("## Scope and provenance\n\n")
	fmt.Fprintf(&out, "- Current binary: %s\n", scenario.Scope.CurrentBinary)
	fmt.Fprintf(&out, "- Target binary: %s\n", scenario.Scope.TargetBinary)
	fmt.Fprintf(&out, "- Exact target ref: `%s` at commit `%s`\n", scenario.Target.Ref, scenario.Target.Commit)
	fmt.Fprintf(&out, "- Contract source: %s\n", scenario.Scope.ContractSource)
	fmt.Fprintf(&out, "- Config source: %s\n", scenario.Scope.ConfigSource)
	fmt.Fprintf(&out, "- Canonical root: %s\n", scenario.Scope.CanonicalRoot)
	fmt.Fprintf(&out, "- Source provenance: %s\n", scenario.Target.Source)
	fmt.Fprintf(&out, "- Compatibility confidence: `%s`\n", scenario.Target.Confidence)

	changeCount := len(scenario.Migrations)
	for _, workflow := range scenario.Workflows {
		fmt.Fprintf(&out, "\n## Workflow `%s` (`%s` -> `%s`)\n\n", workflow.Identity, workflow.CurrentDSL, workflow.TargetDSL)
		out.WriteString("Feature inventory:\n")
		if len(workflow.Features) == 0 {
			out.WriteString("- none\n")
		}
		for _, feature := range workflow.Features {
			detail := feature.Change
			if detail == "" {
				detail = "unchanged at the target"
			}
			fmt.Fprintf(&out, "- `%s`: `%s` - %s. (source: %s; confidence: %s)\n",
				feature.Name, feature.TargetLevel, sentence(detail), feature.Source, feature.Confidence)
		}
		out.WriteString("\nValidation diagnostics:\n")
		for _, diagnostic := range workflow.Diagnostics {
			fmt.Fprintf(&out, "- %s\n", diagnostic)
		}
		if workflow.TargetGraph == nil {
			fmt.Fprintf(&out, "\nCurrent state graph: `%s`\n", strings.Join(workflow.CurrentGraph, "; "))
			out.WriteString("Target canonical state graph: no same-identity canonical workflow exists.\n")
		} else if reflect.DeepEqual(workflow.CurrentGraph, workflow.TargetGraph) {
			fmt.Fprintf(&out, "\nTarget canonical state graph: unchanged: `%s`\n", strings.Join(workflow.CurrentGraph, "; "))
		} else {
			fmt.Fprintf(&out, "\nTarget canonical state-graph diff:\n- current: `%s`\n- target: `%s`\n",
				strings.Join(workflow.CurrentGraph, "; "), strings.Join(workflow.TargetGraph, "; "))
		}
		if reflect.DeepEqual(workflow.CurrentGraph, workflow.ProposedGraph) {
			fmt.Fprintf(&out, "Proposed state graph: unchanged: `%s`\n", strings.Join(workflow.ProposedGraph, "; "))
		} else {
			fmt.Fprintf(&out, "Proposed state-graph diff:\n- current: `%s`\n- proposed: `%s`\n",
				strings.Join(workflow.CurrentGraph, "; "), strings.Join(workflow.ProposedGraph, "; "))
		}

		items := recommendationsFor(workflow, scenario.Drift, scenario.Target)
		out.WriteString("\nRecommendations:\n")
		if len(items) == 0 {
			out.WriteString("- none; this workflow is already current.\n")
		}
		for _, item := range items {
			fmt.Fprintf(&out, "- **%s**: %s. (source: %s; confidence: %s)\n",
				item.Class, sentence(item.Text), item.Source, item.Confidence)
		}
		changeCount += len(items)
	}

	out.WriteString("\n## Ordered upgrade plan\n\n")
	step := 1
	for _, migration := range scenario.Migrations {
		fmt.Fprintf(&out,
			"%d. `%s`: in an isolated scratch copy, run `%s` for `%s -> %s`; dependency: %s; expected file diff: %s; review: %s; validation command: `%s`.\n",
			step, migration.Workflow, migration.Tool, migration.From, migration.To, migration.Dependency,
			migration.ExpectedDiff, migration.Review, migration.Validation)
		step++
	}
	for _, drift := range scenario.Drift {
		if drift.Plan == "" {
			continue
		}
		fmt.Fprintf(&out, "%d. `%s`: %s; dependency: %s; expected file diff: %s; review: %s; validation command: `%s`.\n",
			step, drift.Workflow, drift.Plan, drift.Dependency, drift.ExpectedDiff, drift.Review, drift.Validation)
		step++
	}
	if changeCount == 0 {
		fmt.Fprintf(&out, "%d. No write: all workflows are already at the target and no compatibility or structural change is identified. Retain the target validation baseline.\n", step)
	} else {
		fmt.Fprintf(&out, "%d. Run target `goobers validate --strict`, targeted `goobers config diff`, and the state-graph comparison. A write is complete only when target validation has no warnings and approved tuning is unchanged.\n", step)
	}
	fmt.Fprintf(&out, "\n## Write readiness\n\n`%s`: %s\n", scenario.WriteReadiness, scenario.ReadinessReason)
	return out.String()
}

func recommendationsFor(
	workflow fixtureWorkflow,
	drift []fixtureDrift,
	target fixtureRelease,
) []recommendation {
	var items []recommendation
	if workflow.TargetVersionLevel == "unsupported" {
		items = append(items, recommendation{
			Class:      "required compatibility change",
			Text:       fmt.Sprintf("dslVersion `%s` is unsupported; move to `%s`", workflow.CurrentDSL, workflow.TargetDSL),
			Source:     withTargetProvenance(target.Source, target),
			Confidence: target.Confidence,
		})
	}
	for _, feature := range workflow.Features {
		item := recommendation{
			Source:     withTargetProvenance(feature.Source, target),
			Confidence: feature.Confidence,
		}
		switch feature.TargetLevel {
		case "removed":
			item.Class = "required compatibility change"
			item.Text = fmt.Sprintf("feature `%s` is removed; %s", feature.Name, feature.Change)
		case "deprecated":
			item.Class = "recommended canonical workflow improvement"
			item.Text = fmt.Sprintf("feature `%s` is deprecated; %s", feature.Name, feature.Change)
		default:
			if feature.Change == "" {
				continue
			}
			if feature.Breaking {
				item.Class = "required compatibility change"
			} else {
				item.Class = "user customization requiring human judgment"
			}
			item.Text = fmt.Sprintf("feature `%s` changed; %s", feature.Name, feature.Change)
		}
		items = append(items, item)
	}
	for _, difference := range drift {
		if difference.Workflow != workflow.Identity {
			continue
		}
		item := recommendation{
			Source:     withTargetProvenance(difference.Source, target),
			Confidence: difference.Confidence,
		}
		switch difference.Kind {
		case "structural":
			item.Class = "recommended canonical workflow improvement"
		case "tuning":
			item.Class = "local operational tuning"
		case "custom":
			item.Class = "user customization requiring human judgment"
		}
		item.Text = fmt.Sprintf("`%s`: %s", difference.Path, difference.Detail)
		items = append(items, item)
	}
	return items
}

func withTargetProvenance(source string, target fixtureRelease) string {
	return fmt.Sprintf("%s; target %s at commit %s", source, target.Ref, target.Commit)
}

type loadedFixtureWorkflow struct {
	document apiv1.Workflow
	graph    []string
	features map[string]bool
}

func loadFixtureConfig(
	t *testing.T,
	scenario fixtureScenario,
	fixtures []string,
	allowInvalid bool,
) map[string]loadedFixtureWorkflow {
	t.Helper()
	if len(fixtures) == 0 {
		t.Fatal("fixture config set is empty")
	}

	root := filepath.Join(t.TempDir(), "instance")
	if _, err := instance.Init(root); err != nil {
		t.Fatal(err)
	}
	for _, skill := range []string{"implement", "run-tests"} {
		if err := os.MkdirAll(filepath.Join(root, "skills", skill), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	workflowDir := filepath.Join(root, "config", "gaggles", "example", "workflows")
	if err := os.Remove(filepath.Join(workflowDir, "default-implement.yaml")); err != nil {
		t.Fatal(err)
	}
	var workflowNames []string
	for _, name := range fixtures {
		data := readFixture(t, name)
		var document apiv1.Workflow
		if err := yaml.Unmarshal(data, &document); err != nil {
			t.Fatalf("parse fixture %s: %v", name, err)
		}
		if err := os.WriteFile(filepath.Join(workflowDir, document.Name+".yaml"), data, 0o644); err != nil {
			t.Fatal(err)
		}
		workflowNames = append(workflowNames, document.Name)
	}
	sort.Strings(workflowNames)
	gooberPath := filepath.Join(root, "config", "gaggles", "example", "goobers", "coder", "goober.yaml")
	gooberData, err := os.ReadFile(gooberPath)
	if err != nil {
		t.Fatal(err)
	}
	var workflowRefs strings.Builder
	for _, name := range workflowNames {
		fmt.Fprintf(&workflowRefs, "    - %s\n", name)
	}
	updatedGoober := strings.Replace(string(gooberData), "    - default-implement\n", workflowRefs.String(), 1)
	if err := os.WriteFile(gooberPath, []byte(updatedGoober), 0o644); err != nil {
		t.Fatal(err)
	}

	var (
		set     *instance.ConfigSet
		report  *validate.Report
		loadErr error
	)
	if allowInvalid {
		set, report, loadErr = instance.LoadConfigDirForComparison(filepath.Join(root, "config"))
	} else {
		set, report, loadErr = instance.LoadConfigDir(filepath.Join(root, "config"))
	}
	if set == nil {
		t.Fatalf("load fixture config: %v (report: %+v)", loadErr, report)
	}
	if !allowInvalid && loadErr != nil {
		t.Fatalf("target interpreter rejected fixture config: %v", loadErr)
	}
	if allowInvalid && loadErr != nil && !scenarioAllowsInvalidCurrent(scenario) {
		t.Fatalf("current fixture config is unexpectedly invalid: %v (report: %+v)", loadErr, report)
	}
	// DVL020 (deprecated dslVersion) is expected on fixtures that deliberately
	// sit on a historical DSL version (#2700 deprecated 1.4): the advisor's
	// whole subject is configs on old versions, so the deprecation warning is
	// evidence the lifecycle works, not fixture rot. Every other warning still
	// fails the load.
	var unexpected []validate.CodedWarning
	for _, warning := range report.Warnings() {
		if warning.Code == validate.WarningDeprecatedDSLVersion {
			continue
		}
		unexpected = append(unexpected, warning)
	}
	if len(unexpected) != 0 &&
		(!allowInvalid || !scenarioAllowsInvalidCurrent(scenario)) {
		t.Fatalf("target validation is not clean: %v", unexpected)
	}

	loaded := make(map[string]loadedFixtureWorkflow, len(set.Workflows))
	for _, document := range set.Workflows {
		definition := workflow.Definition{
			Name:       document.Name,
			Version:    1,
			DSLVersion: document.DSLVersion,
			Spec:       document.Spec,
		}
		machine, compileErr := workflow.Compile(definition, workflow.WithPreviewFeatures(true))
		if compileErr != nil && allowInvalid && strings.Contains(compileErr.Error(), "not supported") {
			definition.DSLVersion = targetDSLFor(scenario, workflowIdentity(document))
			machine, compileErr = workflow.Compile(definition, workflow.WithPreviewFeatures(true))
		}
		if compileErr != nil && !allowInvalid {
			t.Fatalf("compile fixture workflow %q: %v", document.Name, compileErr)
		}
		features, featureErr := workflow.FeaturesForWorkflow(definition)
		if featureErr != nil {
			t.Fatalf("discover fixture workflow %q features: %v", document.Name, featureErr)
		}
		featureSet := make(map[string]bool, len(features))
		for _, feature := range features {
			featureSet[string(feature.ID)] = true
		}
		graph := graphLinesFromDocument(document)
		if compileErr == nil {
			graph = graphLines(machine.Graph())
		}
		loaded[workflowIdentity(document)] = loadedFixtureWorkflow{
			document: document,
			graph:    graph,
			features: featureSet,
		}
	}
	return loaded
}

func targetDSLFor(scenario fixtureScenario, identity string) string {
	for _, candidate := range scenario.Workflows {
		if candidate.Identity == identity {
			return candidate.TargetDSL
		}
	}
	return ""
}

func workflowIdentity(document apiv1.Workflow) string {
	return document.Spec.Gaggle + "/" + document.Name
}

func featureLevelAtDSLVersion(feature workflow.Feature, version string) (string, bool) {
	for _, support := range feature.DSLVersions {
		if support.Version == version {
			return string(support.Level), true
		}
	}
	return "", false
}

func scenarioAllowsInvalidCurrent(scenario fixtureScenario) bool {
	for _, candidate := range scenario.Workflows {
		if candidate.TargetVersionLevel == "unsupported" {
			return true
		}
	}
	return false
}

func graphLines(graph workflow.Graph) []string {
	lines := []string{"start: " + graph.Start}
	for _, node := range graph.Nodes {
		var edges []workflow.GraphEdge
		for _, edge := range graph.Edges {
			if edge.Source == node.ID {
				edges = append(edges, edge)
			}
		}
		switch string(node.Kind) {
		case string(workflow.GraphNodeGate):
			branches := make([]string, 0, len(edges))
			for _, edge := range edges {
				branches = append(branches, fmt.Sprintf("%s -> %s", edge.Outcome, graphTarget(edge)))
			}
			lines = append(lines, fmt.Sprintf("%s[%s](%s)", node.ID, node.Kind, strings.Join(branches, ", ")))
		case "parallel":
			branches := make([]string, 0, len(edges))
			for _, edge := range edges {
				if edge.Branch != "" {
					branches = append(branches, fmt.Sprintf("branch %s -> %s", edge.Branch, graphTarget(edge)))
				} else {
					branches = append(branches, fmt.Sprintf("%s -> %s", edge.Outcome, graphTarget(edge)))
				}
			}
			lines = append(lines, fmt.Sprintf("%s[%s](%s)", node.ID, node.Kind, strings.Join(branches, ", ")))
		default:
			if len(edges) != 1 {
				lines = append(lines, fmt.Sprintf("%s[%s] -> <invalid>", node.ID, node.Kind))
				continue
			}
			lines = append(lines, fmt.Sprintf("%s[%s] -> %s", node.ID, node.Kind, graphTarget(edges[0])))
		}
	}
	return lines
}

func graphLinesFromDocument(document apiv1.Workflow) []string {
	lines := make([]string, 0, 1+len(document.Spec.Tasks)+len(document.Spec.Gates)+len(document.Spec.Parallels))
	lines = append(lines, "start: "+document.Spec.Start)
	for _, task := range document.Spec.Tasks {
		lines = append(lines, fmt.Sprintf("%s[%s] -> %s", task.Name, task.Type, rawGraphTarget(task.Next)))
	}
	for _, gate := range document.Spec.Gates {
		outcomes := make([]string, 0, len(gate.Branches))
		for outcome := range gate.Branches {
			outcomes = append(outcomes, outcome)
		}
		sort.Slice(outcomes, func(i, j int) bool {
			return outcomeRank(outcomes[i]) < outcomeRank(outcomes[j])
		})
		branches := make([]string, 0, len(outcomes))
		for _, outcome := range outcomes {
			branches = append(branches, fmt.Sprintf("%s -> %s", outcome, rawGraphTarget(gate.Branches[outcome])))
		}
		lines = append(lines, fmt.Sprintf("%s[gate](%s)", gate.Name, strings.Join(branches, ", ")))
	}
	for _, parallel := range document.Spec.Parallels {
		edges := make([]string, 0, len(parallel.Branches)+2)
		for _, branch := range parallel.Branches {
			edges = append(edges, fmt.Sprintf("branch %s -> %s", branch.Name, branch.Start))
		}
		edges = append(edges, fmt.Sprintf("join -> %s", rawGraphTarget(parallel.Join)))
		if parallel.OnFailure != "" {
			edges = append(edges, fmt.Sprintf("branch-failed -> %s", rawGraphTarget(parallel.OnFailure)))
		}
		lines = append(lines, fmt.Sprintf("%s[parallel](%s)", parallel.Name, strings.Join(edges, ", ")))
	}
	return lines
}

func outcomeRank(outcome string) string {
	switch outcome {
	case "pass":
		return "0"
	case "fail":
		return "1"
	default:
		return "2" + outcome
	}
}

func rawGraphTarget(target string) string {
	switch target {
	case "":
		return "done"
	case "@abort":
		return "abort"
	case "@escalate":
		return "escalate"
	default:
		return target
	}
}

func graphTarget(edge workflow.GraphEdge) string {
	switch edge.Terminal {
	case workflow.GraphTerminalComplete:
		return "done"
	case workflow.GraphTerminalAbort:
		return "abort"
	case workflow.GraphTerminalEscalate:
		return "escalate"
	default:
		return edge.Target
	}
}

func assertDriftBehavior(
	t *testing.T,
	scenario fixtureScenario,
	current, canonical, proposed map[string]loadedFixtureWorkflow,
) {
	t.Helper()
	for _, difference := range scenario.Drift {
		currentWorkflow := current[difference.Workflow]
		proposedWorkflow := proposed[difference.Workflow]
		switch difference.Kind {
		case "tuning":
			canonicalWorkflow, ok := canonical[difference.Workflow]
			if !ok {
				t.Errorf("tuning drift %q has no canonical workflow", difference.Path)
				continue
			}
			currentValue := tuningValue(currentWorkflow.document, difference.Path)
			canonicalValue := tuningValue(canonicalWorkflow.document, difference.Path)
			proposedValue := tuningValue(proposedWorkflow.document, difference.Path)
			if reflect.DeepEqual(currentValue, canonicalValue) {
				t.Errorf("tuning drift %q is not present in the fixtures", difference.Path)
			}
			if !reflect.DeepEqual(currentValue, proposedValue) {
				t.Errorf("tuning drift %q was not preserved", difference.Path)
			}
		case "structural":
			taskName := bracketedName(difference.Path)
			canonicalWorkflow, ok := canonical[difference.Workflow]
			if !ok {
				t.Errorf("structural drift %q has no canonical workflow", difference.Path)
				continue
			}
			if hasTask(currentWorkflow.document, taskName) ||
				!hasTask(canonicalWorkflow.document, taskName) ||
				!hasTask(proposedWorkflow.document, taskName) {
				t.Errorf("structural drift %q is not represented by a surgical proposed patch", difference.Path)
			}
		case "custom":
			if _, ok := canonical[difference.Workflow]; ok {
				t.Errorf("custom workflow %q unexpectedly has a canonical peer", difference.Workflow)
			}
			if !reflect.DeepEqual(currentWorkflow.document.Spec, proposedWorkflow.document.Spec) {
				t.Errorf("custom workflow %q changed in the proposed config", difference.Workflow)
			}
		default:
			t.Errorf("unknown drift kind %q", difference.Kind)
		}
	}
}

func tuningValue(document apiv1.Workflow, path string) any {
	switch path {
	case "spec.triggers[0].schedule":
		if len(document.Spec.Triggers) == 0 {
			return ""
		}
		return document.Spec.Triggers[0].Schedule
	case "spec.readiness.maxConcurrentRuns":
		return document.Spec.Readiness.MaxConcurrentRuns
	default:
		return nil
	}
}

func bracketedName(path string) string {
	start := strings.IndexByte(path, '[')
	end := strings.LastIndexByte(path, ']')
	if start < 0 || end <= start {
		return ""
	}
	return path[start+1 : end]
}

func hasTask(document apiv1.Workflow, name string) bool {
	for _, task := range document.Spec.Tasks {
		if task.Name == name {
			return true
		}
	}
	return false
}

func tuningFromWorkflow(document apiv1.Workflow) tuning {
	var value tuning
	value.Triggers = make([]struct {
		Type     string `yaml:"type"`
		Schedule string `yaml:"schedule"`
	}, len(document.Spec.Triggers))
	for i, trigger := range document.Spec.Triggers {
		value.Triggers[i].Type = string(trigger.Type)
		value.Triggers[i].Schedule = trigger.Schedule
	}
	value.Readiness.MaxConcurrentRuns = int(document.Spec.Readiness.MaxConcurrentRuns)
	value.Readiness.MaxRunsPerHour = int(document.Spec.Readiness.MaxRunsPerHour)
	value.Readiness.MaxRunsPerDay = int(document.Spec.Readiness.MaxRunsPerDay)
	value.Readiness.MaxOpenPRs = int(document.Spec.Readiness.MaxOpenPRs)
	return value
}

func configDocuments(config map[string]loadedFixtureWorkflow) map[string]apiv1.Workflow {
	documents := make(map[string]apiv1.Workflow, len(config))
	for name, loaded := range config {
		documents[name] = loaded.document
	}
	return documents
}

func sentence(value string) string {
	return strings.TrimSuffix(value, ".")
}

func unifiedDiff(from, to, before, after string) string {
	diff, err := difflib.GetUnifiedDiffString(difflib.UnifiedDiff{
		A:        difflib.SplitLines(before),
		B:        difflib.SplitLines(after),
		FromFile: from,
		ToFile:   to,
		Context:  3,
	})
	if err != nil {
		panic(err)
	}
	return diff
}

type tuning struct {
	Triggers []struct {
		Type     string `yaml:"type"`
		Schedule string `yaml:"schedule"`
	} `yaml:"triggers"`
	Readiness struct {
		MaxConcurrentRuns int `yaml:"maxConcurrentRuns"`
		MaxRunsPerHour    int `yaml:"maxRunsPerHour"`
		MaxRunsPerDay     int `yaml:"maxRunsPerDay"`
		MaxOpenPRs        int `yaml:"maxOpenPRs"`
	} `yaml:"readiness"`
}

func workflowTuning(t *testing.T, data []byte) tuning {
	t.Helper()
	var document struct {
		Spec tuning `yaml:"spec"`
	}
	if err := yaml.Unmarshal(data, &document); err != nil {
		t.Fatal(err)
	}
	return document.Spec
}

func assertInOrder(t *testing.T, text string, values ...string) {
	t.Helper()
	last := -1
	for _, value := range values {
		index := strings.Index(text, value)
		if index < 0 {
			t.Errorf("missing %q", value)
			continue
		}
		if index < last {
			t.Errorf("%q appears out of order", value)
		}
		last = index
	}
}
