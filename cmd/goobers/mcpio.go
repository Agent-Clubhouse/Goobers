package main

import (
	"flag"
	"io"
	"os"

	"github.com/goobers/goobers/internal/mcpio"
)

const mcpIOHelp = "Usage: goobers mcp-io --config <path>\n\n" +
	"Run the goobers-io MCP server over stdio: publish_output, list_inputs,\n" +
	"read_input, and grep_input — the generic replacement for writing an\n" +
	"agentic stage's declared output with a file-editing tool (#2406). Not\n" +
	"meant to be run interactively — the harness spawns this automatically\n" +
	"via --additional-mcp-config for any eligible stage (one with a declared\n" +
	"artifactFile and/or upstream context); nothing in a goober's own YAML\n" +
	"needs to name it. --config points at the workspace-relative runtime\n" +
	"config (workspace, declared artifactFile, available inputs) the\n" +
	"harness writes before invocation — deliberately not $COPILOT_HOME-\n" +
	"relative, so this works whether or not the invocation has Copilot's\n" +
	"stored-login auth or any other MCP server configured.\n" +
	"Exit codes: 0 = the stdio session ended cleanly (stdin closed),\n" +
	"1 = missing or invalid configuration, 2 = usage error.\n"

func runMCPIO(args []string, stdout, stderr io.Writer) int {
	fs := newCLIFlagSet("mcp-io", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = helpUsage(stderr, "mcp-io")
	configPath := fs.String("config", "", "path to the goobers-io runtime config written by the harness")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 || *configPath == "" {
		fs.Usage()
		return 2
	}
	cfg, err := mcpio.LoadConfig(*configPath)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	srv := mcpio.NewServer(mcpio.NewToolset(cfg))
	if err := srv.Serve(os.Stdin, stdout, stderr); err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	return 0
}
