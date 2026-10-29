package validate

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const mcpPreviewConsentConfig = `apiVersion: goobers.dev/v1alpha1
kind: Manifest
metadata:
  name: fixture
spec:
  instance:
    name: fixture
    environment: dev
  gaggles: [fixture]
---
apiVersion: goobers.dev/v1alpha1
kind: Gaggle
metadata:
  name: fixture
spec:
  project:
    provider: github
    owner: example
    name: placeholder
  backlog:
    provider: github
    project: example/placeholder
  isolation:
    namespace: fixture
---
apiVersion: goobers.dev/v1alpha1
kind: Goober
metadata:
  name: fixture
spec:
  gaggle: fixture
  role: developer
  instructions: instructions/coder.md
  harness: copilot
  capabilities: [agent:model]
  workflows: [fixture]
  tools: [read-context]
  mcpServers:
    - name: placeholder
      url: https://example.invalid/mcp
      required: false
---
` + mcpPreviewConsentWorkflow

const mcpPreviewConsentWorkflow = `apiVersion: goobers.dev/v1alpha1
kind: Workflow
dslVersion: "3.1"
metadata:
  name: fixture
  annotations:
    goobers.dev/allow-preview-features: "true"
spec:
  gaggle: fixture
  triggers:
    - type: manual
  start: validate
  tasks:
    - name: validate
      type: agentic
      goober: fixture
      goal: Synthetic validation fixture; do not execute.
      capabilities: [agent:model]
`

func TestValidateDirMCPPreviewConsent(t *testing.T) {
	for _, required := range []string{"false", "true", ""} {
		for _, workflowConsent := range []string{"true", "", "false"} {
			for _, manifestConsent := range []string{"", "true", "false"} {
				name := fmt.Sprintf("required=%s/workflow=%s/manifest=%s", required, workflowConsent, manifestConsent)
				t.Run(name, func(t *testing.T) {
					config := mcpPreviewConsentVariant(required, workflowConsent, manifestConsent)
					report := validateDSL30(t, config)
					acknowledged := workflowConsent == "true"

					preview := issuesWithCode(report, WarningPreviewFeature)
					if required == "" {
						if len(preview) != 0 {
							t.Errorf("omitted required must not need feature consent:\n%s", joinIssues(report))
						}
					} else if len(preview) != 1 {
						t.Errorf("want one MCP preview finding:\n%s", joinIssues(report))
					} else {
						want := Error
						if acknowledged {
							want = Warning
						}
						if preview[0].Severity != want {
							t.Errorf("preview severity = %s, want %s:\n%s", preview[0].Severity, want, joinIssues(report))
						}
						if !acknowledged && (!strings.Contains(preview[0].Message, "Workflow/fixture") ||
							strings.Contains(preview[0].Message, "Manifest")) {
							t.Errorf("missing consent must name the Workflow, not request Manifest consent: %s", preview[0].Message)
						}
					}
					if got := len(issuesWithCode(report, ErrorPreviewDSLVersionBlocked)); (got == 0) != acknowledged {
						t.Errorf("Workflow consent refusal count = %d, acknowledged = %v:\n%s", got, acknowledged, joinIssues(report))
					}
					if got := len(issuesWithCode(report, WarningManifestPreviewAnnotationDeprecated)); (got != 0) != (manifestConsent != "") {
						t.Errorf("Manifest deprecation count = %d, annotation = %q:\n%s", got, manifestConsent, joinIssues(report))
					}
					for _, issue := range report.Issues {
						if issue.Severity != Error {
							continue
						}
						if !acknowledged && (issue.Code == ErrorPreviewDSLVersionBlocked ||
							(required != "" && issue.Code == WarningPreviewFeature)) {
							continue
						}
						if acknowledged && required != "" && isMCPRequiredCompilerError(issue) {
							t.Logf("separate compiler issue #8072 remains: %s", issue.Message)
							continue
						}
						t.Errorf("unexpected validation error: %+v", issue)
					}
				})
			}
		}
	}
}

func TestValidateDirMCPPreviewConsentSharedConsumers(t *testing.T) {
	for _, shared := range []bool{false, true} {
		for _, manifestConsent := range []string{"", "true"} {
			for _, tc := range []struct {
				name          string
				firstConsent  string
				secondConsent string
				secondVersion string
			}{
				{"both acknowledged", "true", "true", "3.1"},
				{"first unacknowledged", "", "true", "3.1"},
				{"second unacknowledged", "true", "", "3.1"},
				{"neither acknowledged", "", "", "3.1"},
				{"older 3.0 consumer", "true", "true", "3.0"},
				{"older 2.0 consumer", "true", "true", "2.0"},
			} {
				name := fmt.Sprintf("instance-shared=%t/manifest=%s/%s", shared, manifestConsent, tc.name)
				t.Run(name, func(t *testing.T) {
					config := mcpPreviewConsentVariant("false", tc.firstConsent, manifestConsent)
					config = strings.Replace(config, "  workflows: [fixture]", "  workflows: [fixture, other]", 1)
					second := strings.Replace(mcpPreviewConsentWorkflow, "  name: fixture\n", "  name: other\n", 1)
					second = strings.Replace(second, `dslVersion: "3.1"`, fmt.Sprintf("dslVersion: %q", tc.secondVersion), 1)
					second = strings.Replace(second, mcpPreviewAnnotation("true"), mcpPreviewAnnotation(tc.secondConsent), 1)
					config += "---\n" + second
					report := validateMCPPreviewConsumers(t, config, shared)

					wantPreview := map[string]Severity{"fixture": Warning}
					wantBlocked := map[string]bool{}
					if tc.firstConsent != "true" {
						wantPreview["fixture"] = Error
						wantBlocked["fixture"] = true
					}
					if tc.secondVersion == "3.1" {
						wantPreview["other"] = Warning
						if tc.secondConsent != "true" {
							wantPreview["other"] = Error
							wantBlocked["other"] = true
						}
					}
					preview := issuesWithCode(report, WarningPreviewFeature)
					if len(preview) != len(wantPreview) {
						t.Errorf("preview findings = %d, want %d:\n%s", len(preview), len(wantPreview), joinIssues(report))
					}
					for consumer, severity := range wantPreview {
						found := 0
						for _, issue := range preview {
							if strings.Contains(issue.Message, "Workflow/"+consumer+" ") && issue.Severity == severity {
								found++
							}
							if strings.Contains(issue.Message, "Manifest") {
								t.Errorf("preview finding still requests Manifest consent: %s", issue.Message)
							}
						}
						if found != 1 {
							t.Errorf("want one %s preview finding for Workflow/%s:\n%s", severity, consumer, joinIssues(report))
						}
					}
					blocked := issuesWithCode(report, ErrorPreviewDSLVersionBlocked)
					if len(blocked) != len(wantBlocked) {
						t.Errorf("blocked workflows = %d, want %d:\n%s", len(blocked), len(wantBlocked), joinIssues(report))
					}
					for _, issue := range blocked {
						if !wantBlocked[issue.Name] {
							t.Errorf("wrong workflow refused preview consent: %+v", issue)
						}
					}
					unsupported := issuesWithCode(report, errorUnsupportedFeature)
					wantUnsupported := 0
					if tc.secondVersion != "3.1" {
						wantUnsupported = 1
					}
					if len(unsupported) != wantUnsupported {
						t.Errorf("incompatible consumers = %d, want %d:\n%s", len(unsupported), wantUnsupported, joinIssues(report))
					}
					for _, issue := range unsupported {
						if issue.Severity != Error || !strings.Contains(issue.Message, "Workflow/other ") ||
							!strings.Contains(issue.Message, `requires dslVersion "3.1"`) {
							t.Errorf("incompatible consumer must be identified and refused: %+v", issue)
						}
					}
					for _, issue := range report.Issues {
						if issue.Severity != Error {
							continue
						}
						if issue.Code == WarningPreviewFeature || issue.Code == ErrorPreviewDSLVersionBlocked ||
							issue.Code == errorUnsupportedFeature {
							continue
						}
						if len(wantBlocked) == 0 && wantUnsupported == 0 && isMCPRequiredCompilerError(issue) {
							t.Logf("separate compiler issue #8072 remains: %s", issue.Message)
							continue
						}
						t.Errorf("unexpected validation error: %+v", issue)
					}
				})
			}
		}
	}
}

func mcpPreviewAnnotation(value string) string {
	if value == "" {
		return ""
	}
	return fmt.Sprintf("  annotations:\n    goobers.dev/allow-preview-features: %q\n", value)
}

func mcpPreviewConsentVariant(required, workflowConsent, manifestConsent string) string {
	requiredLine := ""
	if required != "" {
		requiredLine = "      required: " + required + "\n"
	}
	config := strings.Replace(mcpPreviewConsentConfig, "      required: false\n", requiredLine, 1)
	config = strings.Replace(config, mcpPreviewAnnotation("true"), mcpPreviewAnnotation(workflowConsent), 1)
	const manifest = "kind: Manifest\nmetadata:\n  name: fixture\n"
	return strings.Replace(config, manifest, manifest+mcpPreviewAnnotation(manifestConsent), 1)
}

func validateMCPPreviewConsumers(t *testing.T, config string, shared bool) *Report {
	t.Helper()
	if !shared {
		return validateDSL30(t, config)
	}
	documents := strings.SplitN(config, "---\n", 4)
	if len(documents) != 4 {
		t.Fatal("expected Manifest, Gaggle, Goober, and Workflow fixture documents")
	}
	goober := strings.Replace(documents[2], "  gaggle: fixture\n", "", 1)
	config = strings.Join([]string{documents[0], documents[1], documents[3]}, "---\n")
	root := t.TempDir()
	configDir := filepath.Join(root, "config")
	for name, content := range map[string]string{
		"config/config.yaml":                    config,
		"goobers/fixture/goober.yaml":           goober,
		"goobers/fixture/instructions/coder.md": "Synthetic validation fixture.",
	} {
		path := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	report, err := newV(t).ValidateDir(configDir)
	if err != nil {
		t.Fatal(err)
	}
	return report
}

// #8072 is a separate compiler defect. Do not require it to persist, but do not
// mistake it for a consent regression or allow unrelated compilation failures.
func isMCPRequiredCompilerError(issue Issue) bool {
	return issue.Code == errorWorkflowCompile &&
		strings.Contains(issue.Message, `goober spec.mcpServers.required requires dslVersion "3.1"`)
}
