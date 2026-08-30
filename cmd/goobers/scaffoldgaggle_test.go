package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestScaffoldGaggleCreatesEmptyGaggle reproduces the missing scaffold-ladder
// rung (cold-start swift #1, dotnet #9): "there is no `goobers scaffold
// gaggle`". With no --from it must create a validate-clean, empty gaggle
// (gaggle.yaml + manifest registration) ready for scaffold goober/workflow.
func TestScaffoldGaggleCreatesEmptyGaggle(t *testing.T) {
	root := initDemo(t)

	code, stdout, stderr := runArgs(t, "scaffold", "gaggle", "billing", root)
	if code != 0 {
		t.Fatalf("scaffold gaggle: code=%d stderr=%q", code, stderr)
	}
	gaggleYAML := filepath.Join(root, "config", "gaggles", "billing", "gaggle.yaml")
	manifestPath := filepath.Join(root, "config", "manifest.yaml")
	wantOutput := "created " + gaggleYAML + "\n" +
		"updated " + manifestPath + "\n" +
		"next: goobers validate " + root + "\n"
	if stdout != wantOutput {
		t.Fatalf("scaffold gaggle stdout = %q, want %q", stdout, wantOutput)
	}

	content, err := os.ReadFile(gaggleYAML)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`name: "billing"`,
		`namespace: "gaggle-billing"`,
	} {
		if !strings.Contains(string(content), want) {
			t.Fatalf("gaggle.yaml missing %q:\n%s", want, content)
		}
	}

	manifest, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(manifest), "    - example\n    - billing\n") {
		t.Fatalf("manifest.yaml gaggles list was not appended in place:\n%s", manifest)
	}
	// Comments in the manifest must survive the surgical node edit.
	if !strings.Contains(string(manifest), "Named, reusable connections") {
		t.Fatalf("manifest.yaml lost its comments:\n%s", manifest)
	}

	code, stdout, stderr = runArgs(t, "validate", root)
	if code != 0 {
		t.Fatalf("validate: code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "2 gaggle(s), 1 goober(s), 1 workflow(s)") {
		t.Fatalf("validate did not load the new gaggle: %q", stdout)
	}
	for _, warning := range warningLines(stdout) {
		if strings.HasPrefix(warning, "WARNING "+placeholderFindingCode+" ") ||
			strings.Contains(warning, "has no schedule trigger") {
			continue
		}
		t.Fatalf("unexpected validate warning for a freshly scaffolded gaggle: %s", warning)
	}
}

// TestScaffoldGaggleNewOmitsConnectionRef pins that a scaffolded gaggle never
// declares a connectionRef, whatever the manifest declares. The local runtime
// resolves every access's credential from instance.yaml repos[] by repository
// identity and never consults the field (#3296), so seeding one would hand a
// brand-new gaggle a REF012 finding — and an empty connectionRef is valid to
// REF004 ("left alone").
func TestScaffoldGaggleNewOmitsConnectionRef(t *testing.T) {
	root := initDemo(t)
	manifestPath := filepath.Join(root, "config", "manifest.yaml")
	replaceInFile(t, manifestPath, "        name: repo-token\n", "        name: repo-token\n"+
		"    - name: second-token\n"+
		"      type: repo\n"+
		"      provider: github\n"+
		"      secretRef:\n"+
		"        name: second-token\n")

	code, _, stderr := runArgs(t, "scaffold", "gaggle", "billing", root)
	if code != 0 {
		t.Fatalf("scaffold gaggle: code=%d stderr=%q", code, stderr)
	}
	gaggleYAML := filepath.Join(root, "config", "gaggles", "billing", "gaggle.yaml")
	content, err := os.ReadFile(gaggleYAML)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(content), "connectionRef") {
		t.Fatalf("gaggle.yaml seeded a connectionRef the runtime cannot honor:\n%s", content)
	}

	// Still validate-clean: an empty connectionRef is not a REF004 finding.
	code, stdout, stderr := runArgs(t, "validate", root)
	if code != 0 {
		t.Fatalf("validate: code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	if strings.Contains(stdout, "REF004") {
		t.Fatalf("empty connectionRef produced a REF004 finding:\n%s", stdout)
	}
}

func TestScaffoldGaggleRejectsAlreadyRegisteredName(t *testing.T) {
	root := initDemo(t)
	code, _, stderr := runArgs(t, "scaffold", "gaggle", "example", root)
	if code != 2 || !strings.Contains(stderr, `gaggle "example" is already registered`) {
		t.Fatalf("code=%d stderr=%q", code, stderr)
	}
}

func TestScaffoldGaggleRefusesOverwriteUnlessForced(t *testing.T) {
	root := initDemo(t)
	dir := filepath.Join(root, "config", "gaggles", "billing")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	gaggleYAML := filepath.Join(dir, "gaggle.yaml")
	if err := os.WriteFile(gaggleYAML, []byte("keep me\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	code, _, stderr := runArgs(t, "scaffold", "gaggle", "billing", root)
	if code != 1 || !strings.Contains(stderr, "refusing to overwrite") {
		t.Fatalf("without force: code=%d stderr=%q", code, stderr)
	}
	got, err := os.ReadFile(gaggleYAML)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "keep me\n" {
		t.Fatalf("existing file was changed: %q", got)
	}
	manifest, err := os.ReadFile(filepath.Join(root, "config", "manifest.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(manifest), "billing") {
		t.Fatalf("manifest was registered despite the write refusal:\n%s", manifest)
	}

	code, _, stderr = runArgs(t, "scaffold", "gaggle", "billing", "--force", root)
	if code != 0 {
		t.Fatalf("with force: code=%d stderr=%q", code, stderr)
	}
	got, err = os.ReadFile(gaggleYAML)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) == "keep me\n" {
		t.Fatalf("forced scaffold did not replace gaggle.yaml")
	}
}

// TestScaffoldGaggleRenameRewritesIdentity reproduces the cold-start rename
// pain directly (swift #1: "directory move, plus gaggle.yaml metadata.name,
// isolation.namespace, and config/manifest.yaml's gaggles list"; dotnet #9
// hand-edited "the directory name, metadata.name in gaggle.yaml, the
// spec.gaggles list in config/manifest.yaml — plus spec.gaggle in every
// goober and workflow"). One command must do all of that, using the exact
// invocation shape the task names
// (`goobers scaffold gaggle ledger --from example`, positional name before
// the --from flag).
func TestScaffoldGaggleRenameRewritesIdentity(t *testing.T) {
	root := initDemo(t)
	gaggleDir := filepath.Join(root, "config", "gaggles", "example")
	oldGooberYAML := filepath.Join(gaggleDir, "goobers", "coder", "goober.yaml")
	if before, err := os.ReadFile(oldGooberYAML); err != nil || !strings.Contains(string(before), "gaggle: example") {
		t.Fatalf("fixture precondition: coder goober.yaml = %q, %v", before, err)
	}

	code, stdout, stderr := runArgs(t, "scaffold", "gaggle", "ledger", "--from", "example", root)
	if code != 0 {
		t.Fatalf("scaffold gaggle --from: code=%d stderr=%q", code, stderr)
	}
	newDir := filepath.Join(root, "config", "gaggles", "ledger")
	if !strings.Contains(stdout, "moved   "+gaggleDir+" -> "+newDir+"\n") {
		t.Fatalf("stdout missing the directory move: %q", stdout)
	}
	if _, err := os.Stat(gaggleDir); !os.IsNotExist(err) {
		t.Fatalf("old gaggle directory %s still exists: %v", gaggleDir, err)
	}

	// gaggle.yaml: metadata.name and isolation.namespace rewritten, project/
	// backlog/comments untouched.
	gaggleYAML, err := os.ReadFile(filepath.Join(newDir, "gaggle.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"name: ledger",
		"namespace: gaggle-ledger",
		// unrelated fields survive verbatim
		"owner: your-org",
		"The project codebase this gaggle works on",
	} {
		if !strings.Contains(string(gaggleYAML), want) {
			t.Fatalf("renamed gaggle.yaml missing %q:\n%s", want, gaggleYAML)
		}
	}
	if strings.Contains(string(gaggleYAML), "name: example") {
		t.Fatalf("renamed gaggle.yaml still names the old gaggle:\n%s", gaggleYAML)
	}

	// goober.yaml: spec.gaggle rewritten, everything else (including
	// comments) untouched.
	gooberYAML, err := os.ReadFile(filepath.Join(newDir, "goobers", "coder", "goober.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(gooberYAML), "gaggle: ledger") {
		t.Fatalf("goober.yaml spec.gaggle was not rewritten:\n%s", gooberYAML)
	}
	if !strings.Contains(string(gooberYAML), "relative to this goober definition directory") {
		t.Fatalf("goober.yaml lost its comments:\n%s", gooberYAML)
	}
	if !strings.Contains(string(gooberYAML), "name: coder") {
		t.Fatalf("goober.yaml's own metadata.name changed (must stay instance-global-unique unchanged):\n%s", gooberYAML)
	}

	// workflow.yaml: spec.gaggle rewritten.
	workflowYAML, err := os.ReadFile(filepath.Join(newDir, "workflows", "default-implement.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(workflowYAML), "gaggle: ledger") {
		t.Fatalf("workflow.yaml spec.gaggle was not rewritten:\n%s", workflowYAML)
	}

	// The skill packages that clear SKILL002 move with the directory.
	if _, err := os.Stat(filepath.Join(newDir, "skills", "implement", "SKILL.md")); err != nil {
		t.Fatalf("skill package did not move with the gaggle: %v", err)
	}

	// manifest.yaml: "example" replaced by "ledger" at the same position,
	// comments preserved.
	manifest, err := os.ReadFile(filepath.Join(root, "config", "manifest.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(manifest), "gaggles:\n    - ledger\n") {
		t.Fatalf("manifest.yaml gaggles list was not rewritten in place:\n%s", manifest)
	}
	if strings.Contains(string(manifest), "- example") {
		t.Fatalf("manifest.yaml still lists the old gaggle name:\n%s", manifest)
	}
	if !strings.Contains(string(manifest), "Named, reusable connections") {
		t.Fatalf("manifest.yaml lost its comments:\n%s", manifest)
	}

	// The whole point: validate is clean (module the pre-existing,
	// unavoidable manual-trigger and placeholder warnings the starter
	// scaffold always carries).
	code, stdout, stderr = runArgs(t, "validate", root)
	if code != 0 {
		t.Fatalf("validate: code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	for _, warning := range warningLines(stdout) {
		if strings.HasPrefix(warning, "WARNING "+placeholderFindingCode+" ") ||
			strings.Contains(warning, "has no schedule trigger") {
			continue
		}
		t.Fatalf("unexpected validate warning after rename: %s", warning)
	}

	// And the renamed gaggle is a normal scaffold target: scaffold goober
	// resolves it by directory without a --gaggle flag.
	if code, _, stderr := runArgs(t, "scaffold", "goober", "reviewer", newDir); code != 0 {
		t.Fatalf("scaffold goober against renamed gaggle: code=%d stderr=%q", code, stderr)
	}
}

func TestScaffoldGaggleRenameRejectsForce(t *testing.T) {
	root := initDemo(t)
	code, _, stderr := runArgs(t, "scaffold", "gaggle", "ledger", "--from", "example", "--force", root)
	if code != 2 || !strings.Contains(stderr, "--force is not accepted with --from") {
		t.Fatalf("code=%d stderr=%q", code, stderr)
	}
	if _, err := os.Stat(filepath.Join(root, "config", "gaggles", "example")); err != nil {
		t.Fatalf("source gaggle moved despite the flag conflict: %v", err)
	}
}

func TestScaffoldGaggleRenameRejectsUnknownFrom(t *testing.T) {
	root := initDemo(t)
	code, _, stderr := runArgs(t, "scaffold", "gaggle", "ledger", "--from", "nosuch", root)
	if code != 2 || !strings.Contains(stderr, `gaggle "nosuch" is not active`) {
		t.Fatalf("code=%d stderr=%q", code, stderr)
	}
}

func TestScaffoldGaggleRenameRejectsSameName(t *testing.T) {
	root := initDemo(t)
	code, _, stderr := runArgs(t, "scaffold", "gaggle", "example", "--from", "example", root)
	if code != 2 || !strings.Contains(stderr, "must differ") {
		t.Fatalf("code=%d stderr=%q", code, stderr)
	}
}

func TestScaffoldGaggleRenameRefusesExistingTargetDirectory(t *testing.T) {
	root := initDemo(t)
	// An unregistered stray directory at the target path (not itself a
	// manifest-active gaggle, so it clears the earlier "already registered"
	// check) must still block the move rather than silently merge two
	// gaggles' files together.
	strayDir := filepath.Join(root, "config", "gaggles", "stray")
	if err := os.MkdirAll(strayDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(strayDir, "keep.txt"), []byte("mine\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	code, _, stderr := runArgs(t, "scaffold", "gaggle", "stray", "--from", "example", root)
	if code != 1 || !strings.Contains(stderr, "refusing to rename onto existing gaggle directory") {
		t.Fatalf("code=%d stderr=%q", code, stderr)
	}
	if _, err := os.Stat(filepath.Join(root, "config", "gaggles", "example")); err != nil {
		t.Fatalf("source gaggle moved despite the target collision: %v", err)
	}
	if _, err := os.Stat(filepath.Join(strayDir, "keep.txt")); err != nil {
		t.Fatalf("stray target directory was disturbed: %v", err)
	}
}

func TestScaffoldGaggleRejectsInvalidNames(t *testing.T) {
	root := initDemo(t)
	tests := []struct {
		args []string
		want string
	}{
		{[]string{"scaffold", "gaggle", "../escape", root}, "invalid name"},
		{[]string{"scaffold", "gaggle", "ledger", "--from", "../escape", root}, "invalid --from name"},
	}
	for _, tc := range tests {
		code, _, stderr := runArgs(t, tc.args...)
		if code != 2 || !strings.Contains(stderr, tc.want) {
			t.Fatalf("args=%v code=%d stderr=%q, want %q", tc.args, code, stderr, tc.want)
		}
	}
}
