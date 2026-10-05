package harness

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/mcpio"
	"github.com/goobers/goobers/internal/sessioning"
)

func TestSessionToolsAreHostOnlyPrivateRuntimeRegistration(t *testing.T) {
	for _, name := range []string{"claude", "codex"} {
		t.Run(name, func(t *testing.T) {
			workspace := t.TempDir()
			req := RunRequest{Workspace: workspace, Envelope: testEnvelope(workspace), SessionOperations: &mcpio.SessionOperationAccess{Endpoint: "https://daemon.invalid", BearerToken: sessioning.OperationTokenPrefix + strings.Repeat("a", 64)}}
			var registration string
			if name == "claude" {
				req = withAutoGoobersIOClaude(req, "goobers")
				if !slices.Contains(req.Tools, "mcp__goobers-io__get_backlog_item") {
					t.Fatal("Claude tool unavailable")
				}
				var err error
				registration, err = goobersIOClaudeMCPConfigArg(req, "goobers")
				if err != nil {
					t.Fatal(err)
				}
			} else {
				req = withAutoGoobersIOCodex(req, "goobers")
				_, _, args, err := prepareCodexMCP(context.Background(), req, t.TempDir(), "goobers", nil, nil, true)
				if err != nil {
					t.Fatal(err)
				}
				registration = strings.Join(args, "\n")
				if !strings.Contains(registration, "get_backlog_item") {
					t.Fatal("Codex tool unavailable")
				}
			}
			prompt := renderPrompt(req)
			if !strings.Contains(prompt, "get_backlog_item") || strings.Contains(prompt, req.SessionOperations.BearerToken) || strings.Contains(registration, req.SessionOperations.BearerToken) {
				t.Fatal("prompt/argv registration mismatch")
			}
			path := filepath.Join(workspace, filepath.FromSlash(goobersIORuntimeSubdir), mcpio.ConfigFileName)
			info, err := os.Stat(path)
			if err != nil || info.Mode().Perm() != 0600 {
				t.Fatal("private config missing")
			}
			cfg, err := mcpio.LoadConfig(path)
			if err != nil || cfg.SessionOperations == nil || cfg.SessionOperations.BearerToken != req.SessionOperations.BearerToken {
				t.Fatal("grant not delivered privately")
			}
			req.SessionOperations = nil
			if strings.Contains(renderPrompt(req), "get_backlog_item") {
				t.Fatal("ordinary prompt expanded")
			}
		})
	}
}
