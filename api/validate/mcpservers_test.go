package validate

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGooberSchemaAcceptsMCPServersWithoutInlineSecrets(t *testing.T) {
	raw := []byte(`{
		"apiVersion": "goobers.dev/v1alpha1",
		"kind": "Goober",
		"metadata": {"name": "coder"},
		"spec": {
			"gaggle": "example",
			"role": "coder",
			"instructions": "instructions.md",
			"capabilities": ["contents:read", "github:issues:write"],
			"mcpServers": [
				{
					"name": "local-context",
					"command": "context-server",
					"args": ["--stdio"],
					"credentialRefs": [{"capability": "contents:read", "env": "CONTEXT_TOKEN"}]
				},
				{
					"name": "remote-context",
					"url": "https://mcp.example.test/api",
					"credentialRefs": [{"capability": "github:issues:write", "header": "Authorization", "scheme": "bearer"}]
				}
			]
		}
	}`)
	if err := newV(t).ValidateJSON("goober.schema.json", raw); err != nil {
		t.Fatalf("MCP goober schema: %v", err)
	}

	for name, fragment := range map[string]string{
		"both transports": `"command": "server", "url": "https://example.test"`,
		"inline env":      `"command": "server", "env": {"TOKEN": "secret"}`,
		"inline header":   `"url": "https://example.test", "headers": {"Authorization": "secret"}`,
	} {
		t.Run(name, func(t *testing.T) {
			invalid := []byte(`{
				"apiVersion": "goobers.dev/v1alpha1",
				"kind": "Goober",
				"metadata": {"name": "coder"},
				"spec": {
					"gaggle": "example",
					"role": "coder",
					"instructions": "instructions.md",
					"mcpServers": [{"name": "context", ` + fragment + `}]
				}
			}`)
			if err := newV(t).ValidateJSON("goober.schema.json", invalid); err == nil {
				t.Fatal("malformed or inline-secret MCP configuration passed schema validation")
			}
		})
	}
}

func TestValidateDirRejectsUndeclaredMCPCredential(t *testing.T) {
	root := t.TempDir()
	if err := os.CopyFS(root, os.DirFS("../../config-examples")); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "gaggles", "acme-web", "goobers", "coder", "goober.yaml")
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, writeErr := f.WriteString(`
  mcpServers:
    - name: issue-context
      url: https://mcp.example.test/api
      credentialRefs:
        - capability: github:issues:write
          header: Authorization
          scheme: bearer
`)
	closeErr := f.Close()
	if writeErr != nil {
		t.Fatal(writeErr)
	}
	if closeErr != nil {
		t.Fatal(closeErr)
	}

	report, err := newV(t).ValidateDir(root)
	if err != nil {
		t.Fatal(err)
	}
	issues := joinIssues(report)
	if !report.HasErrors() || !strings.Contains(issues, `capability "github:issues:write" is not declared`) {
		t.Fatalf("undeclared MCP credential was not rejected:\n%s", issues)
	}
}
