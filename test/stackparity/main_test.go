package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The shipped tree must be green, or the gate is describing something other
// than what this repository actually contains.
func TestShippedTreePassesBothChecks(t *testing.T) {
	findings, err := verify(repoRoot(t))
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if len(findings) != 0 {
		t.Fatalf("findings on the shipped tree:\n%s", strings.Join(findings, "\n"))
	}
}

// #2554: the exact residue this gate exists for. A non-Go gaggle whose
// workflow still declares the Go default is caught, and the message names the
// file and the offending literal so the fix needs no further investigation.
func TestRejectsGoResidueInNonGoExample(t *testing.T) {
	root := scratchTree(t)
	writeGaggle(t, root, "webby", `["npm", "run", "ci"]`)
	writeWorkflow(t, root, "webby", "implementation.yaml", `["make", "ci"]`)

	findings, err := verifyExampleCommands(root)
	if err != nil {
		t.Fatalf("verifyExampleCommands: %v", err)
	}
	joined := strings.Join(findings, "\n")
	if len(findings) == 0 {
		t.Fatal("no finding for a non-Go gaggle declaring [make ci]")
	}
	for _, want := range []string{"implementation.yaml", "make ci", "webby"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("finding must name %q; got:\n%s", want, joined)
		}
	}
}

// The stronger half of the same rule: the local-ci literal must equal the
// gaggle's declared ciCommand, so the example cannot show one stack-native
// command while the gaggle resolves a different one.
func TestRejectsLocalCIDisagreeingWithCICommand(t *testing.T) {
	root := scratchTree(t)
	writeGaggle(t, root, "webby", `["npm", "run", "ci"]`)
	writeWorkflow(t, root, "webby", "implementation.yaml", `["npm", "test"]`)

	findings, err := verifyExampleCommands(root)
	if err != nil {
		t.Fatalf("verifyExampleCommands: %v", err)
	}
	if len(findings) != 1 || !strings.Contains(findings[0], "declares ciCommand") {
		t.Fatalf("findings = %v, want one ciCommand-disagreement finding", findings)
	}
}

// A matching stack-native command is the shape the shipped examples now use.
func TestAcceptsStackNativeCommand(t *testing.T) {
	root := scratchTree(t)
	writeGaggle(t, root, "webby", `["npm", "run", "ci"]`)
	writeWorkflow(t, root, "webby", "implementation.yaml", `["npm", "run", "ci"]`)

	findings, err := verifyExampleCommands(root)
	if err != nil {
		t.Fatalf("verifyExampleCommands: %v", err)
	}
	if len(findings) != 0 {
		t.Fatalf("findings = %v, want none", findings)
	}
}

// The documented-fallback escape hatch, and its precondition: a marker with no
// reason is not an exemption, only a way to switch the gate off.
func TestGoFallbackMarkerRequiresAReason(t *testing.T) {
	root := scratchTree(t)
	writeGaggle(t, root, "webby", `["npm", "run", "ci"]`)
	dir := filepath.Join(root, filepath.FromSlash(gagglesDir), "webby", "workflows")
	mustWrite(t, filepath.Join(dir, "fallback.yaml"), `apiVersion: goobers.dev/v1alpha1
kind: Workflow
metadata:
  name: fallback
spec:
  gaggle: webby
  tasks:
    - name: local-ci
      type: deterministic
      run:
        # stackparity:go-fallback demonstrating the no-ciCommand default
        command: ["make", "ci"]
    - name: bare-marker
      type: deterministic
      run:
        # stackparity:go-fallback
        command: ["go", "test", "./..."]
`)

	findings, err := verifyExampleCommands(root)
	if err != nil {
		t.Fatalf("verifyExampleCommands: %v", err)
	}
	joined := strings.Join(findings, "\n")
	if strings.Contains(joined, "local-ci") {
		t.Fatalf("the reasoned marker must exempt its line; got:\n%s", joined)
	}
	if !strings.Contains(joined, "bare-marker") || !strings.Contains(joined, "go test") {
		t.Fatalf("a marker with no reason must not exempt anything; got:\n%s", joined)
	}
}

// A gaggle that declares no ciCommand has nothing resolved away, so its own
// literal is the truth and the gate stays out of it — that is the Go reference
// gaggle's shape.
func TestIgnoresGaggleWithoutCICommand(t *testing.T) {
	root := scratchTree(t)
	writeGaggle(t, root, "goish", "")
	writeWorkflow(t, root, "goish", "implementation.yaml", `["make", "ci"]`)

	findings, err := verifyExampleCommands(root)
	if err != nil {
		t.Fatalf("verifyExampleCommands: %v", err)
	}
	if len(findings) != 0 {
		t.Fatalf("findings = %v, want none", findings)
	}
}

// #2555: the untruthful claim the tier table could previously carry. A row
// says CI-green while nothing in CI enables that stack's leg.
func TestRejectsCIGreenClaimWithNoEnabledLeg(t *testing.T) {
	want := stackEvidence{
		stack:     "Fictional",
		status:    statusCIGreen,
		reference: "config-examples/gaggles/dotnet-service",
		e2eTest:   "test/e2e/dotnet_gaggle_integration_test.go",
		e2eEnv:    "GOOBERS_FICTIONAL_E2E",
	}
	row := tierRow{stack: "Fictional", status: statusCIGreen, reference: want.reference, line: 1}
	findings := evidenceFindings(repoRoot(t), want, row, "jobs:\n  integration:\n")
	if len(findings) != 1 || !strings.Contains(findings[0], "never sets GOOBERS_FICTIONAL_E2E") {
		t.Fatalf("findings = %v, want one unenabled-leg finding", findings)
	}
}

// And the reverse, which is what keeps the label from going stale in the other
// direction: turning the leg on in CI without promoting the row.
func TestRejectsLocalClaimWhenCIEnablesTheLeg(t *testing.T) {
	want := stackEvidence{
		stack:     "Fictional",
		status:    statusLocalGreen,
		reference: "config-examples/gaggles/dotnet-service",
		e2eTest:   "test/e2e/dotnet_gaggle_integration_test.go",
		e2eEnv:    "GOOBERS_FICTIONAL_E2E",
	}
	row := tierRow{stack: "Fictional", status: statusLocalGreen, reference: want.reference, line: 1}
	findings := evidenceFindings(repoRoot(t), want, row, "        env:\n          GOOBERS_FICTIONAL_E2E: \"1\"\n")
	if len(findings) != 1 || !strings.Contains(findings[0], "promote the row") {
		t.Fatalf("findings = %v, want one promote-the-row finding", findings)
	}
}

// A commented-out assignment does not enable anything, which is the drift a
// naive substring search would read as coverage.
func TestEnablesEnvIgnoresCommentedAndFalsyAssignments(t *testing.T) {
	for name, ci := range map[string]string{
		"commented": "          # GOOBERS_JAVA_E2E: \"1\"\n",
		"empty":     "          GOOBERS_JAVA_E2E:\n",
		"zero":      "          GOOBERS_JAVA_E2E: \"0\"\n",
		"false":     "          GOOBERS_JAVA_E2E: \"false\"\n",
	} {
		if enablesEnv(ci, "GOOBERS_JAVA_E2E") {
			t.Errorf("%s: enablesEnv = true, want false", name)
		}
	}
	if !enablesEnv("          GOOBERS_JAVA_E2E: \"1\"\n", "GOOBERS_JAVA_E2E") {
		t.Error("a plain truthy assignment must count as enabled")
	}
}

// A tier-table row nobody wrote evidence for is exactly the state #2555
// describes: a published claim that nothing checks.
func TestRejectsShippedRowWithNoEvidenceEntry(t *testing.T) {
	root := scratchTree(t)
	mustWrite(t, filepath.Join(root, filepath.FromSlash(ciWorkflow)), "jobs: {}\n")
	table := "| Stack | Tier | Reference | Status |\n|---|---|---|---|\n"
	for _, want := range stackEvidenceTable {
		table += "| " + want.stack + " | tier | `" + want.reference + "/` | " + want.status + " |\n"
	}
	table += "| Rust | First-class | `config-examples/gaggles/rust-service/` | Shipped, CI-green |\n"
	mustWrite(t, filepath.Join(root, filepath.FromSlash(tierTableDoc)), table)
	// The evidence rows point at real paths in the checkout, so mirror them in.
	linkRepoPaths(t, root)

	findings, err := verifyTierTable(root)
	if err != nil {
		t.Fatalf("verifyTierTable: %v", err)
	}
	joined := strings.Join(findings, "\n")
	if !strings.Contains(joined, `"Rust"`) || !strings.Contains(joined, "no evidence entry") {
		t.Fatalf("want a missing-evidence finding for Rust; got:\n%s", joined)
	}
}

func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("no go.mod above the working directory")
		}
		dir = parent
	}
}

func scratchTree(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, filepath.FromSlash(gagglesDir)), 0o755); err != nil {
		t.Fatal(err)
	}
	return root
}

// linkRepoPaths copies the reference-gaggle and integration-test paths the
// evidence table names into a scratch tree, so an existence check in a
// tier-table test is not answering a question about the temp directory.
func linkRepoPaths(t *testing.T, root string) {
	t.Helper()
	for _, want := range stackEvidenceTable {
		mustWrite(t, filepath.Join(root, filepath.FromSlash(want.reference), "gaggle.yaml"), "{}\n")
		if want.e2eTest != "" {
			mustWrite(t, filepath.Join(root, filepath.FromSlash(want.e2eTest)), "package e2e\n")
		}
	}
}

func writeGaggle(t *testing.T, root, name, ciCommand string) {
	t.Helper()
	body := "apiVersion: goobers.dev/v1alpha1\nkind: Gaggle\nmetadata:\n  name: " + name + "\nspec:\n  displayName: " + name + "\n"
	if ciCommand != "" {
		body += "  ciCommand: " + ciCommand + "\n"
	}
	mustWrite(t, filepath.Join(root, filepath.FromSlash(gagglesDir), name, "gaggle.yaml"), body)
}

func writeWorkflow(t *testing.T, root, gaggle, file, command string) {
	t.Helper()
	body := "apiVersion: goobers.dev/v1alpha1\nkind: Workflow\nmetadata:\n  name: wf\nspec:\n  gaggle: " + gaggle +
		"\n  tasks:\n    - name: local-ci\n      type: deterministic\n      run:\n        command: " + command + "\n"
	mustWrite(t, filepath.Join(root, filepath.FromSlash(gagglesDir), gaggle, "workflows", file), body)
}

func mustWrite(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}
