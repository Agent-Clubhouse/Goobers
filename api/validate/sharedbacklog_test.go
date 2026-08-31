package validate

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// BKL001 (personal-gaggle-routing §5.8) is the authoring-time counterpart to
// the backlog-scoped claim ledger: once ownership is keyed by backlog rather
// than by gaggle, two same-instance gaggles drawing from one backlog with
// non-disjoint selectors do not duplicate work — they race, and the loser
// starves. These tests pin the exact boundary between the topology the feature
// enables and the misconfiguration it must reject.

const sharedBacklogManifest = `apiVersion: goobers.dev/v1alpha1
kind: Manifest
metadata: {name: inst}
spec:
  instance: {name: acme, environment: dev}
  gaggles: [alpha, beta]
`

func sharedBacklogGaggle(name string) string {
	return `apiVersion: goobers.dev/v1alpha1
kind: Gaggle
metadata: {name: ` + name + `}
spec:
  project: {provider: github, owner: acme, name: web}
  backlog: {provider: github, project: acme/private-backlog}
  isolation: {namespace: gaggle-` + name + `}
`
}

func sharedBacklogWorkflow(gaggle, requireLabels string) string {
	return `apiVersion: goobers.dev/v1alpha1
kind: Workflow
metadata: {name: ` + gaggle + `-implementation}
spec:
  gaggle: ` + gaggle + `
  entry: query-backlog
  tasks:
    - name: query-backlog
      type: deterministic
      goal: Claim one approved backlog item.
      run:
        command: ["goobers", "backlog-query", "--claim"]
      inputs:
        trustLabel: "goobers:approved"
        requireLabels: "` + requireLabels + `"
        resultFile: "claimed-item.json"
      capabilities:
        - github:issues:write
      policyActions:
        - claim-backlog-items
`
}

// writeSharedBacklogTree builds a two-gaggle instance whose gaggles share one
// backlog, with the given effective claim selectors.
func writeSharedBacklogTree(t *testing.T, alphaLabels, betaLabels string) string {
	t.Helper()
	dir := t.TempDir()
	write := func(rel, content string) {
		full := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("manifest.yaml", sharedBacklogManifest)
	write("gaggles/alpha/gaggle.yaml", sharedBacklogGaggle("alpha"))
	write("gaggles/beta/gaggle.yaml", sharedBacklogGaggle("beta"))
	write("gaggles/alpha/workflows/implementation.yaml", sharedBacklogWorkflow("alpha", alphaLabels))
	write("gaggles/beta/workflows/implementation.yaml", sharedBacklogWorkflow("beta", betaLabels))
	return dir
}

func sharedBacklogDiagnostics(t *testing.T, dir string) string {
	t.Helper()
	v := newV(t)
	report, err := v.ValidateDir(dir)
	if err != nil {
		t.Fatalf("ValidateDir: %v", err)
	}
	var found []string
	for _, issue := range report.Issues {
		if issue.Code == errorSharedBacklogOverlap {
			found = append(found, issue.String())
		}
	}
	return strings.Join(found, "\n")
}

// TestSharedBacklogDisjointSelectorsAreAccepted is the reference topology:
// a shared routing label plus per-destination repo labels. Rejecting this would
// make the whole personal-gaggle routing design unauthorable, so it is the most
// important case in this file.
func TestSharedBacklogDisjointSelectorsAreAccepted(t *testing.T) {
	dir := writeSharedBacklogTree(t,
		"goobers:routed,repo:dev-brandiv",
		"goobers:routed,repo:dev-other",
	)
	if got := sharedBacklogDiagnostics(t, dir); got != "" {
		t.Fatalf("disjoint selectors must be accepted, got:\n%s", got)
	}
}

// TestSharedBacklogIdenticalSelectorsAreRejected is the plain race.
func TestSharedBacklogIdenticalSelectorsAreRejected(t *testing.T) {
	dir := writeSharedBacklogTree(t, "goobers:ready", "goobers:ready")
	got := sharedBacklogDiagnostics(t, dir)
	if got == "" {
		t.Fatal("two gaggles claiming one backlog with identical selectors must be reported")
	}
	if !strings.Contains(got, "provably disjoint") {
		t.Fatalf("diagnostic should explain the disjointness requirement, got:\n%s", got)
	}
}

// TestSharedBacklogSubsetSelectorIsRejected is the subtler failure the naive
// "do they share a label?" check gets backwards: alpha's broader selector
// matches every item beta can claim, so alpha races beta for all of beta's work
// even though the two selectors are not identical.
func TestSharedBacklogSubsetSelectorIsRejected(t *testing.T) {
	dir := writeSharedBacklogTree(t, "goobers:routed", "goobers:routed,repo:dev-other")
	got := sharedBacklogDiagnostics(t, dir)
	if got == "" {
		t.Fatal("a selector that is a subset of a sibling's must be reported")
	}
	if !strings.Contains(got, "alpha") {
		t.Fatalf("the BROADER gaggle should be named as the problem, got:\n%s", got)
	}
}

// TestSharedBacklogEmptySelectorIsRejected: an empty selector claims the whole
// backlog, so it can never be disjoint from anything.
func TestSharedBacklogEmptySelectorIsRejected(t *testing.T) {
	dir := writeSharedBacklogTree(t, "", "goobers:routed,repo:dev-other")
	got := sharedBacklogDiagnostics(t, dir)
	if got == "" {
		t.Fatal("an empty claim selector on a shared backlog must be reported")
	}
	if !strings.Contains(got, "empty effective requireLabels") {
		t.Fatalf("diagnostic should name the empty selector, got:\n%s", got)
	}
}

// TestDistinctBacklogsDoNotOverlap is the necessary converse: personal and team
// gaggles targeting ONE repository from DIFFERENT backlogs must not trip this
// check — that is exactly the topology §5.8 exists to permit, and a false
// positive here would be indistinguishable from the real defect.
func TestDistinctBacklogsDoNotOverlap(t *testing.T) {
	dir := t.TempDir()
	write := func(rel, content string) {
		full := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("manifest.yaml", sharedBacklogManifest)
	write("gaggles/alpha/gaggle.yaml", sharedBacklogGaggle("alpha"))
	// beta targets the SAME project repo but a different backlog.
	write("gaggles/beta/gaggle.yaml", `apiVersion: goobers.dev/v1alpha1
kind: Gaggle
metadata: {name: beta}
spec:
  project: {provider: github, owner: acme, name: web}
  backlog: {provider: github, project: acme/team-backlog}
  isolation: {namespace: gaggle-beta}
`)
	write("gaggles/alpha/workflows/implementation.yaml", sharedBacklogWorkflow("alpha", "goobers:ready"))
	write("gaggles/beta/workflows/implementation.yaml", sharedBacklogWorkflow("beta", "goobers:ready"))

	if got := sharedBacklogDiagnostics(t, dir); got != "" {
		t.Fatalf("gaggles with DIFFERENT backlogs must not be reported as overlapping, got:\n%s", got)
	}
}

// TestInactiveGaggleIsNotASharedBacklogConsumer scopes "same instance" to the
// manifest: a config tree is a catalog, and an alternative gaggle definition
// that no manifest activates never shares a running instance's claim ledger.
func TestInactiveGaggleIsNotASharedBacklogConsumer(t *testing.T) {
	dir := writeSharedBacklogTree(t, "goobers:ready", "goobers:ready")
	// Re-declare the manifest with only alpha active; beta stays authored and
	// validated but is not part of this instance.
	manifest := strings.Replace(sharedBacklogManifest, "gaggles: [alpha, beta]", "gaggles: [alpha]", 1)
	if err := os.WriteFile(filepath.Join(dir, "manifest.yaml"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := sharedBacklogDiagnostics(t, dir); got != "" {
		t.Fatalf("a gaggle absent from the manifest must not count as a same-instance consumer, got:\n%s", got)
	}
}

// TestReconcileOnlyGaggleIsNotAClaimConsumer: --reconcile and --release declare
// no selector because they take no work. Counting them as claim scopes would
// report a spurious empty-selector error for ordinary curation gaggles.
func TestReconcileOnlyGaggleIsNotAClaimConsumer(t *testing.T) {
	dir := writeSharedBacklogTree(t, "goobers:routed,repo:a", "goobers:routed,repo:b")
	reconcileWorkflow := `apiVersion: goobers.dev/v1alpha1
kind: Workflow
metadata: {name: alpha-reconcile}
spec:
  gaggle: alpha
  entry: reconcile-backlog
  tasks:
    - name: reconcile-backlog
      type: deterministic
      goal: Reconcile drifted backlog labels.
      run:
        command: ["goobers", "backlog-query", "--reconcile"]
      inputs:
        trustLabel: "goobers:approved"
        resultFile: "backlog-reconciliation.json"
      capabilities:
        - github:issues:write
      policyActions:
        - claim-backlog-items
        - close-issue
`
	path := filepath.Join(dir, "gaggles", "alpha", "workflows", "reconcile.yaml")
	if err := os.WriteFile(path, []byte(reconcileWorkflow), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := sharedBacklogDiagnostics(t, dir); got != "" {
		t.Fatalf("a --reconcile task must not be treated as a claim selector, got:\n%s", got)
	}
}

func TestSubtractLabels(t *testing.T) {
	got := subtractLabels([]string{"a", "b", "c"}, []string{"b"})
	if len(got) != 2 || got[0] != "a" || got[1] != "c" {
		t.Fatalf("subtractLabels = %v, want [a c]", got)
	}
	if len(subtractLabels([]string{"a"}, []string{"a"})) != 0 {
		t.Fatal("identical sets should subtract to empty")
	}
}
