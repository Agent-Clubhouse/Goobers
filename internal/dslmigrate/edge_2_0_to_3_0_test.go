package dslmigrate

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
	k8syaml "sigs.k8s.io/yaml"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

func migrateToV3(t *testing.T, source string) *Result {
	t.Helper()
	res, err := Migrate([]byte(source), "3.0")
	if err != nil {
		t.Fatalf("Migrate --to 3.0: %v", err)
	}
	return res
}

func decodeV3Workflow(t *testing.T, source string) apiv1.Workflow {
	t.Helper()
	var wf apiv1.Workflow
	if err := k8syaml.Unmarshal([]byte(source), &wf); err != nil {
		t.Fatalf("decode migrated workflow: %v\n%s", err, source)
	}
	return wf
}

func taskByName(wf apiv1.Workflow, name string) apiv1.Task {
	for _, task := range wf.Spec.Tasks {
		if task.Name == name {
			return task
		}
	}
	return apiv1.Task{}
}

// TestRule2RequiredCapabilitiesMoveToRunsOn: task requiredCapabilities become
// runsOn.capabilities, grammar unchanged; credential `capabilities:` untouched.
func TestRule2RequiredCapabilitiesMoveToRunsOn(t *testing.T) {
	const src = `apiVersion: goobers.dev/v1alpha1
kind: Workflow
dslVersion: "2.0"
metadata:
  name: caps
spec:
  gaggle: g
  triggers: [{type: backlog-item}]
  start: build
  tasks:
    - name: build
      type: deterministic
      goal: build
      run:
        command: ["make", "ci"]
      requiredCapabilities: [go@1.26, make, gcc]
      capabilities: [repo:push]
`
	wf := decodeV3Workflow(t, migrateToV3(t, src).After)
	if wf.DSLVersion != "3.0" {
		t.Fatalf("dslVersion = %q, want 3.0", wf.DSLVersion)
	}
	build := taskByName(wf, "build")
	if build.RunsOn == nil {
		t.Fatal("build.runsOn is nil, want migrated capabilities")
	}
	if got, want := strings.Join(build.RunsOn.Capabilities, ","), "go@1.26,make,gcc"; got != want {
		t.Fatalf("runsOn.capabilities = %q, want %q", got, want)
	}
	if len(build.RequiredCapabilities) != 0 {
		t.Fatalf("requiredCapabilities not cleared: %v", build.RequiredCapabilities)
	}
	if got, want := strings.Join(build.Capabilities, ","), "repo:push"; got != want {
		t.Fatalf("credential capabilities changed = %q, want %q (must be untouched)", got, want)
	}
}

// TestRule3OSTokensBecomeRunsOnOS covers the three mappings, darwin→macOS in
// particular, and that a mixed os+toolchain set splits cleanly.
func TestRule3OSTokensBecomeRunsOnOS(t *testing.T) {
	cases := []struct {
		token  string
		wantOS string
	}{
		{"os=linux", "linux"},
		{"os=windows", "windows"},
		{"os=darwin", "macOS"},
	}
	for _, tc := range cases {
		t.Run(tc.token, func(t *testing.T) {
			src := `apiVersion: goobers.dev/v1alpha1
kind: Workflow
dslVersion: "2.0"
metadata:
  name: os
spec:
  gaggle: g
  triggers: [{type: backlog-item}]
  start: build
  tasks:
    - name: build
      type: deterministic
      goal: build
      run:
        command: ["make", "ci"]
      requiredCapabilities: [` + tc.token + `, go@1.26]
`
			wf := decodeV3Workflow(t, migrateToV3(t, src).After)
			build := taskByName(wf, "build")
			if build.RunsOn == nil || build.RunsOn.OS != tc.wantOS {
				t.Fatalf("runsOn.os = %v, want %q", build.RunsOn, tc.wantOS)
			}
			if got, want := strings.Join(build.RunsOn.Capabilities, ","), "go@1.26"; got != want {
				t.Fatalf("runsOn.capabilities = %q, want %q (os token stripped)", got, want)
			}
		})
	}
}

// TestRule3ConflictingOSTokensRefuse: two different os tokens on one stage were
// already unsatisfiable and are a migration refusal.
func TestRule3ConflictingOSTokensRefuse(t *testing.T) {
	const src = `apiVersion: goobers.dev/v1alpha1
kind: Workflow
dslVersion: "2.0"
metadata:
  name: conflict
spec:
  gaggle: g
  triggers: [{type: backlog-item}]
  start: build
  tasks:
    - name: build
      type: deterministic
      goal: build
      run:
        command: ["make", "ci"]
      requiredCapabilities: [os=linux, os=windows]
`
	_, err := Migrate([]byte(src), "3.0")
	if err == nil || !strings.Contains(err.Error(), "conflicting os tokens") {
		t.Fatalf("err = %v, want conflicting-os refusal", err)
	}
}

// TestRule3UnknownOSTokenRefuse: an os token DSL 3.0 has no enum for is a
// refusal (it would land as CAP004).
func TestRule3UnknownOSTokenRefuse(t *testing.T) {
	const src = `apiVersion: goobers.dev/v1alpha1
kind: Workflow
dslVersion: "2.0"
metadata:
  name: plan9
spec:
  gaggle: g
  triggers: [{type: backlog-item}]
  start: build
  tasks:
    - name: build
      type: deterministic
      goal: build
      run:
        command: ["make", "ci"]
      requiredCapabilities: [os=plan9]
`
	_, err := Migrate([]byte(src), "3.0")
	if err == nil || !strings.Contains(err.Error(), "GOOS \"plan9\"") {
		t.Fatalf("err = %v, want unknown-os refusal naming plan9", err)
	}
}

// TestRule4NetworkNoneBecomesRestriction: run.network: none folds into
// runsOn.restrictions (D16).
func TestRule4NetworkNoneBecomesRestriction(t *testing.T) {
	const src = `apiVersion: goobers.dev/v1alpha1
kind: Workflow
dslVersion: "2.0"
metadata:
  name: net
spec:
  gaggle: g
  triggers: [{type: backlog-item}]
  start: build
  tasks:
    - name: build
      type: deterministic
      goal: build
      run:
        command: ["make", "ci"]
        network: none
`
	wf := decodeV3Workflow(t, migrateToV3(t, src).After)
	build := taskByName(wf, "build")
	if build.RunsOn == nil || len(build.RunsOn.Restrictions) != 1 || build.RunsOn.Restrictions[0] != "network:none" {
		t.Fatalf("runsOn.restrictions = %v, want [network:none]", build.RunsOn)
	}
	if build.Run != nil && build.Run.Network != "" {
		t.Fatalf("run.network still set = %q, want cleared", build.Run.Network)
	}
}

// TestRule5RepoFromScalarAndList: the reaching-definitions handoff edges are
// inserted — a scalar for one producer, a flow list for CI-repass fan-in.
func TestRule5RepoFromScalarAndList(t *testing.T) {
	const src = `apiVersion: goobers.dev/v1alpha1
kind: Workflow
dslVersion: "2.0"
metadata:
  name: chain
spec:
  gaggle: g
  triggers: [{type: backlog-item}]
  start: implement
  tasks:
    - name: implement
      type: agentic
      goober: coder
      goal: implement
      next: review
    - name: remediate
      type: agentic
      goober: coder
      goal: fix
      next: review
    - name: local-ci
      type: deterministic
      goal: ci
      run:
        command: ["make", "ci"]
      next: gate
  gates:
    - name: review
      evaluator: agentic
      agentic: {goober: reviewer}
      branches: {pass: local-ci, needs-changes: "@escalate", fail: "@abort"}
    - name: gate
      evaluator: automated
      automated: {check: ci-status}
      branches: {pass: "", fail: remediate, timeout: "@escalate"}
`
	wf := decodeV3Workflow(t, migrateToV3(t, src).After)
	localCI := taskByName(wf, "local-ci")
	if got, want := strings.Join([]string(localCI.RepoFrom), ","), "implement,remediate"; got != want {
		t.Fatalf("local-ci repoFrom = %q, want %q (fan-in list)", got, want)
	}
	remediate := taskByName(wf, "remediate")
	if got, want := strings.Join([]string(remediate.RepoFrom), ","), "implement"; got != want {
		t.Fatalf("remediate repoFrom = %q, want %q (own attempts excluded)", got, want)
	}
	if len(taskByName(wf, "implement").RepoFrom) != 0 {
		t.Fatalf("implement repoFrom = %v, want empty (first producer, no incoming edge)",
			taskByName(wf, "implement").RepoFrom)
	}
}

// TestRule6SandboxOverrideRefuses: the per-gaggle sandbox override has no 3.0
// equivalent yet and is a refusal with a pointer, never a silent drop.
func TestRule6SandboxOverrideRefuses(t *testing.T) {
	const src = `apiVersion: goobers.dev/v1alpha1
kind: Workflow
dslVersion: "2.0"
metadata:
  name: sb
spec:
  gaggle: g
  sandbox:
    network: allow
  triggers: [{type: backlog-item}]
  start: build
  tasks:
    - name: build
      type: deterministic
      goal: build
      run:
        command: ["make", "ci"]
`
	_, err := Migrate([]byte(src), "3.0")
	if err == nil || !strings.Contains(err.Error(), "spec.sandbox") || !strings.Contains(err.Error(), "#3516") {
		t.Fatalf("err = %v, want sandbox refusal pointing at #3516", err)
	}
}

// TestGaggleRequiredCapabilitiesMigrate exercises migrateGaggleSpec directly.
// Gaggles are unversioned (#3297), so they carry no top-level dslVersion for
// Migrate to key on and cannot be driven through the normal Migrate(source,
// to) flow, and `goobers fix` iterates only workflow files today — wiring a
// gaggle sweep is the recorded follow-up. The transform itself is exercised
// here so the capability is proven present.
func TestGaggleRequiredCapabilitiesMigrate(t *testing.T) {
	const src = `apiVersion: goobers.dev/v1alpha1
kind: Gaggle
metadata:
  name: g
spec:
  requiredCapabilities: [os=linux, go@1.26]
`
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte(src), &doc); err != nil {
		t.Fatal(err)
	}
	root := documentRoot(&doc)
	spec, _ := mapValue(root, "spec")
	var notes []string
	changed, err := migrateGaggleSpec(spec, &notes)
	if err != nil {
		t.Fatalf("migrateGaggleSpec: %v", err)
	}
	if !changed {
		t.Fatal("changed = false, want true")
	}
	after, err := marshalDocument(&doc)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(after, "runsOn:") ||
		!strings.Contains(after, "os: linux") ||
		!strings.Contains(after, "go@1.26") {
		t.Fatalf("migrated gaggle missing runsOn/os/caps:\n%s", after)
	}
	if strings.Contains(after, "requiredCapabilities") {
		t.Fatalf("gaggle still carries requiredCapabilities:\n%s", after)
	}
}

// TestMigratedGatesCarryNoRunsOn pins the migrator's gate posture (decision
// 001): `goobers fix --to 3.0` cannot invent a gate placement — gates[].runsOn
// is an author opt-in that must name cpu and memory — so a migrated agentic
// gate stays control-plane, exactly its 2.0 behaviour, with no note.
func TestMigratedGatesCarryNoRunsOn(t *testing.T) {
	const src = `apiVersion: goobers.dev/v1alpha1
kind: Workflow
dslVersion: "2.0"
metadata:
  name: gated
spec:
  gaggle: g
  triggers: [{type: backlog-item}]
  start: implement
  tasks:
    - name: implement
      type: agentic
      goober: coder
      goal: implement
      requiredCapabilities: [os=linux]
      next: review
  gates:
    - name: review
      evaluator: agentic
      agentic: {goober: reviewer}
      branches: {pass: "", needs-changes: implement, fail: "@abort"}
`
	result := migrateToV3(t, src)
	wf := decodeV3Workflow(t, result.After)
	if len(wf.Spec.Gates) != 1 || wf.Spec.Gates[0].RunsOn != nil {
		t.Fatalf("migrated gates = %+v, want the review gate with NO runsOn (the migrator never invents placement)", wf.Spec.Gates)
	}
	if taskByName(wf, "implement").RunsOn == nil {
		t.Fatal("the task's requiredCapabilities must still migrate to runsOn (rule 3)")
	}
	for _, note := range result.Notes {
		if strings.Contains(note, "gate") {
			t.Fatalf("notes = %v, want no gate note", result.Notes)
		}
	}
	if strings.Contains(result.After, "runsOn:\n        cpu") {
		t.Fatalf("migrated document invents a gate envelope:\n%s", result.After)
	}
}
