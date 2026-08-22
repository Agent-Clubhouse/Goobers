package main

// Tests for the three-checkpoint admission (#3506, dsl-3.0.md §5):
// checkpoint 1's validate severity split (acceptance §9 items 4 and 8) and
// checkpoint 3's boot-never-kills daemon pass (acceptance §9 item 9,
// #2860). One fixture family: a 3.0 workflow with a satisfiable and an
// unsatisfiable runsOn variant against a declared runners: inventory.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

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
