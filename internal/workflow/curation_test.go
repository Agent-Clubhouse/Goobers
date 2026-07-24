package workflow

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

// TestBacklogCurationCompiles compiles the shipped backlog-curation workflow
// (config-examples/, the config-as-code starter for issue #25) against its
// curator goober and pins a golden digest — issue #9's "every V0 shipped
// workflow is expressible in schema v0" acceptance, applied concretely to the
// first of the three V0 workloads (ARCHITECTURE.md §12).
func TestBacklogCurationCompiles(t *testing.T) {
	root := filepath.Join("..", "..", "config-examples", "gaggles", "acme-web")

	var w apiv1.Workflow
	raw, err := os.ReadFile(filepath.Join(root, "workflows", "backlog-curation.yaml"))
	if err != nil {
		t.Fatalf("read workflow: %v", err)
	}
	if err := yaml.Unmarshal(raw, &w); err != nil {
		t.Fatalf("unmarshal workflow: %v", err)
	}

	var curator apiv1.Goober
	raw, err = os.ReadFile(filepath.Join(root, "goobers", "curator", "goober.yaml"))
	if err != nil {
		t.Fatalf("read curator goober: %v", err)
	}
	if err := yaml.Unmarshal(raw, &curator); err != nil {
		t.Fatalf("unmarshal curator goober: %v", err)
	}

	goobers := map[string]apiv1.GooberSpec{curator.Name: curator.Spec}
	def := Definition{Name: w.Name, Version: 1, Spec: w.Spec}

	m, err := compileAcknowledged(def, WithGoobers(goobers))
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if warnings := CheckWarnings(def); len(warnings) != 0 {
		t.Fatalf("backlog-curation warnings = %v, want warning-clean reference config", warnings)
	}

	// Structural shape: sample-ready-pool -> query-backlog ->
	// surface-duplicates (deterministic) -> curate (agentic) -> release-claim
	// (deterministic, terminal). No gates — curation is issues-only with no
	// review/CI loop (ARCHITECTURE.md §12 reserves reviewer gates for the
	// implementation workflow, not curation).
	// release-claim (issue #234) is the explicit claim-ledger release
	// curation needs since it never reaches issue-close-out's release
	// (implementation-only).
	if w.Spec.Start != "sample-ready-pool" {
		t.Errorf("start = %q, want sample-ready-pool", w.Spec.Start)
	}
	health, ok := m.Task("sample-ready-pool")
	if !ok {
		t.Fatal("sample-ready-pool task not found")
	}
	if health.Next != "query-backlog" || health.Type != apiv1.TaskDeterministic ||
		health.Run == nil || len(health.Run.Command) != 2 || health.Run.Command[1] != "backlog-health" {
		t.Errorf("sample-ready-pool = %+v, want deterministic goobers backlog-health task", health)
	}
	query, ok := m.Task("query-backlog")
	if !ok {
		t.Fatal("query-backlog task not found")
	}
	if query.Next != "surface-duplicates" {
		t.Errorf("query-backlog.next = %q, want surface-duplicates", query.Next)
	}
	dedupe, ok := m.Task("surface-duplicates")
	if !ok {
		t.Fatal("surface-duplicates task not found")
	}
	if dedupe.Next != "curate" {
		t.Errorf("surface-duplicates.next = %q, want curate", dedupe.Next)
	}
	if dedupe.Type != apiv1.TaskDeterministic || dedupe.Run == nil ||
		len(dedupe.Run.Command) != 2 || dedupe.Run.Command[1] != "backlog-dedupe" {
		t.Errorf("surface-duplicates = %+v, want deterministic goobers backlog-dedupe task", dedupe)
	}
	if dedupe.Inputs["maxCandidates"] != "20" || dedupe.Inputs["resultFile"] != "dedupe-candidates.json" {
		t.Errorf("surface-duplicates inputs = %v, want bounded candidate artifact", dedupe.Inputs)
	}
	if query.Inputs["staleAfterDays"] != "90" {
		t.Errorf("query-backlog staleAfterDays = %q, want 90", query.Inputs["staleAfterDays"])
	}
	if query.Inputs["staleAutoClose"] != "false" {
		t.Errorf("query-backlog staleAutoClose = %q, want false", query.Inputs["staleAutoClose"])
	}
	curate, ok := m.Task("curate")
	if !ok {
		t.Fatal("curate task not found")
	}
	if curate.Next != "release-claim" {
		t.Errorf("curate.next = %q, want release-claim", curate.Next)
	}
	if curate.Goober != "curator" {
		t.Errorf("curate.goober = %q, want curator", curate.Goober)
	}
	if !containsString(curate.Capabilities, "github:milestones:write") {
		t.Errorf("curate capabilities = %v, want github:milestones:write", curate.Capabilities)
	}
	if !containsString(curate.PolicyActions, "assign-milestone") {
		t.Errorf("curate policyActions = %v, want assign-milestone", curate.PolicyActions)
	}
	if !strings.Contains(curate.Goal, "roadmap maintenance on directly linked tracking parents.") ||
		strings.Contains(curate.Goal, "tracking parents and children") {
		t.Errorf("curate goal grants roadmap maintenance outside directly linked tracking parents: %q", curate.Goal)
	}
	release, ok := m.Task("release-claim")
	if !ok {
		t.Fatal("release-claim task not found")
	}
	if release.Next != "" {
		t.Errorf("release-claim.next = %q, want terminal", release.Next)
	}
	if release.Type != apiv1.TaskDeterministic {
		t.Errorf("release-claim.type = %q, want deterministic", release.Type)
	}
	if len(release.Capabilities) != 1 || release.Capabilities[0] != "github:issues:write" {
		t.Errorf("release-claim capabilities = %v, want exactly [github:issues:write]", release.Capabilities)
	}

	// Capability grant is issues-only (issue #25 scope: "no repo access").
	if len(curator.Spec.Capabilities) != 2 ||
		curator.Spec.Capabilities[0] != "github:issues:write" ||
		curator.Spec.Capabilities[1] != "github:milestones:write" {
		t.Errorf("curator capabilities = %v, want exactly [github:issues:write github:milestones:write]", curator.Spec.Capabilities)
	}

	// Bumped when intentional workflow contract changes alter the machine.
	const wantDigest = "sha256:901c08bab61ebd3844af3dfd5c328d359dcbefca2fd8a2ac7d5ba8a1041a224c"
	if m.Digest() != wantDigest {
		t.Logf("backlog-curation digest = %s", m.Digest())
		t.Errorf("digest drift for backlog-curation:\n got  %s\n want %s\n(update wantDigest if the change is intended)", m.Digest(), wantDigest)
	}

	// Deterministic: recompiling yields the same digest.
	m2, err := compileAcknowledged(def, WithGoobers(goobers))
	if err != nil {
		t.Fatalf("recompile: %v", err)
	}
	if m.Digest() != m2.Digest() {
		t.Errorf("digest not stable across compiles: %s vs %s", m.Digest(), m2.Digest())
	}
}

func TestCuratorInstructionsDefineRoadmapMaintenance(t *testing.T) {
	for _, path := range []string{
		filepath.Join("..", "..", "config-examples", "gaggles", "acme-web", "goobers", "curator", "instructions.md"),
		filepath.Join("..", "..", "selfhost", "gaggles", "goobers", "goobers", "curator", "instructions.md"),
	} {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		instructions := string(raw)
		for _, required := range []string{
			`"$GOOBERS_BIN" set-milestone --item`,
			"already has the target milestone, leave it completely untouched",
			"genuine roadmap priority call",
			"Keep epic and tracking checklists synchronized",
			"directly linked tracking parent",
			"Before each mutation, re-read its live metadata",
			"Never mutate an unclaimed child",
		} {
			if !strings.Contains(instructions, required) {
				t.Errorf("%s does not define %q", path, required)
			}
		}
	}
}

// TestBacklogCurationRejectsUngrantedCapability guards the capability-admission
// wiring itself: if the curator's grant were ever narrowed below what the
// workflow's stages declare, compilation must fail closed (SEC-042) rather than
// silently ship an under-permissioned or over-trusting definition.
func TestBacklogCurationRejectsUngrantedCapability(t *testing.T) {
	spec := apiv1.WorkflowSpec{
		Gaggle: "acme-web",
		Start:  "curate",
		Tasks: []apiv1.Task{
			{
				Name: "curate", Type: apiv1.TaskAgentic, Goober: "curator", Goal: "curate",
				Capabilities: []string{"github:issues:write", "repo:push"}, // repo:push not granted below
			},
		},
	}
	goobers := map[string]apiv1.GooberSpec{
		"curator": {Role: "curator", Harness: apiv1.HarnessCopilot, Capabilities: []string{"github:issues:write"}},
	}
	_, err := compileAcknowledged(Definition{Name: "backlog-curation", Version: 1, Spec: spec}, WithGoobers(goobers))
	if err == nil {
		t.Fatal("expected compile to reject an ungranted repo:push capability, got nil error")
	}
}
