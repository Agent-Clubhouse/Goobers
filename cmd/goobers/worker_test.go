package main

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunWorkerUsageErrors(t *testing.T) {
	cases := []struct {
		name string
		args []string
	}{
		{name: "unknown flag", args: []string{"--warp-speed"}},
		{name: "positional args", args: []string{"extra"}},
		{name: "empty task queue", args: []string{"--task-queue", ""}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if code := runWorker(tc.args, &stdout, &stderr); code != 2 {
				t.Fatalf("exit = %d, want 2 (usage error)\nstderr: %s", code, stderr.String())
			}
		})
	}
}

func TestRunWorkerHelpComesFromRegistry(t *testing.T) {
	command, ok := commandHelp("worker")
	if !ok {
		t.Fatal("worker is not registered")
	}
	if command.long != workerHelp {
		t.Fatal("worker registry help drifted from workerHelp")
	}
	if command.synopsis == "" {
		t.Fatal("worker has no top-level synopsis")
	}
	var stderr bytes.Buffer
	if code := runWorker([]string{"--warp-speed"}, &bytes.Buffer{}, &stderr); code != 2 {
		t.Fatalf("exit = %d, want 2", code)
	}
	if !strings.Contains(stderr.String(), "Usage: goobers worker") {
		t.Fatalf("usage output does not render the registered help:\n%s", stderr.String())
	}
}

func TestWorkerEngineDepsWiresWorkspacesAndAutomated(t *testing.T) {
	deps, err := workerEngineDeps(filepath.Join(t.TempDir(), "work"))
	if err != nil {
		t.Fatalf("workerEngineDeps: %v", err)
	}
	if deps.Workspaces == nil {
		t.Error("no workspace provisioner wired — every workspace stage would fail closed")
	}
	if deps.Auto == nil {
		t.Error("no automated gate evaluator wired")
	}
	// Agentic/deterministic seams deliberately await the runtime wiring slice.
	if deps.Goober != nil || deps.Det != nil {
		t.Error("executor seams unexpectedly wired; update the worker help text and this test together")
	}
}
