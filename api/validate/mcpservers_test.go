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
				},
				{
					"name": "vendor-context",
					"url": "https://vendor.example.test/mcp",
					"credentialRefs": [{"kind": "byo", "ref": "vendor-api", "header": "X-API-Key"}]
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

func TestGooberSchemaRejectsMalformedBYOMCPCredentials(t *testing.T) {
	template := `{
		"apiVersion": "goobers.dev/v1alpha1",
		"kind": "Goober",
		"metadata": {"name": "coder"},
		"spec": {
			"gaggle": "example",
			"role": "coder",
			"instructions": "instructions.md",
			"mcpServers": [{
				"name": "vendor",
				"command": "vendor-server",
				"credentialRefs": [__REF__]
			}]
		}
	}`
	for name, ref := range map[string]string{
		"ref without kind":   `{"ref": "vendor-api", "env": "TOKEN"}`,
		"kind without ref":   `{"kind": "byo", "env": "TOKEN"}`,
		"capability and BYO": `{"capability": "contents:read", "kind": "byo", "ref": "vendor-api", "env": "TOKEN"}`,
		"unknown kind":       `{"kind": "dynamic", "ref": "vendor-api", "env": "TOKEN"}`,
		"invalid ref":        `{"kind": "byo", "ref": "Vendor API", "env": "TOKEN"}`,
	} {
		t.Run(name, func(t *testing.T) {
			raw := []byte(strings.Replace(template, "__REF__", ref, 1))
			if err := newV(t).ValidateJSON("goober.schema.json", raw); err == nil {
				t.Fatal("malformed BYO MCP credential passed schema validation")
			}
		})
	}
}

func TestGooberSchemaLimitsMCPServerNameOnly(t *testing.T) {
	document := `{
		"apiVersion": "goobers.dev/v1alpha1",
		"kind": "Goober",
		"metadata": {"name": "__GOOBER_NAME__"},
		"spec": {
			"gaggle": "example",
			"role": "coder",
			"instructions": "instructions.md",
			"mcpServers": [{"name": "__MCP_NAME__", "command": "context-server"}]
		}
	}`
	longName := strings.Repeat("a", 64)
	valid := strings.ReplaceAll(document, "__GOOBER_NAME__", longName)
	valid = strings.ReplaceAll(valid, "__MCP_NAME__", "context")
	if err := newV(t).ValidateJSON("goober.schema.json", []byte(valid)); err != nil {
		t.Fatalf("64-character metadata.name should remain valid: %v", err)
	}

	invalid := strings.ReplaceAll(document, "__GOOBER_NAME__", "coder")
	invalid = strings.ReplaceAll(invalid, "__MCP_NAME__", longName)
	if err := newV(t).ValidateJSON("goober.schema.json", []byte(invalid)); err == nil {
		t.Fatal("64-character MCP server name passed schema validation")
	}
}

// TestGooberSchemaAllowsMCPServersForClaudeCode pins #1492: mcpServers is
// adapter-neutral at the schema level — declaring it for claude-code is no
// longer rejected, matching Copilot.
func TestGooberSchemaAllowsMCPServersForClaudeCode(t *testing.T) {
	raw := []byte(`{
		"apiVersion": "goobers.dev/v1alpha1",
		"kind": "Goober",
		"metadata": {"name": "coder"},
		"spec": {
			"gaggle": "example",
			"role": "coder",
			"instructions": "instructions.md",
			"harness": "claude-code",
			"mcpServers": [{"name": "context", "command": "context-server"}]
		}
	}`)
	if err := newV(t).ValidateJSON("goober.schema.json", raw); err != nil {
		t.Fatalf("claude-code MCP configuration failed schema validation: %v", err)
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

// TestValidateDirAllowsMCPServersForClaudeCode pins #1492 end to end through
// directory validation, mirroring TestGooberSchemaAllowsMCPServersForClaudeCode.
func TestValidateDirAllowsMCPServersForClaudeCode(t *testing.T) {
	root := t.TempDir()
	if err := os.CopyFS(root, os.DirFS("../../config-examples")); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "gaggles", "acme-web", "goobers", "coder", "goober.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	data = []byte(strings.Replace(string(data), "  harness: copilot", "  harness: claude-code", 1))
	data = append(data, []byte(`
  mcpServers:
    - name: issue-context
      command: context-server
`)...)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}

	report, err := newV(t).ValidateDir(root)
	if err != nil {
		t.Fatal(err)
	}
	if issues := joinIssues(report); report.HasErrors() {
		t.Fatalf("claude-code MCP configuration failed directory validation:\n%s", issues)
	}
}

func TestValidateDirRejectsWildcardMCPTool(t *testing.T) {
	root := t.TempDir()
	if err := os.CopyFS(root, os.DirFS("../../config-examples")); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "gaggles", "acme-web", "goobers", "coder", "goober.yaml")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// Replace the fixture's existing tools: block with a wildcard rather than
	// appending a second tools: key (#3643: a duplicate key is now a hard
	// error, so this test can no longer rely on last-key-wins to smuggle the
	// wildcard past the original declaration).
	const original = "  tools:\n    - github\n    - shell\n"
	if !strings.Contains(string(raw), original) {
		t.Fatalf("fixture %s does not contain expected tools: block", path)
	}
	updated := strings.Replace(string(raw), original, "  tools: [\"*\"]\n", 1)
	updated += `  mcpServers:
    - name: issue-context
      command: context-server
`
	if err := os.WriteFile(path, []byte(updated), 0o644); err != nil {
		t.Fatal(err)
	}

	report, err := newV(t).ValidateDir(root)
	if err != nil {
		t.Fatal(err)
	}
	issues := joinIssues(report)
	if !report.HasErrors() || !strings.Contains(issues, `tools[0] "*" must be an explicit tool name`) {
		t.Fatalf("wildcard MCP tool was not rejected:\n%s", issues)
	}
}
