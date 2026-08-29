package v30

import (
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/runnercap"
)

// windows_identity_test.go pins the two Windows-conditional rules #3619 adds
// to the 3.0 validator, on top of the OS-blind closed-list check:
//
//   - the privilege=windows-admin capability (the ContainerAdministrator
//     identity of a Windows stage pod) may be required only by a stage whose
//     EFFECTIVE runsOn.os is windows — declared on the stage or by the
//     gaggle floor — and is refused, never defaulted, anywhere else;
//   - a stage whose effective runsOn.os is windows may require only the
//     restrictions Windows can bind (tmp:ephemeral, env:default-deny);
//     network:none, network:allowlist and fs:readonly-except-workspace are
//     refused at validate (restrictions doc D4/D11, acceptance criterion 2).

func windowsTask(name string, runsOn *apiv1.RunsOn) apiv1.Task {
	return implementTask(name, runsOn)
}

func TestCompileAcceptsWindowsAdminOnAWindowsStage(t *testing.T) {
	task := windowsTask("install-service", &apiv1.RunsOn{
		OS:           "windows",
		Capabilities: []string{"dotnet@8", runnercap.CapabilityWindowsAdmin},
		Restrictions: []string{"tmp:ephemeral"},
	})
	if _, err := compileAcknowledged(Definition{Name: "win-admin-ok", Version: 1, Spec: singleTaskSpec(task)}); err != nil {
		t.Fatalf("a Windows stage requiring privilege=windows-admin must compile: %v", err)
	}

	// The OS may come from the gaggle floor; the token may too.
	task = windowsTask("install-service", &apiv1.RunsOn{Capabilities: []string{runnercap.CapabilityWindowsAdmin}})
	if _, err := compileAcknowledged(
		Definition{Name: "win-admin-floor-os", Version: 1, Spec: singleTaskSpec(task)},
		WithGaggleRunsOn(&apiv1.GaggleRunsOn{OS: "windows"}),
	); err != nil {
		t.Fatalf("the gaggle floor's windows OS must satisfy the privilege rule: %v", err)
	}
	task = windowsTask("install-service", &apiv1.RunsOn{OS: "windows"})
	if _, err := compileAcknowledged(
		Definition{Name: "win-admin-floor-token", Version: 1, Spec: singleTaskSpec(task)},
		WithGaggleRunsOn(&apiv1.GaggleRunsOn{Capabilities: []string{runnercap.CapabilityWindowsAdmin}}),
	); err != nil {
		t.Fatalf("a floor-carried privilege token on a windows stage must compile: %v", err)
	}
}

func TestCompileRefusesWindowsAdminOffWindows(t *testing.T) {
	// A Linux stage requiring the Windows identity is meaningless.
	task := windowsTask("install-service", &apiv1.RunsOn{OS: "linux", Capabilities: []string{runnercap.CapabilityWindowsAdmin}})
	_, err := compileAcknowledged(Definition{Name: "win-admin-linux", Version: 1, Spec: singleTaskSpec(task)})
	if err == nil ||
		!strings.Contains(err.Error(), `task "install-service" runsOn.capabilities requires "privilege=windows-admin"`) ||
		!strings.Contains(err.Error(), `effective runsOn.os is "linux"`) {
		t.Fatalf("Compile error = %v, want the privilege/OS refusal naming the stage and its OS", err)
	}

	// Explicit-complete cuts the other way for a privilege: an UNSET OS is
	// refused too, with the rewrite hint — the stage would otherwise place by
	// the accident of which runners claim the token.
	task = windowsTask("install-service", &apiv1.RunsOn{Capabilities: []string{runnercap.CapabilityWindowsAdmin}})
	_, err = compileAcknowledged(Definition{Name: "win-admin-unset", Version: 1, Spec: singleTaskSpec(task)})
	if err == nil ||
		!strings.Contains(err.Error(), `effective runsOn.os is unset`) ||
		!strings.Contains(err.Error(), `declare runsOn.os: windows explicitly`) {
		t.Fatalf("Compile error = %v, want the unset-OS refusal with the runsOn.os: windows hint", err)
	}

	// A floor carrying the token under a non-Windows gaggle OS is reported
	// ONCE, at the gaggle, not once per stage.
	task = windowsTask("install-service", nil)
	spec := singleTaskSpec(task)
	spec.Tasks = append(spec.Tasks, windowsTask("second", nil))
	spec.Tasks[0].Next = "second"
	_, err = compileAcknowledged(
		Definition{Name: "win-admin-floor-linux", Version: 1, Spec: spec},
		WithGaggleRunsOn(&apiv1.GaggleRunsOn{OS: "linux", Capabilities: []string{runnercap.CapabilityWindowsAdmin}}),
	)
	if err == nil || !strings.Contains(err.Error(), `gaggle runsOn.capabilities requires "privilege=windows-admin"`) {
		t.Fatalf("Compile error = %v, want the gaggle-level privilege refusal", err)
	}
	if strings.Count(err.Error(), "privilege=windows-admin") != 1 {
		t.Fatalf("Compile error = %v, want the floor refusal reported exactly once", err)
	}

	// The same rule reads a placed agentic gate's block.
	gate := apiv1.Gate{
		Name: "review", Evaluator: apiv1.EvaluatorAgentic,
		Agentic:  &apiv1.AgenticGate{Goober: "reviewer"},
		RunsOn:   &apiv1.RunsOn{OS: "linux", CPU: "1000m", Memory: "2Gi", Capabilities: []string{runnercap.CapabilityWindowsAdmin}},
		Branches: map[string]string{"pass": "@complete", "fail": "@abort"},
	}
	task = windowsTask("implement", nil)
	task.Next = "review"
	spec = singleTaskSpec(task)
	spec.Gates = []apiv1.Gate{gate}
	_, err = compileAcknowledged(Definition{Name: "win-admin-gate", Version: 1, Spec: spec})
	if err == nil || !strings.Contains(err.Error(), `gate "review" runsOn.capabilities requires "privilege=windows-admin"`) {
		t.Fatalf("Compile error = %v, want the gate-attributed privilege refusal", err)
	}
}

func TestCompileRefusesUnbindableRestrictionsOnAWindowsStage(t *testing.T) {
	for _, restriction := range []string{"network:none", "network:allowlist", "fs:readonly-except-workspace"} {
		task := windowsTask("build", &apiv1.RunsOn{OS: "windows", Restrictions: []string{"tmp:ephemeral", restriction}})
		_, err := compileAcknowledged(Definition{Name: "win-" + restriction, Version: 1, Spec: singleTaskSpec(task)})
		if err == nil ||
			!strings.Contains(err.Error(), `task "build" runsOn.restrictions: "`+restriction+`" has no Windows binding in v1`) ||
			!strings.Contains(err.Error(), "may require only env:default-deny, tmp:ephemeral") {
			t.Fatalf("restriction %s: Compile error = %v, want the Windows-undeclarable refusal", restriction, err)
		}
		// The declarable pair never appears in the refusal.
		if strings.Contains(err.Error(), `"tmp:ephemeral" has no Windows binding`) {
			t.Fatalf("restriction %s: Compile error = %v, tmp:ephemeral wrongly refused on Windows", restriction, err)
		}
	}

	// The two Windows-bindable effects compile on a Windows stage (the shipped
	// parity-probe-windows / xplat-handoff shape declares tmp:ephemeral).
	task := windowsTask("build", &apiv1.RunsOn{OS: "windows", Restrictions: []string{"tmp:ephemeral", "env:default-deny"}})
	if _, err := compileAcknowledged(Definition{Name: "win-ok", Version: 1, Spec: singleTaskSpec(task)}); err != nil {
		t.Fatalf("tmp:ephemeral and env:default-deny must compile on a Windows stage: %v", err)
	}

	// The same effects on a LINUX stage stay accepted: the rule is
	// OS-conditional, not a shrink of the closed list.
	task = windowsTask("build", &apiv1.RunsOn{OS: "linux", Restrictions: []string{"network:none", "fs:readonly-except-workspace"}})
	if _, err := compileAcknowledged(Definition{Name: "linux-ok", Version: 1, Spec: singleTaskSpec(task)}); err != nil {
		t.Fatalf("the full closed list must still compile on a Linux stage: %v", err)
	}

	// An OS-less stage is unconstrained by this rule (it is the solver's job
	// to find an enforcing runner).
	task = windowsTask("build", &apiv1.RunsOn{Restrictions: []string{"network:none"}})
	if _, err := compileAcknowledged(Definition{Name: "osless-ok", Version: 1, Spec: singleTaskSpec(task)}); err != nil {
		t.Fatalf("an OS-less stage requiring network:none must compile: %v", err)
	}
}

func TestCompileRefusesUnbindableRestrictionsThroughTheGaggleFloor(t *testing.T) {
	// Floor OS windows + stage-declared fs:readonly: the stage's effective OS
	// is windows, so the stage is refused.
	task := windowsTask("build", &apiv1.RunsOn{Restrictions: []string{"fs:readonly-except-workspace"}})
	_, err := compileAcknowledged(
		Definition{Name: "floor-os", Version: 1, Spec: singleTaskSpec(task)},
		WithGaggleRunsOn(&apiv1.GaggleRunsOn{OS: "windows"}),
	)
	if err == nil || !strings.Contains(err.Error(), `task "build" runsOn.restrictions: "fs:readonly-except-workspace" has no Windows binding`) {
		t.Fatalf("Compile error = %v, want the stage refused under the floor's windows OS", err)
	}

	// Floor restriction network:none + stage-declared windows OS: the floor
	// merges into the stage, so the stage is refused (the floor itself has no
	// OS, so it is not reported at the gaggle).
	task = windowsTask("build", &apiv1.RunsOn{OS: "windows"})
	_, err = compileAcknowledged(
		Definition{Name: "floor-restriction", Version: 1, Spec: singleTaskSpec(task)},
		WithGaggleRunsOn(&apiv1.GaggleRunsOn{Restrictions: []string{"network:none"}}),
	)
	if err == nil || !strings.Contains(err.Error(), `task "build" runsOn.restrictions: "network:none" has no Windows binding`) {
		t.Fatalf("Compile error = %v, want the floor restriction refused on the windows stage", err)
	}

	// Floor OS windows + floor restriction: reported ONCE at the gaggle, and
	// not again per stage.
	task = windowsTask("build", nil)
	spec := singleTaskSpec(task)
	spec.Tasks = append(spec.Tasks, windowsTask("second", nil))
	spec.Tasks[0].Next = "second"
	_, err = compileAcknowledged(
		Definition{Name: "floor-both", Version: 1, Spec: spec},
		WithGaggleRunsOn(&apiv1.GaggleRunsOn{OS: "windows", Restrictions: []string{"network:allowlist"}}),
	)
	if err == nil || !strings.Contains(err.Error(), `gaggle runsOn.restrictions: "network:allowlist" has no Windows binding`) {
		t.Fatalf("Compile error = %v, want the gaggle-level refusal", err)
	}
	if got := strings.Count(err.Error(), "has no Windows binding"); got != 1 {
		t.Fatalf("Compile error = %v, want the floor refusal exactly once, got %d", err, got)
	}

	// A floor restriction Windows CAN bind stays accepted under a windows
	// floor OS.
	if _, err := compileAcknowledged(
		Definition{Name: "floor-ok", Version: 1, Spec: singleTaskSpec(windowsTask("build", nil))},
		WithGaggleRunsOn(&apiv1.GaggleRunsOn{OS: "windows", Restrictions: []string{"tmp:ephemeral"}}),
	); err != nil {
		t.Fatalf("a windows floor declaring tmp:ephemeral must compile: %v", err)
	}
}

// The token is registered as a feature at every declaration site, so the
// resolved surface of a document names it; an ordinary capability token adds
// nothing beyond the container feature.
func TestWindowsAdminTokenIsAFeatureAtEverySite(t *testing.T) {
	hasFeature := func(features []Feature, id FeatureID) bool {
		for _, f := range features {
			if f.ID == id {
				return true
			}
		}
		return false
	}
	task := windowsTask("install-service", &apiv1.RunsOn{OS: "windows", Capabilities: []string{runnercap.CapabilityWindowsAdmin}})
	features, err := FeaturesForWorkflow(Definition{Name: "f", Version: 1, Spec: singleTaskSpec(task)})
	if err != nil {
		t.Fatalf("FeaturesForWorkflow: %v", err)
	}
	if !hasFeature(features, featureTaskRunsOnWindowsAdmin) {
		t.Fatalf("task feature %s missing from %v", featureTaskRunsOnWindowsAdmin, features)
	}
	task = windowsTask("build", &apiv1.RunsOn{OS: "windows", Capabilities: []string{"dotnet@8"}})
	features, err = FeaturesForWorkflow(Definition{Name: "g", Version: 1, Spec: singleTaskSpec(task)})
	if err != nil {
		t.Fatalf("FeaturesForWorkflow: %v", err)
	}
	if hasFeature(features, featureTaskRunsOnWindowsAdmin) {
		t.Fatalf("an ordinary token must not resolve %s", featureTaskRunsOnWindowsAdmin)
	}
	gaggle, err := FeaturesForGaggle(apiv1.GaggleSpec{RunsOn: &apiv1.GaggleRunsOn{OS: "windows", Capabilities: []string{runnercap.CapabilityWindowsAdmin}}})
	if err != nil {
		t.Fatalf("FeaturesForGaggle: %v", err)
	}
	if !hasFeature(gaggle, featureGaggleRunsOnWindowsAdmin) {
		t.Fatalf("gaggle feature %s missing from %v", featureGaggleRunsOnWindowsAdmin, gaggle)
	}
	for _, id := range []FeatureID{featureTaskRunsOnWindowsAdmin, featureGateRunsOnWindowsAdmin, featureGaggleRunsOnWindowsAdmin} {
		f, ok := LookupFeature(id)
		if !ok || f.Level != SupportGA || f.SinceVersion != "v0.4.0" {
			t.Fatalf("feature %s = %+v (ok=%v), want GA since v0.4.0 (GA-within-3.0, never a per-feature preview gate)", id, f, ok)
		}
	}
}
