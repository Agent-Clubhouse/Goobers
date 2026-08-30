package v30

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
// inventoried subcommands. testdata/golden/runtime-policy.yaml is the
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

// --- The DSL 3.0 scheduling surface (dsl-3.0.md §2) -----------------------

func implementTask(name string, runsOn *apiv1.RunsOn) apiv1.Task {
	return apiv1.Task{
		Name:   name,
		Type:   apiv1.TaskAgentic,
		Goober: "coder",
		Goal:   "implement",
		RunsOn: runsOn,
	}
}

func TestCompileRejectsOSTokensAnywhereInDocument(t *testing.T) {
	// CAP004 (dsl-3.0.md D12): os=* tokens cannot exist in a 3.0 document —
	// stage-level runsOn.capabilities...
	task := implementTask("implement", &apiv1.RunsOn{Capabilities: []string{"go@1.26", "os=windows"}})
	_, err := compileAcknowledged(Definition{Name: "os-token", Version: 1, Spec: singleTaskSpec(task)})
	if err == nil ||
		!strings.Contains(err.Error(), `task "implement" runsOn.capabilities contains "os=windows"`) ||
		!strings.Contains(err.Error(), "declare runsOn.os: windows instead") {
		t.Fatalf("Compile error = %v, want CAP004 rejection with the runsOn.os rewrite hint", err)
	}

	// ...the darwin token maps to the product spelling macOS...
	task = implementTask("implement", &apiv1.RunsOn{Capabilities: []string{"os=darwin"}})
	_, err = compileAcknowledged(Definition{Name: "darwin-token", Version: 1, Spec: singleTaskSpec(task)})
	if err == nil || !strings.Contains(err.Error(), "declare runsOn.os: macOS instead") {
		t.Fatalf("Compile error = %v, want the darwin→macOS rewrite hint", err)
	}

	// ...and the gaggle-level floor.
	task = implementTask("implement", nil)
	_, err = compileAcknowledged(
		Definition{Name: "gaggle-os-token", Version: 1, Spec: singleTaskSpec(task)},
		WithGaggleRunsOn(&apiv1.GaggleRunsOn{Capabilities: []string{"os=linux"}}),
	)
	if err == nil || !strings.Contains(err.Error(), `gaggle runsOn.capabilities contains "os=linux"`) {
		t.Fatalf("Compile error = %v, want CAP004 rejection at the gaggle floor", err)
	}
}

func TestCompileRejectsUnknownRestrictionWithSuggestion(t *testing.T) {
	// CAP005: a restriction outside the closed v1 effect list errors with a
	// did-you-mean suggestion (the CAP002 idiom).
	task := implementTask("implement", &apiv1.RunsOn{Restrictions: []string{"network:allow-list"}})
	_, err := compileAcknowledged(Definition{Name: "restriction-typo", Version: 1, Spec: singleTaskSpec(task)})
	if err == nil ||
		!strings.Contains(err.Error(), `unknown restriction "network:allow-list"`) ||
		!strings.Contains(err.Error(), `did you mean "network:allowlist"?`) {
		t.Fatalf("Compile error = %v, want CAP005 rejection with suggestion", err)
	}

	// Far-off tokens still reject, without a suggestion.
	task = implementTask("implement", &apiv1.RunsOn{Restrictions: []string{"seccomp"}})
	_, err = compileAcknowledged(Definition{Name: "restriction-far", Version: 1, Spec: singleTaskSpec(task)})
	if err == nil || !strings.Contains(err.Error(), `unknown restriction "seccomp"`) {
		t.Fatalf("Compile error = %v, want CAP005 rejection", err)
	}
	if strings.Contains(err.Error(), "did you mean") {
		t.Fatalf("Compile error = %v, want no suggestion for a far-off token", err)
	}

	// The whole closed list is accepted, at stage and gaggle level.
	all := []string{"network:none", "network:allowlist", "fs:readonly-except-workspace", "tmp:ephemeral", "env:default-deny"}
	task = implementTask("implement", &apiv1.RunsOn{Restrictions: all})
	if _, err := compileAcknowledged(
		Definition{Name: "restrictions-ok", Version: 1, Spec: singleTaskSpec(task)},
		WithGaggleRunsOn(&apiv1.GaggleRunsOn{Restrictions: []string{"network:allowlist"}}),
	); err != nil {
		t.Fatalf("closed-list restrictions must compile: %v", err)
	}
}

func TestCompileValidatesRunsOnOSAndQuantities(t *testing.T) {
	// The os enum uses product spellings, not GOOS.
	task := implementTask("implement", &apiv1.RunsOn{OS: "darwin"})
	_, err := compileAcknowledged(Definition{Name: "bad-os", Version: 1, Spec: singleTaskSpec(task)})
	if err == nil || !strings.Contains(err.Error(), `task "implement" runsOn.os "darwin" is not one of linux, windows, macOS`) {
		t.Fatalf("Compile error = %v, want os enum rejection", err)
	}

	// Quantities are Kubernetes quantity strings, verbatim.
	task = implementTask("implement", &apiv1.RunsOn{CPU: "two cores"})
	_, err = compileAcknowledged(Definition{Name: "bad-cpu", Version: 1, Spec: singleTaskSpec(task)})
	if err == nil || !strings.Contains(err.Error(), `runsOn.cpu "two cores" must be a Kubernetes quantity string`) {
		t.Fatalf("Compile error = %v, want quantity rejection", err)
	}
	task = implementTask("implement", &apiv1.RunsOn{Memory: "-4Gi"})
	_, err = compileAcknowledged(Definition{Name: "neg-mem", Version: 1, Spec: singleTaskSpec(task)})
	if err == nil || !strings.Contains(err.Error(), "runsOn.memory must be positive") {
		t.Fatalf("Compile error = %v, want positive-quantity rejection", err)
	}

	// A fully-specified valid block compiles.
	task = implementTask("implement", &apiv1.RunsOn{
		OS: "linux", CPU: "2000m", Memory: "4Gi", Disk: "20Gi",
		Capabilities: []string{"go@1.26", "make", "gcc"},
	})
	if _, err := compileAcknowledged(Definition{Name: "full-runson", Version: 1, Spec: singleTaskSpec(task)}); err != nil {
		t.Fatalf("valid runsOn must compile: %v", err)
	}
}

func TestCompileGaggleFloorOSConflictIsError(t *testing.T) {
	task := implementTask("implement", &apiv1.RunsOn{OS: "windows"})
	def := Definition{Name: "os-conflict", Version: 1, Spec: singleTaskSpec(task)}
	_, err := compileAcknowledged(def, WithGaggleRunsOn(&apiv1.GaggleRunsOn{OS: "linux"}))
	if err == nil ||
		!strings.Contains(err.Error(), `task "implement" runsOn.os "windows" conflicts with the gaggle-level runsOn.os "linux"`) {
		t.Fatalf("Compile error = %v, want gaggle-vs-stage OS conflict rejection", err)
	}

	// Same OS on both sides is not a conflict; stage-only and gaggle-only pins
	// are floors, not conflicts.
	if _, err := compileAcknowledged(def, WithGaggleRunsOn(&apiv1.GaggleRunsOn{OS: "windows"})); err != nil {
		t.Fatalf("matching OS must compile: %v", err)
	}
	if _, err := compileAcknowledged(def); err != nil {
		t.Fatalf("stage-only OS must compile: %v", err)
	}
}

func TestCompileRejectsRemovedTwoPointOhSurface(t *testing.T) {
	// requiredCapabilities does not exist in 3.0 (dsl-3.0.md D1/D12).
	task := implementTask("implement", nil)
	task.RequiredCapabilities = []string{"go@1.26"}
	_, err := compileAcknowledged(Definition{Name: "reqcaps", Version: 1, Spec: singleTaskSpec(task)})
	if err == nil ||
		!strings.Contains(err.Error(), `task "implement" declares requiredCapabilities, which does not exist in DSL 3.0`) ||
		!strings.Contains(err.Error(), "runsOn.capabilities") {
		t.Fatalf("Compile error = %v, want requiredCapabilities refusal with runsOn pointer", err)
	}

	// run.network folds into runsOn.restrictions (D16).
	netTask := apiv1.Task{
		Name: "hermetic",
		Type: apiv1.TaskDeterministic,
		Goal: "build hermetically",
		Run:  &apiv1.DeterministicRun{Command: []string{"make", "build"}, Network: apiv1.NetworkNone},
	}
	_, err = compileAcknowledged(Definition{Name: "run-network", Version: 1, Spec: singleTaskSpec(netTask)})
	if err == nil ||
		!strings.Contains(err.Error(), `task "hermetic" declares run.network, which does not exist in DSL 3.0`) ||
		!strings.Contains(err.Error(), "runsOn.restrictions: [network:none]") {
		t.Fatalf("Compile error = %v, want run.network refusal with restrictions pointer", err)
	}
}

// TestOSUnspecifiedMeansNoRequirement pins §9 item 6 at compile level
// (explicit-complete semantics, D3): an os-unspecified stage compiles with no
// OS requirement anywhere — no default, no warning, no derived os tag. The
// Linux-preferring placement is dispatch policy the solver owns (#3506
// onward), never a compiled-in requirement.
func TestOSUnspecifiedMeansNoRequirement(t *testing.T) {
	task := implementTask("implement", &apiv1.RunsOn{Capabilities: []string{"go@1.26"}})
	def := Definition{Name: "os-unspecified", Version: 1, Spec: singleTaskSpec(task)}
	if _, err := compileAcknowledged(def); err != nil {
		t.Fatalf("os-unspecified stage must compile: %v", err)
	}
	effective := EffectiveRunsOn(taskPlacementStage(def.Spec.Tasks[0], nil), nil)
	if effective.OS != "" {
		t.Fatalf("effective OS = %q, want empty (unspecified = no requirement)", effective.OS)
	}
	for _, tag := range EffectiveCapabilities(taskPlacementStage(def.Spec.Tasks[0], nil), nil) {
		if strings.HasPrefix(tag, "os") {
			t.Fatalf("effective capabilities carry OS tag %q; unspecified must stay unspecified", tag)
		}
	}
}

// --- Base contract + derivation (dsl-3.0.md D6/D7) ------------------------

func TestDerivedCapabilities(t *testing.T) {
	goobers := map[string]apiv1.GooberSpec{
		"coder":    {Gaggle: "web", Harness: apiv1.HarnessClaudeCode},
		"reviewer": {Gaggle: "web"}, // harness empty → schema default copilot
	}

	agentic := implementTask("implement", nil)
	agentic.Goober = "coder"
	if got := DerivedCapabilities(agentic, goobers); len(got) != 1 || got[0] != "harness:claude-code" {
		t.Fatalf("agentic derived = %v, want [harness:claude-code]", got)
	}
	agentic.Goober = "reviewer"
	if got := DerivedCapabilities(agentic, goobers); len(got) != 1 || got[0] != "harness:copilot" {
		t.Fatalf("default-harness derived = %v, want [harness:copilot]", got)
	}

	// sh/make stages derive shell; builtins derive only the (implicit,
	// unspellable) base contract; scripts are shell too.
	makeTask := apiv1.Task{
		Name: "ci", Type: apiv1.TaskDeterministic, Goal: "ci",
		Run: &apiv1.DeterministicRun{Command: []string{"make", "ci"}},
	}
	if got := DerivedCapabilities(makeTask, nil); len(got) != 1 || got[0] != DerivedShellTag {
		t.Fatalf("make derived = %v, want [shell]", got)
	}
	script := apiv1.Task{
		Name: "script", Type: apiv1.TaskDeterministic, Goal: "script",
		Run: &apiv1.DeterministicRun{Script: "echo hi"},
	}
	if got := DerivedCapabilities(script, nil); len(got) != 1 || got[0] != DerivedShellTag {
		t.Fatalf("script derived = %v, want [shell]", got)
	}
	builtin := apiv1.Task{
		Name: "publish", Type: apiv1.TaskDeterministic, Goal: "publish",
		Run: &apiv1.DeterministicRun{Command: []string{"goobers", "push-branch"}},
	}
	if got := DerivedCapabilities(builtin, nil); len(got) != 0 {
		t.Fatalf("builtin derived = %v, want empty (base contract only)", got)
	}
}

func TestEffectiveCapabilitiesUnionNeverSubtracts(t *testing.T) {
	goobers := map[string]apiv1.GooberSpec{"coder": {Gaggle: "web", Harness: apiv1.HarnessCopilot}}
	task := implementTask("implement", &apiv1.RunsOn{Capabilities: []string{"go@1.26"}})
	got := EffectiveCapabilities(taskPlacementStage(task, goobers), &apiv1.GaggleRunsOn{Capabilities: []string{"make", "go@1.26"}})
	want := []string{"go@1.26", "harness:copilot", "make"}
	if len(got) != len(want) {
		t.Fatalf("effective capabilities = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("effective capabilities = %v, want %v", got, want)
		}
	}
}
