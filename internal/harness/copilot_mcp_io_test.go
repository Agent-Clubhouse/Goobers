package harness

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/mcpio"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

func TestDeclaresGoobersIO(t *testing.T) {
	if declaresGoobersIO(RunRequest{}) {
		t.Fatal("empty MCPServers must not declare goobers-io")
	}
	if declaresGoobersIO(RunRequest{MCPServers: []apiv1.MCPServer{{Name: "other"}}}) {
		t.Fatal("unrelated server name must not count as goobers-io")
	}
	if !declaresGoobersIO(RunRequest{MCPServers: []apiv1.MCPServer{{Name: "other"}, {Name: goobersIOServerName}}}) {
		t.Fatal("goobers-io present but not detected")
	}
}

func TestWriteGoobersIOConfig(t *testing.T) {
	workspace := t.TempDir()
	mcpHome := t.TempDir()

	envelope := testEnvelope(workspace)
	envelope.Inputs = map[string]interface{}{InputArtifactFile: "findings.md"}

	req := RunRequest{
		Envelope:  envelope,
		Workspace: workspace,
		ContextPaths: map[string]string{
			"review-code-quality.artifact[0]": ".goobers/context/00-review-code-quality.artifact_0_",
		},
	}

	if err := writeGoobersIOConfig(req, mcpHome); err != nil {
		t.Fatal(err)
	}

	cfg, err := mcpio.LoadConfig(filepath.Join(mcpHome, mcpio.ConfigFileName))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Workspace != workspace {
		t.Errorf("Workspace = %q, want %q", cfg.Workspace, workspace)
	}
	if cfg.ArtifactFile != "findings.md" {
		t.Errorf("ArtifactFile = %q, want findings.md", cfg.ArtifactFile)
	}
	if got := cfg.Inputs["review-code-quality.artifact[0]"]; got != ".goobers/context/00-review-code-quality.artifact_0_" {
		t.Errorf("Inputs mapping = %q", got)
	}

	// The file itself must be private (0600) — mirrors mcp-config.json's own
	// permissions, since it lives in the same scoped-per-invocation home.
	raw, err := os.ReadFile(filepath.Join(mcpHome, mcpio.ConfigFileName))
	if err != nil {
		t.Fatal(err)
	}
	var roundTrip mcpio.Config
	if err := json.Unmarshal(raw, &roundTrip); err != nil {
		t.Fatalf("config is not valid JSON: %v", err)
	}
}

func TestWriteGoobersIOConfigWithoutArtifactFile(t *testing.T) {
	workspace := t.TempDir()
	mcpHome := t.TempDir()
	req := RunRequest{Envelope: testEnvelope(workspace), Workspace: workspace}

	if err := writeGoobersIOConfig(req, mcpHome); err != nil {
		t.Fatal(err)
	}
	cfg, err := mcpio.LoadConfig(filepath.Join(mcpHome, mcpio.ConfigFileName))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ArtifactFile != "" {
		t.Errorf("ArtifactFile = %q, want empty when the task declares none", cfg.ArtifactFile)
	}
}

func TestAutoGoobersIOEligible(t *testing.T) {
	workspace := t.TempDir()
	bare := RunRequest{Envelope: testEnvelope(workspace), Workspace: workspace}
	if autoGoobersIOEligible(bare) {
		t.Fatal("a task with no artifactFile and no context must not be eligible")
	}

	withArtifact := bare
	withArtifact.Envelope.Inputs = map[string]interface{}{InputArtifactFile: "out.md"}
	if !autoGoobersIOEligible(withArtifact) {
		t.Fatal("a declared artifactFile must make a task eligible")
	}

	withContext := bare
	withContext.ContextPaths = map[string]string{"x": "y"}
	if !autoGoobersIOEligible(withContext) {
		t.Fatal("non-empty ContextPaths must make a task eligible")
	}
}

func TestWithAutoGoobersIONoOpsWithoutSelfBin(t *testing.T) {
	workspace := t.TempDir()
	req := RunRequest{Envelope: testEnvelope(workspace), Workspace: workspace}
	req.Envelope.Inputs = map[string]interface{}{InputArtifactFile: "out.md"}

	got := withAutoGoobersIO(req, "")
	if declaresGoobersIO(got) {
		t.Fatal("must not auto-wire without a known self-binary path")
	}
}

func TestWithAutoGoobersIONoOpsWhenIneligible(t *testing.T) {
	workspace := t.TempDir()
	req := RunRequest{Envelope: testEnvelope(workspace), Workspace: workspace}

	got := withAutoGoobersIO(req, "/usr/local/bin/goobers")
	if declaresGoobersIO(got) {
		t.Fatal("must not auto-wire a task with no artifactFile and no context")
	}
	if len(got.MCPServers) != 0 || len(got.Tools) != 0 {
		t.Fatalf("ineligible task must be returned unchanged, got MCPServers=%v Tools=%v", got.MCPServers, got.Tools)
	}
}

func TestWithAutoGoobersIOWiresServerAndTools(t *testing.T) {
	workspace := t.TempDir()
	req := RunRequest{
		Envelope:  testEnvelope(workspace),
		Workspace: workspace,
		Tools:     []string{"shell"},
	}
	req.Envelope.Inputs = map[string]interface{}{InputArtifactFile: "out.md"}

	got := withAutoGoobersIO(req, "/usr/local/bin/goobers")
	if !declaresGoobersIO(got) {
		t.Fatal("expected goobers-io to be auto-wired")
	}
	var server apiv1.MCPServer
	for _, s := range got.MCPServers {
		if s.Name == goobersIOServerName {
			server = s
		}
	}
	if server.Command != "/usr/local/bin/goobers" || len(server.Args) != 1 || server.Args[0] != "mcp-io" {
		t.Fatalf("unexpected server config: %+v", server)
	}
	wantTools := []string{"shell", "publish_output", "list_inputs", "read_input", "grep_input"}
	if len(got.Tools) != len(wantTools) {
		t.Fatalf("Tools = %v, want %v", got.Tools, wantTools)
	}
	for i, want := range wantTools {
		if got.Tools[i] != want {
			t.Errorf("Tools[%d] = %q, want %q", i, got.Tools[i], want)
		}
	}
}

func TestWithAutoGoobersIODoesNotDuplicateExplicitDeclaration(t *testing.T) {
	workspace := t.TempDir()
	explicit := apiv1.MCPServer{Name: goobersIOServerName, Command: "custom-command", Args: []string{"--flag"}}
	req := RunRequest{
		Envelope:   testEnvelope(workspace),
		Workspace:  workspace,
		MCPServers: []apiv1.MCPServer{explicit},
	}
	req.Envelope.Inputs = map[string]interface{}{InputArtifactFile: "out.md"}

	got := withAutoGoobersIO(req, "/usr/local/bin/goobers")
	if len(got.MCPServers) != 1 || got.MCPServers[0].Command != "custom-command" {
		t.Fatalf("an explicit goobers-io declaration must be left alone, got %+v", got.MCPServers)
	}
}

func TestAppendMissingDedupesAndPreservesOrder(t *testing.T) {
	got := appendMissing([]string{"shell", "publish_output"}, "publish_output", "list_inputs")
	want := []string{"shell", "publish_output", "list_inputs"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}

func TestGoobersIOPromptSection(t *testing.T) {
	workspace := t.TempDir()
	base := testEnvelope(workspace)

	writeOnly := RunRequest{Envelope: base, Workspace: workspace}
	writeOnly.Envelope.Inputs = map[string]interface{}{InputArtifactFile: "out.md"}
	section := goobersIOPromptSection(writeOnly)
	if !strings.Contains(section, "publish_output") {
		t.Error("expected the publish_output directive when artifactFile is declared")
	}
	if strings.Contains(section, "grep_input") {
		t.Error("did not expect the read directive when there is no context")
	}

	readOnly := RunRequest{Envelope: base, Workspace: workspace, ContextPaths: map[string]string{"x": "y"}}
	section = goobersIOPromptSection(readOnly)
	if strings.Contains(section, "publish_output") {
		t.Error("did not expect the publish_output directive when no artifactFile is declared")
	}
	if !strings.Contains(section, "grep_input") || !strings.Contains(section, "read_input") {
		t.Error("expected the read directive when there is upstream context")
	}
}

func TestRenderPromptIncludesGoobersIOSectionOnlyWhenWired(t *testing.T) {
	workspace := t.TempDir()
	req := RunRequest{
		Envelope:       testEnvelope(workspace),
		Workspace:      workspace,
		CompletionPath: "result.json",
	}
	if strings.Contains(renderPrompt(req), "## goobers-io tools") {
		t.Fatal("must not mention goobers-io tools when they were never wired")
	}

	req.Envelope.Inputs = map[string]interface{}{InputArtifactFile: "out.md"}
	wired := withAutoGoobersIO(req, "/usr/local/bin/goobers")
	rendered := renderPrompt(wired)
	if !strings.Contains(rendered, "## goobers-io tools") || !strings.Contains(rendered, "publish_output") {
		t.Fatalf("expected the goobers-io section once wired, got:\n%s", rendered)
	}
}
