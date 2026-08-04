package main

import (
	"flag"
	"io"
	"os"

	"github.com/goobers/goobers/internal/mcpio"
)

const mcpIOHelp = "Usage: goobers mcp-io\n\n" +
	"Run the goobers-io MCP server over stdio: publish_output, list_inputs,\n" +
	"and read_input, the generic replacement for writing an agentic stage's\n" +
	"declared output with a file-editing tool (#2406). Not meant to be run\n" +
	"interactively — a harness spawns this as a local MCP server for a\n" +
	"goober that declares mcpServers: [{name: goobers-io, command: goobers,\n" +
	"args: [mcp-io]}]. Configuration (workspace, declared artifactFile,\n" +
	"available inputs) is read from $COPILOT_HOME/goobers-io-config.json,\n" +
	"written by the harness before invocation — there is no other input.\n" +
	"Exit codes: 0 = the stdio session ended cleanly (stdin closed),\n" +
	"1 = missing or invalid configuration, 2 = usage error.\n"

func runMCPIO(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("mcp-io", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = helpUsage(stderr, "mcp-io")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 {
		fs.Usage()
		return 2
	}
	cfg, err := mcpio.LoadConfigFromEnv()
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
