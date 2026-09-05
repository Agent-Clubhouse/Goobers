package v20

import (
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/capability"
)

// --- Unknown built-in subcommand rejection (C) ---------------------------

func singleTaskSpec(task apiv1.Task) apiv1.WorkflowSpec {
	return apiv1.WorkflowSpec{
		Gaggle:   "web",
		Triggers: []apiv1.Trigger{{Type: apiv1.TriggerBacklogItem}},
		Start:    task.Name,
		Tasks:    []apiv1.Task{task},
	}
}

func TestCompileRejectsDesiredConcurrencyAboveMax(t *testing.T) {
	task := apiv1.Task{Name: "implement", Type: apiv1.TaskDeterministic, Goal: "implement", Run: &apiv1.DeterministicRun{Command: []string{"true"}}}
	spec := singleTaskSpec(task)
	spec.Readiness = apiv1.ReadinessConditions{MaxConcurrentRuns: 2, DesiredConcurrentRuns: 3}
	_, err := compileAcknowledged(Definition{Name: "bad-refill", Version: 1, Spec: spec})
	if err == nil || !strings.Contains(err.Error(), "desiredConcurrentRuns (3) must be less than or equal to spec.readiness.maxConcurrentRuns (2)") {
		t.Fatalf("Compile error = %v, want desired/max readiness rejection", err)
	}
}

func TestCompileRejectsDesiredConcurrencyAboveDefaultMax(t *testing.T) {
	task := apiv1.Task{Name: "implement", Type: apiv1.TaskDeterministic, Goal: "implement", Run: &apiv1.DeterministicRun{Command: []string{"true"}}}
	spec := singleTaskSpec(task)
	spec.Readiness = apiv1.ReadinessConditions{DesiredConcurrentRuns: 2}
	_, err := compileAcknowledged(Definition{Name: "bad-refill-default", Version: 1, Spec: spec})
	if err == nil || !strings.Contains(err.Error(), "desiredConcurrentRuns (2) must be less than or equal to spec.readiness.maxConcurrentRuns (1)") {
		t.Fatalf("Compile error = %v, want desired/default-max readiness rejection", err)
	}
}

func TestCompileRejectsUnknownBuiltinSubcommand(t *testing.T) {
	task := apiv1.Task{
		Name: "publish",
		Type: apiv1.TaskDeterministic,
		Goal: "publish the branch",
		Run:  &apiv1.DeterministicRun{Command: []string{"goobers", "push-brach"}},
	}
	_, err := compileAcknowledged(Definition{Name: "typo", Version: 1, Spec: singleTaskSpec(task)})
	if err == nil ||
		!strings.Contains(err.Error(), `task "publish" shells out to unknown built-in subcommand "push-brach"`) ||
		!strings.Contains(err.Error(), `did you mean "push-branch"?`) {
		t.Fatalf("Compile error = %v, want unknown-subcommand rejection with nearest-match suggestion", err)
	}

	// Far-off names get no suggestion but still reject.
	task.Run.Command = []string{"goobers", "detonate-production"}
	_, err = compileAcknowledged(Definition{Name: "far-off", Version: 1, Spec: singleTaskSpec(task)})
	if err == nil || !strings.Contains(err.Error(), `unknown built-in subcommand "detonate-production"`) {
		t.Fatalf("Compile error = %v, want unknown-subcommand rejection", err)
	}
	if strings.Contains(err.Error(), "did you mean") {
		t.Fatalf("Compile error = %v, want no suggestion for a far-off name", err)
	}
}

func TestCompileAcceptsInventoriedSubcommandShellOut(t *testing.T) {
	task := apiv1.Task{
		Name:          "publish",
		Type:          apiv1.TaskDeterministic,
		Goal:          "publish the branch",
		Run:           &apiv1.DeterministicRun{Command: []string{"goobers", "push-branch"}},
		Capabilities:  []string{string(capability.RepoPush)},
		PolicyActions: []string{"push-repository-branch"},
	}
	if _, err := compileAcknowledged(Definition{Name: "inventoried", Version: 1, Spec: singleTaskSpec(task)}); err != nil {
		t.Fatalf("inventoried shell-out should compile: %v", err)
	}

	// An explicit kind: shell is the same shell-out and stays checked.
	task.Inputs = map[string]string{"kind": "shell"}
	task.Run.Command = []string{"goobers", "push-brach"}
	if _, err := compileAcknowledged(Definition{Name: "explicit-shell", Version: 1, Spec: singleTaskSpec(task)}); err == nil ||
		!strings.Contains(err.Error(), `unknown built-in subcommand "push-brach"`) {
		t.Fatalf("Compile error = %v, want kind=shell to stay subcommand-checked", err)
	}
}

// TestCompileKindExemptionKeepsPlaceholderCommandsUnchecked pins the
// load-bearing exemption (ruled release-blocking if broken): a stage with a
// non-shell inputs.kind never shells out — the runner dispatches on the kind
// and the command is a schema-required placeholder — so its command must stay
// entirely unchecked even though "ci-poll" and "external-telemetry" are not
// inventoried subcommands. v_2_0/testdata/golden/runtime-policy.yaml is the
// canary; its digest is pinned by TestGoldenCompiledSemanticDigests.
func TestCompileKindExemptionKeepsPlaceholderCommandsUnchecked(t *testing.T) {
	poll := apiv1.Task{
		Name:         "poll",
		Type:         apiv1.TaskDeterministic,
		Goal:         "poll CI",
		Run:          &apiv1.DeterministicRun{Command: []string{"goobers", "ci-poll"}},
		Inputs:       map[string]string{"kind": "ci-poll"},
		Capabilities: []string{string(capability.ProviderPRWrite)},
	}
	if _, err := compileAcknowledged(Definition{Name: "kind-ci-poll", Version: 1, Spec: singleTaskSpec(poll)}); err != nil {
		t.Fatalf("kind=ci-poll placeholder command must stay accepted: %v", err)
	}

	telemetry := apiv1.Task{
		Name:         "query",
		Type:         apiv1.TaskDeterministic,
		Goal:         "query telemetry",
		Run:          &apiv1.DeterministicRun{Command: []string{"goobers", "external-telemetry"}},
		Inputs:       map[string]string{"kind": "external-telemetry"},
		Capabilities: []string{string(capability.TelemetryRead)},
	}
	if _, err := compileAcknowledged(Definition{Name: "kind-telemetry", Version: 1, Spec: singleTaskSpec(telemetry)}); err != nil {
		t.Fatalf("kind=external-telemetry placeholder command must stay accepted: %v", err)
	}

	// Non-goobers commands are out of scope regardless of name.
	makeTask := apiv1.Task{
		Name: "build",
		Type: apiv1.TaskDeterministic,
		Goal: "build",
		Run:  &apiv1.DeterministicRun{Command: []string{"make", "ci"}},
	}
	if _, err := compileAcknowledged(Definition{Name: "non-goobers", Version: 1, Spec: singleTaskSpec(makeTask)}); err != nil {
		t.Fatalf("non-goobers command must stay accepted: %v", err)
	}
}

// --- Subsumption wiring + over-privilege warning -------------------------

func TestCompileSubsumedCapabilitySatisfiesBuiltinRequirement(t *testing.T) {
	dedupe := apiv1.Task{
		Name: "dedupe",
		Type: apiv1.TaskDeterministic,
		Goal: "find duplicates",
		Run:  &apiv1.DeterministicRun{Command: []string{"goobers", "backlog-dedupe"}},
	}

	_, err := compileAcknowledged(Definition{Name: "no-caps", Version: 1, Spec: singleTaskSpec(dedupe)})
	if err == nil || !strings.Contains(err.Error(), `does not declare capability "github:issues:read"`) {
		t.Fatalf("Compile error = %v, want missing read capability", err)
	}

	dedupe.Capabilities = []string{string(capability.GitHubIssuesWrite)}
	if _, err := compileAcknowledged(Definition{Name: "write-subsumes", Version: 1, Spec: singleTaskSpec(dedupe)}); err == nil || !strings.Contains(err.Error(), "GOOBERS_CRED_GITHUB_ISSUES_READ") {
		t.Fatalf("write grant must not satisfy the separately brokered read requirement: %v", err)
	}

	dedupe.Capabilities = []string{string(capability.GitHubIssuesRead)}
	if _, err := compileAcknowledged(Definition{Name: "exact-read", Version: 1, Spec: singleTaskSpec(dedupe)}); err != nil {
		t.Fatalf("exact read grant must keep compiling: %v", err)
	}
}

func TestCheckWarningsOverPrivilegedSubsumingCapability(t *testing.T) {
	dedupe := apiv1.Task{
		Name:         "dedupe",
		Type:         apiv1.TaskDeterministic,
		Goal:         "find duplicates",
		Run:          &apiv1.DeterministicRun{Command: []string{"goobers", "backlog-dedupe"}},
		Capabilities: []string{string(capability.GitHubIssuesWrite)},
	}
	def := Definition{Name: "over-privileged", Version: 1, Spec: singleTaskSpec(dedupe)}
	warnings := CheckWarnings(def)
	var overPrivilege []string
	for _, w := range warnings {
		if strings.Contains(w, "strictly subsumes") {
			overPrivilege = append(overPrivilege, w)
		}
	}
	if len(overPrivilege) != 1 ||
		!strings.Contains(overPrivilege[0], `task "dedupe" declares capability "github:issues:write"`) ||
		!strings.Contains(overPrivilege[0], `"github:issues:read"`) {
		t.Fatalf("warnings = %v, want exactly one over-privilege warning naming the narrower capability", warnings)
	}

	// Exact declaration: no over-privilege warning.
	dedupe.Capabilities = []string{string(capability.GitHubIssuesRead)}
	for _, w := range CheckWarnings(Definition{Name: "exact", Version: 1, Spec: singleTaskSpec(dedupe)}) {
		if strings.Contains(w, "strictly subsumes") {
			t.Fatalf("unexpected over-privilege warning for exact declaration: %v", w)
		}
	}

	// Kind-backed stages never shell out, so their placeholder command must
	// not produce over-privilege noise either.
	poll := apiv1.Task{
		Name:         "poll",
		Type:         apiv1.TaskDeterministic,
		Goal:         "poll",
		Run:          &apiv1.DeterministicRun{Command: []string{"goobers", "backlog-dedupe"}},
		Inputs:       map[string]string{"kind": "ci-poll"},
		Capabilities: []string{string(capability.ProviderPRWrite), string(capability.GitHubIssuesWrite)},
	}
	for _, w := range CheckWarnings(Definition{Name: "kind-exempt", Version: 1, Spec: singleTaskSpec(poll)}) {
		if strings.Contains(w, "strictly subsumes") {
			t.Fatalf("unexpected over-privilege warning for kind-backed stage: %v", w)
		}
	}
}

// --- Push-boundary admission (#2861) -------------------------------------

func twoTaskSpec(upstream, downstream apiv1.Task) apiv1.WorkflowSpec {
	upstream.Next = downstream.Name
	return apiv1.WorkflowSpec{
		Gaggle:   "web",
		Triggers: []apiv1.Trigger{{Type: apiv1.TriggerBacklogItem}},
		Start:    upstream.Name,
		Tasks:    []apiv1.Task{upstream, downstream},
	}
}

func implementTask(name string, requiredCapabilities ...string) apiv1.Task {
	return apiv1.Task{
		Name:                 name,
		Type:                 apiv1.TaskAgentic,
		Goober:               "coder",
		Goal:                 "implement",
		RequiredCapabilities: requiredCapabilities,
	}
}

func TestCompileRejectsCrossPlatformTransitionWithoutPushBoundary(t *testing.T) {
	upstream := implementTask("implement-windows", "os=windows")
	downstream := apiv1.Task{
		Name:                 "test-linux",
		Type:                 apiv1.TaskDeterministic,
		Goal:                 "test on linux",
		Run:                  &apiv1.DeterministicRun{Command: []string{"make", "test"}},
		RequiredCapabilities: []string{"os=linux"},
	}
	_, err := compileAcknowledged(Definition{Name: "cross-platform", Version: 1, Spec: twoTaskSpec(upstream, downstream)})
	if err == nil ||
		!strings.Contains(err.Error(), `task "implement-windows" (os=windows)`) ||
		!strings.Contains(err.Error(), `task "test-linux" (os=linux)`) ||
		!strings.Contains(err.Error(), "push-branch") {
		t.Fatalf("Compile error = %v, want push-boundary rejection naming both stages and the boundary", err)
	}
}

func TestCompileRejectsCrossPlatformTransitionThroughAGate(t *testing.T) {
	upstream := implementTask("implement-windows", "os=windows")
	upstream.Next = "review"
	downstream := apiv1.Task{
		Name:                 "test-linux",
		Type:                 apiv1.TaskDeterministic,
		Goal:                 "test on linux",
		Run:                  &apiv1.DeterministicRun{Command: []string{"make", "test"}},
		RequiredCapabilities: []string{"os=linux"},
	}
	spec := apiv1.WorkflowSpec{
		Gaggle:   "web",
		Triggers: []apiv1.Trigger{{Type: apiv1.TriggerBacklogItem}},
		Start:    upstream.Name,
		Tasks:    []apiv1.Task{upstream, downstream},
		Gates: []apiv1.Gate{{
			Name:      "review",
			Evaluator: apiv1.EvaluatorAgentic,
			Agentic:   &apiv1.AgenticGate{Goober: "reviewer"},
			Branches: map[string]string{
				"pass":          "test-linux",
				"fail":          TargetAbort,
				"needs-changes": "implement-windows",
			},
		}},
	}
	_, err := compileAcknowledged(Definition{Name: "gated-cross-platform", Version: 1, Spec: spec})
	if err == nil || !strings.Contains(err.Error(), `task "implement-windows" (os=windows)`) ||
		!strings.Contains(err.Error(), `task "test-linux" (os=linux)`) {
		t.Fatalf("Compile error = %v, want push-boundary rejection across the gate", err)
	}
}

func TestCompileAcceptsCrossPlatformTransitionWithPushBoundary(t *testing.T) {
	// The upstream stage itself is the push boundary.
	pusher := apiv1.Task{
		Name:                 "push-windows",
		Type:                 apiv1.TaskDeterministic,
		Goal:                 "publish the branch",
		Run:                  &apiv1.DeterministicRun{Command: []string{"goobers", "push-branch"}},
		Capabilities:         []string{string(capability.RepoPush)},
		PolicyActions:        []string{"push-repository-branch"},
		RequiredCapabilities: []string{"os=windows"},
	}
	downstream := apiv1.Task{
		Name:                 "test-linux",
		Type:                 apiv1.TaskDeterministic,
		Goal:                 "test on linux",
		Run:                  &apiv1.DeterministicRun{Command: []string{"make", "test"}},
		RequiredCapabilities: []string{"os=linux"},
	}
	if _, err := compileAcknowledged(Definition{Name: "boundary-first", Version: 1, Spec: twoTaskSpec(pusher, downstream)}); err != nil {
		t.Fatalf("push stage crossing platforms should compile: %v", err)
	}

	// A push stage between the writer and the cross-platform stage.
	writer := implementTask("implement-windows", "os=windows")
	writer.Next = "push-windows"
	boundary := pusher
	boundary.Next = "test-linux"
	spec := apiv1.WorkflowSpec{
		Gaggle:   "web",
		Triggers: []apiv1.Trigger{{Type: apiv1.TriggerBacklogItem}},
		Start:    writer.Name,
		Tasks:    []apiv1.Task{writer, boundary, downstream},
	}
	if _, err := compileAcknowledged(Definition{Name: "boundary-between", Version: 1, Spec: spec}); err != nil {
		t.Fatalf("cross-platform transition behind a push boundary should compile: %v", err)
	}
}

func TestCompileLeavesSamePlatformAndSingleRunnerTransitionsUnchanged(t *testing.T) {
	// Same platform on both sides.
	samePlatform := twoTaskSpec(
		implementTask("implement-linux", "os=linux"),
		apiv1.Task{
			Name:                 "test-linux",
			Type:                 apiv1.TaskDeterministic,
			Goal:                 "test",
			Run:                  &apiv1.DeterministicRun{Command: []string{"make", "test"}},
			RequiredCapabilities: []string{"os=linux"},
		},
	)
	if _, err := compileAcknowledged(Definition{Name: "same-platform", Version: 1, Spec: samePlatform}); err != nil {
		t.Fatalf("same-platform transition should compile: %v", err)
	}

	// No os= tokens anywhere: the single-runner default stays unflagged —
	// absence of a token is not a provable platform difference.
	singleRunner := twoTaskSpec(
		implementTask("implement"),
		apiv1.Task{
			Name: "test",
			Type: apiv1.TaskDeterministic,
			Goal: "test",
			Run:  &apiv1.DeterministicRun{Command: []string{"make", "test"}},
		},
	)
	if _, err := compileAcknowledged(Definition{Name: "single-runner", Version: 1, Spec: singleRunner}); err != nil {
		t.Fatalf("single-runner workflow should compile: %v", err)
	}

	// Only one side pins a platform: not provable, not flagged.
	oneSided := twoTaskSpec(
		implementTask("implement-windows", "os=windows"),
		apiv1.Task{
			Name: "test",
			Type: apiv1.TaskDeterministic,
			Goal: "test",
			Run:  &apiv1.DeterministicRun{Command: []string{"make", "test"}},
		},
	)
	if _, err := compileAcknowledged(Definition{Name: "one-sided", Version: 1, Spec: oneSided}); err != nil {
		t.Fatalf("one-sided platform pin should compile: %v", err)
	}
}

func TestCompilePushBoundaryUsesEffectiveGaggleMergedCapabilities(t *testing.T) {
	// Stage-level tokens alone look disjoint (os=windows vs os=darwin), but
	// the gaggle-level os=linux token is shared by both effective sets, so
	// the transition is not provably cross-platform.
	spec := twoTaskSpec(
		implementTask("implement", "os=windows"),
		apiv1.Task{
			Name:                 "test",
			Type:                 apiv1.TaskDeterministic,
			Goal:                 "test",
			Run:                  &apiv1.DeterministicRun{Command: []string{"make", "test"}},
			RequiredCapabilities: []string{"os=darwin"},
		},
	)
	def := Definition{Name: "gaggle-merged", Version: 1, Spec: spec}
	if _, err := compileAcknowledged(def, WithGaggleRequiredCapabilities([]string{"os=linux"})); err != nil {
		t.Fatalf("gaggle-shared platform token must suppress the rejection: %v", err)
	}
	if _, err := compileAcknowledged(def); err == nil {
		t.Fatal("without the gaggle merge the disjoint stage tokens must reject")
	}
}

func TestCompileScratchWorkspaceCarriesNoRepoStateAcrossPlatforms(t *testing.T) {
	upstream := apiv1.Task{
		Name:                 "collect-windows",
		Type:                 apiv1.TaskDeterministic,
		Goal:                 "collect diagnostics",
		Run:                  &apiv1.DeterministicRun{Command: []string{"make", "collect"}, Workspace: apiv1.WorkspaceScratch},
		RequiredCapabilities: []string{"os=windows"},
	}
	downstream := apiv1.Task{
		Name:                 "report-linux",
		Type:                 apiv1.TaskDeterministic,
		Goal:                 "report",
		Run:                  &apiv1.DeterministicRun{Command: []string{"make", "report"}},
		RequiredCapabilities: []string{"os=linux"},
	}
	if _, err := compileAcknowledged(Definition{Name: "scratch-upstream", Version: 1, Spec: twoTaskSpec(upstream, downstream)}); err != nil {
		t.Fatalf("scratch-workspace upstream writes no repo state; must compile: %v", err)
	}

	// repo-readonly likewise cannot hand off unpushed writes. (task.workspace
	// is a 2.0 construct — the same field is a rejection in the frozen 1.4
	// interpreter, v_current's TestCompileRejectsTaskWorkspace.)
	readOnly := implementTask("research-windows", "os=windows")
	readOnly.Workspace = apiv1.WorkspaceRepoReadOnly
	if _, err := compileAcknowledged(Definition{Name: "readonly-upstream", Version: 1, Spec: twoTaskSpec(readOnly, downstream)}); err != nil {
		t.Fatalf("repo-readonly upstream writes no repo state; must compile: %v", err)
	}
}
