package validate

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func mcpRequirednessConfig(required string, versions ...string) string {
	var workflows, names []string
	for i, version := range versions {
		name := fmt.Sprintf("mcp-%d", i)
		names = append(names, name)
		workflows = append(workflows, fmt.Sprintf(`apiVersion: goobers.dev/v1alpha1
kind: Workflow
dslVersion: %q
metadata:
  name: %s
  annotations:
    goobers.dev/allow-preview-features: "true"
spec:
  gaggle: web
  triggers: [{type: manual}]
  start: implement
  tasks:
    - name: implement
      type: agentic
      goober: coder
      goal: Synthetic validation fixture; do not execute.
      capabilities: [agent:model]
`, version, name))
	}
	config := dsl30Config(false, strings.Join(workflows, "---\n"))
	// The legacy annotation isolates the compiler regression from #8073.
	config = strings.Replace(config, "  name: dsl30\n", `  name: dsl30
  annotations:
    goobers.dev/allow-preview-features: "true"
`, 1)
	requiredField := ""
	if required != "" {
		requiredField = "      required: " + required + "\n"
	}
	return strings.Replace(config, "  capabilities: [agent:model]\n---", fmt.Sprintf(`  capabilities: [agent:model]
  workflows: [%s]
  tools: [read-context]
  mcpServers:
    - name: context
      url: https://example.invalid/mcp
%s---`, strings.Join(names, ", "), requiredField), 1)
}

func validateSharedMCPRequiredness(t *testing.T, config string) *Report {
	t.Helper()
	docs := strings.SplitN(config, "---\n", 4)
	if len(docs) != 4 || !strings.Contains(docs[2], "\nkind: Goober\n") {
		t.Fatal("expected Manifest, Gaggle, Goober, and Workflow fixture documents")
	}
	goober := strings.Replace(docs[2], "  gaggle: web\n", "", 1)
	root := t.TempDir()
	for path, contents := range map[string]string{
		"config/config.yaml":                  strings.Join([]string{docs[0], docs[1], docs[3]}, "---\n"),
		"goobers/coder/goober.yaml":           goober,
		"goobers/coder/instructions/coder.md": "Synthetic validation fixture.",
	} {
		absolute := filepath.Join(root, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(absolute), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(absolute, []byte(contents), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	report, err := newV(t).ValidateDir(filepath.Join(root, "config"))
	if err != nil {
		t.Fatalf("ValidateDir: %v", err)
	}
	return report
}

func TestValidateDirDSL31MCPRequiredness(t *testing.T) {
	for _, tc := range []struct {
		name     string
		required string
	}{
		{name: "optional", required: "false"},
		{name: "required", required: "true"},
		{name: "omitted"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			report := validateDSL30(t, mcpRequirednessConfig(tc.required, "3.1"))
			if report.HasErrors() {
				t.Fatalf("acknowledged DSL 3.1 workflow failed validation:\n%s", joinIssues(report))
			}
			if len(issuesWithCode(report, "DVL012")) != 1 {
				t.Fatalf("expected legacy Manifest annotation warning:\n%s", joinIssues(report))
			}
		})
	}
}

func TestValidateDirMCPRequirednessSharedConsumers(t *testing.T) {
	for _, scope := range []string{"gaggle", "shared"} {
		t.Run(scope, func(t *testing.T) {
			for _, consumers := range []struct {
				versions            []string
				wantExplicitRefusal bool
			}{
				{versions: []string{"2.0"}, wantExplicitRefusal: true},
				{versions: []string{"3.0"}, wantExplicitRefusal: true},
				{versions: []string{"3.1", "3.1"}},
				{versions: []string{"3.1", "2.0"}, wantExplicitRefusal: true},
				{versions: []string{"3.1", "3.0"}, wantExplicitRefusal: true},
			} {
				t.Run(strings.Join(consumers.versions, "_"), func(t *testing.T) {
					for _, tc := range []struct {
						name     string
						required string
					}{
						{name: "optional", required: "false"},
						{name: "required", required: "true"},
						{name: "omitted"},
					} {
						t.Run(tc.name, func(t *testing.T) {
							config := mcpRequirednessConfig(tc.required, consumers.versions...)
							var report *Report
							if scope == "shared" {
								report = validateSharedMCPRequiredness(t, config)
							} else {
								report = validateDSL30(t, config)
							}
							if tc.required != "" && consumers.wantExplicitRefusal {
								if !report.HasErrors() {
									t.Fatalf("legacy consumer accepted explicit requiredness:\n%s", joinIssues(report))
								}
								for _, issue := range report.Issues {
									if issue.Severity == Error && (issue.Kind != "Goober" || issue.Name != "coder" ||
										!strings.Contains(issue.Message, `requires dslVersion "3.1"`)) {
										t.Fatalf("unexpected error masking requiredness refusal: %+v", issue)
									}
								}
								return
							}
							if report.HasErrors() {
								t.Fatalf("compatible consumers failed validation:\n%s", joinIssues(report))
							}
						})
					}
				})
			}
		})
	}
}

func TestValidateDirMCPRequirednessRequiresWorkflowPreview(t *testing.T) {
	config := mcpRequirednessConfig("false", "3.1", "3.1")
	config = strings.Replace(config, `  name: mcp-1
  annotations:
    goobers.dev/allow-preview-features: "true"
`, "  name: mcp-1\n", 1)
	report := validateDSL30(t, config)
	issues := issuesWithCode(report, ErrorPreviewDSLVersionBlocked)
	if len(issues) != 1 || issues[0].Kind != "Workflow" || issues[0].Name != "mcp-1" {
		t.Fatalf("unacknowledged consumer must retain its own preview refusal:\n%s", joinIssues(report))
	}
}
