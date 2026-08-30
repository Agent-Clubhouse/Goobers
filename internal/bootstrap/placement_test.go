package bootstrap

import (
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/dispatcher"
	"github.com/goobers/goobers/internal/engine"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/supportmatrix"
	"github.com/goobers/goobers/internal/workflow"
)

func placementSpec(tasks ...apiv1.Task) workflow.Definition {
	return workflow.Definition{
		Name: "impl",
		Spec: apiv1.WorkflowSpec{
			Gaggle:   "web",
			Triggers: []apiv1.Trigger{{Type: apiv1.TriggerBacklogItem}},
			Start:    tasks[0].Name,
			Tasks:    tasks,
		},
	}
}

func distributedConfig() *instance.Config {
	return &instance.Config{Runners: []instance.RunnerEntry{
		{
			// A declared self OS keeps the pin host-independent in this test:
			// SelectRunner is Linux-preferring, so self wins every stage the
			// remote runner does not uniquely satisfy.
			Name: "self", Host: "self",
			Provides: instance.RunnerProvides{OS: instance.RunnerOSLinux},
		},
		{
			Name: "win-ci", Host: "ghcr.io/example/win:v1",
			Provides: instance.RunnerProvides{
				OS:           instance.RunnerOSWindows,
				Memory:       "16Gi",
				Capabilities: []string{"win-tools"},
			},
		},
	}}
}

func placementConfigSet() *instance.ConfigSet {
	return &instance.ConfigSet{
		Gaggles: []apiv1.Gaggle{{
			ObjectMeta: metav1.ObjectMeta{Name: "web"},
			Spec:       apiv1.GaggleSpec{},
		}},
	}
}

// Zero-declaration invariance (architecture §11 item 1): no runners: block,
// and a runners: block whose every entry is self, pin NOTHING — RunInput then
// carries no placements and every workflow arm behaves byte-identically to
// before the cutover.
func TestPinStagePlacementsLocalModePinsNothing(t *testing.T) {
	def := placementSpec(apiv1.Task{Name: "build", Type: apiv1.TaskDeterministic,
		Run: &apiv1.DeterministicRun{Command: []string{"true"}}})
	for _, tc := range []struct {
		name string
		cfg  *instance.Config
	}{
		{"no runners block", &instance.Config{}},
		{"self-only inventory", &instance.Config{Runners: []instance.RunnerEntry{{Name: "self", Host: "self"}}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			placements, err := PinStagePlacements(tc.cfg, placementConfigSet(), "web", def)
			if err != nil {
				t.Fatalf("PinStagePlacements: %v", err)
			}
			if placements != nil {
				t.Fatalf("placements = %+v, want nil (the local path untouched)", placements)
			}
		})
	}
}

// On a distributed inventory: a stage only the remote runner satisfies pins
// to its (gaggle × runner) queue with the eligible spec and requirement
// facts; a stage self satisfies pins Self and carries none of them.
func TestPinStagePlacementsDistributedInventory(t *testing.T) {
	def := placementSpec(
		apiv1.Task{Name: "build", Type: apiv1.TaskDeterministic,
			RequiredCapabilities: []string{"win-tools"},
			Run:                  &apiv1.DeterministicRun{Command: []string{"build.cmd"}},
			Next:                 "close-out"},
		apiv1.Task{Name: "close-out", Type: apiv1.TaskDeterministic,
			PolicyActions: []string{"claim-backlog-items"},
			Run:           &apiv1.DeterministicRun{Command: []string{"goobers", "backlog-query"}}},
	)
	placements, err := PinStagePlacements(distributedConfig(), placementConfigSet(), "web", def)
	if err != nil {
		t.Fatalf("PinStagePlacements: %v", err)
	}
	if len(placements) != 2 {
		t.Fatalf("placements = %+v, want one per task", placements)
	}

	build := placements[0]
	if build.Stage != "build" || build.Self {
		t.Fatalf("build placement = %+v, want a remote pin", build)
	}
	if build.Queue != "goobers-dispatch.web.win-ci" {
		t.Fatalf("build queue = %q, want the (gaggle x runner) queue", build.Queue)
	}
	if len(build.Eligible) != 1 || build.Eligible[0].Name != "win-ci" || build.Eligible[0].Host != "ghcr.io/example/win:v1" {
		t.Fatalf("build eligible = %+v, want the resolved remote runner spec", build.Eligible)
	}

	closeOut := placements[1]
	if !closeOut.Self || closeOut.Queue != "" || closeOut.Eligible != nil {
		t.Fatalf("close-out placement = %+v, want the self pin with no dispatch facts", closeOut)
	}
	if !closeOut.LedgerTouching {
		t.Fatal("a stage declaring claim-backlog-items must pin as ledger-touching (architecture D12)")
	}
}

// A ledger-touching stage whose only eligible runner is Windows is refused at
// START with the structural fact named — never green-lit into a run that can
// only die at dispatch (architecture D12: ledger-touching never places on
// Windows).
func TestPinStagePlacementsRefusesLedgerTouchingOnWindowsOnly(t *testing.T) {
	def := placementSpec(apiv1.Task{Name: "claim", Type: apiv1.TaskDeterministic,
		RequiredCapabilities: []string{"win-tools"},
		PolicyActions:        []string{"claim-backlog-items"},
		Run:                  &apiv1.DeterministicRun{Command: []string{"goobers", "backlog-query"}}})
	_, err := PinStagePlacements(distributedConfig(), placementConfigSet(), "web", def)
	if err == nil || !strings.Contains(err.Error(), "ledger") {
		t.Fatalf("error = %v, want the ledger-touching Windows refusal named", err)
	}
}

// An unplaceable stage refuses the start with the solver's diagnostic rather
// than stranding the run at schedule-to-start.
func TestPinStagePlacementsRefusesUnsatisfiableStage(t *testing.T) {
	def := placementSpec(apiv1.Task{Name: "build", Type: apiv1.TaskDeterministic,
		RequiredCapabilities: []string{"gpu"},
		Run:                  &apiv1.DeterministicRun{Command: []string{"true"}}})
	_, err := PinStagePlacements(distributedConfig(), placementConfigSet(), "web", def)
	if err == nil || !strings.Contains(err.Error(), "cannot place") {
		t.Fatalf("error = %v, want the unsatisfiable refusal with the diagnostic", err)
	}
}

// --- Agentic-gate placement (decision 001, #3798) -------------------------

// placementSpecV30 is a 3.0-pinned definition: only the 3.0 interpreter emits
// gate rows (the pre-3.0 router arm walks tasks alone).
func placementSpecV30(tasks []apiv1.Task, gates []apiv1.Gate) workflow.Definition {
	return workflow.Definition{
		Name:       "impl",
		DSLVersion: supportmatrix.V3DSLVersion,
		Spec: apiv1.WorkflowSpec{
			Gaggle:   "web",
			Triggers: []apiv1.Trigger{{Type: apiv1.TriggerBacklogItem}},
			Start:    tasks[0].Name,
			Tasks:    tasks,
			Gates:    gates,
		},
	}
}

// agenticConfig declares a Linux self runner and one remote agentic runner
// class (the live inventory's linux-agentic shape: an image host claiming the
// copilot harness and enforcing the restrictions a reviewer requires). Self
// enforces no restriction implicitly, so a stage requiring network:allowlist
// can only place remotely.
func agenticConfig() *instance.Config {
	return &instance.Config{Runners: []instance.RunnerEntry{
		{
			Name: "self", Host: "self",
			Provides: instance.RunnerProvides{OS: instance.RunnerOSLinux},
		},
		{
			Name: "linux-agentic", Host: "ghcr.io/example/goobers-ci:v1",
			Provides: instance.RunnerProvides{
				OS: instance.RunnerOSLinux, CPU: "2000m", Memory: "4Gi",
				Harnesses: []string{"copilot"},
			},
			Restrictions: []instance.RunnerRestriction{"network:allowlist", "tmp:ephemeral"},
		},
	}}
}

func reviewerConfigSet() *instance.ConfigSet {
	set := placementConfigSet()
	set.Goobers = []apiv1.Goober{{
		ObjectMeta: metav1.ObjectMeta{Name: "reviewer"},
		Spec:       apiv1.GooberSpec{Gaggle: "web", Harness: apiv1.HarnessCopilot},
	}}
	return set
}

// placedReviewGate declares a placement only the remote agentic runner
// satisfies (a restriction self does not enforce).
func placedReviewGate(name, next string) apiv1.Gate {
	return apiv1.Gate{
		Name: name, Evaluator: apiv1.EvaluatorAgentic,
		Agentic:  &apiv1.AgenticGate{Goober: "reviewer"},
		RunsOn:   &apiv1.RunsOn{CPU: "1000m", Memory: "2Gi", Restrictions: []string{"network:allowlist"}},
		Branches: map[string]string{"pass": next, "fail": "@abort"},
	}
}

// selfPlacedReviewGate declares a placement self satisfies (quantities only;
// self declares no ceiling, which constrains nothing), so the Linux-preferring
// selector pins it self.
func selfPlacedReviewGate(name, next string) apiv1.Gate {
	gate := placedReviewGate(name, next)
	gate.RunsOn = &apiv1.RunsOn{CPU: "1000m", Memory: "2Gi"}
	return gate
}

func findPin(t *testing.T, placements []engine.PinnedPlacement, stage string) engine.PinnedPlacement {
	t.Helper()
	for _, pin := range placements {
		if pin.Stage == stage {
			return pin
		}
	}
	t.Fatalf("no pin for stage %q in %+v", stage, placements)
	return engine.PinnedPlacement{}
}

// A placed agentic gate is pinned BY NAME — its own pin, never ledger-touching
// — and the task pins around it keep their own facts (decision 001 ruling 6).
// The gate here declares a placement self satisfies, so it pins self (the
// remote case is TestPinStagePlacementsPinsRemoteGate below).
func TestPinStagePlacementsPinsPlacedGateByName(t *testing.T) {
	def := placementSpecV30(
		[]apiv1.Task{
			{Name: "implement", Type: apiv1.TaskAgentic, Goober: "reviewer", Goal: "implement", Next: "review"},
			{Name: "close-out", Type: apiv1.TaskDeterministic,
				PolicyActions: []string{"claim-backlog-items"},
				Run:           &apiv1.DeterministicRun{Command: []string{"goobers", "backlog-query"}}},
		},
		[]apiv1.Gate{selfPlacedReviewGate("review", "close-out")},
	)
	placements, err := PinStagePlacements(agenticConfig(), reviewerConfigSet(), "web", def)
	if err != nil {
		t.Fatalf("PinStagePlacements: %v", err)
	}
	if len(placements) != 3 {
		t.Fatalf("placements = %+v, want one per task plus the placed gate", placements)
	}

	review := findPin(t, placements, "review")
	if !review.Self || review.Queue != "" || review.Eligible != nil {
		t.Fatalf("review placement = %+v, want the self pin (ruling 8's in-process arm) with no dispatch facts", review)
	}
	if review.LedgerTouching {
		t.Fatal("a gate must never pin as ledger-touching: only a task's PolicyActions can name a claims action")
	}

	closeOut := findPin(t, placements, "close-out")
	if !closeOut.Self || !closeOut.LedgerTouching {
		t.Fatalf("close-out placement = %+v, want the self pin still marked ledger-touching", closeOut)
	}
	implement := findPin(t, placements, "implement")
	if !implement.Self || implement.LedgerTouching {
		t.Fatalf("implement placement = %+v, want a plain self pin", implement)
	}
}

// The engine half of decision 001 (rulings 7–8): a gate whose placement only
// a REMOTE runner satisfies pins that runner exactly as a task would — the
// pinned queue engine.evaluateGate routes ActDispatchStage to, the eligible
// set, and the gate's own cpu/memory/restrictions — never ledger-touching.
// This is the test that stood as #3848's HOLD (refusing the start with the
// runner and queue named) until evaluateGate honoured a non-self gate pin;
// the same document with the restriction dropped pins self (the test above).
func TestPinStagePlacementsPinsRemoteGate(t *testing.T) {
	def := placementSpecV30(
		[]apiv1.Task{{Name: "implement", Type: apiv1.TaskAgentic, Goober: "reviewer", Goal: "implement", Next: "review"}},
		[]apiv1.Gate{placedReviewGate("review", "")},
	)
	placements, err := PinStagePlacements(agenticConfig(), reviewerConfigSet(), "web", def)
	if err != nil {
		t.Fatalf("PinStagePlacements: %v (a remote gate pin is honoured by evaluateGate's dispatch arm; the #3848 refusal must be gone)", err)
	}
	review := findPin(t, placements, "review")
	if review.Self {
		t.Fatalf("review placement = %+v, want the REMOTE pin: the gate's restriction is one only linux-agentic satisfies", review)
	}
	if want := dispatcher.QueueName("web", "linux-agentic"); review.Queue != want {
		t.Fatalf("review queue = %q, want %q (the pinned per-(gaggle × runner) queue evaluateGate dispatches on)", review.Queue, want)
	}
	if len(review.Eligible) != 1 || review.Eligible[0].Name != "linux-agentic" {
		t.Fatalf("review eligible = %+v, want exactly linux-agentic", review.Eligible)
	}
	if review.CPU != "1000m" || review.Memory != "2Gi" {
		t.Fatalf("review quantities = cpu %q memory %q, want the gate's own runsOn (ruling 5: never inherited)", review.CPU, review.Memory)
	}
	if len(review.Restrictions) == 0 {
		t.Fatalf("review placement = %+v, want the gate's declared restriction carried to the dispatcher", review)
	}
	if review.LedgerTouching {
		t.Fatal("a gate must never pin as ledger-touching")
	}
	// The task beside it keeps its own pin: a remote gate pin never leaks
	// into the task rows (ruling 6's name keying, in the remote case).
	implement := findPin(t, placements, "implement")
	if !implement.Self || implement.LedgerTouching {
		t.Fatalf("implement placement = %+v, want a plain self pin beside the placed gate", implement)
	}
}

// The index-shuffle discriminator (decision 001 ruling 6): the workflow is
// written with its gates: block BEFORE its tasks: block, the gate name sorts
// before the task name, and the workflow carries exactly one task — so a
// solver row for the gate sits at index 1 while def.Spec.Tasks has length 1.
// Positional keying (requirements[i] -> def.Spec.Tasks[i]) cannot survive
// this document; name keying pins both stages with the right facts. The gate
// declares a self-satisfiable placement so the hold above does not fire.
func TestPinStagePlacementsGateBeforeTaskInYAMLOrderKeysByName(t *testing.T) {
	const doc = `apiVersion: goobers.dev/v1alpha1
kind: Workflow
dslVersion: "3.0"
metadata:
  name: shuffled
spec:
  gaggle: web
  triggers:
    - type: backlog-item
  start: z-close-out
  gates:
    - name: a-review
      evaluator: agentic
      agentic:
        goober: reviewer
      runsOn:
        cpu: 1000m
        memory: 2Gi
      branches:
        pass: ""
        fail: "@abort"
  tasks:
    - name: z-close-out
      type: deterministic
      goal: close out
      policyActions: [claim-backlog-items]
      run:
        command: ["goobers", "backlog-query"]
      next: a-review
`
	var parsed apiv1.Workflow
	if err := yaml.Unmarshal([]byte(doc), &parsed); err != nil {
		t.Fatalf("decode fixture: %v", err)
	}
	def := workflow.Definition{Name: parsed.Name, DSLVersion: parsed.DSLVersion, Spec: parsed.Spec}

	placements, err := PinStagePlacements(agenticConfig(), reviewerConfigSet(), "web", def)
	if err != nil {
		t.Fatalf("PinStagePlacements: %v", err)
	}
	if len(placements) != 2 {
		t.Fatalf("placements = %+v, want the task pin and the gate pin", placements)
	}
	closeOut := findPin(t, placements, "z-close-out")
	if !closeOut.Self || !closeOut.LedgerTouching {
		t.Fatalf("z-close-out placement = %+v, want self + ledger-touching (its own facts)", closeOut)
	}
	review := findPin(t, placements, "a-review")
	if !review.Self || review.LedgerTouching || review.Queue != "" {
		t.Fatalf("a-review placement = %+v, want the gate's own self pin (never ledger-touching), never the task's facts", review)
	}
}

// Name keying is only as safe as name uniqueness: a task and a gate sharing a
// name would let the gate's ledgerFor=false silently erase the task's
// claims-action fact — the inverse of the ruling-6 mis-attribution. The 3.0
// compiler refuses duplicate state names first; this is the pin's own
// defence in depth for an uncompiled definition: an error, never a silent
// overwrite.
func TestPinStagePlacementsRefusesTaskAndGateSharingAName(t *testing.T) {
	def := placementSpecV30(
		[]apiv1.Task{{Name: "review", Type: apiv1.TaskDeterministic,
			PolicyActions: []string{"claim-backlog-items"},
			Run:           &apiv1.DeterministicRun{Command: []string{"goobers", "backlog-query"}}}},
		[]apiv1.Gate{selfPlacedReviewGate("review", "")},
	)
	placements, err := PinStagePlacements(agenticConfig(), reviewerConfigSet(), "web", def)
	if err == nil {
		t.Fatalf("PinStagePlacements = %+v, want a duplicate-name refusal", placements)
	}
	if !strings.Contains(err.Error(), `"review"`) || !strings.Contains(err.Error(), "stage names must be unique") {
		t.Fatalf("error = %v, want the duplicate stage name refused by name", err)
	}
}

// An agentic gate that declares no runsOn is UNPINNED: it evaluates in the
// control plane byte-identically to before the field existed (decision 001
// ruling 8), and never inherits a placement from the floor or the task.
func TestPinStagePlacementsLeavesUnplacedGateUnpinned(t *testing.T) {
	gate := placedReviewGate("review", "")
	gate.RunsOn = nil
	def := placementSpecV30(
		[]apiv1.Task{{Name: "implement", Type: apiv1.TaskAgentic, Goober: "reviewer", Goal: "implement", Next: "review"}},
		[]apiv1.Gate{gate},
	)
	placements, err := PinStagePlacements(agenticConfig(), reviewerConfigSet(), "web", def)
	if err != nil {
		t.Fatalf("PinStagePlacements: %v", err)
	}
	if len(placements) != 1 || placements[0].Stage != "implement" {
		t.Fatalf("placements = %+v, want the task pin alone (an unannotated gate is unpinned)", placements)
	}
}

// Zero-declaration invariance holds with a placed gate in the document: a
// self-only inventory still pins nothing (LocalMode), so the type-1/type-2
// paths never see a gate pin.
func TestPinStagePlacementsLocalModeIgnoresPlacedGate(t *testing.T) {
	def := placementSpecV30(
		[]apiv1.Task{{Name: "implement", Type: apiv1.TaskAgentic, Goober: "reviewer", Goal: "implement", Next: "review"}},
		[]apiv1.Gate{placedReviewGate("review", "")},
	)
	cfg := &instance.Config{Runners: []instance.RunnerEntry{{Name: "self", Host: "self"}}}
	placements, err := PinStagePlacements(cfg, reviewerConfigSet(), "web", def)
	if err != nil {
		t.Fatalf("PinStagePlacements: %v", err)
	}
	if placements != nil {
		t.Fatalf("placements = %+v, want nil on a local-mode inventory", placements)
	}
}
