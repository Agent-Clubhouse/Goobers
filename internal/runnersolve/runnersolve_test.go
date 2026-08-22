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
// contract: for declared (non-derived) tokens, the solver's self-runner miss
// list is exactly runnercap.Claimed.Missing — same tokens, same order, same
// de-duplication — so the scheduler's refusal diagnostics for legacy configs
// cannot change.
func TestMissingCapabilitiesMatchesRunnercap(t *testing.T) {
	claims := []string{"dotnet@8", "go@1.26"}
	required := []string{"dotnet@10", "go@1.26", "xcode", "dotnet@10", "netfx@4.8"}
	got := SelfRunner(claims).MissingCapabilities(required)
	want := runnercap.NewClaimed(claims).Missing(required)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("solver missing = %v, runnercap missing = %v — checkpoint 2 diverged", got, want)
	}
}

// TestDerivedTagsSatisfiedBySelfOnly: harness:<name> and shell are implicit
// on the self runner (the daemon host runs every configured harness through
// the local path); a non-self runner has no way to claim a harness tag in
// v1 (the author grammar rejects the colon), so agentic stages place only on
// self until the image/dispatcher advertisement contract lands (#3513).
func TestDerivedTagsSatisfiedBySelfOnly(t *testing.T) {
	self := Runner{Name: "self", Self: true, OS: OSLinux}
	remote := Runner{Name: "ci", OS: OSLinux}
	if missing := self.MissingCapabilities([]string{"harness:copilot", "shell"}); len(missing) != 0 {
		t.Fatalf("self must satisfy derived tags implicitly, missing %v", missing)
	}
	if missing := remote.MissingCapabilities([]string{"harness:copilot"}); len(missing) != 1 {
		t.Fatalf("a non-self runner must not satisfy a harness tag, missing %v", missing)
	}
	// A remote runner MAY claim the plain "shell" token explicitly.
	remote.Capabilities = []string{"shell"}
	if missing := remote.MissingCapabilities([]string{"shell"}); len(missing) != 0 {
		t.Fatalf("an explicit shell claim must satisfy the derived tag, missing %v", missing)
	}
}

// TestSolveRestrictions: stage requires ⊆ runner enforces; the self runner
// implicitly enforces network:none (D16 keeps the local executor mechanism);
// instance mandates merge into every stage as a floor.
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
	mustEligible(t, result, "netless", "self")
	mustEligible(t, result, "allowlisted", "locked")
	unsat := mustUnsat(t, result, "impossible", UnsatRequirement)
	if !strings.Contains(unsat.Diagnostic, "does not enforce restrictions fs:readonly-except-workspace") {
		t.Errorf("diagnostic must name the unenforced restriction: %s", unsat.Diagnostic)
	}

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

// TestHostOSEnum pins the GOOS → product-spelling map for the platform the
// test runs on (one of the three schedulable ones on every CI target).
func TestHostOSEnum(t *testing.T) {
	switch got := HostOS(); got {
	case OSLinux, OSWindows, OSMacOS:
	default:
		t.Fatalf("HostOS() = %q, want one of the schedulable enum values", got)
	}
}
