package runnersolve

import (
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/runnercap"
)

// The privilege=windows-admin token (#3619) is an ORDINARY capability to the
// solver: exact set membership, no derivation, no self-implicit
// satisfaction. A stage requiring it therefore places only on a runner whose
// claim set carries it — a Windows runner that merely matches the OS is not
// eligible, and neither is a self runner that did not claim it.
func TestSolveWindowsAdminMatchesOnlyProvidingRunners(t *testing.T) {
	inv := Inventory{Runners: []Runner{
		{Name: "self", Self: true, OS: OSLinux},
		{Name: "windows-shell", OS: OSWindows, Capabilities: []string{"dotnet@8"}, Restrictions: []string{"tmp:ephemeral"}},
		{Name: "windows-admin", OS: OSWindows, Capabilities: []string{"dotnet@8", runnercap.CapabilityWindowsAdmin}, Restrictions: []string{"tmp:ephemeral"}},
	}}
	result := Solve(inv, []StageRequirement{
		{Stage: "install-service", OS: OSWindows, Capabilities: []string{runnercap.CapabilityWindowsAdmin}},
		{Stage: "build", OS: OSWindows, Capabilities: []string{"dotnet@8"}},
	})
	mustEligible(t, result, "install-service", "windows-admin")
	// A stage that does not require the privilege stays eligible on BOTH
	// Windows classes — providing admin does not narrow a class's eligibility
	// (the dispatcher runs such a stage as ContainerUser regardless).
	mustEligible(t, result, "build", "windows-shell", "windows-admin")

	// No providing runner: unsatisfiable, with the token named per runner.
	noAdmin := Inventory{Runners: inv.Runners[:2]}
	result = Solve(noAdmin, []StageRequirement{
		{Stage: "install-service", OS: OSWindows, Capabilities: []string{runnercap.CapabilityWindowsAdmin}},
	})
	unsat := mustUnsat(t, result, "install-service", UnsatRequirement)
	if !strings.Contains(unsat.Diagnostic, `runner "windows-shell" is missing capabilities privilege=windows-admin`) {
		t.Errorf("diagnostic must name the missing privilege on the Windows runner: %s", unsat.Diagnostic)
	}

	// A self runner on a Windows host does not satisfy the token implicitly:
	// it is not a derived tag.
	winSelf := Inventory{Runners: []Runner{{Name: "self", Self: true, OS: OSWindows}}}
	result = Solve(winSelf, []StageRequirement{
		{Stage: "install-service", OS: OSWindows, Capabilities: []string{runnercap.CapabilityWindowsAdmin}},
	})
	mustUnsat(t, result, "install-service", UnsatRequirement)
}
