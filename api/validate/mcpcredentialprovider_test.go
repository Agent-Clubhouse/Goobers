package validate

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

const mcpProviderManifest = `apiVersion: goobers.dev/v1alpha1
kind: Manifest
metadata:
  name: mcp-provider
spec:
  instance:
    name: mcp-provider
    environment: dev
  gaggles: [ado, mixed, gh, tea, idle]
`

func mcpProviderGaggle(name, project, backlog string) string {
	return `---
apiVersion: goobers.dev/v1alpha1
kind: Gaggle
metadata:
  name: ` + name + `
spec:
  project:
    provider: ` + project + `
    owner: example-org
    project: example-project
    name: ` + name + `
  backlog:
    provider: ` + backlog + `
    project: ` + map[bool]string{true: "example-project", false: "example-org/example-backlog"}[backlog == "ado"] + `
  isolation:
    namespace: ` + name + `
`
}

// mcpProviderGoober is a goober whose single remote MCP server binds refs, one
// "capability" or "byo:<ref>" per entry. An empty gaggle is instance-shared.
func mcpProviderGoober(name, gaggle string, refs ...string) string {
	var capabilities, bindings strings.Builder
	for i, ref := range refs {
		header := "X-Credential-" + string(rune('a'+i))
		if byo, ok := strings.CutPrefix(ref, "byo:"); ok {
			bindings.WriteString("        - kind: byo\n          ref: " + byo + "\n          header: " + header + "\n")
			continue
		}
		capabilities.WriteString("    - " + ref + "\n")
		bindings.WriteString("        - capability: " + ref + "\n          header: " + header + "\n")
	}
	owner := ""
	if gaggle != "" {
		owner = "  gaggle: " + gaggle + "\n"
	}
	return `---
apiVersion: goobers.dev/v1alpha1
kind: Goober
metadata:
  name: ` + name + `
spec:
` + owner + `  role: coder
  capabilities:
    - agent:model
` + capabilities.String() + `  mcpServers:
    - name: ` + name + `-mcp
      url: https://mcp.example.test/api
      credentialRefs:
` + bindings.String()
}

func mcpProviderWorkflow(gaggle string, goobers ...string) string {
	var tasks strings.Builder
	for i, goober := range goobers {
		next := ""
		if i+1 < len(goobers) {
			next = "      next: " + goobers[i+1] + "-task\n"
		}
		tasks.WriteString("    - name: " + goober + "-task\n      type: agentic\n      goal: work\n      goober: " + goober + "\n" + next)
	}
	return `---
apiVersion: goobers.dev/v1alpha1
kind: Workflow
dslVersion: "2.0"
metadata:
  name: work
spec:
  gaggle: ` + gaggle + `
  triggers:
    - type: manual
  start: ` + goobers[0] + `-task
  tasks:
` + tasks.String()
}

// TestMCPCredentialProviderWarning covers MCP002: a non-byo MCP credentialRef
// whose capability belongs to another provider than the project of a gaggle
// that runs the goober is never materialised by the harness, so validate
// warns ahead of the failed run, naming the server and pointing at kind: byo.
// Only goobers a workflow runs are checked, owned and shared alike.
func TestMCPCredentialProviderWarning(t *testing.T) {
	root := t.TempDir()
	configDir := filepath.Join(root, "config")
	config := mcpProviderManifest +
		mcpProviderGaggle("ado", "ado", "ado") +
		mcpProviderGaggle("mixed", "ado", "github") +
		mcpProviderGaggle("gh", "github", "github") +
		mcpProviderGaggle("tea", "gitea", "gitea") +
		mcpProviderGaggle("idle", "ado", "ado") +
		// A github:* ref on ADO warns; byo, provider-neutral and ado:* refs fit.
		mcpProviderGoober("ado-coder", "ado", "github:pr:write", "byo:vendor-api", "repo:push", "ado:pr:write") +
		// Owned by an ADO gaggle but run by no workflow: never runs, no warning.
		mcpProviderGoober("ado-unused", "ado", "github:pr:write") +
		// Topology (b): the GitHub backlog does not back an agentic stage's MCP ref.
		mcpProviderGoober("triager", "mixed", "github:issues:read") +
		// github:* fits GitHub and Gitea; ado:* does not fit GitHub.
		mcpProviderGoober("gh-coder", "gh", "github:pr:write", "ado:pr:write") +
		mcpProviderGoober("tea-coder", "tea", "github:pr:write") +
		mcpProviderWorkflow("ado", "ado-coder", "shared") +
		mcpProviderWorkflow("mixed", "triager") +
		mcpProviderWorkflow("gh", "gh-coder", "shared") +
		mcpProviderWorkflow("tea", "tea-coder")
	if err := os.MkdirAll(filepath.Join(root, "goobers", "shared"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(configDir, "instance.yaml"), []byte(config), 0o644); err != nil {
		t.Fatal(err)
	}
	// Shared, run on gh (fits) and ado (warns), never on idle.
	shared := strings.TrimPrefix(mcpProviderGoober("shared", "", "github:issues:read"), "---\n")
	if err := os.WriteFile(filepath.Join(root, "goobers", "shared", "goober.yaml"), []byte(shared), 0o644); err != nil {
		t.Fatal(err)
	}
	report, err := newV(t).ValidateDir(configDir)
	if err != nil {
		t.Fatalf("ValidateDir: %v", err)
	}
	var got []string
	for _, issue := range report.Issues {
		if issue.Code != WarningMCPCredentialProvider {
			continue
		}
		got = append(got, issue.Name+"@"+issue.Gaggle)
		if issue.Severity != Warning || issue.Kind != "Goober" {
			t.Errorf("MCP002 = %+v, want a Goober warning", issue)
		}
		for _, want := range []string{`MCP server "` + issue.Name + `-mcp"`, `gaggle "` + issue.Gaggle + `"`, "kind: byo"} {
			if !strings.Contains(issue.Message, want) {
				t.Errorf("MCP002 message %q lacks %q", issue.Message, want)
			}
		}
		backlogClause := strings.Contains(issue.Message, "not routed to the backlog provider")
		if backlogClause != (issue.Gaggle == "mixed") {
			t.Errorf("MCP002 backlog clause = %v for gaggle %q: %s", backlogClause, issue.Gaggle, issue.Message)
		}
	}
	sort.Strings(got)
	want := []string{"ado-coder@ado", "gh-coder@gh", "shared@ado", "triager@mixed"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("MCP002 findings = %q, want %q\n%s", got, want, joinIssues(report))
	}
	for _, issue := range report.Issues {
		if issue.Code == WarningMCPCredentialProvider && strings.Contains(issue.Message, `"repo:push"`) {
			t.Errorf("provider-neutral ref reported: %s", issue.Message)
		}
	}
}
