package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/runtimeplan"
)

func TestRuntimePreflightMCPProductionWiringStartsOnlyControlProcess(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("direct controlled Copilot protocol is unsupported on Windows")
	}
	root := writeRuntimePreflightFixture(t)
	bin := t.TempDir()
	marker := filepath.Join(bin, "invoked")
	program := "#!/bin/sh\nprintf '%s\\n' \"$@\" >> '" + strings.ReplaceAll(marker, "'", "'\\''") + "'\nexit 1\n"
	if err := os.WriteFile(filepath.Join(bin, "copilot"), []byte(program), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	var stdout, stderr bytes.Buffer
	code := runRuntimePreflight([]string{"--instance", root, "--workflow", "implement", "--check-readiness", "--json"}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("code=%d stderr=%s", code, stderr.String())
	}
	var report runtimePreflightReport
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		t.Fatalf("decode: %v stderr=%s", err, stderr.String())
	}
	if len(report.MCP) != 1 || report.MCP[0].Facts.Category != "transport_failure" {
		t.Fatalf("MCP=%+v", report.MCP)
	}
	args, err := os.ReadFile(marker)
	if err != nil {
		t.Fatal("production MCP adapter was never launched:", err)
	}
	if !strings.Contains(string(args), "--headless") || strings.Contains(string(args), "-p=") {
		t.Fatalf("unsafe argv=%s", args)
	}
	if report.MCP[0].Process.PID != os.Getpid() || report.MCP[0].Source.Fidelity != "observed" {
		t.Fatalf("incorrect process fidelity: %+v", report.MCP[0])
	}
	if err := os.Remove(marker); err != nil {
		t.Fatal(err)
	}
	stdout.Reset()
	_ = runRuntimePreflight([]string{"--instance", root, "--workflow", "implement", "--json"}, &stdout, &stderr)
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("static report launched a probe")
	}
}

func TestRuntimePreflightMCPUnsupportedAdapterKeepsConfiguration(t *testing.T) {
	goobers := map[string]apiv1.GooberSpec{"coder": {MCPServers: []apiv1.MCPServer{{Name: "external", URL: "https://example.invalid/mcp"}}, Tools: []string{"delete_repository"}}}
	stages := []runtimePreflightStage{{Name: "implement", Goober: "coder", Harness: "claude-code"}}
	rows, checks := runtimePreflightMCPChecks(context.Background(), &instance.Config{}, goobers, stages, runtimeplan.ObserveProcess(), true)
	if len(rows) != 2 || len(checks) != 2 {
		t.Fatalf("rows=%+v checks=%+v", rows, checks)
	}
	for _, row := range rows {
		if row.Facts.Category != "check_unobservable" || row.Facts.Authorization != "unobservable" {
			t.Fatalf("unknown is not ready: %+v", row)
		}
	}
	if rows[1].Facts.Server != "external" || rows[1].Facts.ToolAllowlist[0] != "delete_repository" || len(rows[1].Facts.RequiredTools) != 0 {
		t.Fatalf("configured facts lost: %+v", rows[1])
	}
}
