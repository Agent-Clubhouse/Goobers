package workflow

import (
	"os"
	"path/filepath"
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

	m, err := Compile(def, WithGoobers(goobers))
	if err != nil {
		t.Fatalf("compile: %v", err)
	}

	// Structural shape: query-backlog (deterministic) -> curate (agentic) ->
	// release-claim (deterministic, terminal). No gates — curation is
	// issues-only with no review/CI loop (ARCHITECTURE.md §12 reserves
	// reviewer gates for the implementation workflow, not curation).
	// release-claim (issue #234) is the explicit claim-ledger release
	// curation needs since it never reaches issue-close-out's release
	// (implementation-only).
	if w.Spec.Start != "query-backlog" {
		t.Errorf("start = %q, want query-backlog", w.Spec.Start)
	}
	query, ok := m.Task("query-backlog")
	if !ok {
		t.Fatal("query-backlog task not found")
	}
	if query.Next != "curate" {
		t.Errorf("query-backlog.next = %q, want curate", query.Next)
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

	// Capability grant is issues-only (issue #25 scope: "no repo access").
	if len(curator.Spec.Capabilities) != 1 || curator.Spec.Capabilities[0] != "github:issues:write" {
		t.Errorf("curator capabilities = %v, want exactly [github:issues:write]", curator.Spec.Capabilities)
	}

	// Bumped for #236 (query-backlog declares a resultFile so the claimed-items
	// batch is lifted into an artifact and reaches the curator), recomputed on
	// top of #234's release stage now present in the compiled workflow.
	const wantDigest = "sha256:e91f816c4bff46be78876ef7dd4ddbf8fbbc166f8fae1470906e58b03bb38e5c"
	if m.Digest() != wantDigest {
		t.Logf("backlog-curation digest = %s", m.Digest())
		t.Errorf("digest drift for backlog-curation:\n got  %s\n want %s\n(update wantDigest if the change is intended)", m.Digest(), wantDigest)
	}

	// Deterministic: recompiling yields the same digest.
	m2, err := Compile(def, WithGoobers(goobers))
	if err != nil {
		t.Fatalf("recompile: %v", err)
	}
	if m.Digest() != m2.Digest() {
		t.Errorf("digest not stable across compiles: %s vs %s", m.Digest(), m2.Digest())
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
	_, err := Compile(Definition{Name: "backlog-curation", Version: 1, Spec: spec}, WithGoobers(goobers))
	if err == nil {
		t.Fatal("expected compile to reject an ungranted repo:push capability, got nil error")
	}
}
