// Package providermatrix is the provider compile-matrix gate (ADO-N1,
// docs/design/ado-parity-dsl-2-0.md §8.1): every shipped workflow tree and
// every `goobers init --template=standard` scaffold is validated against a
// gaggle on each provider, so a DSL 2.0 workflow that cannot be configured on
// Azure DevOps fails here instead of on an adopter's instance.
//
// The gate is hermetic (no git, no network) so it runs in the unit shards.
package providermatrix

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/api/validate"
	"github.com/goobers/goobers/internal/instance"
)

// finding is one validation failure of one shipped subject on one provider.
// Object names the workflow (gaggle/workflow) or other definition the failure
// belongs to, and Diagnostic the specific capability or validation code, so an
// expected-failure entry covers exactly one gap and each fixing item deletes
// only its own entry.
type finding struct {
	Subject    string
	Provider   apiv1.Provider
	Object     string
	Diagnostic string
}

func (f finding) String() string {
	return fmt.Sprintf("%s on %s: %s: %s", f.Subject, f.Provider, f.Object, f.Diagnostic)
}

// assertedProviders fail the gate. Gitea is experimental and report-only
// (design §8.5): its results are logged, never asserted.
var assertedProviders = []apiv1.Provider{apiv1.ProviderGitHub, apiv1.ProviderADO}

// matrixProviders is every provider the gate validates against.
var matrixProviders = []apiv1.Provider{apiv1.ProviderGitHub, apiv1.ProviderADO, apiv1.ProviderGitea}

// expectedFailures lists the known gaps, each citing the item that closes it.
// The PR that closes a gap deletes its entry; an entry whose failure no longer
// occurs fails the gate, so the list cannot rot. Keep it keyed by one
// capability or diagnostic per entry so fixing items stay independent.
var expectedFailures = map[finding]string{}

// TestShippedWorkflowsCompileOnEveryProvider is the compile-matrix gate.
func TestShippedWorkflowsCompileOnEveryProvider(t *testing.T) {
	t.Parallel()
	var findings []finding
	loadFailed := map[subjectColumn]bool{}
	for _, subject := range matrixSubjects(t) {
		for _, provider := range matrixProviders {
			dir := subject.build(t, provider)
			subjectFindings, workflows, loaded := compileSubject(t, subject.name, provider, dir)
			if workflows == 0 && len(subjectFindings) == 0 {
				t.Fatalf("%s on %s loaded no workflows; the gate would pass vacuously", subject.name, provider)
			}
			if !loaded {
				loadFailed[subjectColumn{subject.name, provider}] = true
			}
			t.Logf("%s on %s: %d workflow(s), %d finding(s)", subject.name, provider, workflows, len(subjectFindings))
			findings = append(findings, subjectFindings...)
		}
	}
	for _, f := range findings {
		if f.Provider == apiv1.ProviderGitea {
			t.Logf("advisory (Gitea is report-only): %s", f)
		}
	}
	unexpected, stale := reconcileFindings(findings, expectedFailures)
	for _, f := range unexpected {
		t.Errorf("unexpected compile-matrix failure: %s (fix it, or add an expectedFailures entry naming the item that fixes it)", f)
	}
	for _, f := range stale {
		t.Error(staleEntryMessage(f, expectedFailures[f], loadFailed))
	}
}

// subjectColumn is one subject validated on one provider.
type subjectColumn struct {
	subject  string
	provider apiv1.Provider
}

// staleEntryMessage reports an expected failure that did not occur. A CONF-6
// (capability) entry for a subject whose load failed was never evaluated —
// compileSubject runs CONF-6 only on a tree that loads — so it is not stale:
// the advice there is to fix the load failure, never to delete the entry.
func staleEntryMessage(f finding, reason string, loadFailed map[subjectColumn]bool) string {
	if strings.HasPrefix(f.Diagnostic, capabilityDiagnosticPrefix) && loadFailed[subjectColumn{f.Subject, f.Provider}] {
		return fmt.Sprintf("expected failure not observed: %s (%s); CONF-6 not evaluated: load failed for %s on %s. "+
			"Fix that subject's validation findings first; do not delete this entry until CONF-6 runs and passes",
			f, reason, f.Subject, f.Provider)
	}
	return fmt.Sprintf("expected failure no longer occurs: %s (%s); delete its expectedFailures entry in the PR that fixed it", f, reason)
}

// reconcileFindings compares the asserted providers' findings against the
// expected-failure list. unexpected holds failures the list does not name;
// stale holds entries whose failure did not occur. Gitea findings and Gitea
// entries are never asserted.
func reconcileFindings(findings []finding, expected map[finding]string) (unexpected, stale []finding) {
	seen := make(map[finding]bool, len(findings))
	for _, f := range findings {
		if !isAsserted(f.Provider) || seen[f] {
			continue
		}
		seen[f] = true
		if _, ok := expected[f]; !ok {
			unexpected = append(unexpected, f)
		}
	}
	for f := range expected {
		if isAsserted(f.Provider) && !seen[f] {
			stale = append(stale, f)
		}
	}
	sortFindings(unexpected)
	sortFindings(stale)
	return unexpected, stale
}

func isAsserted(provider apiv1.Provider) bool {
	for _, asserted := range assertedProviders {
		if provider == asserted {
			return true
		}
	}
	return false
}

func sortFindings(findings []finding) {
	sort.Slice(findings, func(i, j int) bool { return findings[i].String() < findings[j].String() })
}

// compileSubject runs full config validation (api/validate schema, semantics,
// policy actions and manifest admission, through the production loader) and
// then CONF-6 for every workflow, returning each failure, the number of
// workflows loaded, and whether the tree loaded at all (CONF-6 runs only when
// it did).
func compileSubject(t *testing.T, subject string, provider apiv1.Provider, dir string) ([]finding, int, bool) {
	t.Helper()
	set, report, err := instance.LoadConfigDir(dir)
	if err != nil {
		return reportFindings(subject, provider, report, err), 0, false
	}
	var findings []finding
	for _, problem := range instance.ProviderCapabilityProblems(set) {
		findings = append(findings, finding{
			Subject: subject, Provider: provider,
			Object:     problem.Gaggle + "/" + problem.Workflow,
			Diagnostic: capabilityDiagnosticPrefix + string(problem.Capability),
		})
	}
	return findings, len(set.Workflows), true
}

// capabilityDiagnosticPrefix marks a CONF-6 (provider capability) finding.
const capabilityDiagnosticPrefix = "capability "

// reportFindings turns a failed load into one finding per error-severity
// issue, keyed by the issue's code (or message when it has none).
func reportFindings(subject string, provider apiv1.Provider, report *validate.Report, err error) []finding {
	var findings []finding
	if report != nil {
		for _, issue := range report.Issues {
			if issue.Severity != validate.Error {
				continue
			}
			diagnostic := string(issue.Code)
			if diagnostic == "" {
				diagnostic = issue.Message
			}
			findings = append(findings, finding{
				Subject: subject, Provider: provider,
				Object:     issueObject(issue),
				Diagnostic: "validate " + diagnostic,
			})
		}
	}
	if len(findings) == 0 {
		findings = append(findings, finding{Subject: subject, Provider: provider, Object: "load", Diagnostic: err.Error()})
	}
	return findings
}

func issueObject(issue validate.Issue) string {
	name := issue.Name
	if issue.Gaggle != "" {
		name = issue.Gaggle + "/" + name
	}
	if issue.Kind != "" && issue.Kind != "Workflow" {
		name = issue.Kind + " " + name
	}
	if name == "" {
		name = issue.File
	}
	return name
}

// matrixSubject is one shipped tree or scaffold; build materialises it in a
// fresh directory with its gaggles on provider and returns that directory.
type matrixSubject struct {
	name  string
	build func(t *testing.T, provider apiv1.Provider) string
}

func matrixSubjects(t *testing.T) []matrixSubject {
	t.Helper()
	root := repositoryRoot(t)
	subjects := []matrixSubject{
		shippedTreeSubject("reference-workflows", filepath.Join(root, "reference-workflows")),
		shippedTreeSubject("config-examples", filepath.Join(root, "config-examples")),
		shippedTreeSubject("examples/ios-simulator", filepath.Join(root, "examples", "ios-simulator")),
	}
	// The trees `goobers init` embeds and seeds (internal/instance's
	// starterFS), read from their checked-in source.
	for _, template := range []string{"starter", "quickstart-v1", "demo"} {
		subjects = append(subjects, shippedTreeSubject("init/"+template, filepath.Join(root, "internal", "instance", template)))
	}
	subjects = append(subjects, standaloneWorkflowSubject("examples/hello-world",
		filepath.Join(root, "internal", "instance", "starter"), "example", filepath.Join(root, "examples", "hello-world.yaml")))
	for _, variant := range scaffoldVariants {
		subjects = append(subjects, scaffoldSubject(variant))
	}
	return subjects
}

// shippedTreeSubject copies a checked-in config tree and rewrites every gaggle
// onto the provider under test.
func shippedTreeSubject(name, source string) matrixSubject {
	return matrixSubject{name: name, build: func(t *testing.T, provider apiv1.Provider) string {
		t.Helper()
		dir := filepath.Join(t.TempDir(), name)
		if err := os.CopyFS(dir, os.DirFS(source)); err != nil {
			t.Fatalf("copy %s: %v", source, err)
		}
		rewriteGaggles(t, dir, provider)
		return dir
	}}
}

// standaloneWorkflowSubject validates a shipped workflow file that has no
// instance tree of its own: it is dropped into gaggle's workflows in a copy
// of host, the tree whose gaggle it names, and that tree as a whole is
// compiled.
func standaloneWorkflowSubject(name, host, gaggle, workflow string) matrixSubject {
	hostSubject := shippedTreeSubject(name, host)
	return matrixSubject{name: name, build: func(t *testing.T, provider apiv1.Provider) string {
		t.Helper()
		dir := hostSubject.build(t, provider)
		raw, err := os.ReadFile(workflow)
		if err != nil {
			t.Fatalf("read %s: %v", workflow, err)
		}
		target := filepath.Join(dir, "gaggles", gaggle, "workflows", filepath.Base(workflow))
		if _, err := os.Stat(target); err == nil {
			t.Fatalf("%s already exists in %s; the subject would not add %s", target, host, workflow)
		}
		if err := os.WriteFile(target, raw, 0o600); err != nil {
			t.Fatalf("write %s: %v", target, err)
		}
		return dir
	}}
}

func repositoryRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve test source path")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
}

// TestProviderMatrixMutationDropsLandingGrant proves the gate has teeth: the
// shipped merge-review with github:pr:merge removed from merge-pr must fail
// validation on GitHub and on ADO.
func TestProviderMatrixMutationDropsLandingGrant(t *testing.T) {
	t.Parallel()
	source := filepath.Join(repositoryRoot(t), "reference-workflows")
	for _, provider := range assertedProviders {
		dir := shippedTreeSubject("reference-workflows", source).build(t, provider)
		dropMergePRLandingGrant(t, filepath.Join(dir, "gaggles", "goobers", "workflows", "merge-review.yaml"))
		_, report, err := instance.LoadConfigDir(dir)
		if err == nil {
			t.Errorf("merge-review without github:pr:merge on merge-pr validated on %s; the gate cannot catch a missing landing grant", provider)
			continue
		}
		if !reportNames(report, "github:pr:merge") {
			t.Errorf("mutated merge-review on %s failed for another reason than the missing landing grant: %v (report %+v)", provider, err, report)
		}
	}
}

// reportNames reports whether an error-severity issue in report mentions text.
func reportNames(report *validate.Report, text string) bool {
	if report == nil {
		return false
	}
	for _, issue := range report.Issues {
		if issue.Severity == validate.Error && strings.Contains(issue.Message, text) {
			return true
		}
	}
	return false
}

// dropMergePRLandingGrant removes the first `- github:pr:merge` capability
// line after merge-pr's command, which is merge-pr's own grant.
func dropMergePRLandingGrant(t *testing.T, path string) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	text := string(raw)
	command := strings.Index(text, `command: ["goobers", "merge-pr"]`)
	if command < 0 {
		t.Fatalf("%s has no merge-pr stage; the mutation would be vacuous", path)
	}
	const grant = "\n        - github:pr:merge\n"
	offset := strings.Index(text[command:], grant)
	if offset < 0 {
		t.Fatalf("merge-pr in %s declares no github:pr:merge; the mutation would be vacuous", path)
	}
	at := command + offset
	mutated := text[:at] + "\n" + text[at+len(grant):]
	if err := os.WriteFile(path, []byte(mutated), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// TestReconcileFindingsFailsOnStaleExpectedEntry is the meta-test: an
// expected-failure entry whose failure no longer occurs must be reported, an
// unlisted failure must be reported, and Gitea must never be asserted.
func TestReconcileFindingsFailsOnStaleExpectedEntry(t *testing.T) {
	t.Parallel()
	listed := finding{Subject: "s", Provider: apiv1.ProviderADO, Object: "g/w", Diagnostic: "capability pr.merge"}
	fixed := finding{Subject: "s", Provider: apiv1.ProviderADO, Object: "g/w", Diagnostic: "capability pr.compare"}
	unlisted := finding{Subject: "s", Provider: apiv1.ProviderGitHub, Object: "g/w", Diagnostic: "validate WF001"}
	gitea := finding{Subject: "s", Provider: apiv1.ProviderGitea, Object: "g/w", Diagnostic: "capability pr.merge"}
	giteaEntry := finding{Subject: "s", Provider: apiv1.ProviderGitea, Object: "g/x", Diagnostic: "capability pr.merge"}
	expected := map[finding]string{listed: "N-a", fixed: "N-b", giteaEntry: "advisory"}

	unexpected, stale := reconcileFindings([]finding{listed, unlisted, unlisted, gitea}, expected)
	if len(unexpected) != 1 || unexpected[0] != unlisted {
		t.Errorf("unexpected = %v, want only %v", unexpected, unlisted)
	}
	if len(stale) != 1 || stale[0] != fixed {
		t.Errorf("stale = %v, want only %v (an entry that unexpectedly passes must fail the gate)", stale, fixed)
	}
}

// TestExpectedFailuresNameTheirFixingItem keeps each entry actionable.
func TestExpectedFailuresNameTheirFixingItem(t *testing.T) {
	t.Parallel()
	for f, reason := range expectedFailures {
		if !strings.HasPrefix(reason, "ADO-N") && !strings.HasPrefix(reason, "unowned") {
			t.Errorf("expectedFailures[%s] = %q; cite the fixing item (ADO-Nx) or say unowned", f, reason)
		}
		if !isAsserted(f.Provider) {
			t.Errorf("expectedFailures[%s] is for report-only provider %s; Gitea results are never asserted", f, f.Provider)
		}
	}
}

// TestStaleEntryMessageNamesASkippedCONF6: when a subject fails to load,
// CONF-6 never runs for it, so its capability entries go unobserved without
// being fixed. The gate must say so rather than advise deleting them.
func TestStaleEntryMessageNamesASkippedCONF6(t *testing.T) {
	t.Parallel()
	capability := finding{Subject: "s", Provider: apiv1.ProviderADO, Object: "g/w", Diagnostic: capabilityDiagnosticPrefix + "pr.merge"}
	validation := finding{Subject: "s", Provider: apiv1.ProviderADO, Object: "g/w", Diagnostic: "validate WF001"}
	failed := map[subjectColumn]bool{{"s", apiv1.ProviderADO}: true}

	skipped := staleEntryMessage(capability, "ADO-N1", failed)
	if !strings.Contains(skipped, "CONF-6 not evaluated: load failed") || strings.Contains(skipped, "delete its expectedFailures entry") {
		t.Errorf("capability entry on a subject that failed to load: %q; want the CONF-6-not-evaluated note and no delete advice", skipped)
	}
	for name, msg := range map[string]string{
		"capability entry on a subject that loaded":         staleEntryMessage(capability, "ADO-N1", map[subjectColumn]bool{{"s", apiv1.ProviderGitHub}: true}),
		"validation entry on a subject that failed to load": staleEntryMessage(validation, "ADO-N1", failed),
	} {
		if !strings.Contains(msg, "delete its expectedFailures entry") || strings.Contains(msg, "CONF-6 not evaluated") {
			t.Errorf("%s: %q; want the delete advice", name, msg)
		}
	}
}
