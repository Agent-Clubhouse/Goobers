package workflowsafety

import (
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/providerstage"
	"github.com/goobers/goobers/internal/runcontrol"
	wf "github.com/goobers/goobers/internal/workflow"
)

func shell(name, next string, command ...string) apiv1.Task {
	return apiv1.Task{Name: name, Type: apiv1.TaskDeterministic, Goal: "Fixture", Next: next,
		Run: &apiv1.DeterministicRun{Command: command}}
}

func reviewDefinition() wf.Definition {
	return wf.Definition{Name: "test", Version: 1, DSLVersion: "2.0",
		Spec: apiv1.WorkflowSpec{Gaggle: "example", Start: "implement",
			Tasks: []apiv1.Task{
				{Name: "implement", Type: apiv1.TaskAgentic, Goober: "worker", Goal: "Change", Next: "check"},
				shell("check", "review", "git", "diff", "--check"),
			},
			Gates: []apiv1.Gate{{Name: "review", Evaluator: apiv1.EvaluatorAgentic,
				Agentic:  &apiv1.AgenticGate{Goober: "worker"},
				Branches: map[string]string{"pass": "", "needs-changes": "implement", "fail": wf.TargetAbort}}},
		}}
}

func annotate(t testing.TB, d *wf.Definition, c Contracts) {
	t.Helper()
	c.Version = 1
	raw, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	if d.Annotations == nil {
		d.Annotations = map[string]string{}
	}
	d.Annotations[Annotation] = string(raw)
}

func compile(t testing.TB, d wf.Definition) *wf.Machine {
	t.Helper()
	m, err := wf.Compile(d, wf.WithPreviewFeatures(true), wf.WithGoobers(map[string]apiv1.GooberSpec{
		"worker": {Gaggle: "example", Harness: "copilot", Instructions: "unused.md"},
	}))
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func findingsFor(findings []Finding, code string) []Finding {
	return slices.DeleteFunc(slices.Clone(findings), func(f Finding) bool { return f.Code != code })
}

func assertFinding(t testing.TB, m *wf.Machine, code string, want bool) []Finding {
	t.Helper()
	findings := Analyze(m, Options{})
	got := findingsFor(findings, code)
	if (len(got) > 0) != want {
		t.Fatalf("%s present=%v, want %v; findings=%+v", code, len(got) > 0, want, findings)
	}
	for _, f := range got {
		if f.Details.Action == "" || f.Details.Impact == "" || len(f.Details.WitnessPath) == 0 ||
			!strings.Contains(f.Message(), "Advisory only") {
			t.Fatalf("incomplete diagnostic: %+v", f)
		}
	}
	return got
}

func TestSafetyEvidenceRoutes(t *testing.T) {
	for _, tc := range []struct {
		name, profile string
		workspace     apiv1.WorkspaceMode
		command       []string
		want          bool
	}{
		{"implicit default", "", "", []string{"git", "diff", "--check"}, false},
		{"implicit explicit repo", "", apiv1.WorkspaceRepo, []string{"git", "diff", "--check"}, false},
		{"empty check scratch", "", apiv1.WorkspaceScratch, []string{"git", "diff", "--check"}, true},
		{"base-only uncertain", "", apiv1.WorkspaceRepoReadOnly, []string{"git", "diff", "--check"}, true},
		{"explicit alternate producer", "", apiv1.WorkspaceScratch, []string{"git", "diff", "main...HEAD"}, false},
		{"explicit readonly evidence", "", apiv1.WorkspaceRepoReadOnly, []string{"git", "diff", "main...HEAD"}, false},
		{"self comparison is empty", "", apiv1.WorkspaceScratch, []string{"git", "diff", "HEAD...HEAD"}, true},
		{"internal non-code review", "internal", apiv1.WorkspaceScratch, []string{"git", "diff", "--check"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := reviewDefinition()
			d.Spec.Gates[0].Agentic.Workspace = tc.workspace
			d.Spec.Tasks[1].Run.Command = tc.command
			if tc.profile != "" {
				annotate(t, &d, Contracts{Stages: map[string]StageContract{"review": {Review: tc.profile}}})
			}
			assertFinding(t, compile(t, d), EvidenceCode, tc.want)
		})
	}
	d := reviewDefinition()
	d.Spec.Tasks[0].Next = "review"
	d.Spec.Tasks = d.Spec.Tasks[:1]
	assertFinding(t, compile(t, d), EvidenceCode, false)
	// Writable agentic planning is not, by itself, a recognized code role.
	d.Spec.Gates[0].Agentic.Workspace = apiv1.WorkspaceScratch
	assertFinding(t, compile(t, d), EvidenceCode, false)
	annotate(t, &d, Contracts{Stages: map[string]StageContract{"review": {Review: "code"}}})
	assertFinding(t, compile(t, d), EvidenceCode, true)
}

func publicationDefinition(t testing.TB) wf.Definition {
	d := reviewDefinition()
	d.Spec.Tasks = append(d.Spec.Tasks, shell("publish", wf.TargetEscalate, "custom-publisher"))
	d.Spec.Gates[0].Branches = map[string]string{"pass": "", "needs-changes": "publish", "fail": wf.TargetAbort}
	annotate(t, &d, Contracts{Stages: map[string]StageContract{
		"review": {Review: "pr"}, "publish": {Publishes: "review"},
	}})
	return d
}

func TestSafetyPublicationIsPathAndVerdictSpecific(t *testing.T) {
	d := publicationDefinition(t)
	bad := assertFinding(t, compile(t, d), PublishCode, true)
	if !strings.Contains(strings.Join(bad[0].Details.WitnessPath, " "), "review(fail)") {
		t.Fatalf("publisher on another branch masked witness: %+v", bad)
	}
	d.Spec.Gates[0].Branches["fail"] = "publish"
	assertFinding(t, compile(t, d), PublishCode, false)

	// A renamed intermediate task is legal; no stage-name heuristic.
	d.Spec.Tasks[1].Name = "renamed-check"
	d.Spec.Tasks[0].Next = "renamed-check"
	d.Spec.Tasks = append(d.Spec.Tasks, shell("release", wf.TargetEscalate, "true"))
	d.Spec.Tasks[2].Next = "release"
	assertFinding(t, compile(t, d), PublishCode, false)

	// A publisher can fail and still reach a park when continueOnError is set.
	d.Spec.Tasks[2].ContinueOnError = true
	assertFinding(t, compile(t, d), PublishCode, true)
}

func TestSafetyBudgetExhaustionPublication(t *testing.T) {
	d := publicationDefinition(t)
	d.Spec.Gates[0].Branches["needs-changes"] = "implement"
	d.Spec.Gates[0].Branches["fail"] = "publish"
	d.Spec.Gates[0].MaxRepasses = 5
	bad := assertFinding(t, compile(t, d), PublishCode, true)
	if !strings.Contains(strings.Join(bad[0].Details.WitnessPath, " "), "budget-exhausted(5 policy repasses)") {
		t.Fatalf("missing exhaustion witness: %+v", bad)
	}
	d.Spec.Gates[0].Branches[wf.BranchEscalate] = "publish"
	assertFinding(t, compile(t, d), PublishCode, false)
}

func TestSafetyNoWorkCannotSkipPendingPublication(t *testing.T) {
	d := publicationDefinition(t)
	d.Spec.Gates[0].Branches["fail"] = "select"
	d.Spec.Gates[0].Branches["needs-changes"] = "select"
	selector := shell("select", "publish", "goobers", "backlog-query")
	for _, use := range providerstage.ForVersion("2.0").RequiredCapabilities("backlog-query", nil) {
		selector.Capabilities = append(selector.Capabilities, string(use.Capability))
	}
	d.Spec.Tasks = append(d.Spec.Tasks, selector)
	got := assertFinding(t, compile(t, d), PublishCode, true)
	if !strings.Contains(strings.Join(got[0].Details.WitnessPath, " "), "no-work") {
		t.Fatalf("no-work terminal was not checked: %+v", got)
	}
	assertFinding(t, compile(t, d), RecoveryCode, false)
	d.Spec.Tasks[3].Run.Command = []string{"true"}
	assertFinding(t, compile(t, d), PublishCode, false)
	annotate(t, &d, Contracts{Stages: map[string]StageContract{
		"review": {Review: "pr"}, "publish": {Publishes: "review", Parks: true},
	}})
	assertFinding(t, compile(t, d), PublishCode, false)
}

func TestSafetyPatchMustBelongToSelectedSubject(t *testing.T) {
	d := reviewDefinition()
	d.Spec.Tasks[0] = shell("select", "check", "goobers", "gather-sibling-context")
	d.Spec.Tasks[0].Inputs = map[string]string{"selectedNumber": "42"}
	d.Spec.Tasks[0].PolicyActions = []string{"flag-scope-drift", "route-verdict"}
	for _, use := range providerstage.ForVersion("2.0").RequiredCapabilities("gather-sibling-context", nil) {
		d.Spec.Tasks[0].Capabilities = append(d.Spec.Tasks[0].Capabilities, string(use.Capability))
	}
	d.Spec.Start = "select"
	d.Spec.Tasks[1].Run.Command = []string{"git", "diff", "main...HEAD"}
	d.Spec.Gates[0].Branches["needs-changes"] = "check"
	d.Spec.Gates[0].Agentic.Workspace = apiv1.WorkspaceScratch
	got := assertFinding(t, compile(t, d), EvidenceCode, true)
	if got[0].Details.Confidence != "uncertain" {
		t.Fatalf("conditional PR binding must remain uncertain: %+v", got)
	}
	d.Spec.Tasks[1].Run.Command = []string{"custom-subject-patch"}
	annotate(t, &d, Contracts{Stages: map[string]StageContract{"check": {Evidence: "patch"}}})
	assertFinding(t, compile(t, d), EvidenceCode, false)
}

func TestSafetyFeedbackUsesRuntimeContextSelection(t *testing.T) {
	d := reviewDefinition()
	d.Spec.Tasks[0].ContextFrom = []string{"check"}
	findings := assertFinding(t, compile(t, d), FeedbackCode, true)
	if findings[0].Details.Budget != 3 || !strings.Contains(findings[0].Details.BudgetSource, "instance override") {
		t.Fatalf("missing effective budget limitation: %+v", findings[0])
	}
	d.Spec.Tasks[0].ContextFrom = []string{"check", "review"}
	assertFinding(t, compile(t, d), FeedbackCode, false)
	d.Spec.Tasks[0].ContextFrom = nil
	assertFinding(t, compile(t, d), FeedbackCode, false)
}

func TestSafetyUnchangedCycleVersusHealthyRework(t *testing.T) {
	d := reviewDefinition()
	assertFinding(t, compile(t, d), CycleCode, false)
	d.Spec.Start = "check"
	d.Spec.Tasks = d.Spec.Tasks[1:]
	d.Spec.Gates[0].Branches["needs-changes"] = "check"
	assertFinding(t, compile(t, d), CycleCode, true)
	d.Spec.Tasks[0].Run.Command = []string{"custom-rework"}
	assertFinding(t, compile(t, d), CycleCode, false)
}

func TestSafetyRecoveryRequiresDeclaredObligation(t *testing.T) {
	d := wf.Definition{Name: "queue", Version: 1, DSLVersion: "2.0", Spec: apiv1.WorkflowSpec{
		Gaggle: "example", Start: "select", Tasks: []apiv1.Task{
			shell("select", "recovery", "goobers", "update-behind-pr"),
			shell("recovery", "", "true"),
		},
	}}
	// Admit the real built-in's capability contract rather than bypassing it.
	for _, use := range providerstage.ForVersion("2.0").RequiredCapabilities("update-behind-pr", nil) {
		d.Spec.Tasks[0].Capabilities = append(d.Spec.Tasks[0].Capabilities, string(use.Capability))
	}
	d.Spec.Tasks[0].Inputs = map[string]string{"resultFile": "update-behind-result.json"}
	d.Spec.Tasks[0].PolicyActions = []string{"update-pr-branch", "clear-remediation"}
	assertFinding(t, compile(t, d), RecoveryCode, false)
	annotate(t, &d, Contracts{Stages: map[string]StageContract{"select": {RecoveryOnNoWork: "recovery"}}})
	assertFinding(t, compile(t, d), RecoveryCode, true)
	d.Spec.Start = "recovery"
	d.Spec.Tasks[1].Next, d.Spec.Tasks[0].Next = "select", ""
	assertFinding(t, compile(t, d), RecoveryCode, false)
	annotate(t, &d, Contracts{})
	assertFinding(t, compile(t, d), RecoveryCode, false)
}

func TestSafetyCustomBoundaryAndInspectableSuppression(t *testing.T) {
	d := reviewDefinition()
	d.Spec.Gates[0].Agentic.Workspace = apiv1.WorkspaceScratch
	d.Spec.Tasks[1].Run.Command = []string{"custom-evidence"}
	annotate(t, &d, Contracts{Stages: map[string]StageContract{"review": {Review: "code"}}})
	assertFinding(t, compile(t, d), CoverageCode, true)
	got := assertFinding(t, compile(t, d), EvidenceCode, true)
	if got[0].Details.Confidence != "uncertain" {
		t.Fatalf("unknown treated as known absence: %+v", got)
	}
	annotate(t, &d, Contracts{Stages: map[string]StageContract{"check": {Evidence: "patch"}}})
	assertFinding(t, compile(t, d), EvidenceCode, false)
	assertFinding(t, compile(t, d), CoverageCode, false)

	d.Spec.Tasks[1].Run.Command = []string{"git", "diff", "--check"}
	annotate(t, &d, Contracts{Suppressions: []Suppression{{Code: EvidenceCode, Stage: "review", Reason: "external patch is supplied by our adapter"}}})
	assertFinding(t, compile(t, d), EvidenceCode, false)
	suppressed := assertFinding(t, compile(t, d), SuppressedCode, true)
	if suppressed[0].Details.SuppressedCode != EvidenceCode || suppressed[0].Details.SuppressionReason == "" {
		t.Fatalf("suppression not inspectable: %+v", suppressed)
	}
	annotate(t, &d, Contracts{Suppressions: []Suppression{{Code: EvidenceCode, Stage: "review"}}})
	assertFinding(t, compile(t, d), EvidenceCode, true)
	unknown := Analyze(compile(t, d), Options{})
	if len(findingsFor(unknown, CoverageCode)) == 0 {
		t.Fatal("invalid suppression silently accepted")
	}
}

func TestSafetyEquivalentRulesAcrossDSLVersions(t *testing.T) {
	for _, version := range []string{"2.0", "3.0"} {
		t.Run(version, func(t *testing.T) {
			d := publicationDefinition(t)
			d.DSLVersion = version
			d.Spec.Gates[0].Agentic.Workspace = apiv1.WorkspaceScratch
			d.Spec.Gates[0].Branches["needs-changes"] = "implement"
			d.Spec.Gates[0].Branches["fail"] = "publish"
			d.Spec.Tasks[0].ContextFrom = []string{"check"}
			if version == "3.0" {
				d.Spec.Tasks[1].RepoFrom = apiv1.RepoFrom{"implement"}
				d.Spec.Tasks[2].Workspace = apiv1.WorkspaceScratch
			}
			m := compile(t, d)
			assertFinding(t, m, EvidenceCode, true)
			assertFinding(t, m, PublishCode, true)
			assertFinding(t, m, FeedbackCode, true)
			d.Spec.Gates[0].Agentic.Workspace = apiv1.WorkspaceRepo
			d.Spec.Gates[0].Branches["fail"] = "publish"
			d.Spec.Gates[0].Branches[wf.BranchEscalate] = "publish"
			d.Spec.Tasks[0].ContextFrom = nil
			m = compile(t, d)
			assertFinding(t, m, EvidenceCode, false)
			assertFinding(t, m, PublishCode, false)
			assertFinding(t, m, FeedbackCode, false)
		})
	}
}

func TestSafetyNoWorkStatusGateRemainsCompileError(t *testing.T) {
	d := wf.Definition{Name: "queue", Version: 1, DSLVersion: "2.0", Spec: apiv1.WorkflowSpec{
		Gaggle: "example", Start: "select", Tasks: []apiv1.Task{
			shell("select", "empty", "goobers", "backlog-query"),
			shell("recover", wf.TargetEscalate, "true"),
		},
		Gates: []apiv1.Gate{{Name: "empty", Evaluator: apiv1.EvaluatorAutomated,
			Automated: &apiv1.AutomatedGate{Check: "status-equals", Params: map[string]string{"equals": "no-work"}},
			Branches:  map[string]string{"pass": "recover", "fail": ""}}},
	}}
	for _, use := range providerstage.ForVersion("2.0").RequiredCapabilities("backlog-query", nil) {
		d.Spec.Tasks[0].Capabilities = append(d.Spec.Tasks[0].Capabilities, string(use.Capability))
	}
	if _, err := wf.Compile(d); err == nil || !strings.Contains(err.Error(), `"no-work" is not one of`) {
		t.Fatalf("invalid status gate must retain its existing compile error: %v", err)
	}
	d.Spec.Gates[0].Automated.Params["equals"] = "success"
	assertFinding(t, compile(t, d), RecoveryCode, false)
}

func TestSafetyAssertionsAreScopedAndCannotContradictBuiltins(t *testing.T) {
	d := reviewDefinition()
	d.Spec.Gates[0].Agentic.Workspace = apiv1.WorkspaceScratch
	annotate(t, &d, Contracts{Stages: map[string]StageContract{"check": {Evidence: "patch"}}})
	assertFinding(t, compile(t, d), CoverageCode, true)
	assertFinding(t, compile(t, d), EvidenceCode, true)
	annotate(t, &d, Contracts{Stages: map[string]StageContract{"review": {Evidence: "patch"}}})
	assertFinding(t, compile(t, d), CoverageCode, true)
	annotate(t, &d, Contracts{Suppressions: []Suppression{{Code: EvidenceCode, Stage: "check", Reason: "not this gate"}}})
	assertFinding(t, compile(t, d), EvidenceCode, true)

	d.Spec.Start = "check"
	d.Spec.Tasks = d.Spec.Tasks[1:]
	d.Spec.Tasks[0].Run.Command = []string{"custom-check"}
	d.Spec.Gates[0].Branches["needs-changes"] = "check"
	annotate(t, &d, Contracts{Stages: map[string]StageContract{"check": {Evidence: "none"}}})
	assertFinding(t, compile(t, d), CycleCode, false)
}

func TestSafetyWrongGatePublisherDoesNotDischargeRejection(t *testing.T) {
	d := publicationDefinition(t)
	d.Spec.Gates[0].Branches["pass"] = "other"
	d.Spec.Gates = append(d.Spec.Gates, apiv1.Gate{Name: "other", Evaluator: apiv1.EvaluatorAgentic,
		Agentic:  &apiv1.AgenticGate{Goober: "worker"},
		Branches: map[string]string{"pass": "", "fail": "", "needs-changes": ""}})
	annotate(t, &d, Contracts{Stages: map[string]StageContract{
		"review": {Review: "pr"}, "other": {Review: "internal"}, "publish": {Publishes: "other"},
	}})
	got := assertFinding(t, compile(t, d), PublishCode, true)
	var uncovered bool
	for _, f := range got {
		if strings.Contains(strings.Join(f.Details.WitnessPath, " "), "publish") {
			uncovered = true
		}
	}
	// Remove the other uncovered branch so it cannot win diagnostic deduplication.
	d.Spec.Gates[0].Branches["fail"] = "publish"
	got = assertFinding(t, compile(t, d), PublishCode, true)
	uncovered = uncovered || strings.Contains(strings.Join(got[0].Details.WitnessPath, " "), "publish")
	if !uncovered {
		t.Fatalf("wrong-gate publisher hid the rejection: %+v", got)
	}
}

func TestSafetyIdentityAndNoExecutionMutation(t *testing.T) {
	d := reviewDefinition()
	d.Spec.Gates[0].Agentic.Workspace = apiv1.WorkspaceScratch
	m := compile(t, d)
	before, err := json.Marshal(m.Def)
	if err != nil {
		t.Fatal(err)
	}
	digest := m.Digest()
	a, b := Analyze(m, Options{BinaryIdentity: "a"}), Analyze(m, Options{BinaryIdentity: "a"})
	if !reflect.DeepEqual(a, b) {
		t.Fatal("repeated analysis did not deduplicate deterministically")
	}
	c := Analyze(m, Options{BinaryIdentity: "b"})
	if len(a) == 0 || a[0].Details.ID == c[0].Details.ID {
		t.Fatal("binary upgrade reused finding identity")
	}
	withControls := Analyze(m, Options{BinaryIdentity: "a", GaggleRunControls: &apiv1.RunControls{MaxRepasses: 9}})
	if a[0].Details.ID == withControls[0].Details.ID {
		t.Fatal("gaggle run-control change reused finding identity")
	}
	changed := d
	changed.Spec.RunControls = &apiv1.RunControls{MaxRepasses: 8}
	if next := Analyze(compile(t, changed), Options{BinaryIdentity: "a"}); a[0].Details.ID == next[0].Details.ID {
		t.Fatal("configuration change reused finding identity")
	}
	after, err := json.Marshal(m.Def)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) || digest != m.Digest() {
		t.Fatal("advisory analysis changed execution definition or digest")
	}
}

func TestSafetyBoundsAreVisible(t *testing.T) {
	d := wf.Definition{Name: "long", Version: 1, DSLVersion: "2.0", Spec: apiv1.WorkflowSpec{Gaggle: "example", Start: "s0"}}
	for i := 0; i < maxDepth+2; i++ {
		next := ""
		if i < maxDepth+1 {
			next = fmt.Sprintf("s%d", i+1)
		}
		d.Spec.Tasks = append(d.Spec.Tasks, shell(fmt.Sprintf("s%d", i), next, "true"))
	}
	findings := Analyze(compile(t, d), Options{})
	if len(findings) != 1 || findings[0].Code != CoverageCode || findings[0].Details.Coverage != "incomplete" {
		t.Fatalf("bound falsely claimed complete coverage: %+v", findings)
	}
}

func TestSafetyBudgetMatchesProductionHelper(t *testing.T) {
	d := reviewDefinition()
	d.Spec.Tasks[0].ContextFrom = []string{"check"}
	d.Spec.RunControls = &apiv1.RunControls{MaxRepasses: 7}
	m := compile(t, d)
	findings := findingsFor(Analyze(m, Options{}), FeedbackCode)
	var b runcontrol.RepassBudget
	charge := b.Charge(d.Spec.Gates[0], "needs-changes", "implement", true, 7)
	if len(findings) != 1 || findings[0].Details.Budget != charge.Bound {
		t.Fatalf("lint and runtime budgets disagree: %+v, %+v", findings, charge)
	}
}

func TestSafetyFiveGaggleTwentyThreeWorkflowBudget(t *testing.T) {
	machines := representativeMachines(t)
	start := time.Now()
	for _, m := range machines {
		Analyze(m, Options{})
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("23-workflow safety analysis exceeded two seconds: %s", elapsed)
	}
}

func representativeMachines(t testing.TB) []*wf.Machine {
	t.Helper()
	var machines []*wf.Machine
	for i := 0; i < 23; i++ {
		d := publicationDefinition(t)
		d.Name = fmt.Sprintf("workflow-%d", i)
		d.Spec.Start = "prepare-0"
		for step := 0; step < 8; step++ {
			next := fmt.Sprintf("prepare-%d", step+1)
			if step == 7 {
				next = "implement"
			}
			d.Spec.Tasks = append(d.Spec.Tasks, shell(fmt.Sprintf("prepare-%d", step), next, "true"))
		}
		d.Spec.RunControls = &apiv1.RunControls{MaxRepasses: int32(3 + i%5)}
		if i%2 == 0 {
			d.Spec.Gates[0].Branches["needs-changes"] = "implement"
			d.Spec.Gates[0].Branches["fail"] = "publish"
			d.Spec.Tasks[0].ContextFrom = []string{"check"}
		}
		if i%3 == 0 {
			d.Spec.Gates[0].Agentic.Workspace = apiv1.WorkspaceScratch
		}
		// Goober binding remains example during compilation. The independent
		// machines represent the same topology across five gaggle scopes.
		d.Spec.Gaggle = fmt.Sprintf("gaggle-%d", i%5)
		m, err := wf.Compile(d, wf.WithGoobers(map[string]apiv1.GooberSpec{
			"worker": {Gaggle: d.Spec.Gaggle, Harness: "copilot", Instructions: "unused.md"},
		}))
		if err != nil {
			t.Fatal(err)
		}
		machines = append(machines, m)
	}
	return machines
}

func BenchmarkSafetyFiveGaggleTwentyThreeWorkflows(b *testing.B) {
	machines := representativeMachines(b)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		for _, m := range machines {
			Analyze(m, Options{})
		}
	}
}

func TestSafetyCommandCatalogExactForms(t *testing.T) {
	for _, tc := range []struct {
		command []string
		want    Effects
	}{
		{[]string{"git", "diff", "--check"}, Effects{Known: true, EmptySuccess: true, CodeSubject: true}},
		{[]string{"git", "diff", "main...HEAD"}, Effects{Known: true, Patch: true, CodeSubject: true}},
		{[]string{"git", "diff", "--stat"}, Effects{}},
		{[]string{"git", "diff", "--check", "main...HEAD"}, Effects{}},
		{[]string{"sh", "-c", "goobers apply-verdict"}, Effects{}},
		{[]string{"goobers", "apply-verdict"}, Effects{Known: true, Publishes: "review"}},
		{[]string{"goobers", "apply-verdict", "--gate", "renamed"}, Effects{Known: true, Publishes: "renamed"}},
		{[]string{"goobers", "apply-verdict", "--unknown"}, Effects{}},
		{[]string{"goobers", "gather-pr-context"}, Effects{Known: true, SelectsPR: true, Rebinds: true, NoWork: true}},
		{[]string{"goobers", "gather-sibling-context"}, Effects{Known: true, SelectsPR: true, ConditionalRebind: true}},
		{[]string{"goobers", "remediation-checkpoint", "--escalate"}, Effects{Known: true, Parks: true}},
	} {
		t.Run(strings.Join(tc.command, " "), func(t *testing.T) {
			got := CommandEffects(shell("arbitrary-name", "", tc.command...))
			if got != tc.want {
				t.Fatalf("catalog got %+v, want %+v", got, tc.want)
			}
		})
	}
}
