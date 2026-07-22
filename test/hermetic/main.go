// Command hermetic runs the repository's unit tests with an isolated tool PATH.
package main

import (
	"errors"
	"fmt"
	"go/ast"
	"go/build"
	"go/parser"
	"go/token"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
)

const integrationGuidance = "tag this test with //go:build integration and run it in the integration tier"

type toolSpec struct {
	name     string
	required bool
}

type resolvedTool struct {
	name string
	path string
}

type violation struct {
	position token.Position
	tool     string
}

type diagnosticCollector struct {
	mu      sync.Mutex
	allowed map[string]struct{}
	tools   map[string]struct{}
}

type diagnosticWriter struct {
	destination io.Writer
	collector   *diagnosticCollector
	mu          sync.Mutex
	pending     string
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	goCommand, args, err := parseInvocation(args)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "hermetic tier: %v\n", err)
		_, _ = fmt.Fprintln(stderr, "usage: go run ./test/hermetic [--go-command <go>] -- <go test arguments>")
		return 2
	}

	root, err := findModuleRoot()
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "hermetic tier: %v\n", err)
		return 1
	}

	tools, compilerName, err := resolveTools(goCommand)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "hermetic tier: %v\n", err)
		return 1
	}
	allowed := toolNames(tools)
	violations, err := auditTestExecs(root, allowed)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "hermetic tier: audit test subprocesses: %v\n", err)
		return 1
	}
	if len(violations) > 0 {
		reportViolations(stderr, violations)
		return 1
	}

	toolDir, err := os.MkdirTemp("", "goobers-hermetic-path-")
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "hermetic tier: create tool PATH: %v\n", err)
		return 1
	}
	defer func() { _ = os.RemoveAll(toolDir) }()

	if err := populateToolPath(toolDir, tools); err != nil {
		_, _ = fmt.Fprintf(stderr, "hermetic tier: populate tool PATH: %v\n", err)
		return 1
	}

	goArgs := append([]string{"test"}, args...)
	command := exec.Command(filepath.Join(toolDir, executableName("go")), goArgs...)
	command.Dir = root
	command.Env = hermeticEnvironment(os.Environ(), toolDir, compilerName)

	collector := &diagnosticCollector{allowed: allowed, tools: make(map[string]struct{})}
	stdoutWriter := &diagnosticWriter{destination: stdout, collector: collector}
	stderrWriter := &diagnosticWriter{destination: stderr, collector: collector}
	command.Stdout = stdoutWriter
	command.Stderr = stderrWriter
	err = command.Run()
	stdoutWriter.flush()
	stderrWriter.flush()
	if err == nil {
		return 0
	}
	for _, tool := range collector.missingTools() {
		_, _ = fmt.Fprintf(stderr, "hermetic tier: %s not allowlisted - %s\n", tool, integrationGuidance)
	}
	return 1
}

func parseInvocation(args []string) (string, []string, error) {
	goCommand := "go"
	if len(args) > 0 && args[0] == "--go-command" {
		if len(args) < 2 || strings.TrimSpace(args[1]) == "" {
			return "", nil, errors.New("--go-command requires an executable")
		}
		goCommand = args[1]
		args = args[2:]
	}
	if len(args) > 0 && args[0] == "--" {
		args = args[1:]
	}
	if len(args) == 0 {
		return "", nil, errors.New("go test arguments are required")
	}
	return goCommand, args, nil
}

func findModuleRoot() (string, error) {
	current, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("get working directory: %w", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(current, "go.mod")); err == nil {
			return current, nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return "", fmt.Errorf("inspect %s: %w", current, err)
		}
		parent := filepath.Dir(current)
		if parent == current {
			return "", errors.New("go.mod not found in working directory or its parents")
		}
		current = parent
	}
}

func resolveTools(goCommand string) ([]resolvedTool, string, error) {
	var specs []toolSpec
	if runtime.GOOS == "windows" {
		specs = []toolSpec{
			{name: "git", required: true},
			{name: "cmd.exe", required: true},
			{name: "icacls", required: true},
		}
	} else {
		specs = []toolSpec{
			{name: "git", required: true},
			{name: "sh", required: true},
			{name: "bash"},
			{name: "cat", required: true},
			{name: "dirname", required: true},
			{name: "echo", required: true},
			{name: "false", required: true},
			{name: "head", required: true},
			{name: "mkdir", required: true},
			{name: "rm", required: true},
			{name: "sleep", required: true},
			{name: "tr", required: true},
			{name: "true", required: true},
			{name: "wc", required: true},
			{name: "yes", required: true},
		}
	}

	goPath, err := exec.LookPath(goCommand)
	if err != nil {
		return nil, "", fmt.Errorf("configured Go command %q is unavailable: %w", goCommand, err)
	}
	tools := make([]resolvedTool, 0, len(specs)+2)
	tools = append(tools, resolvedTool{name: "go", path: goPath})
	for _, spec := range specs {
		path, err := exec.LookPath(spec.name)
		if err != nil {
			if spec.required {
				return nil, "", fmt.Errorf("required allowlisted tool %q is unavailable: %w", spec.name, err)
			}
			continue
		}
		tools = append(tools, resolvedTool{name: spec.name, path: path})
	}

	output, err := exec.Command(goPath, "env", "CC").Output()
	if err != nil {
		return nil, "", fmt.Errorf("resolve Go C compiler: %w", err)
	}
	compilerCommand := strings.TrimSpace(string(output))
	if fields := strings.Fields(compilerCommand); len(fields) != 1 {
		return nil, "", fmt.Errorf("go C compiler command %q must be a single executable", compilerCommand)
	}
	compilerPath, err := exec.LookPath(compilerCommand)
	if err != nil {
		return nil, "", fmt.Errorf("required race-detector C compiler %q is unavailable: %w", compilerCommand, err)
	}
	compilerName := filepath.Base(compilerCommand)
	if runtime.GOOS == "windows" && !strings.HasSuffix(strings.ToLower(compilerName), ".exe") {
		compilerName += ".exe"
	}
	if _, exists := toolNames(tools)[compilerName]; !exists {
		tools = append(tools, resolvedTool{name: compilerName, path: compilerPath})
	}
	return tools, compilerName, nil
}

func toolNames(tools []resolvedTool) map[string]struct{} {
	names := make(map[string]struct{}, len(tools))
	for _, tool := range tools {
		names[tool.name] = struct{}{}
	}
	return names
}

func populateToolPath(directory string, tools []resolvedTool) error {
	for _, tool := range tools {
		destination := filepath.Join(directory, executableName(tool.name))
		if err := linkTool(tool.path, destination); err != nil {
			return fmt.Errorf("link %s: %w", tool.name, err)
		}
	}
	return nil
}

func linkTool(source, destination string) (returnErr error) {
	absolute, err := filepath.Abs(source)
	if err != nil {
		return err
	}
	if runtime.GOOS != "windows" {
		return os.Symlink(absolute, destination)
	}
	if err := os.Link(absolute, destination); err == nil {
		return nil
	}
	input, err := os.Open(absolute)
	if err != nil {
		return err
	}
	defer func() {
		returnErr = errors.Join(returnErr, input.Close())
	}()
	info, err := input.Stat()
	if err != nil {
		return err
	}
	output, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_EXCL, info.Mode().Perm())
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(output, input)
	closeErr := output.Close()
	return errors.Join(copyErr, closeErr)
}

func executableName(name string) string {
	if runtime.GOOS == "windows" && !strings.HasSuffix(strings.ToLower(name), ".exe") {
		return name + ".exe"
	}
	return name
}

func hermeticEnvironment(base []string, toolPath, compilerName string) []string {
	overrides := map[string]string{
		"CC":          compilerName,
		"GOENV":       "off",
		"GOFLAGS":     "-mod=readonly",
		"GONOPROXY":   "none",
		"GONOSUMDB":   "none",
		"GOPRIVATE":   "",
		"GOPROXY":     "off",
		"GOSUMDB":     "off",
		"GOTOOLCHAIN": "local",
		"GOVCS":       "*:off",
		"PATH":        toolPath,
	}
	result := make([]string, 0, len(base)+len(overrides))
	for _, variable := range base {
		name := environmentName(variable)
		if _, overridden := environmentOverride(overrides, name); !overridden {
			result = append(result, variable)
		}
	}
	names := make([]string, 0, len(overrides))
	for name := range overrides {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		result = append(result, name+"="+overrides[name])
	}
	return result
}

func environmentOverride(overrides map[string]string, name string) (string, bool) {
	if runtime.GOOS != "windows" {
		value, ok := overrides[name]
		return value, ok
	}
	for candidate, value := range overrides {
		if strings.EqualFold(candidate, name) {
			return value, true
		}
	}
	return "", false
}

func environmentName(variable string) string {
	if index := strings.IndexByte(variable, '='); index >= 0 {
		return variable[:index]
	}
	return variable
}

func auditTestExecs(root string, allowed map[string]struct{}) ([]violation, error) {
	var violations []violation
	fset := token.NewFileSet()
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			switch entry.Name() {
			case ".git", ".goobers", "bin", "node_modules", "vendor":
				if path != root {
					return filepath.SkipDir
				}
			}
			return nil
		}
		if !strings.HasSuffix(entry.Name(), "_test.go") {
			return nil
		}
		matched, err := build.Default.MatchFile(filepath.Dir(path), entry.Name())
		if err != nil {
			return err
		}
		if !matched {
			return nil
		}

		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		file, err := parser.ParseFile(fset, filepath.ToSlash(relative), content, 0)
		if err != nil {
			return err
		}
		execAliases := make(map[string]struct{})
		for _, imported := range file.Imports {
			path, err := strconv.Unquote(imported.Path.Value)
			if err != nil || path != "os/exec" {
				continue
			}
			alias := "exec"
			if imported.Name != nil {
				alias = imported.Name.Name
			}
			execAliases[alias] = struct{}{}
		}
		ast.Inspect(file, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok || len(call.Args) == 0 {
				return true
			}
			selector, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || selector.Sel.Name != "Command" && selector.Sel.Name != "CommandContext" {
				return true
			}
			identifier, ok := selector.X.(*ast.Ident)
			if !ok {
				return true
			}
			if _, ok := execAliases[identifier.Name]; !ok {
				return true
			}
			commandIndex := 0
			if selector.Sel.Name == "CommandContext" {
				commandIndex = 1
			}
			if len(call.Args) <= commandIndex {
				return true
			}
			literal, ok := call.Args[commandIndex].(*ast.BasicLit)
			if !ok || literal.Kind != token.STRING {
				return true
			}
			name, err := strconv.Unquote(literal.Value)
			if err != nil || strings.ContainsAny(name, `/\`) {
				return true
			}
			if _, ok := allowed[name]; ok {
				return true
			}
			violations = append(violations, violation{position: fset.Position(literal.Pos()), tool: name})
			return true
		})
		return nil
	})
	sort.Slice(violations, func(i, j int) bool {
		left, right := violations[i], violations[j]
		if left.position.Filename != right.position.Filename {
			return left.position.Filename < right.position.Filename
		}
		if left.position.Line != right.position.Line {
			return left.position.Line < right.position.Line
		}
		return left.tool < right.tool
	})
	return violations, err
}

func reportViolations(destination io.Writer, violations []violation) {
	for _, item := range violations {
		_, _ = fmt.Fprintf(
			destination,
			"%s: hermetic tier: %s not allowlisted - %s\n",
			item.position,
			item.tool,
			integrationGuidance,
		)
	}
}

func (w *diagnosticWriter) Write(data []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.pending += string(data)
	for {
		index := strings.IndexByte(w.pending, '\n')
		if index < 0 {
			break
		}
		w.collector.observe(w.pending[:index])
		w.pending = w.pending[index+1:]
	}
	return w.destination.Write(data)
}

func (w *diagnosticWriter) flush() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.pending != "" {
		w.collector.observe(w.pending)
		w.pending = ""
	}
}

func (c *diagnosticCollector) observe(line string) {
	name := missingExecTool(line)
	if name == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, allowed := c.allowed[name]; !allowed {
		c.tools[name] = struct{}{}
	}
}

func (c *diagnosticCollector) missingTools() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	tools := make([]string, 0, len(c.tools))
	for tool := range c.tools {
		tools = append(tools, tool)
	}
	sort.Strings(tools)
	return tools
}

func missingExecTool(line string) string {
	const execPrefix = `exec: "`
	if start := strings.Index(line, execPrefix); start >= 0 {
		rest := line[start+len(execPrefix):]
		if end := strings.Index(rest, `": executable file not found`); end > 0 {
			return rest[:end]
		}
	}
	for _, suffix := range []string{": command not found", ": not found"} {
		if !strings.HasSuffix(strings.TrimSpace(line), suffix) {
			continue
		}
		prefix := strings.TrimSuffix(strings.TrimSpace(line), suffix)
		if index := strings.LastIndex(prefix, ": "); index >= 0 {
			return strings.TrimSpace(prefix[index+2:])
		}
	}
	return ""
}
