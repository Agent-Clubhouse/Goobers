package workflowupgrade

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/pmezard/go-difflib/difflib"
	"gopkg.in/yaml.v3"

	"github.com/goobers/goobers/internal/dslmigrate"
	"github.com/goobers/goobers/internal/instance"
)

type fixtureDocument struct {
	Scenarios []fixtureScenario `json:"scenarios"`
}

type fixtureScenario struct {
	Name       string             `json:"name"`
	Golden     string             `json:"golden"`
	Target     fixtureRelease     `json:"target"`
	Workflows  []fixtureWorkflow  `json:"workflows"`
	Drift      []fixtureDrift     `json:"drift"`
	Migrations []fixtureMigration `json:"migrations"`
}

type fixtureRelease struct {
	Version, Commit, Source, Confidence string
}

type fixtureWorkflow struct {
	Name, CurrentDSL, TargetDSL, TargetVersionLevel string
	Features                                        []fixtureFeature
	CurrentGraph, TargetGraph                       []string
}

type fixtureFeature struct {
	Name, TargetLevel, Change, Source, Confidence string
	Breaking                                      bool
}

type fixtureDrift struct {
	Workflow, Kind, Path, Detail, Source, Confidence string
	Plan, Dependency, Review                         string
}

type fixtureMigration struct {
	Workflow, From, To, Tool, Dependency, Review string
	Available                                    bool
}

type recommendation struct {
	Class, Text, Source, Confidence string
}

func TestAdvisorFixtures(t *testing.T) {
	document := loadFixtures(t)
	seen := map[string]bool{}
	featureCoverage := map[string]bool{}
	for _, scenario := range document.Scenarios {
		t.Run(scenario.Name, func(t *testing.T) {
			seen[scenario.Name] = true
			got := renderAdvisory(scenario)
			want, err := os.ReadFile(filepath.Join("testdata", scenario.Golden))
			if err != nil {
				t.Fatal(err)
			}
			if got != string(want) {
				t.Fatalf("advisory mismatch\n%s", unifiedDiff("golden", "actual", string(want), got))
			}
			for _, workflow := range scenario.Workflows {
				for _, feature := range workflow.Features {
					featureCoverage[feature.TargetLevel] = true
					if feature.Change != "" {
						featureCoverage["changed"] = true
					}
				}
				for _, item := range recommendationsFor(workflow, scenario.Drift, scenario.Target) {
					if item.Source == "" || item.Confidence == "" {
						t.Errorf("recommendation lacks provenance: %+v", item)
					}
				}
			}
			for _, migration := range scenario.Migrations {
				if migration.Available && !strings.Contains(migration.Tool, "goobers fix --to") {
					t.Errorf("available migration %s -> %s does not delegate to goobers fix", migration.From, migration.To)
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
	for _, featureCase := range []string{"removed", "deprecated", "changed"} {
		if !featureCoverage[featureCase] {
			t.Errorf("fixture suite does not cover a %s feature", featureCase)
		}
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
	if !strings.Contains(report, "no same-name canonical workflow exists") {
		t.Fatal("custom workflow report does not preserve the unmatched workflow")
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
		"`features --used` returns an instance-wide union",
		"`config diff` classifies these paths as",
		"delegate the mechanical transform to",
		"one adjacent compatibility edge",
		"Every recommendation must carry source provenance",
		"Never copy an entire canonical",
		"no upgrade-related warning remains",
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
	return document
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
	fmt.Fprintf(&out, "Target release: `%s` (`%s`)\n\n", scenario.Target.Version, scenario.Target.Commit)
	fmt.Fprintf(&out, "Source provenance: %s\n\n", scenario.Target.Source)
	fmt.Fprintf(&out, "Compatibility confidence: `%s`\n", scenario.Target.Confidence)

	changeCount := len(scenario.Migrations)
	for _, workflow := range scenario.Workflows {
		fmt.Fprintf(&out, "\n## Workflow `%s` (`%s` -> `%s`)\n\n", workflow.Name, workflow.CurrentDSL, workflow.TargetDSL)
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
		if reflect.DeepEqual(workflow.CurrentGraph, workflow.TargetGraph) {
			fmt.Fprintf(&out, "\nState graph: unchanged: `%s`\n", strings.Join(workflow.CurrentGraph, "; "))
		} else {
			fmt.Fprintf(&out, "\nState-graph diff:\n- current: `%s`\n- target: `%s`\n",
				strings.Join(workflow.CurrentGraph, "; "), strings.Join(workflow.TargetGraph, "; "))
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
			"%d. `%s`: in an isolated scratch copy, run `%s` for `%s -> %s`; dependency: %s; review: %s. Apply that edge only after review, then validate it with the target interpreter.\n",
			step, migration.Workflow, migration.Tool, migration.From, migration.To, migration.Dependency, migration.Review)
		step++
	}
	for _, drift := range scenario.Drift {
		if drift.Plan == "" {
			continue
		}
		fmt.Fprintf(&out, "%d. `%s`: %s; dependency: %s; review: %s.\n",
			step, drift.Workflow, drift.Plan, drift.Dependency, drift.Review)
		step++
	}
	if changeCount == 0 {
		fmt.Fprintf(&out, "%d. No write: all workflows are already at the target and no compatibility or structural change is identified. Retain the target validation baseline.\n", step)
	} else {
		fmt.Fprintf(&out, "%d. Run target `goobers validate`, targeted `goobers config diff`, and the state-graph comparison. A write is complete only when target validation is clean and approved tuning is unchanged.\n", step)
	}
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
			Source:     target.Source,
			Confidence: target.Confidence,
		})
	}
	for _, feature := range workflow.Features {
		item := recommendation{Source: feature.Source, Confidence: feature.Confidence}
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
		if difference.Workflow != workflow.Name {
			continue
		}
		item := recommendation{
			Source:     difference.Source,
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
