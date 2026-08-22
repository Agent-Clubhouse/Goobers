package runnersolve

import (
	"reflect"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/goobers/goobers/internal/runnercap"
)

func quantity(t *testing.T, s string) *resource.Quantity {
	t.Helper()
	q, err := resource.ParseQuantity(s)
	if err != nil {
		t.Fatalf("parse quantity %q: %v", s, err)
	}
	return &q
}

func mustEligible(t *testing.T, result Result, stage string, want ...string) {
	t.Helper()
	for _, s := range result.Stages {
		if s.Stage != stage {
			continue
		}
		if s.Unsat != nil {
			t.Fatalf("stage %q unsatisfiable: %s", stage, s.Unsat.Diagnostic)
		}
		if !reflect.DeepEqual(s.Eligible, want) {
			t.Fatalf("stage %q eligible = %v, want %v", stage, s.Eligible, want)
		}
		return
	}
	t.Fatalf("stage %q not in result", stage)
}

func mustUnsat(t *testing.T, result Result, stage string, kind UnsatKind) *Unsat {
	t.Helper()
	for _, s := range result.Stages {
		if s.Stage != stage {
			continue
		}
		if s.Unsat == nil {
			t.Fatalf("stage %q satisfiable (eligible %v), want unsatisfiable", stage, s.Eligible)
		}
		if s.Unsat.Kind != kind {
			t.Fatalf("stage %q unsat kind = %q, want %q (diagnostic: %s)", stage, s.Unsat.Kind, kind, s.Unsat.Diagnostic)
		}
		return s.Unsat
	}
	t.Fatalf("stage %q not in result", stage)
	return nil
}

// TestSolveOSExplicitComplete: unspecified = no requirement (D3); a declared
// OS matches exactly; a runner claiming no OS satisfies no OS requirement.
func TestSolveOSExplicitComplete(t *testing.T) {
	inv := Inventory{Runners: []Runner{
		{Name: "linux-ci", OS: OSLinux},
		{Name: "win-ci", OS: OSWindows},
		{Name: "unclaimed"},
	}}
	result := Solve(inv, []StageRequirement{
		{Stage: "any-os"},
		{Stage: "needs-windows", OS: OSWindows},
		{Stage: "needs-macos", OS: OSMacOS},
	})
	mustEligible(t, result, "any-os", "linux-ci", "win-ci", "unclaimed")
	mustEligible(t, result, "needs-windows", "win-ci")
	unsat := mustUnsat(t, result, "needs-macos", UnsatRequirement)
	for _, want := range []string{`stage "needs-macos"`, `os "macOS"`, `runner "linux-ci" provides os "linux"`, `runner "unclaimed" claims no os`} {
		if !strings.Contains(unsat.Diagnostic, want) {
			t.Errorf("diagnostic missing %q: %s", want, unsat.Diagnostic)
		}
	}
}

// TestSolveCapabilityMembership: exact set membership, RNR001 when no runner
// claims a required token, diagnostic naming the missing tokens per runner.
func TestSolveCapabilityMembership(t *testing.T) {
	inv := Inventory{Runners: []Runner{
		{Name: "go-runner", Capabilities: []string{"go@1.26", "make"}},
		{Name: "dotnet-runner", Capabilities: []string{"dotnet@8"}},
	}}
	result := Solve(inv, []StageRequirement{
		{Stage: "build", Capabilities: []string{"go@1.26", "make"}},
		{Stage: "impossible", Capabilities: []string{"xcode"}},
	})
	mustEligible(t, result, "build", "go-runner")
	unsat := mustUnsat(t, result, "impossible", UnsatRequirement)
	if !strings.Contains(unsat.Diagnostic, `runner "go-runner" is missing capabilities xcode`) ||
		!strings.Contains(unsat.Diagnostic, `runner "dotnet-runner" is missing capabilities xcode`) {
		t.Errorf("diagnostic must name the missing capability per runner: %s", unsat.Diagnostic)
	}
}

// TestMissingCapabilitiesMatchesRunnercap pins the checkpoint-2 byte-identity
// contract: for every author-spellable token, the solver's self-runner miss
// list is exactly runnercap.Claimed.Missing — same tokens, same order, same
// de-duplication — so the scheduler's refusal diagnostics for legacy configs
// cannot change. The adversarial rows pin the finding-3 regression: the
// plain "shell" token is AUTHOR-SPELLABLE (it passes the token grammar,
// unlike the colon-namespaced derived run:shell), so a legacy config
// requiring it must behave byte-identically to runnercap — never picked up
// by the self-implicit derived-tag skip — including under duplication,
// reordering, and case variants (matching is exact and case-sensitive).
func TestMissingCapabilitiesMatchesRunnercap(t *testing.T) {
	cases := []struct {
		name     string
		claims   []string
		required []string
	}{
		{
			name:     "declared tokens",
			claims:   []string{"dotnet@8", "go@1.26"},
			required: []string{"dotnet@10", "go@1.26", "xcode", "dotnet@10", "netfx@4.8"},
		},
		{
			name:     "author-spelled shell unclaimed",
			claims:   []string{"dotnet@8"},
			required: []string{"shell"},
		},
		{
			name:     "author-spelled shell claimed, case variants unclaimed",
			claims:   []string{"shell"},
			required: []string{"shell", "Shell", "SHELL"},
		},
		{
			name:     "shell duplicated in the required set",
			claims:   nil,
			required: []string{"shell", "shell", "xcode", "shell"},
		},
		{
			name:     "shell reordered among declared tokens",
			claims:   []string{"go@1.26"},
			required: []string{"xcode", "shell", "go@1.26", "SHELL", "shell"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := SelfRunner(tc.claims).MissingCapabilities(tc.required)
			want := runnercap.NewClaimed(tc.claims).Missing(tc.required)
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("solver missing = %v, runnercap missing = %v — checkpoint 2 diverged", got, want)
			}
		})
	}
}

// TestDerivedTagsSatisfiedBySelfOnly: harness:<name> and run:shell are
// implicit on the self runner (the daemon host runs every configured harness
// and shell stage through the local path); a non-self runner has no way to
// claim ANY derived tag in v1 (the author grammar rejects the colon), so
// agentic and shell stages place only on self until the image/dispatcher
// advertisement contract lands (#3513). The plain "shell" spelling is an
// ordinary author token, never a derived tag — self does not satisfy it
// implicitly (finding-3 regression: derived spellings live outside the
// author grammar).
func TestDerivedTagsSatisfiedBySelfOnly(t *testing.T) {
	self := Runner{Name: "self", Self: true, OS: OSLinux}
	remote := Runner{Name: "ci", OS: OSLinux}
	if missing := self.MissingCapabilities([]string{"harness:copilot", "run:shell"}); len(missing) != 0 {
		t.Fatalf("self must satisfy derived tags implicitly, missing %v", missing)
	}
	if missing := remote.MissingCapabilities([]string{"harness:copilot"}); len(missing) != 1 {
		t.Fatalf("a non-self runner must not satisfy a harness tag, missing %v", missing)
	}
	if missing := remote.MissingCapabilities([]string{"run:shell"}); len(missing) != 1 {
		t.Fatalf("a non-self runner must not satisfy the derived shell tag, missing %v", missing)
	}
	if !runnercap.DerivedTag("run:shell") || runnercap.DerivedTag("shell") {
		t.Fatal("run:shell must be the derived spelling and plain shell must not be")
	}
	if runnercap.ValidToken("run:shell") {
		t.Fatal("the derived shell spelling must live outside the author token grammar")
	}
	// The plain "shell" token is author territory: unclaimed it is missing
	// even on self; claimed it matches by ordinary membership.
	if missing := self.MissingCapabilities([]string{"shell"}); !reflect.DeepEqual(missing, []string{"shell"}) {
		t.Fatalf("an author-spelled shell token must not be implicit on self, missing %v", missing)
	}
	self.Capabilities = []string{"shell"}
	if missing := self.MissingCapabilities([]string{"shell"}); len(missing) != 0 {
		t.Fatalf("an explicit shell claim must satisfy an author-spelled shell requirement, missing %v", missing)
	}
}

// TestSolveRestrictions: stage requires ⊆ runner DECLARED enforces — self
// included. Self enforces nothing implicitly (finding-5 regression: in 3.0
// no execution path wires a runsOn restriction into executor isolation, so
// an implicit grant would be a guarantee nothing delivers; the wiring and
// self's declarable set land with #3516). A self entry that explicitly
// declares an effect in the inventory is trusted like any other claim
// (RRQ-1/D10). Instance mandates merge into every stage as a floor.
func TestSolveRestrictions(t *testing.T) {
	inv := Inventory{Runners: []Runner{
		{Name: "self", Self: true, OS: OSLinux},
		{Name: "locked", OS: OSLinux, Restrictions: []string{"network:allowlist", "tmp:ephemeral"}},
	}}
	result := Solve(inv, []StageRequirement{
		{Stage: "netless", Restrictions: []string{"network:none"}},
		{Stage: "allowlisted", Restrictions: []string{"network:allowlist"}},
		{Stage: "impossible", Restrictions: []string{"fs:readonly-except-workspace"}},
	})
	// network:none on a default self-only view: honestly unsatisfiable —
	// nothing declares (and nothing would enforce) the isolation.
	unsat := mustUnsat(t, result, "netless", UnsatRequirement)
	if !strings.Contains(unsat.Diagnostic, `runner "self" does not enforce restrictions network:none`) {
		t.Errorf("diagnostic must name self's undeclared restriction: %s", unsat.Diagnostic)
	}
	mustEligible(t, result, "allowlisted", "locked")
	unsat = mustUnsat(t, result, "impossible", UnsatRequirement)
	if !strings.Contains(unsat.Diagnostic, "does not enforce restrictions fs:readonly-except-workspace") {
		t.Errorf("diagnostic must name the unenforced restriction: %s", unsat.Diagnostic)
	}

	// A self runner that explicitly DECLARES restrictions: [network:none] in
	// the inventory is eligible — declared, trusted per RRQ-1/D10.
	declared := Inventory{Runners: []Runner{
		{Name: "self", Self: true, OS: OSLinux, Restrictions: []string{"network:none"}},
	}}
	result = Solve(declared, []StageRequirement{{Stage: "netless", Restrictions: []string{"network:none"}}})
	mustEligible(t, result, "netless", "self")

	// Mandate floor (seam): with tmp:ephemeral mandated instance-wide, only
	// the enforcing runner remains eligible for an unrestricted stage.
	mandated := Inventory{Runners: inv.Runners, Mandates: []string{"tmp:ephemeral"}}
	result = Solve(mandated, []StageRequirement{{Stage: "plain"}})
	mustEligible(t, result, "plain", "locked")
}

// TestSolveQuantitiesDistributed: on a distributed-shape inventory quantities
// are hard constraints — a minimum exceeding every otherwise-eligible
// runner's declared ceiling is RNR003-class; an undeclared ceiling constrains
// nothing.
func TestSolveQuantitiesDistributed(t *testing.T) {
	inv := Inventory{Runners: []Runner{
		{Name: "small", OS: OSLinux, CPU: quantity(t, "2000m"), Memory: quantity(t, "4Gi")},
		{Name: "big", OS: OSLinux, CPU: quantity(t, "8000m"), Memory: quantity(t, "16Gi")},
		{Name: "unbounded", OS: OSWindows},
	}}
	result := Solve(inv, []StageRequirement{
		{Stage: "fits-big", CPU: "4000m"},
		{Stage: "fits-nothing", OS: OSLinux, CPU: "16000m"},
		{Stage: "undeclared-ceiling", OS: OSWindows, CPU: "64", Memory: "512Gi"},
	})
	mustEligible(t, result, "fits-big", "big", "unbounded")
	unsat := mustUnsat(t, result, "fits-nothing", UnsatQuantity)
	for _, want := range []string{"cpu minimum 16000m", `runner "small" ceiling cannot cover`, `runner "big" ceiling cannot cover`} {
		if !strings.Contains(unsat.Diagnostic, want) {
			t.Errorf("diagnostic missing %q: %s", want, unsat.Diagnostic)
		}
	}
	// A runner with no declared ceiling has nothing a minimum can exceed.
	mustEligible(t, result, "undeclared-ceiling", "unbounded")
}

// TestSolveQuantitiesLocalModeAdvisory: with no non-self runner, resource
// minimums are advisory (dsl-3.0.md D4): the stage stays eligible and the
// shortfall is an RNR004-class advisory naming the resource.
func TestSolveQuantitiesLocalModeAdvisory(t *testing.T) {
	inv := Inventory{Runners: []Runner{
		{Name: "self", Self: true, OS: OSLinux, CPU: quantity(t, "4000m")},
	}}
	result := Solve(inv, []StageRequirement{{Stage: "heavy", CPU: "16000m"}})
	mustEligible(t, result, "heavy", "self")
	stage := result.Stages[0]
	if len(stage.Advisories) != 1 {
		t.Fatalf("advisories = %v, want exactly one", stage.Advisories)
	}
	for _, want := range []string{`stage "heavy"`, "cpu minimum 16000m", "advisory on a local-mode inventory"} {
		if !strings.Contains(stage.Advisories[0].Diagnostic, want) {
			t.Errorf("advisory missing %q: %s", want, stage.Advisories[0].Diagnostic)
		}
	}
	if len(result.Unsatisfiable()) != 0 {
		t.Fatal("local-mode quantity shortfall must never be unsatisfiable")
	}
}

// TestSolveEligibleOrderIsInventoryOrder: the dispatcher consumes placements
// later (#3513), so eligibility must be a deterministic function of declared
// inputs — inventory order, not an internal sort.
func TestSolveEligibleOrderIsInventoryOrder(t *testing.T) {
	inv := Inventory{Runners: []Runner{
		{Name: "zeta", OS: OSLinux},
		{Name: "alpha", OS: OSLinux},
	}}
	result := Solve(inv, []StageRequirement{{Stage: "s"}})
	mustEligible(t, result, "s", "zeta", "alpha")
}

// TestSolveBaseContractOnly: a stage with no requirement at all matches every
// runner — tier-1/2 authors write nothing and lose nothing (D6).
func TestSolveBaseContractOnly(t *testing.T) {
	inv := Inventory{Runners: []Runner{{Name: "self", Self: true}}}
	result := Solve(inv, []StageRequirement{{Stage: "plain"}})
	mustEligible(t, result, "plain", "self")
}

// TestSolveExecutableRefusesRemoteOnlyStages is the finding-1 (blocker)
// regression at the solver level: checkpoints 2/3 decide EXECUTION
// placement, so a stage only a remote runner satisfies must be
// UnsatSubstrate under SolveExecutable — named for where it COULD place,
// with the #3513 pointer — never green-lit to execute on the daemon host
// that does not satisfy it. Solve (checkpoint 1: config validity) keeps the
// same stage satisfiable on the whole declared inventory.
func TestSolveExecutableRefusesRemoteOnlyStages(t *testing.T) {
	inv := Inventory{Runners: []Runner{
		{Name: "self", Self: true, OS: OSLinux, Host: "self"},
		{Name: "ci", OS: OSWindows, Host: "ghcr.io/example/ci:v1", Capabilities: []string{"dotnet@8"}},
	}}
	stages := []StageRequirement{
		{Stage: "local", OS: OSLinux},
		{Stage: "win-only", OS: OSWindows},
		{Stage: "needs-dotnet", Capabilities: []string{"dotnet@8"}},
		{Stage: "impossible", OS: OSMacOS},
	}

	// Checkpoint 1: the config is valid — the declared inventory satisfies
	// every stage but the truly impossible one.
	if unsat := Solve(inv, stages); len(unsat.Unsatisfiable()) != 1 || unsat.Unsatisfiable()[0].Stage != "impossible" {
		t.Fatalf("whole-inventory solve must satisfy the remote-satisfiable stages: %+v", unsat.Unsatisfiable())
	}

	result := SolveExecutable(inv, stages)
	mustEligible(t, result, "local", "self")
	for _, stage := range []string{"win-only", "needs-dotnet"} {
		unsat := mustUnsat(t, result, stage, UnsatSubstrate)
		for _, want := range []string{
			"placeable only on runner(s) [ci (host: ghcr.io/example/ci:v1)]",
			"distributed dispatch arrives with #3513",
		} {
			if !strings.Contains(unsat.Diagnostic, want) {
				t.Errorf("stage %q diagnostic missing %q: %s", stage, want, unsat.Diagnostic)
			}
		}
	}
	// The capability-axis refusal must NAME the capability the substrate is
	// missing (finding 2: the operator signal survives on declared
	// inventories).
	if unsat := mustUnsat(t, result, "needs-dotnet", UnsatSubstrate); !strings.Contains(unsat.Diagnostic, "missing capabilities dotnet@8") {
		t.Errorf("capability refusal must name the missing capability: %s", unsat.Diagnostic)
	}
	// A stage nothing satisfies keeps its plain requirement diagnostic (no
	// misleading remote-only clause).
	if unsat := mustUnsat(t, result, "impossible", UnsatRequirement); strings.Contains(unsat.Diagnostic, "#3513") {
		t.Errorf("a config-invalid stage must not carry the substrate clause: %s", unsat.Diagnostic)
	}
}

// TestSolveExecutableKeepsDeclaredModeForQuantities: mode is a fact of the
// DECLARED inventory, not the substrate — on a distributed-shape inventory a
// quantity only a remote runner's ceiling covers is a hard substrate
// refusal, never demoted to a local-mode advisory just because only self
// executes today. On a self-only declared inventory SolveExecutable and
// Solve agree exactly (zero behavior change for local modes).
func TestSolveExecutableKeepsDeclaredModeForQuantities(t *testing.T) {
	distributed := Inventory{Runners: []Runner{
		{Name: "self", Self: true, OS: OSLinux, Host: "self", CPU: quantity(t, "4000m")},
		{Name: "big", OS: OSLinux, Host: "big-workers", CPU: quantity(t, "64")},
	}}
	stages := []StageRequirement{{Stage: "heavy", CPU: "16"}}
	unsat := mustUnsat(t, SolveExecutable(distributed, stages), "heavy", UnsatSubstrate)
	for _, want := range []string{"placeable only on runner(s) [big (host: big-workers)]", "#3513"} {
		if !strings.Contains(unsat.Diagnostic, want) {
			t.Errorf("diagnostic missing %q: %s", want, unsat.Diagnostic)
		}
	}

	selfOnly := Inventory{Runners: []Runner{
		{Name: "self", Self: true, OS: OSLinux, Host: "self", CPU: quantity(t, "4000m")},
	}}
	if !reflect.DeepEqual(SolveExecutable(selfOnly, stages), Solve(selfOnly, stages)) {
		t.Fatal("on a self-only inventory SolveExecutable must equal Solve")
	}
}

// TestExecutableSubstrateSeam pins the seam the #3513 dispatcher widens:
// self entries only, inventory order, mandates carried.
func TestExecutableSubstrateSeam(t *testing.T) {
	inv := Inventory{
		Runners: []Runner{
			{Name: "ci", Host: "ghcr.io/example/ci:v1"},
			{Name: "self", Self: true, Host: "self"},
		},
		Mandates: []string{"tmp:ephemeral"},
	}
	substrate := inv.ExecutableSubstrate()
	if len(substrate.Runners) != 1 || substrate.Runners[0].Name != "self" {
		t.Fatalf("substrate = %+v, want the self entry only", substrate.Runners)
	}
	if !reflect.DeepEqual(substrate.Mandates, inv.Mandates) {
		t.Fatalf("substrate mandates = %v, want the inventory floor carried", substrate.Mandates)
	}
}

// TestHostOSEnum pins the GOOS → product-spelling map for the platform the
// test runs on (one of the three schedulable ones on every CI target).
func TestHostOSEnum(t *testing.T) {
	switch got := HostOS(); got {
	case OSLinux, OSWindows, OSMacOS:
	default:
		t.Fatalf("HostOS() = %q, want one of the schedulable enum values", got)
	}
}
