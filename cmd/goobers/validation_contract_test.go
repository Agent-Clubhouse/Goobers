package main

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/goobers/goobers/api/validate"
)

func TestStrictWarningPromotionContract(t *testing.T) {
	for _, code := range strictNeutralWarningCodes {
		t.Run(string(code), func(t *testing.T) {
			report := &validate.Report{Issues: []validate.Issue{{
				Code: code, Severity: validate.Warning, Message: "strict-neutral test finding",
			}}}
			if got := strictPromotedWarningCount(report, 0); got != 0 {
				t.Fatalf("strict-promoted warning count = %d, want 0 for %s", got, code)
			}
		})
	}

	ordinary := &validate.Report{Issues: []validate.Issue{{
		Code: validate.WarningModelFallback, Severity: validate.Warning, Message: "ordinary warning",
	}}}
	if got := strictPromotedWarningCount(ordinary, 0); got != 1 {
		t.Fatalf("strict-promoted warning count = %d, want 1 for ordinary warning", got)
	}
	if got := strictPromotedWarningCount(nil, 1); got != 1 {
		t.Fatalf("strict-promoted warning count = %d, want 1 for placeholder warning", got)
	}
}

func TestStrictNeutralContractWordingAcrossHelpCompletionAndReference(t *testing.T) {
	wantCodes := strictNeutralWarningCodeText()
	for name, text := range map[string]string{
		"validate help": validateHelp,
		"lint help":     lintHelp,
		"flag help":     strictFlagDescription(),
	} {
		if !strings.Contains(text, "strict-neutral") || !strings.Contains(text, wantCodes) {
			t.Errorf("%s does not publish the strict-neutral contract %q:\n%s", name, wantCodes, text)
		}
	}

	for _, command := range []string{"validate", "lint"} {
		var description string
		for _, spec := range completionFlagSpecs[command] {
			if spec.name == "strict" {
				description = spec.desc
				break
			}
		}
		if !strings.Contains(description, "strict-neutral") || !strings.Contains(description, wantCodes) {
			t.Errorf("%s completion description does not publish %q: %q", command, wantCodes, description)
		}
	}

	reference := renderCLIDocs()["cli/README.md"]
	if got := strings.Count(reference, "automation may rely on this stable code set"); got != 2 {
		t.Fatalf("CLI reference publishes stable strict-neutral contract %d times, want 2", got)
	}
}

func TestWS001RemainsStrictNeutralInValidateAndLintJSON(t *testing.T) {
	root := initDeterministicDemo(t)
	instancePath := filepath.Join(root, "instance.yaml")
	gagglePath := filepath.Join(root, "config", "gaggles", "example", "gaggle.yaml")
	workflowPath := filepath.Join(root, "config", "gaggles", "example", "workflows", "default-implement.yaml")
	replaceInFile(t, instancePath, "your-org", "acme")
	replaceInFile(t, instancePath, "your-repo", "widgets")
	for range 2 {
		replaceInFile(t, gagglePath, "your-org", "acme")
		replaceInFile(t, gagglePath, "your-repo", "widgets")
	}
	replaceInFile(t, workflowPath, `command: ["true"]`, `command: ["go", "test", "./..."]`)

	for _, command := range []string{"validate", "lint"} {
		t.Run(command, func(t *testing.T) {
			code, stdout, stderr := runArgs(t, command, "--strict", "--json", root)
			if code != 0 || stderr != "" {
				t.Fatalf("%s --strict --json code=%d, want 0; stdout=%q stderr=%q", command, code, stdout, stderr)
			}
			envelope := decodeDiagnosticsEnvelope(t, stdout)
			if !envelope.OK || envelope.Counts.Errors != 0 {
				t.Fatalf("%s strict-neutral envelope unexpectedly failed: %+v", command, envelope)
			}
			var ws001 []diagnosticFinding
			for _, finding := range envelope.Findings {
				if finding.Code == string(validate.WarningImplicitWritableWorkspace) {
					ws001 = append(ws001, finding)
				}
			}
			if len(ws001) != 1 {
				t.Fatalf("%s WS001 findings = %+v, want exactly one", command, ws001)
			}
			if ws001[0].Severity != string(validate.Warning) ||
				!strings.Contains(ws001[0].Message, "run.workspace") {
				t.Fatalf("%s WS001 finding = %+v, want warning with deterministic run.workspace guidance", command, ws001[0])
			}
		})
	}
}
