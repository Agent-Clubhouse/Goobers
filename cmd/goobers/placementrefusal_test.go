package main

// Tests for the three-checkpoint admission (#3506, dsl-3.0.md §5):
// checkpoint 1's validate severity split (acceptance §9 items 4 and 8) and
// checkpoint 3's boot-never-kills daemon pass (acceptance §9 item 9,
// #2860). One fixture family: a 3.0 workflow with a satisfiable and an
// unsatisfiable runsOn variant against a declared runners: inventory.

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/readservice"
)

// unsatisfiableV30WorkflowYAML requires an OS the fixture inventory does not
// offer; the satisfiable variant differs only in runsOn.os.
const unsatisfiableV30WorkflowYAML = `apiVersion: goobers.dev/v1alpha1
kind: Workflow
dslVersion: "3.0"
metadata:
  name: win-build
spec:
  gaggle: example
  triggers:
    - type: schedule
      schedule: "@every 24h"
  start: build
  tasks:
    - name: build
      type: deterministic
      goal: run a no-op build that requires windows
      runsOn:
        os: windows
      run:
        command: ["true"]
`

// declaredSelfInventoryYAML is a schemaVersion-2 instance fragment declaring
// a single self runner that claims linux — a declared inventory whose mode
// is still local (self-only), pinned to a deterministic OS so the solve does
// not depend on the test host.
const declaredSelfInventoryYAML = `schemaVersion: 2
runners:
  - name: self
    host: self
    provides:
      os: linux
`

// declareInventory rewrites the scaffolded instance.yaml with the declared
// self inventory, and opts the manifest into preview DSL versions so the 3.0
// workflow loads (DSL 3.0 is preview until its supportmatrix flip).
func declareInventory(t *testing.T, root string) {
	t.Helper()
	replaceInFile(t, filepath.Join(root, "instance.yaml"), "runner: {}", "runner: {}\n"+declaredSelfInventoryYAML)
	replaceInFile(t, filepath.Join(root, "config", "manifest.yaml"),
		"metadata:\n  name: example-instance",
		"metadata:\n  name: example-instance\n  annotations:\n    goobers.dev/allow-preview-features: \"true\"")
}

func writeSecondWorkflow(t *testing.T, root, yaml string) {
	t.Helper()
	path := filepath.Join(root, "config", "gaggles", "example", "workflows", "win-build.yaml")
	if err := os.WriteFile(path, []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
}

// declareRemoteRunner appends an engine: block (a remote runner requires the
// engine connection, RNR002) and adds one remote runner entry after the
// declared self entry.
func declareRemoteRunner(t *testing.T, root, entryYAML string) {
	t.Helper()
	appendToFile(t, filepath.Join(root, "instance.yaml"),
		"engine:\n  hostPort: temporal.example:7233\n")
	replaceInFile(t, filepath.Join(root, "instance.yaml"),
		"    provides:\n      os: linux\n",
		"    provides:\n      os: linux\n"+entryYAML)
}

// remoteOnlyV30WorkflowYAML is the reviewer's finding-1 probe shape: a
// builtin stage (derives no self-only tag) whose runsOn only a remote
// runner can satisfy. The base spelling requires windows; capability
// variants string-replace the runsOn block.
const remoteOnlyV30WorkflowYAML = `apiVersion: goobers.dev/v1alpha1
kind: Workflow
dslVersion: "3.0"
metadata:
  name: win-build
spec:
  gaggle: example
  triggers:
    - type: schedule
      schedule: "@every 24h"
  start: build
  tasks:
    - name: build
      type: deterministic
      goal: run a builtin stage that only a remote runner satisfies
      runsOn:
        os: windows
      run:
        command: ["goobers", "docs-churn"]
`

// TestValidatePlacementSever: acceptance §9 item 4. The same unsatisfiable
// 3.0 workflow is an ERROR (exit 1, RNR001) when the instance declares a
// runners: inventory, and a WARNING (exit 0) on the inventory-less instance
// — the #3497 exit-0 trap closed for the declared case only.
func TestValidatePlacementSeverity(t *testing.T) {
	root := initDeterministicDemo(t)
	declareInventory(t, root)
	writeSecondWorkflow(t, root, unsatisfiableV30WorkflowYAML)

	code, stdout, stderr := runArgs(t, "validate", root)
	if code != 1 {
		t.Fatalf("validate code = %d, want 1 (declared inventory cannot satisfy the stage); stdout=%q stderr=%q", code, stdout, stderr)
	}
	for _, want := range []string{
		"ERROR RNR001 Workflow/win-build",
		`stage "build" requires os "windows"`,
		`runner "self" provides os "linux"`,
		"no run of this workflow can be scheduled on the declared inventory",
		"the declared runners: inventory cannot satisfy the configuration",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("validate stdout missing %q:\n%s", want, stdout)
		}
	}

	// Same workflow, inventory-less instance: advisory warning, exit 0.
	legacy := initDeterministicDemo(t)
	replaceInFile(t, filepath.Join(legacy, "config", "manifest.yaml"),
		"metadata:\n  name: example-instance",
		"metadata:\n  name: example-instance\n  annotations:\n    goobers.dev/allow-preview-features: \"true\"")
	writeSecondWorkflow(t, legacy, unsatisfiableV30WorkflowYAML)
	code, stdout, stderr = runArgs(t, "validate", legacy)
	if code != 0 {
		t.Fatalf("inventory-less validate code = %d, want 0 (warning only); stdout=%q stderr=%q", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "WARNING RNR001 Workflow/win-build") {
		t.Errorf("inventory-less validate must warn RNR001:\n%s", stdout)
	}

	// Satisfiable variant on the declared inventory: clean.
	satisfiable := strings.Replace(unsatisfiableV30WorkflowYAML, "os: windows", "os: linux", 1)
	writeSecondWorkflow(t, root, satisfiable)
	code, stdout, stderr = runArgs(t, "validate", root)
	if code != 0 {
		t.Fatalf("satisfiable validate code = %d, want 0; stdout=%q stderr=%q", code, stdout, stderr)
	}
	if strings.Contains(stdout, "RNR001") {
		t.Errorf("satisfiable variant must produce no RNR001:\n%s", stdout)
	}
}

// TestValidateQuantityFindings: RNR003 errors on a distributed-shape
// inventory whose ceilings no runner covers; RNR004 stays an advisory
// warning on a local-mode (self-only) inventory — every severity branch of
// the quantity table row.
func TestValidateQuantityFindings(t *testing.T) {
	root := initDeterministicDemo(t)
	declareInventory(t, root)
	heavy := strings.Replace(unsatisfiableV30WorkflowYAML,
		"      runsOn:\n        os: windows\n",
		"      runsOn:\n        cpu: \"64\"\n", 1)
	writeSecondWorkflow(t, root, heavy)

	// Self-only inventory with a declared ceiling: local mode, advisory.
	replaceInFile(t, filepath.Join(root, "instance.yaml"),
		"    provides:\n      os: linux\n",
		"    provides:\n      os: linux\n      cpu: 4000m\n")
	code, stdout, stderr := runArgs(t, "validate", root)
	if code != 0 {
		t.Fatalf("local-mode quantity validate code = %d, want 0 (RNR004 advisory); stdout=%q stderr=%q", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "WARNING RNR004 Workflow/win-build") ||
		!strings.Contains(stdout, "cpu minimum 64") {
		t.Errorf("local-mode quantity shortfall must warn RNR004 naming the resource:\n%s", stdout)
	}

	// Add a remote runner: distributed shape, quantity becomes a hard
	// constraint nothing covers — RNR003 at error severity.
	appendToFile(t, filepath.Join(root, "instance.yaml"),
		"engine:\n  hostPort: temporal.example:7233\n")
	replaceInFile(t, filepath.Join(root, "instance.yaml"),
		"      cpu: 4000m\n",
		"      cpu: 4000m\n  - name: ci\n    host: ghcr.io/example/ci:v1\n    provides:\n      os: linux\n      cpu: 8000m\n")
	code, stdout, stderr = runArgs(t, "validate", root)
	if code != 1 {
		t.Fatalf("distributed quantity validate code = %d, want 1 (RNR003 error); stdout=%q stderr=%q", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "ERROR RNR003 Workflow/win-build") ||
		!strings.Contains(stdout, "exceed every eligible runner's declared ceiling") {
		t.Errorf("distributed quantity shortfall must error RNR003:\n%s", stdout)
	}

	// RNR003's warning branch: the same tree through the startup preflight —
	// advisory there, so the boot proceeds to checkpoint 3's per-workflow
	// refusal instead of dying on the config.
	var preflight strings.Builder
	if code := runStartupConfigPreflight(root, false, &preflight); code != 0 {
		t.Fatalf("startup preflight code = %d, want 0 (RNR003 demoted at boot); stderr=%q", code, preflight.String())
	}
}

// TestRefusedWorkflowStatusLines: the `goobers status` rendering names each
// refused workflow with its solver diagnostic.
func TestRefusedWorkflowStatusLines(t *testing.T) {
	if got := refusedWorkflowStatusLines(readservice.SchedulerStatus{}); got != "" {
		t.Fatalf("no refusals must render nothing, got %q", got)
	}
	got := refusedWorkflowStatusLines(readservice.SchedulerStatus{RefusedWorkflows: []readservice.WorkflowRefusalStatus{
		{Gaggle: "example", Workflow: "win-build", Reason: `stage "build" requires os "windows"; no runner satisfies it`},
	}})
	for _, want := range []string{"example/win-build", "refused", `os "windows"`} {
		if !strings.Contains(got, want) {
			t.Errorf("status line missing %q: %q", want, got)
		}
	}
}

// TestStartupPreflightKeepsPlacementFindingsAdvisory: the daemon's boot-time
// validation pass must not turn checkpoint 1's placement errors back into a
// boot-kill — at boot their consequence is a per-workflow refusal
// (checkpoint 3, #2860), so the preflight reports them as warnings and
// passes.
func TestStartupPreflightKeepsPlacementFindingsAdvisory(t *testing.T) {
	root := initDeterministicDemo(t)
	declareInventory(t, root)
	writeSecondWorkflow(t, root, unsatisfiableV30WorkflowYAML)

	// The same tree fails operator-invoked validate (checkpoint 1)...
	if code, _, _ := runArgs(t, "validate", root); code != 1 {
		t.Fatalf("operator validate code = %d, want 1", code)
	}
	// ...and passes the startup preflight with the finding demoted.
	var stderr strings.Builder
	if code := runStartupConfigPreflight(root, false, &stderr); code != 0 {
		t.Fatalf("startup preflight code = %d, want 0 (boot never kills, #2860); stderr=%q", code, stderr.String())
	}
}

// TestValidateSourceTreePlacementAdvisory: `goobers validate --source-tree`
// has no real instance.yaml — it solves against instance.yaml.example — so
// its placement findings are advisory-only warnings even when the example
// declares an inventory that cannot satisfy a stage (RNR001's and RNR003's
// warning branches).
func TestValidateSourceTreePlacementAdvisory(t *testing.T) {
	root := initDeterministicDemo(t)
	declareInventory(t, root)
	writeSecondWorkflow(t, root, unsatisfiableV30WorkflowYAML)
	tree := filepath.Join(root, "config")
	instanceYAML, err := os.ReadFile(filepath.Join(root, "instance.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tree, "instance.yaml.example"), instanceYAML, 0o644); err != nil {
		t.Fatal(err)
	}

	code, stdout, stderr := runArgs(t, "validate", "--source-tree", tree)
	if code != 0 {
		t.Fatalf("source-tree validate code = %d, want 0 (advisory-only); stdout=%q stderr=%q", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "WARNING RNR001 Workflow/win-build") {
		t.Errorf("source-tree validate must carry the advisory RNR001 warning:\n%s", stdout)
	}
	if strings.Contains(stdout, "ERROR RNR001") {
		t.Errorf("source-tree placement findings must never be errors:\n%s", stdout)
	}
}

// TestValidateRefusesRemoteRunnerWithoutEngine: acceptance §9 item 8 — a
// runner entry with an image host and no engine: block is refused with the
// stable RNR002 code.
func TestValidateRefusesRemoteRunnerWithoutEngine(t *testing.T) {
	root := initDeterministicDemo(t)
	replaceInFile(t, filepath.Join(root, "instance.yaml"), "runner: {}",
		"runner: {}\nschemaVersion: 2\nrunners:\n  - name: ci\n    host: ghcr.io/example/ci:v1\n    provides:\n      os: linux\n")

	code, stdout, stderr := runArgs(t, "validate", root)
	if code != 1 {
		t.Fatalf("validate code = %d, want 1; stdout=%q stderr=%q", code, stdout, stderr)
	}
	for _, want := range []string{
		"ERROR RNR002",
		"declares no engine: block",
		"INVALID instance.yaml",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("validate stdout missing %q:\n%s", want, stdout)
		}
	}
}

// TestDaemonStartsWithOneUnsatisfiableWorkflow: acceptance §9 item 9 and
// goobernetes-architecture.md §11 item 8 — a booting daemon whose config
// contains one unplaceable workflow STARTS, serves every other workflow, and
// carries the refused workflow's named diagnostic for per-run refusal
// (#2860; the CheckCapabilityRequirements boot-kill path is gone).
func TestDaemonStartsWithOneUnsatisfiableWorkflow(t *testing.T) {
	root := initDeterministicDemo(t)
	declareInventory(t, root)
	writeSecondWorkflow(t, root, unsatisfiableV30WorkflowYAML)

	var wg sync.WaitGroup
	setup, err := buildSchedulerSetup(context.Background(), instance.NewLayout(root), &wg)
	if err != nil {
		t.Fatalf("the daemon must start with an unsatisfiable workflow in config (boot never kills, #2860): %v", err)
	}
	defer setup.Shutdown(context.Background())

	var refused, healthy *localscheduler.WorkflowEntry
	for i := range setup.Entries {
		switch setup.Entries[i].Workflow {
		case "win-build":
			refused = &setup.Entries[i]
		case "default-implement":
			healthy = &setup.Entries[i]
		}
	}
	if refused == nil || healthy == nil {
		t.Fatalf("expected both workflows among entries: %+v", setup.Entries)
	}
	if refused.PlacementRefusal == "" {
		t.Fatal("the unsatisfiable workflow must carry a placement refusal")
	}
	for _, want := range []string{`stage "build"`, `os "windows"`, `runner "self"`} {
		if !strings.Contains(refused.PlacementRefusal, want) {
			t.Errorf("refusal diagnostic missing %q: %s", want, refused.PlacementRefusal)
		}
	}
	if healthy.PlacementRefusal != "" {
		t.Fatalf("the satisfiable workflow must serve, got refusal %q", healthy.PlacementRefusal)
	}

	// The scheduler journals the refusal when it learns the entries — the
	// record `goobers status` surfaces.
	localscheduler.New(setup.Entries, setup.InstanceLog)
	events, err := journal.ReadInstanceLog(setup.InstanceLog.Dir())
	if err != nil {
		t.Fatal(err)
	}
	var sawRefusal bool
	for _, ev := range events {
		if ev.Type == journal.EventWorkflowRefused && ev.Workflow == "win-build" {
			sawRefusal = true
			if !strings.Contains(ev.Reason, `os "windows"`) {
				t.Errorf("workflow.refused must carry the named diagnostic: %+v", ev)
			}
		}
		if ev.Type == journal.EventWorkflowRefused && ev.Workflow == "default-implement" {
			t.Errorf("the healthy workflow must not be refused: %+v", ev)
		}
	}
	if !sawRefusal {
		t.Errorf("expected a workflow.refused event for win-build: %+v", events)
	}
}

// TestRemoteOnlyStageValidatesButBootRefuses is the finding-1 (blocker)
// regression in the reviewer's probe shape: a stage satisfiable ONLY by a
// declared remote runner VALIDATES clean (checkpoint 1 judges config
// validity against the whole declared inventory), and is REFUSED at boot
// and at dispatch (checkpoints 2/3 judge execution placement against the
// substrate that actually executes — self only, until #3513) with a named
// diagnostic, journaled workflow.refused — and nothing ever executes on the
// daemon host that does not satisfy the stage.
func TestRemoteOnlyStageValidatesButBootRefuses(t *testing.T) {
	root := initDeterministicDemo(t)
	declareInventory(t, root)
	declareRemoteRunner(t, root, "  - name: ci\n    host: ghcr.io/example/ci:v1\n    provides:\n      os: windows\n")
	writeSecondWorkflow(t, root, remoteOnlyV30WorkflowYAML)

	// Checkpoint 1: the config is VALID — the declared inventory satisfies
	// the stage (the remote windows runner). Exit 0, no placement finding.
	code, stdout, stderr := runArgs(t, "validate", root)
	if code != 0 {
		t.Fatalf("validate code = %d, want 0 (a remote-satisfiable config is valid); stdout=%q stderr=%q", code, stdout, stderr)
	}
	if strings.Contains(stdout, "RNR001") {
		t.Errorf("checkpoint 1 must not flag a remote-satisfiable stage:\n%s", stdout)
	}

	// Checkpoint 3: boot marks the workflow refused with the substrate
	// diagnostic naming where it COULD place and the #3513 pointer.
	var wg sync.WaitGroup
	setup, err := buildSchedulerSetup(context.Background(), instance.NewLayout(root), &wg)
	if err != nil {
		t.Fatalf("boot must never kill (#2860): %v", err)
	}
	defer setup.Shutdown(context.Background())
	var refused *localscheduler.WorkflowEntry
	for i := range setup.Entries {
		if setup.Entries[i].Workflow == "win-build" {
			refused = &setup.Entries[i]
		}
	}
	if refused == nil {
		t.Fatalf("win-build missing from entries: %+v", setup.Entries)
	}
	for _, want := range []string{
		`placeable only on runner(s) [ci (host: ghcr.io/example/ci:v1)]`,
		"distributed dispatch arrives with #3513",
	} {
		if !strings.Contains(refused.PlacementRefusal, want) {
			t.Errorf("refusal diagnostic missing %q: %s", want, refused.PlacementRefusal)
		}
	}

	// The scheduler journals the refusal and refuses dispatch; nothing
	// executes on self (checkpoint 2).
	sched := localscheduler.New(setup.Entries, setup.InstanceLog)
	if _, err := sched.Trigger(context.Background(), "win-build", time.Now()); err == nil {
		t.Fatal("dispatch of a remote-only workflow must be refused")
	} else {
		var rejected *localscheduler.TriggerRejectedError
		if !errors.As(err, &rejected) {
			t.Fatalf("expected *TriggerRejectedError, got %T: %v", err, err)
		}
		if !strings.HasPrefix(rejected.Reason, localscheduler.ReasonPlacementUnsatisfiable) ||
			!strings.Contains(rejected.Reason, "#3513") {
			t.Errorf("dispatch refusal must carry the substrate diagnostic: %q", rejected.Reason)
		}
	}
	events, err := journal.ReadInstanceLog(setup.InstanceLog.Dir())
	if err != nil {
		t.Fatal(err)
	}
	var sawRefusal bool
	for _, ev := range events {
		if ev.Type == journal.EventWorkflowRefused && ev.Workflow == "win-build" {
			sawRefusal = true
			if !strings.Contains(ev.Reason, "#3513") {
				t.Errorf("workflow.refused must carry the substrate diagnostic: %+v", ev)
			}
		}
		if ev.Type == journal.EventRunStarted {
			t.Errorf("nothing may execute on the daemon host: %+v", ev)
		}
	}
	if !sawRefusal {
		t.Errorf("expected a workflow.refused event for win-build: %+v", events)
	}
}

// TestRemoteOnlyCapabilityRefusalSurfacesInStatus verifies the finding-2
// ruling: on a declared inventory the capability axis is enforced and
// SURFACED — a stage needing a capability only a remote runner claims boots
// into a workflow.refused naming the capability and the remote-only
// placement, and `goobers status` (text and --json shapes) shows it, so the
// operator signal that used to come from CheckCapabilityRequirements
// survives declared inventories.
func TestRemoteOnlyCapabilityRefusalSurfacesInStatus(t *testing.T) {
	root := initDeterministicDemo(t)
	declareInventory(t, root)
	declareRemoteRunner(t, root, "  - name: ci\n    host: ghcr.io/example/ci:v1\n    provides:\n      os: linux\n      capabilities: [\"dotnet@8\"]\n")
	dotnet := strings.Replace(remoteOnlyV30WorkflowYAML,
		"      runsOn:\n        os: windows\n",
		"      runsOn:\n        capabilities: [\"dotnet@8\"]\n", 1)
	writeSecondWorkflow(t, root, dotnet)

	// Validates clean: the declared inventory claims the capability.
	if code, stdout, stderr := runArgs(t, "validate", root); code != 0 {
		t.Fatalf("validate code = %d, want 0; stdout=%q stderr=%q", code, stdout, stderr)
	}

	var wg sync.WaitGroup
	setup, err := buildSchedulerSetup(context.Background(), instance.NewLayout(root), &wg)
	if err != nil {
		t.Fatalf("boot must never kill (#2860): %v", err)
	}
	defer setup.Shutdown(context.Background())
	localscheduler.New(setup.Entries, setup.InstanceLog)

	events, err := journal.ReadInstanceLog(setup.InstanceLog.Dir())
	if err != nil {
		t.Fatal(err)
	}
	var reason string
	for _, ev := range events {
		if ev.Type == journal.EventWorkflowRefused && ev.Workflow == "win-build" {
			reason = ev.Reason
		}
	}
	for _, want := range []string{
		"missing capabilities dotnet@8",
		`placeable only on runner(s) [ci (host: ghcr.io/example/ci:v1)]`,
		"#3513",
	} {
		if !strings.Contains(reason, want) {
			t.Errorf("workflow.refused reason missing %q: %q", want, reason)
		}
	}

	// The same journal drives the status projection: text and --json both
	// carry the refusal.
	service, err := readservice.NewLocal(readservice.LocalSources{
		Layout:      instance.NewLayout(root),
		Definitions: setup.Definitions,
	}, func() bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	status, err := service.SchedulerStatus(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	text := refusedWorkflowStatusLines(status)
	for _, want := range []string{"example/win-build", "dotnet@8", "#3513"} {
		if !strings.Contains(text, want) {
			t.Errorf("status text missing %q: %q", want, text)
		}
	}
	blob, err := json.Marshal(statusJSONOutput{RefusedWorkflows: status.RefusedWorkflows})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"refusedWorkflows", "dotnet@8", "#3513"} {
		if !strings.Contains(string(blob), want) {
			t.Errorf("status --json missing %q: %s", want, blob)
		}
	}
}

// TestValidateSelfOSUnknownIsMachineIndependent is the finding-4 regression:
// with a declared inventory whose self entry declares no provides.os, an
// os-requiring stage must produce the SAME findings and exit code on every
// GOOS — a warning with guidance, never a host-dependent silent pass or
// error. Two stages pin the two OSes a biased substitution would treat
// differently: whatever the validating host runs, neither may satisfy
// statically (no silent pass) and neither may error (no exit-code flip) —
// asserting both shapes at once is what makes the test itself
// machine-independent instead of skipping per platform.
func TestValidateSelfOSUnknownIsMachineIndependent(t *testing.T) {
	root := initDeterministicDemo(t)
	declareInventory(t, root)
	// Strip the fixture's declared os: the self entry becomes os-UNKNOWN.
	replaceInFile(t, filepath.Join(root, "instance.yaml"),
		"    provides:\n      os: linux\n", "")
	twoOS := strings.Replace(remoteOnlyV30WorkflowYAML,
		"      run:\n        command: [\"goobers\", \"docs-churn\"]\n",
		"      run:\n        command: [\"goobers\", \"docs-churn\"]\n      next: lin\n"+
			"    - name: lin\n      type: deterministic\n      goal: needs linux\n      runsOn:\n        os: linux\n      run:\n        command: [\"goobers\", \"docs-churn\"]\n", 1)
	writeSecondWorkflow(t, root, twoOS)

	code, stdout, stderr := runArgs(t, "validate", root)
	if code != 0 {
		t.Fatalf("validate code = %d, want 0 on every GOOS (os-unknown self downgrades to warning); stdout=%q stderr=%q", code, stdout, stderr)
	}
	if strings.Contains(stdout, "ERROR RNR001") {
		t.Errorf("an os-unknown-self finding must never be an error (machine-dependent exit code):\n%s", stdout)
	}
	for _, want := range []string{
		"WARNING RNR001 Workflow/win-build",
		`os "windows"`,
		`os "linux"`,
		"declare provides.os on the self runner",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("validate stdout missing %q:\n%s", want, stdout)
		}
	}
}

// TestValidateNetworkNoneRequiresDeclaredEnforcement is the finding-5
// regression: the solver consults DECLARED restrictions only — self
// enforces nothing implicitly (no execution path wires runsOn restrictions
// into executor isolation until #3516), so network:none on a default
// self-only inventory is honestly unsatisfiable: RNR001 at error severity
// iff a runners: inventory is declared, warning on the inventory-less
// instance — and a self entry explicitly declaring the effect is eligible
// (declared, trusted per RRQ-1).
func TestValidateNetworkNoneRequiresDeclaredEnforcement(t *testing.T) {
	netless := strings.Replace(remoteOnlyV30WorkflowYAML,
		"      runsOn:\n        os: windows\n",
		"      runsOn:\n        restrictions: [\"network:none\"]\n", 1)

	// Declared self-only inventory, no restrictions declared: error.
	root := initDeterministicDemo(t)
	declareInventory(t, root)
	writeSecondWorkflow(t, root, netless)
	code, stdout, stderr := runArgs(t, "validate", root)
	if code != 1 {
		t.Fatalf("validate code = %d, want 1 (undeclared enforcement on a declared inventory); stdout=%q stderr=%q", code, stdout, stderr)
	}
	for _, want := range []string{
		"ERROR RNR001 Workflow/win-build",
		"does not enforce restrictions network:none",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("validate stdout missing %q:\n%s", want, stdout)
		}
	}

	// The self entry explicitly declaring the effect: eligible, clean.
	replaceInFile(t, filepath.Join(root, "instance.yaml"),
		"    provides:\n      os: linux\n",
		"    provides:\n      os: linux\n    restrictions: [\"network:none\"]\n")
	code, stdout, stderr = runArgs(t, "validate", root)
	if code != 0 {
		t.Fatalf("validate code = %d, want 0 (declared enforcement is trusted); stdout=%q stderr=%q", code, stdout, stderr)
	}
	if strings.Contains(stdout, "RNR001") {
		t.Errorf("a declared network:none self runner must satisfy the stage:\n%s", stdout)
	}

	// Default (inventory-less) instance: advisory warning, exit 0.
	legacy := initDeterministicDemo(t)
	replaceInFile(t, filepath.Join(legacy, "config", "manifest.yaml"),
		"metadata:\n  name: example-instance",
		"metadata:\n  name: example-instance\n  annotations:\n    goobers.dev/allow-preview-features: \"true\"")
	writeSecondWorkflow(t, legacy, netless)
	code, stdout, stderr = runArgs(t, "validate", legacy)
	if code != 0 {
		t.Fatalf("inventory-less validate code = %d, want 0 (warning only); stdout=%q stderr=%q", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "WARNING RNR001 Workflow/win-build") {
		t.Errorf("inventory-less network:none must warn RNR001:\n%s", stdout)
	}
}

// TestDaemonZeroDeclarationKeepsLegacyBehavior: the invariance guard
// (goobernetes-architecture.md §11 item 1): with no runners: inventory, no
// entry carries a placement refusal and no workflow.refused event is
// journaled — the capability union check stays per-run at dispatch,
// byte-identical to previous releases.
func TestDaemonZeroDeclarationKeepsLegacyBehavior(t *testing.T) {
	root := initDeterministicDemo(t)
	// An unclaimed capability on the legacy surface: startup warns (not
	// fatal), and the refusal happens per-run at dispatch.
	appendToFile(t, filepath.Join(root, "config", "gaggles", "example", "gaggle.yaml"),
		"  requiredCapabilities:\n    - nosuchtoolchain@42\n")

	var wg sync.WaitGroup
	setup, err := buildSchedulerSetup(context.Background(), instance.NewLayout(root), &wg)
	if err != nil {
		t.Fatalf("a zero-declaration instance must start with an unclaimed capability: %v", err)
	}
	defer setup.Shutdown(context.Background())
	for _, entry := range setup.Entries {
		if entry.PlacementRefusal != "" {
			t.Fatalf("zero-declaration entries must never carry a boot refusal, got %q on %q", entry.PlacementRefusal, entry.Workflow)
		}
	}
	localscheduler.New(setup.Entries, setup.InstanceLog)
	events, err := journal.ReadInstanceLog(setup.InstanceLog.Dir())
	if err != nil {
		t.Fatal(err)
	}
	for _, ev := range events {
		if ev.Type == journal.EventWorkflowRefused {
			t.Fatalf("zero-declaration instances must journal no workflow.refused (byte-identical journals): %+v", ev)
		}
	}
}

// placedGateV30WorkflowYAML is a 3.0 workflow whose only placement
// requirement sits on an AGENTIC GATE (decision 001): the reviewer requires
// windows, which the declared linux self runner cannot satisfy.
const placedGateV30WorkflowYAML = `apiVersion: goobers.dev/v1alpha1
kind: Workflow
dslVersion: "3.0"
metadata:
  name: win-build
spec:
  gaggle: example
  triggers:
    - type: schedule
      schedule: "@every 24h"
  start: build
  tasks:
    - name: build
      type: deterministic
      goal: run a no-op build
      run:
        command: ["true"]
      next: review
  gates:
    - name: review
      evaluator: agentic
      agentic:
        goober: reviewer
      runsOn:
        os: windows
        cpu: 1000m
        memory: 2Gi
      branches:
        pass: ""
        fail: "@abort"
        needs-changes: build
`

// writeReviewerGoober adds the reviewer goober the placed gate names to the
// deterministic demo (which drops the starter's agentic goober).
func writeReviewerGoober(t *testing.T, root string) {
	t.Helper()
	dir := filepath.Join(root, "config", "gaggles", "example", "goobers", "reviewer")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	const goober = `apiVersion: goobers.dev/v1alpha1
kind: Goober
metadata:
  name: reviewer
spec:
  gaggle: example
  role: reviewer
  instructions: instructions.md
  harness: copilot
  capabilities: [agent:model]
`
	if err := os.WriteFile(filepath.Join(dir, "goober.yaml"), []byte(goober), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "instructions.md"), []byte("# reviewer\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestValidatePlacementCoversPlacedGate: checkpoint 1 solves a placed agentic
// gate's requirement exactly as a task's (decision 001): RNR001 at error
// severity on the declared inventory, attributed to the gate's own runsOn
// block; the satisfiable variant is clean.
func TestValidatePlacementCoversPlacedGate(t *testing.T) {
	root := initDeterministicDemo(t)
	declareInventory(t, root)
	writeReviewerGoober(t, root)
	writeSecondWorkflow(t, root, placedGateV30WorkflowYAML)

	code, stdout, stderr := runArgs(t, "validate", root)
	if code != 1 {
		t.Fatalf("validate code = %d, want 1 (the gate cannot place on the declared inventory); stdout=%q stderr=%q", code, stdout, stderr)
	}
	for _, want := range []string{
		"ERROR RNR001 Workflow/win-build",
		`stage "review" requires os "windows"`,
		"capabilities [harness:copilot]",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("validate stdout missing %q:\n%s", want, stdout)
		}
	}
	// The finding is attributed to the GATE's own runsOn block (the JSON
	// pointer rides the --json rendering only).
	code, stdout, stderr = runArgs(t, "validate", "--json", root)
	if code != 1 {
		t.Fatalf("validate --json code = %d, want 1; stdout=%q stderr=%q", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "/spec/gates/0/runsOn") {
		t.Errorf("validate --json must attribute the finding to the gate's runsOn block:\n%s", stdout)
	}

	satisfiable := strings.Replace(placedGateV30WorkflowYAML, "os: windows", "os: linux", 1)
	writeSecondWorkflow(t, root, satisfiable)
	code, stdout, stderr = runArgs(t, "validate", root)
	if code != 0 {
		t.Fatalf("satisfiable validate code = %d, want 0; stdout=%q stderr=%q", code, stdout, stderr)
	}
	if strings.Contains(stdout, "RNR001") || strings.Contains(stdout, "WF023") {
		t.Errorf("satisfiable placed gate must produce no placement finding:\n%s", stdout)
	}
}
