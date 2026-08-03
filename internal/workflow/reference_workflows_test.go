package workflow

import (
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

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
	for _, name := range []string{"implementer", "reviewer", "curator", "nominator", "analyst", "config-author", "quality-researcher", "quality-lead"} {
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

	for _, file := range []string{"implementation.yaml", "backlog-curation.yaml", "work-nomination.yaml", "tutor.yaml", "merge-review.yaml", "pr-remediation.yaml", "quality-sprint.yaml"} {
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

	for _, name := range []string{"implementer", "reviewer", "curator", "nominator", "analyst", "config-author", "quality-researcher", "quality-lead"} {
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

	for _, file := range []string{"implementation.yaml", "backlog-curation.yaml", "work-nomination.yaml", "tutor.yaml", "merge-review.yaml", "pr-remediation.yaml", "quality-sprint.yaml"} {
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
	for _, file := range []string{"work-nomination.yaml", "tutor.yaml"} {
		t.Run(file, func(t *testing.T) {
			wantResultFile := "telemetry-signals.json"
			if file == "work-nomination.yaml" {
				wantResultFile = "candidate-findings.json"
			}
			raw, err := os.ReadFile(filepath.Join(root, file))
			if err != nil {
				t.Fatalf("read %s: %v", file, err)
			}
			var w apiv1.Workflow
			if err := yaml.Unmarshal(raw, &w); err != nil {
				t.Fatalf("unmarshal %s: %v", file, err)
			}
			for _, task := range w.Spec.Tasks {
				if task.Name == "gather-signals" {
					if got := task.Inputs["resultFile"]; got != wantResultFile {
						t.Fatalf("gather-signals resultFile = %q, want %s", got, wantResultFile)
					}
					return
				}
			}
			t.Fatal("gather-signals task not found")
		})
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
			if declared == "github:pr:write" {
				return
			}
		}
		t.Fatalf("ci-poll task %q capabilities = %v, want github:pr:write", task.Name, task.Capabilities)
	}
	t.Fatal("implementation workflow has no inputs.kind=ci-poll task")
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
