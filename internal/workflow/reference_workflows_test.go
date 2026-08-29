package workflow

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/capability"
	"github.com/goobers/goobers/internal/credentials"
	"github.com/goobers/goobers/internal/journal"
)

func TestReferenceWorkflowsREADMEInventoryAndMergePosture(t *testing.T) {
	configRoot := filepath.Join("..", "..", "reference-workflows")
	definitionRoot := filepath.Join(configRoot, "gaggles", "goobers")
	readmeRaw, err := os.ReadFile(filepath.Join(configRoot, "README.md"))
	if err != nil {
		t.Fatalf("read reference workflow guide: %v", err)
	}
	readme := string(readmeRaw)

	gooberEntries, err := os.ReadDir(filepath.Join(definitionRoot, "goobers"))
	if err != nil {
		t.Fatalf("read goober definitions: %v", err)
	}
	var roles []string
	for _, entry := range gooberEntries {
		if !entry.IsDir() {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(definitionRoot, "goobers", entry.Name(), "goober.yaml"))
		if err != nil {
			t.Fatalf("read %s goober: %v", entry.Name(), err)
		}
		var goober apiv1.Goober
		if err := yaml.Unmarshal(raw, &goober); err != nil {
			t.Fatalf("unmarshal %s goober: %v", entry.Name(), err)
		}
		roles = append(roles, goober.Spec.Role)
	}

	workflowEntries, err := os.ReadDir(filepath.Join(definitionRoot, "workflows"))
	if err != nil {
		t.Fatalf("read workflow definitions: %v", err)
	}
	var workflows []apiv1.Workflow
	for _, entry := range workflowEntries {
		if entry.IsDir() || (filepath.Ext(entry.Name()) != ".yaml" && filepath.Ext(entry.Name()) != ".yml") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(definitionRoot, "workflows", entry.Name()))
		if err != nil {
			t.Fatalf("read %s workflow: %v", entry.Name(), err)
		}
		var workflow apiv1.Workflow
		if err := yaml.Unmarshal(raw, &workflow); err != nil {
			t.Fatalf("unmarshal %s workflow: %v", entry.Name(), err)
		}
		workflows = append(workflows, workflow)
	}

	inventoryMarker := fmt.Sprintf("<!-- reference-inventory: goobers=%d workflows=%d -->", len(roles), len(workflows))
	if !strings.Contains(readme, inventoryMarker) {
		t.Errorf("README inventory marker does not match loaded definitions; want %q", inventoryMarker)
	}
	validationOutput := fmt.Sprintf("config/ valid (1 gaggle(s), %d goober(s), %d workflow(s))", len(roles), len(workflows))
	if !strings.Contains(readme, validationOutput) {
		t.Errorf("README validation sample does not match loaded definitions; want %q", validationOutput)
	}
	for _, role := range roles {
		if !strings.Contains(readme, "`"+role+"`") {
			t.Errorf("README inventory omits goober role %q", role)
		}
	}
	for _, definition := range workflows {
		if !strings.Contains(readme, "`"+definition.Name+"`") {
			t.Errorf("README inventory omits workflow %q", definition.Name)
		}
	}
	for _, credential := range []string{
		"GOOBERS_GITHUB_TOKEN",
		"GOOBERS_GITHUB_REVIEW_TOKEN",
		"GOOBERS_COPILOT_TOKEN",
	} {
		if !strings.Contains(readme, "`"+credential+"`") {
			t.Errorf("README credential inventory omits %s", credential)
		}
	}

	var mergeReview *apiv1.Workflow
	for i := range workflows {
		if workflows[i].Name == "merge-review" {
			mergeReview = &workflows[i]
			break
		}
	}
	if mergeReview == nil {
		t.Fatal("loaded workflows have no merge-review definition")
	}
	var mergeTask *apiv1.Task
	for i := range mergeReview.Spec.Tasks {
		task := &mergeReview.Spec.Tasks[i]
		if task.Name == "merge-pr" {
			mergeTask = task
			break
		}
	}
	if mergeTask == nil || mergeTask.Run == nil || !slices.Equal(mergeTask.Run.Command, []string{"goobers", "merge-pr"}) {
		t.Fatalf("merge-review merge authority = %+v, want deterministic goobers merge-pr task", mergeTask)
	}
	if !containsString(mergeTask.Capabilities, string(capability.GitHubPRMerge)) {
		t.Fatalf("merge-pr capabilities = %v, want %s opt-in", mergeTask.Capabilities, capability.GitHubPRMerge)
	}
	for _, gate := range mergeReview.Spec.Gates {
		if !strings.Contains(readme, "`"+gate.Name+"`") {
			t.Errorf("README merge posture omits gate %q", gate.Name)
		}
	}
	for _, claim := range []string{
		"explicit, fail-closed opt-in",
		"Removing that task/grant",
		"independently re-checks",
	} {
		if !strings.Contains(readme, claim) {
			t.Errorf("README merge posture omits %q", claim)
		}
	}
}

// TestReferenceWorkflowsCompile is #124's divergence guard: it compiles the
// REAL reference-workflows/ definitions (this repo's own dogfood config) directly,
// against the compiler's full admission checks (capabilities, harness, and
// gate-outcome coverage). testdata/shipped/*.yaml are separately maintained,
// deliberately minimal synthetic fixtures pinned to golden digests — nothing
// previously compiled the actual reference-workflows YAML, so it could (and did, per
// #124's architect review of testdata/shipped/implementation.yaml) drift
// invalid without any test catching it.
func TestReferenceWorkflowsCompile(t *testing.T) {
	root := filepath.Join("..", "..", "reference-workflows", "gaggles", "goobers")

	goobers := map[string]apiv1.GooberSpec{}
	for _, name := range []string{"implementer", "reviewer", "curator", "nominator", "analyst", "config-author", "quality-researcher", "quality-lead", "test-quality-analyst"} {
		var g apiv1.Goober
		raw, err := os.ReadFile(filepath.Join(root, "goobers", name, "goober.yaml"))
		if err != nil {
			t.Fatalf("read %s goober: %v", name, err)
		}
		if err := yaml.Unmarshal(raw, &g); err != nil {
			t.Fatalf("unmarshal %s goober: %v", name, err)
		}
		goobers[g.Name] = g.Spec
	}

	for _, file := range []string{"implementation.yaml", "backlog-curation.yaml", "work-nomination.yaml", "tutor.yaml", "merge-review.yaml", "pr-remediation.yaml", "quality-sprint.yaml", "test-suite-quality.yaml"} {
		t.Run(file, func(t *testing.T) {
			raw, err := os.ReadFile(filepath.Join(root, "workflows", file))
			if err != nil {
				t.Fatalf("read %s: %v", file, err)
			}
			var w apiv1.Workflow
			if err := yaml.Unmarshal(raw, &w); err != nil {
				t.Fatalf("unmarshal %s: %v", file, err)
			}
			def := Definition{Name: w.Name, Version: 1, DSLVersion: w.DSLVersion, Spec: w.Spec}
			if _, err := compileAcknowledged(def, WithGoobers(goobers)); err != nil {
				t.Fatalf("compile %s against the real reference workflows' goobers: %v", file, err)
			}
			if file == "backlog-curation.yaml" {
				if warnings := CheckWarnings(def); len(warnings) != 0 {
					t.Fatalf("%s warnings = %v, want warning-clean reference config", file, warnings)
				}
			}
		})
	}
}

func TestReadOnlyWorkflowTasksDoNotMaterializeIssueWriter(t *testing.T) {
	root := filepath.Join("..", "..")
	cases := []struct {
		path            string
		task            string
		command         []string
		expectedOutputs []string
	}{
		{"reference-workflows/gaggles/goobers/workflows/backlog-curation.yaml", "sample-ready-pool", []string{"goobers", "backlog-health"}, []string{"backlog-health"}},
		{"reference-workflows/gaggles/goobers/workflows/backlog-curation.yaml", "surface-duplicates", []string{"goobers", "backlog-dedupe"}, []string{"dedupe-candidates"}},
		{"config-examples/gaggles/acme-web/workflows/backlog-curation.yaml", "sample-ready-pool", []string{"goobers", "backlog-health"}, []string{"backlog-health"}},
		{"config-examples/gaggles/acme-web/workflows/backlog-curation.yaml", "surface-duplicates", []string{"goobers", "backlog-dedupe"}, []string{"dedupe-candidates"}},
		{"config-examples/gaggles/acme-web-claude/workflows/backlog-curation.yaml", "sample-ready-pool", []string{"goobers", "backlog-health"}, []string{"backlog-health"}},
		{"config-examples/gaggles/acme-web-claude/workflows/backlog-curation.yaml", "surface-duplicates", []string{"goobers", "backlog-dedupe"}, []string{"dedupe-candidates"}},
		{"reference-workflows/gaggles/goobers/workflows/pr-remediation.yaml", "gather-pr-context", []string{"goobers", "gather-pr-context"}, []string{"selectedNumber", "head", "base", "isBehindBase", "hasSubstantiveFindings", "hasFailingCI", "workspaceBranch"}},
		{"reference-workflows/gaggles/goobers/workflows/pr-remediation.yaml", "gather-issue-context", []string{"goobers", "gather-issue-context"}, nil},
		{"reference-workflows/gaggles/goobers/workflows/pr-remediation.yaml", "validate-finding-responses", []string{"goobers", "respond-to-findings", "--check"}, nil},
	}

	t.Setenv("GOOBERS_TEST_ISSUE_WRITER", "writer-token-must-not-materialize")
	resolver, err := credentials.NewResolver([]credentials.TokenRef{{
		Name: "issue-writer",
		Env:  "GOOBERS_TEST_ISSUE_WRITER",
	}})
	if err != nil {
		t.Fatalf("build credential resolver: %v", err)
	}
	injector, err := credentials.NewInjector(resolver, []credentials.Grant{{
		Capability: string(capability.GitHubIssuesWrite),
		Ref:        "issue-writer",
	}}, journal.NewRegistryScrubber())
	if err != nil {
		t.Fatalf("build credential injector: %v", err)
	}

	for _, tc := range cases {
		t.Run(tc.task+"/"+tc.path, func(t *testing.T) {
			raw, err := os.ReadFile(filepath.Join(root, tc.path))
			if err != nil {
				t.Fatalf("read workflow: %v", err)
			}
			var workflow apiv1.Workflow
			if err := yaml.Unmarshal(raw, &workflow); err != nil {
				t.Fatalf("unmarshal workflow: %v", err)
			}
			var task *apiv1.Task
			for i := range workflow.Spec.Tasks {
				if workflow.Spec.Tasks[i].Name == tc.task {
					task = &workflow.Spec.Tasks[i]
					break
				}
			}
			if task == nil {
				t.Fatal("task not found")
			}
			if task.Run == nil || !slices.Equal(task.Run.Command, tc.command) {
				t.Errorf("command = %v, want %v", task.Run, tc.command)
			}
			if !slices.Equal(task.ExpectedOutputs, tc.expectedOutputs) {
				t.Errorf("expectedOutputs = %v, want %v", task.ExpectedOutputs, tc.expectedOutputs)
			}
			if containsString(task.Capabilities, string(capability.GitHubIssuesWrite)) {
				t.Errorf("capabilities = %v, must not include issue-write authority", task.Capabilities)
			}

			set, err := injector.Materialize(context.Background(), task.Capabilities)
			if err != nil {
				t.Fatalf("materialize task credentials: %v", err)
			}
			if _, err := set.Token(context.Background(), string(capability.GitHubIssuesWrite)); !errors.Is(err, credentials.ErrUndeclaredCapability) {
				t.Errorf("issue writer lookup error = %v, want ErrUndeclaredCapability", err)
			}
		})
	}
}

func TestReferenceImplementationHandlesProviderMutationsOnlyWithEvidence(t *testing.T) {
	root := filepath.Join("..", "..", "reference-workflows", "gaggles", "goobers")

	raw, err := os.ReadFile(filepath.Join(root, "workflows", "implementation.yaml"))
	if err != nil {
		t.Fatalf("read implementation workflow: %v", err)
	}
	var workflow apiv1.Workflow
	if err := yaml.Unmarshal(raw, &workflow); err != nil {
		t.Fatalf("unmarshal implementation workflow: %v", err)
	}
	foundImplement := false
	for _, task := range workflow.Spec.Tasks {
		if task.Name == "implement" {
			foundImplement = true
			if !strings.Contains(task.Goal, "only when attached context proves") ||
				!strings.Contains(task.Goal, "PROVIDER_ACTION_REQUIRED") ||
				!strings.Contains(task.Goal, "rather than attempting the mutation") {
				t.Fatalf("implement goal does not require evidence or an explicit provider-action failure: %q", task.Goal)
			}
			break
		}
	}
	if !foundImplement {
		t.Fatal("implement task not found")
	}

	raw, err = os.ReadFile(filepath.Join(root, "goobers", "implementer", "instructions.md"))
	if err != nil {
		t.Fatalf("read implementer instructions: %v", err)
	}
	instructions := strings.Join(strings.Fields(string(raw)), " ")
	if !strings.Contains(instructions, "attached context explicitly proves") ||
		!strings.Contains(instructions, "`error.code: PROVIDER_ACTION_REQUIRED`") ||
		!strings.Contains(instructions, "never assume or silently claim") {
		t.Fatalf("implementer instructions do not require proof or explicitly fail outstanding provider mutations")
	}
}

func TestReferenceReviewerUsesFindingsAsCompleteBlockerLedger(t *testing.T) {
	path := filepath.Join(
		"..", "..", "reference-workflows", "gaggles", "goobers",
		"goobers", "reviewer", "instructions.md",
	)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read reviewer instructions: %v", err)
	}
	instructions := strings.Join(strings.Fields(string(raw)), " ")
	for _, required := range []string{
		"Structured findings are the complete blocker ledger.",
		"every distinct condition you describe as blocking readiness MUST have a corresponding entry in `findings`",
		"Never leave a blocker only in prose.",
		"do not add a proxy finding or describe failing CI as a verdict blocker",
		"requires exactly one response per entry",
	} {
		if !strings.Contains(instructions, required) {
			t.Errorf("reviewer instructions omit verdict completeness contract %q", required)
		}
	}
}

func TestReferenceWorkflowsCuratorDeclaresRoadmapMutation(t *testing.T) {
	root := filepath.Join("..", "..", "reference-workflows", "gaggles", "goobers")

	var curator apiv1.Goober
	raw, err := os.ReadFile(filepath.Join(root, "goobers", "curator", "goober.yaml"))
	if err != nil {
		t.Fatalf("read curator goober: %v", err)
	}
	if err := yaml.Unmarshal(raw, &curator); err != nil {
		t.Fatalf("unmarshal curator goober: %v", err)
	}
	if !containsString(curator.Spec.Capabilities, "github:milestones:write") {
		t.Errorf("curator capabilities = %v, want github:milestones:write", curator.Spec.Capabilities)
	}
	if !containsString(curator.Spec.PolicyActions, "assign-milestone") {
		t.Errorf("curator policyActions = %v, want assign-milestone", curator.Spec.PolicyActions)
	}

	var curation apiv1.Workflow
	raw, err = os.ReadFile(filepath.Join(root, "workflows", "backlog-curation.yaml"))
	if err != nil {
		t.Fatalf("read backlog-curation workflow: %v", err)
	}
	if err := yaml.Unmarshal(raw, &curation); err != nil {
		t.Fatalf("unmarshal backlog-curation workflow: %v", err)
	}
	for _, task := range curation.Spec.Tasks {
		if task.Name != "curate" {
			continue
		}
		if !containsString(task.Capabilities, "github:milestones:write") {
			t.Errorf("curate capabilities = %v, want github:milestones:write", task.Capabilities)
		}
		if !containsString(task.PolicyActions, "assign-milestone") {
			t.Errorf("curate policyActions = %v, want assign-milestone", task.PolicyActions)
		}
		if !strings.Contains(task.Goal, "roadmap maintenance on directly linked tracking parents.") ||
			strings.Contains(task.Goal, "tracking parents and children") {
			t.Errorf("curate goal grants roadmap maintenance outside directly linked tracking parents: %q", task.Goal)
		}
		return
	}
	t.Fatal("curate task not found")
}

func TestReferenceWorkflowsPolicyActionAuditCoversDeclaredVocabulary(t *testing.T) {
	root := filepath.Join("..", "..", "reference-workflows", "gaggles", "goobers")
	actions := map[string]bool{}

	for _, name := range []string{"implementer", "reviewer", "curator", "nominator", "analyst", "config-author", "quality-researcher", "quality-lead", "test-quality-analyst"} {
		var goober apiv1.Goober
		raw, err := os.ReadFile(filepath.Join(root, "goobers", name, "goober.yaml"))
		if err != nil {
			t.Fatalf("read %s goober: %v", name, err)
		}
		if err := yaml.Unmarshal(raw, &goober); err != nil {
			t.Fatalf("unmarshal %s goober: %v", name, err)
		}
		for _, action := range append(goober.Spec.PolicyActions, goober.Spec.ConditionalPolicyActions...) {
			actions[action] = true
		}
	}

	for _, file := range []string{"implementation.yaml", "backlog-curation.yaml", "work-nomination.yaml", "tutor.yaml", "merge-review.yaml", "pr-remediation.yaml", "quality-sprint.yaml", "test-suite-quality.yaml"} {
		var workflow apiv1.Workflow
		raw, err := os.ReadFile(filepath.Join(root, "workflows", file))
		if err != nil {
			t.Fatalf("read %s: %v", file, err)
		}
		if err := yaml.Unmarshal(raw, &workflow); err != nil {
			t.Fatalf("unmarshal %s: %v", file, err)
		}
		for _, task := range workflow.Spec.Tasks {
			for _, action := range task.PolicyActions {
				actions[action] = true
			}
		}
	}

	audit, err := os.ReadFile(filepath.Join("..", "..", "docs", "requirements", "pr-lifecycle.md"))
	if err != nil {
		t.Fatalf("read policy audit: %v", err)
	}
	var missing []string
	for action := range actions {
		if !strings.Contains(string(audit), "`"+action+"`") {
			missing = append(missing, action)
		}
	}
	sort.Strings(missing)
	if len(missing) != 0 {
		t.Fatalf("capability-vs-policy audit omits declared reference-workflows actions: %v", missing)
	}
}

func TestReferenceWorkflowsRemediationRejectsOmittedPersonaActions(t *testing.T) {
	root := filepath.Join("..", "..", "reference-workflows", "gaggles", "goobers")

	var implementer apiv1.Goober
	raw, err := os.ReadFile(filepath.Join(root, "goobers", "implementer", "goober.yaml"))
	if err != nil {
		t.Fatalf("read implementer goober: %v", err)
	}
	if err := yaml.Unmarshal(raw, &implementer); err != nil {
		t.Fatalf("unmarshal implementer goober: %v", err)
	}

	var remediation apiv1.Workflow
	raw, err = os.ReadFile(filepath.Join(root, "workflows", "pr-remediation.yaml"))
	if err != nil {
		t.Fatalf("read pr-remediation workflow: %v", err)
	}
	if err := yaml.Unmarshal(raw, &remediation); err != nil {
		t.Fatalf("unmarshal pr-remediation workflow: %v", err)
	}
	for index := range remediation.Spec.Tasks {
		if remediation.Spec.Tasks[index].Name == "implement" {
			remediation.Spec.Tasks[index].PolicyActions = nil
			break
		}
	}

	_, err = compileAcknowledged(
		Definition{Name: remediation.Name, Version: 1, Spec: remediation.Spec},
		WithGoobers(map[string]apiv1.GooberSpec{implementer.Name: implementer.Spec}),
	)
	const want = `task "implement" invokes goober "implementer" whose persona prescribes policy action "modify-repository", but policyActions does not declare it`
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("compile error = %v, want containing %q", err, want)
	}
}

func TestReferenceWorkflowsTelemetryQueriesDeclareResultFile(t *testing.T) {
	root := filepath.Join("..", "..", "reference-workflows", "gaggles", "goobers", "workflows")
	tests := []struct {
		file, task, resultFile string
	}{
		{"work-nomination.yaml", "gather-signals", "candidate-findings.json"},
		{"tutor.yaml", "gather-signals", "telemetry-signals.json"},
		{"test-suite-quality.yaml", "gather-recurring-failures", "recurring-failures.json"},
	}
	for _, tc := range tests {
		t.Run(tc.file, func(t *testing.T) {
			raw, err := os.ReadFile(filepath.Join(root, tc.file))
			if err != nil {
				t.Fatalf("read %s: %v", tc.file, err)
			}
			var w apiv1.Workflow
			if err := yaml.Unmarshal(raw, &w); err != nil {
				t.Fatalf("unmarshal %s: %v", tc.file, err)
			}
			for _, task := range w.Spec.Tasks {
				if task.Name == tc.task {
					if got := task.Inputs["resultFile"]; got != tc.resultFile {
						t.Fatalf("%s resultFile = %q, want %s", tc.task, got, tc.resultFile)
					}
					return
				}
			}
			t.Fatalf("%s task not found", tc.task)
		})
	}
}

func TestReferenceTestSuiteQualityUsesRecurringEvidenceBeforeNomination(t *testing.T) {
	path := filepath.Join("..", "..", "reference-workflows", "gaggles", "goobers", "workflows", "test-suite-quality.yaml")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var workflow apiv1.Workflow
	if err := yaml.Unmarshal(raw, &workflow); err != nil {
		t.Fatal(err)
	}
	tasks := make(map[string]apiv1.Task)
	for _, task := range workflow.Spec.Tasks {
		tasks[task.Name] = task
	}

	gather := tasks["gather-recurring-failures"]
	wantCommand := []string{
		"goobers", "telemetry-query", "--window", "168h",
		"--aggregate", "ci-check-failure",
		"--threshold", "min-ci-check-failure-runs=2",
		"--format", "candidate-findings",
	}
	if gather.Run == nil || !slices.Equal(gather.Run.Command, wantCommand) ||
		gather.Next != "classify-flakes" ||
		!containsString(gather.Capabilities, string(capability.TelemetryRead)) {
		t.Fatalf("gather-recurring-failures = %+v, want bounded recurring-error telemetry query", gather)
	}

	classify := tasks["classify-flakes"]
	if classify.Goober != "test-quality-analyst" ||
		classify.Next != "nominate" ||
		!containsString(classify.Capabilities, string(capability.JournalRead)) ||
		containsString(classify.Capabilities, string(capability.GitHubIssuesWrite)) {
		t.Fatalf("classify-flakes = %+v, want read-only journal-backed analyst", classify)
	}

	nominate := tasks["nominate"]
	if nominate.InputsFrom["candidateFindings"] != "findingsRef" ||
		!containsString(nominate.Capabilities, string(capability.GitHubIssuesWrite)) ||
		nominate.Next != "" {
		t.Fatalf("nominate = %+v, want terminal issue-only proposal stage", nominate)
	}
}

func TestReferenceWorkflowsImplementationCIPollDeclaresRequiredCapability(t *testing.T) {
	path := filepath.Join("..", "..", "reference-workflows", "gaggles", "goobers", "workflows", "implementation.yaml")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read implementation workflow: %v", err)
	}
	var w apiv1.Workflow
	if err := yaml.Unmarshal(raw, &w); err != nil {
		t.Fatalf("unmarshal implementation workflow: %v", err)
	}
	for _, task := range w.Spec.Tasks {
		if task.Inputs["kind"] != "ci-poll" {
			continue
		}
		for _, declared := range task.Capabilities {
			if declared == string(capability.ProviderPRWrite) {
				return
			}
		}
		t.Fatalf("ci-poll task %q capabilities = %v, want %s", task.Name, task.Capabilities, capability.ProviderPRWrite)
	}
	t.Fatal("implementation workflow has no inputs.kind=ci-poll task")
}

func TestReferenceWorkflowsImplementationCheckpointsBeforeStrictIntegration(t *testing.T) {
	path := filepath.Join("..", "..", "reference-workflows", "gaggles", "goobers", "workflows", "implementation.yaml")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read implementation workflow: %v", err)
	}
	var w apiv1.Workflow
	if err := yaml.Unmarshal(raw, &w); err != nil {
		t.Fatalf("unmarshal implementation workflow: %v", err)
	}
	var localCI *apiv1.Task
	for i := range w.Spec.Tasks {
		task := &w.Spec.Tasks[i]
		if task.Name != "local-ci" {
			continue
		}
		localCI = task
		break
	}
	if localCI == nil {
		t.Fatal("implementation workflow has no local-ci task")
	}
	wantCommand := []string{"make", "ci", "test-integration-strict"}
	if localCI.Run == nil || !slices.Equal(localCI.Run.Command, wantCommand) {
		t.Fatalf("local-ci command = %v, want %v", localCI.Run, wantCommand)
	}
	if !localCI.Run.SyncBase {
		t.Fatal("local-ci syncBase = false, want true")
	}
	// #3377: 2400s (40m), not the prior 1500s (25m) — `make ci` shells out to
	// `go test -race -timeout 30m`, so the stage budget must clear that inner
	// subprocess ceiling, not just typical-case duration.
	if localCI.TimeoutSeconds != 2400 {
		t.Fatalf("local-ci timeoutSeconds = %d, want 2400", localCI.TimeoutSeconds)
	}
	if localCI.Retry == nil || localCI.Retry.MaxAttempts != 1 {
		t.Fatalf("local-ci retry = %+v, want maxAttempts 1", localCI.Retry)
	}
	if localCI.Next != "local-gate" {
		t.Fatalf("local-ci next = %q, want local-gate", localCI.Next)
	}
	var pushBranch *apiv1.Task
	for i := range w.Spec.Tasks {
		if w.Spec.Tasks[i].Name == "push-branch" {
			pushBranch = &w.Spec.Tasks[i]
			break
		}
	}
	if pushBranch == nil || pushBranch.Next != "local-ci" {
		t.Fatalf("push-branch = %+v, want next local-ci", pushBranch)
	}
	for _, workflowGate := range w.Spec.Gates {
		if workflowGate.Name == localCI.Next {
			if got := workflowGate.Branches["pass"]; got != "open-pr" {
				t.Fatalf("local-gate pass branch = %q, want open-pr", got)
			}
			if got := workflowGate.Branches["infra"]; got != "local-ci" {
				t.Fatalf("local-gate infra branch = %q, want local-ci", got)
			}
			return
		}
	}
	t.Fatal("implementation workflow has no local-gate")
}

// TestReferenceWorkflowsAgentModelDeclarations guards model-token admission for every
// shipped agentic task. The reviewer is an agentic gate with no stage-level
// capabilities field, so its grant remains sourced from reviewer/goober.yaml.
func TestReferenceWorkflowsAgentModelDeclarations(t *testing.T) {
	root := filepath.Join("..", "..", "reference-workflows", "gaggles", "goobers")

	taskCaps := func(t *testing.T, file, task string) []string {
		t.Helper()
		raw, err := os.ReadFile(filepath.Join(root, "workflows", file))
		if err != nil {
			t.Fatalf("read %s: %v", file, err)
		}
		var w apiv1.Workflow
		if err := yaml.Unmarshal(raw, &w); err != nil {
			t.Fatalf("unmarshal %s: %v", file, err)
		}
		for _, ta := range w.Spec.Tasks {
			if ta.Name == task {
				return ta.Capabilities
			}
		}
		t.Fatalf("%s: task %q not found", file, task)
		return nil
	}
	gooberCaps := func(t *testing.T, name string) []string {
		t.Helper()
		raw, err := os.ReadFile(filepath.Join(root, "goobers", name, "goober.yaml"))
		if err != nil {
			t.Fatalf("read %s goober: %v", name, err)
		}
		var g apiv1.Goober
		if err := yaml.Unmarshal(raw, &g); err != nil {
			t.Fatalf("unmarshal %s goober: %v", name, err)
		}
		return g.Spec.Capabilities
	}
	has := func(caps []string, want string) bool {
		for _, c := range caps {
			if c == want {
				return true
			}
		}
		return false
	}

	// Each agentic task declares agent:model alongside its existing grants.
	for _, tc := range []struct {
		file, task string
		alsoNeeds  string // a pre-existing capability the addition must not drop
	}{
		{"backlog-curation.yaml", "curate", "github:issues:write"},
		{"implementation.yaml", "implement", "repo:push"},
		{"work-nomination.yaml", "nominate", "github:issues:write"},
		{"tutor.yaml", "analyze", "journal:read"},
		{"tutor.yaml", "draft-change", "repo:push"},
	} {
		caps := taskCaps(t, tc.file, tc.task)
		if !has(caps, "agent:model") {
			t.Errorf("%s/%s: expected agent:model in %v", tc.file, tc.task, caps)
		}
		if !has(caps, tc.alsoNeeds) {
			t.Errorf("%s/%s: agent:model must not drop %q (got %v)", tc.file, tc.task, tc.alsoNeeds, caps)
		}
	}

	for _, tc := range []struct {
		name, alsoNeeds string
	}{
		{"analyst", "journal:read"},
		{"config-author", "repo:push"},
	} {
		caps := gooberCaps(t, tc.name)
		if !has(caps, "agent:model") {
			t.Errorf("%s goober: expected agent:model in %v", tc.name, caps)
		}
		if !has(caps, tc.alsoNeeds) {
			t.Errorf("%s goober: agent:model must not drop %q (got %v)", tc.name, tc.alsoNeeds, caps)
		}
	}
}

func TestReferenceWorkflowsTutorValidatesBeforePush(t *testing.T) {
	path := filepath.Join("..", "..", "reference-workflows", "gaggles", "goobers", "workflows", "tutor.yaml")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var tutor apiv1.Workflow
	if err := yaml.Unmarshal(raw, &tutor); err != nil {
		t.Fatal(err)
	}

	tasks := make(map[string]apiv1.Task, len(tutor.Spec.Tasks))
	for _, task := range tutor.Spec.Tasks {
		tasks[task.Name] = task
	}
	if got := tasks["draft-change"].Next; got != "gate-removal-guard" {
		t.Fatalf("draft-change next = %q, want gate-removal-guard", got)
	}
	guardTask, ok := tasks["gate-removal-guard"]
	if !ok {
		t.Fatal("tutor workflow has no gate-removal-guard task")
	}
	if guardTask.Type != apiv1.TaskDeterministic {
		t.Fatalf("gate-removal-guard type = %q, want deterministic", guardTask.Type)
	}
	if guardTask.Run == nil || len(guardTask.Run.Command) != 2 ||
		guardTask.Run.Command[0] != "goobers" || guardTask.Run.Command[1] != "gate-removal-guard" {
		t.Fatalf("gate-removal-guard run = %+v, want the gate-removal-guard command", guardTask.Run)
	}
	if guardTask.Next != "gate-removal-clear" {
		t.Fatalf("gate-removal-guard next = %q, want gate-removal-clear", guardTask.Next)
	}

	var sawGateRemovalClear bool
	for _, gate := range tutor.Spec.Gates {
		if gate.Name != "gate-removal-clear" {
			continue
		}
		sawGateRemovalClear = true
		if gate.Evaluator != apiv1.EvaluatorAutomated || gate.Automated == nil || gate.Automated.Check != "status-equals" {
			t.Fatalf("gate-removal-clear evaluator = %+v, want automated status-equals", gate)
		}
		if gate.Branches["pass"] != "validate-config" || gate.Branches["fail"] != "@abort" {
			t.Fatalf("gate-removal-clear branches = %v, want pass->validate-config and fail->@abort", gate.Branches)
		}
	}
	if !sawGateRemovalClear {
		t.Fatal("tutor workflow has no gate-removal-clear gate")
	}

	validateTask, ok := tasks["validate-config"]
	if !ok {
		t.Fatal("tutor workflow has no validate-config task")
	}
	if validateTask.Type != apiv1.TaskDeterministic {
		t.Fatalf("validate-config type = %q, want deterministic", validateTask.Type)
	}
	if validateTask.Run == nil ||
		len(validateTask.Run.Command) != 4 ||
		validateTask.Run.Command[0] != "goobers" ||
		validateTask.Run.Command[1] != "validate" ||
		validateTask.Run.Command[2] != "--source-tree" ||
		validateTask.Run.Command[3] != "reference-workflows" {
		t.Fatalf("validate-config run = %+v, want direct reference-workflows source-tree validation", validateTask.Run)
	}
	if validateTask.Next != "config-valid" {
		t.Fatalf("validate-config next = %q, want config-valid", validateTask.Next)
	}

	gates := make(map[string]apiv1.Gate, len(tutor.Spec.Gates))
	for _, gate := range tutor.Spec.Gates {
		gates[gate.Name] = gate
	}
	configValid, ok := gates["config-valid"]
	if !ok {
		t.Fatal("tutor workflow has no config-valid gate")
	}
	if configValid.Evaluator != apiv1.EvaluatorAutomated || configValid.Automated == nil || configValid.Automated.Check != "status-equals" {
		t.Fatalf("config-valid evaluator = %+v, want automated status-equals", configValid)
	}
	if configValid.Branches["pass"] != "check-fail-first" || configValid.Branches["fail"] != "@abort" {
		t.Fatalf("config-valid branches = %v, want pass->check-fail-first and fail->@abort", configValid.Branches)
	}
}

// TestReferenceWorkflowsTutorEnforcesFailFirst is TUT-A2's (#1214) config-side
// counterpart to TestReferenceWorkflowsTutorValidatesBeforePush: the tutor workflow must
// mechanically gate on `goobers check-fail-first` between config-valid and
// push-branch, not merely document the fail-first contract in prose.
func TestReferenceWorkflowsTutorEnforcesFailFirst(t *testing.T) {
	path := filepath.Join("..", "..", "reference-workflows", "gaggles", "goobers", "workflows", "tutor.yaml")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var tutor apiv1.Workflow
	if err := yaml.Unmarshal(raw, &tutor); err != nil {
		t.Fatal(err)
	}

	tasks := make(map[string]apiv1.Task, len(tutor.Spec.Tasks))
	for _, task := range tutor.Spec.Tasks {
		tasks[task.Name] = task
	}
	checkTask, ok := tasks["check-fail-first"]
	if !ok {
		t.Fatal("tutor workflow has no check-fail-first task")
	}
	if checkTask.Type != apiv1.TaskDeterministic {
		t.Fatalf("check-fail-first type = %q, want deterministic", checkTask.Type)
	}
	if checkTask.Run == nil || len(checkTask.Run.Command) != 2 ||
		checkTask.Run.Command[0] != "goobers" || checkTask.Run.Command[1] != "check-fail-first" {
		t.Fatalf("check-fail-first run = %+v, want [\"goobers\", \"check-fail-first\"]", checkTask.Run)
	}
	if checkTask.Next != "fail-first-valid" {
		t.Fatalf("check-fail-first next = %q, want fail-first-valid", checkTask.Next)
	}

	gates := make(map[string]apiv1.Gate, len(tutor.Spec.Gates))
	for _, gate := range tutor.Spec.Gates {
		gates[gate.Name] = gate
	}
	failFirstValid, ok := gates["fail-first-valid"]
	if !ok {
		t.Fatal("tutor workflow has no fail-first-valid gate")
	}
	if failFirstValid.Evaluator != apiv1.EvaluatorAutomated || failFirstValid.Automated == nil || failFirstValid.Automated.Check != "status-equals" {
		t.Fatalf("fail-first-valid evaluator = %+v, want automated status-equals", failFirstValid)
	}
	if failFirstValid.Branches["pass"] != "push-branch" || failFirstValid.Branches["fail"] != "@abort" {
		t.Fatalf("fail-first-valid branches = %v, want pass->push-branch and fail->@abort", failFirstValid.Branches)
	}
}

// TestReferenceWorkflowsTutorDeclaresPerGaggleScopeAndConfinesWrites is TUT-A4's
// contract guard: the tutor's topology tier must be explicit in the
// workflow definition, and its write boundary must be scoped to this
// gaggle's own config subtree, not the whole (potentially multi-gaggle)
// reference-workflows instance config — the hard silo, applied to the one shipped
// tutor definition. TUT-A5/#1217 widened the single configRoot boundary to
// the per-target-action-root boundary (confineToActionRoots/actionRoots,
// exclusive across roots) so the tutor can also author skill bodies; this
// still must include the gaggle-scoped config root, not the whole reference-workflows
// instance config.
func TestReferenceWorkflowsTutorDeclaresPerGaggleScopeAndConfinesWrites(t *testing.T) {
	path := filepath.Join("..", "..", "reference-workflows", "gaggles", "goobers", "workflows", "tutor.yaml")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	var tutor apiv1.Workflow
	if err := yaml.Unmarshal(raw, &tutor); err != nil {
		t.Fatal(err)
	}

	if tutor.Spec.TutorScope == nil {
		t.Fatal("tutor workflow has no spec.tutorScope; per-workflow vs per-gaggle scope must be explicit (TUT-A4)")
	}
	if tutor.Spec.TutorScope.Tier != apiv1.TutorScopePerGaggle {
		t.Fatalf("tutorScope.tier = %q, want %q", tutor.Spec.TutorScope.Tier, apiv1.TutorScopePerGaggle)
	}
	if tutor.Spec.TutorScope.Target != "" {
		t.Fatalf("tutorScope.target = %q, want empty for tier %q", tutor.Spec.TutorScope.Target, apiv1.TutorScopePerGaggle)
	}

	for _, task := range tutor.Spec.Tasks {
		if task.Name != "open-pr" {
			continue
		}
		if task.Inputs["confineToActionRoots"] != "true" {
			t.Fatalf("open-pr confineToActionRoots = %q, want %q", task.Inputs["confineToActionRoots"], "true")
		}
		wantRoot := "reference-workflows/gaggles/" + tutor.Spec.Gaggle
		found := false
		for _, root := range strings.Split(task.Inputs["actionRoots"], ",") {
			if strings.TrimSpace(root) == wantRoot {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("open-pr actionRoots = %q, want it to include %q (gaggle-scoped, not the whole reference-workflows instance config)", task.Inputs["actionRoots"], wantRoot)
		}
		return
	}
	t.Fatal("tutor workflow has no open-pr task")
}

func TestReferenceWorkflowsTutorRunsLiveVerificationBeforeNewFindings(t *testing.T) {
	path := filepath.Join("..", "..", "reference-workflows", "gaggles", "goobers", "workflows", "tutor.yaml")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	var tutor apiv1.Workflow
	if err := yaml.Unmarshal(raw, &tutor); err != nil {
		t.Fatal(err)
	}
	if tutor.Spec.Start != "verify-live-holdouts" {
		t.Fatalf("start = %q, want verify-live-holdouts", tutor.Spec.Start)
	}

	tasks := make(map[string]apiv1.Task, len(tutor.Spec.Tasks))
	for _, task := range tutor.Spec.Tasks {
		tasks[task.Name] = task
	}
	verify := tasks["verify-live-holdouts"]
	if verify.Run == nil || strings.Join(verify.Run.Command, " ") !=
		"goobers telemetry-query --format tutor-live-verification --window 168h" {
		t.Fatalf("verify-live-holdouts run = %+v", verify.Run)
	}
	if verify.Inputs["resultFile"] != "tutor-live-verification.json" || verify.Next != "gather-signals" {
		t.Fatalf("verify-live-holdouts = %+v", verify)
	}
	if !slices.Contains(verify.Capabilities, "github:pr:write") {
		t.Fatalf("verify-live-holdouts capabilities = %v, want GitHub PR polling grant", verify.Capabilities)
	}
	openPR := tasks["open-pr"]
	if openPR.Inputs["recordLiveVerification"] != "true" || openPR.Inputs["tutorConfigSource"] != "reference-workflows" {
		t.Fatalf("open-pr live-verification inputs = %v", openPR.Inputs)
	}
}

func TestReferenceWorkflowsRouteDurableLearningThroughGovernedActions(t *testing.T) {
	root := filepath.Join("..", "..", "reference-workflows", "gaggles", "goobers")
	load := func(name string) apiv1.Workflow {
		t.Helper()
		raw, err := os.ReadFile(filepath.Join(root, "workflows", name))
		if err != nil {
			t.Fatal(err)
		}
		var got apiv1.Workflow
		if err := yaml.Unmarshal(raw, &got); err != nil {
			t.Fatal(err)
		}
		return got
	}
	tasks := func(workflow apiv1.Workflow) map[string]apiv1.Task {
		result := make(map[string]apiv1.Task, len(workflow.Spec.Tasks))
		for _, task := range workflow.Spec.Tasks {
			result[task.Name] = task
		}
		return result
	}

	tutorTasks := tasks(load("tutor.yaml"))
	gather := tutorTasks["gather-signals"]
	command := strings.Join(gather.Run.Command, " ")
	for _, action := range []string{
		"--learning-action instruction-or-skill",
		"--learning-action workflow-or-gate",
		"--learning-action targeted-test-mapping",
	} {
		if !strings.Contains(command, action) {
			t.Fatalf("Tutor gather command %q missing %q", command, action)
		}
	}
	if strings.Contains(command, "code-issue") {
		t.Fatalf("Tutor gather command routes code defects to PR authoring: %q", command)
	}
	openPR := tutorTasks["open-pr"]
	if openPR.Inputs["confineToActionRoots"] != "true" ||
		openPR.Inputs["recordLiveVerification"] != "true" ||
		!strings.Contains(openPR.Inputs["actionRoots"], "skills") {
		t.Fatalf("Tutor open-pr governance inputs = %v", openPR.Inputs)
	}
	for _, task := range tutorTasks {
		if slices.Contains(task.Capabilities, "github:issues:approve") ||
			slices.Contains(task.PolicyActions, "approve-issue") ||
			slices.Contains(task.PolicyActions, "merge-pull-request") {
			t.Fatalf("Tutor task %q can approve or merge: %+v", task.Name, task)
		}
	}

	nominationTasks := tasks(load("work-nomination.yaml"))
	nominationGather := strings.Join(nominationTasks["gather-signals"].Run.Command, " ")
	if !strings.Contains(nominationGather, "--aggregate learning-episode") ||
		!strings.Contains(nominationGather, "--learning-action code-issue") {
		t.Fatalf("work-nomination learning route = %q", nominationGather)
	}
	nominate := nominationTasks["nominate"]
	if slices.Contains(nominate.Capabilities, "github:issues:approve") ||
		slices.Contains(nominate.PolicyActions, "approve-issue") {
		t.Fatalf("code-defect nomination can self-approve: %+v", nominate)
	}

	configAuthor, err := os.ReadFile(filepath.Join(root, "goobers", "config-author", "instructions.md"))
	if err != nil {
		t.Fatal(err)
	}
	nominator, err := os.ReadFile(filepath.Join(root, "goobers", "nominator", "instructions.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(configAuthor), "Never update model weights") ||
		!strings.Contains(string(configAuthor), "cannot approve or merge") {
		t.Fatalf("config-author lacks learning governance contract")
	}
	if !strings.Contains(string(nominator), "always remains unapproved") ||
		!strings.Contains(string(nominator), "`code-defect`") {
		t.Fatalf("nominator lacks unapproved code-defect contract")
	}
}
