package main

import (
	"os"
	"path/filepath"
	"strings"
)

// ciStackSignal names one build-manifest signal and the local CI command it
// implies. Detection is presence-only (no manifest content is parsed) — the
// goal is a sane starting default for the guided prompt, not certainty.
type ciStackSignal struct {
	stack      string   // human-readable label shown in the detection message
	names      []string // exact filenames; any one present triggers this signal
	suffix     string   // OR: any directory entry with this suffix triggers it
	command    []string // suggested ciCommand
	capability string   // suggested runner capability
}

// ciStackSignals is checked in order; the first match wins. Makefile comes
// first because its presence means the repo already names its own CI
// entrypoint regardless of underlying stack (#2071's own framing: a Makefile
// makes `make ci` a safe default independent of what it wraps). The rest
// mirror the stacks this repo already ships reference gaggles for
// (config-examples/gaggles/{dotnet,java,python}-service, acme-web) so a
// detected default matches an existing, reviewed convention rather than an
// invented one.
var ciStackSignals = []ciStackSignal{
	{stack: "Makefile", names: []string{"Makefile", "makefile", "GNUmakefile"}, command: []string{"make", "ci"}, capability: "make"},
	{stack: "Go", names: []string{"go.mod"}, command: []string{"go", "test", "./..."}, capability: "go@1.26"},
	{stack: ".NET", suffix: ".sln", command: []string{"dotnet", "test"}, capability: "dotnet@9"},
	{stack: ".NET", suffix: ".csproj", command: []string{"dotnet", "test"}, capability: "dotnet@9"},
	{stack: "Node.js", names: []string{"package.json"}, command: []string{"npm", "run", "ci"}, capability: "node@20"},
	{stack: "Maven", names: []string{"pom.xml"}, command: []string{"mvn", "-B", "-q", "verify"}, capability: "java@21"},
	{stack: "Gradle", names: []string{"build.gradle", "build.gradle.kts"}, command: []string{"gradle", "check"}, capability: "java@21"},
	{stack: "Rust", names: []string{"Cargo.toml"}, command: []string{"cargo", "test"}, capability: "rust"},
	{stack: "Swift", names: []string{"Package.swift"}, command: []string{"swift", "test"}, capability: "swift"},
	{stack: "Python", names: []string{"pyproject.toml", "setup.py", "requirements.txt"}, command: []string{"python3", "-m", "pytest", "-q"}, capability: "python@3.12"},
}

// detectCICommandDefault inspects dir's top-level entries (non-recursive, a
// single os.ReadDir) for a recognized build-system manifest and reports the
// first matching stack's suggested ciCommand (#2071). An unreadable dir or no
// recognized manifest returns empty values — the caller must then force
// explicit answers rather than silently offering Go-specific defaults.
func detectCICommandDefault(dir string) (stack string, command []string, requiredCapability string) {
	if dir == "" {
		return "", nil, ""
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", nil, ""
	}
	names := make(map[string]bool, len(entries))
	var suffixes []string
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		names[entry.Name()] = true
		suffixes = append(suffixes, entry.Name())
	}
	for _, signal := range ciStackSignals {
		for _, name := range signal.names {
			if names[name] {
				return signal.stack, signal.command, signal.capability
			}
		}
		if signal.suffix != "" {
			for _, name := range suffixes {
				if strings.HasSuffix(name, signal.suffix) {
					return signal.stack, signal.command, signal.capability
				}
			}
		}
	}
	for _, guidance := range []string{"AGENTS.md", "README.md", "CONTRIBUTING.md"} {
		data, err := os.ReadFile(filepath.Join(dir, guidance))
		if err != nil {
			continue
		}
		text := strings.ToLower(string(data))
		switch {
		case strings.Contains(text, "dev bi ut"):
			return ".NET / DevTool", []string{"dev", "bi", "ut"}, "devtool"
		case strings.Contains(text, "dev buildincremental"):
			return ".NET / DevTool", []string{"dev", "buildincremental"}, "devtool"
		}
	}
	return "", nil, ""
}
