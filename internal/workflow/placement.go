package workflow

// placement.go routes the shared constraint solver's per-stage requirement
// build (internal/runnersolve, dsl-3.0.md §5) to the workflow's own
// interpreter: 3.0 documents compile the full effective runsOn (declared ∪
// derived ∪ gaggle floor — v30.StagePlacements); pre-3.0 documents degrade to
// the declared requiredCapabilities union, exactly the surface their frozen
// interpreters know (PO-D0: 2.0 never learns distributed features), so a
// byte-untouched 2.0 workflow produces byte-identical requirements — and
// therefore byte-identical admission — before and after the 3.0 release.

import (
	"fmt"
	"strings"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"

	"github.com/goobers/goobers/internal/runnersolve"
	v30 "github.com/goobers/goobers/internal/workflow/v_3_0"
)

// StagePlacements returns each placeable stage's effective placement
// requirement for def's DSL version — the solver input every admission
// checkpoint shares: tasks in task order and, on a 3.0 document, the agentic
// gates that declare runsOn after them (decision 001). Rows are keyed by
// StageRequirement.Stage; consumers must never assume a row's position names
// a task. gaggle is the workflow's own gaggle spec (its runsOn floor for 3.0
// documents, its requiredCapabilities for earlier ones); goobers supplies
// referenced goober specs for 3.0 harness derivation.
func StagePlacements(def Definition, gaggle apiv1.GaggleSpec, goobers map[string]apiv1.GooberSpec) ([]runnersolve.StageRequirement, error) {
	interp, err := interpreterForDefinition(def)
	if err != nil {
		return nil, err
	}
	return interp.stagePlacements(def, gaggle, goobers)
}

// v30StagePlacements adapts the 3.0 interpreter's builder to the router
// signature.
func v30StagePlacements(def Definition, gaggle apiv1.GaggleSpec, goobers map[string]apiv1.GooberSpec) ([]runnersolve.StageRequirement, error) {
	return v30.StagePlacements(def, gaggle.RunsOn, goobers), nil
}

// preV30StagePlacements is the arm for every interpreter before 3.0: the only
// placement surface those versions have is requiredCapabilities (task-level
// plus the gaggle floor), matched as an exact tag set — no OS, no quantities,
// no restrictions, no derivation, and no gate rows (a 2.0 gate cannot carry
// runsOn, so every 2.0 gate stays in the control plane). It lives here in
// the router, like preV30SurfaceProblems, so the frozen packages stay
// untouched.
//
// The one token the product interprets — privilege=windows-admin (#3619) —
// is refused rather than matched: it would otherwise pin a 2.0 task to an
// admin-providing class and render ContainerAdministrator with no OS and no
// coherence rule (preV30WindowsAdminProblems). Compile refuses the same
// document first; this arm re-asserts it on the solver input because
// validate's checkpoint solve and the run-start pin read StagePlacements
// directly.
func preV30StagePlacements(def Definition, gaggle apiv1.GaggleSpec, _ map[string]apiv1.GooberSpec) ([]runnersolve.StageRequirement, error) {
	if problems := preV30WindowsAdminProblems(def, gaggle.RequiredCapabilities); len(problems) > 0 {
		return nil, fmt.Errorf("invalid workflow %q: %s", def.Name, strings.Join(problems, "; "))
	}
	requirements := make([]runnersolve.StageRequirement, 0, len(def.Spec.Tasks))
	for _, task := range def.Spec.Tasks {
		var capabilities []string
		capabilities = append(capabilities, task.RequiredCapabilities...)
		for _, token := range gaggle.RequiredCapabilities {
			if !containsToken(capabilities, token) {
				capabilities = append(capabilities, token)
			}
		}
		requirements = append(requirements, runnersolve.StageRequirement{
			Stage:        task.Name,
			Capabilities: capabilities,
		})
	}
	return requirements, nil
}

func containsToken(values []string, want string) bool {
	for _, v := range values {
		if v == want {
			return true
		}
	}
	return false
}
