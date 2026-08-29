// Command hermetic runs the repository's unit tests with an isolated tool PATH.
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"go/ast"
	"go/build"
	"go/parser"
	"go/token"
	"io"
	"math"
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
const shardWeightsPath = ".github/unit-shard-weights.json"

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

type invocation struct {
	goCommand    string
	timingJob    string
	timingOutput string
	shard        shardSpec
	testArgs     []string
}

// shardSpec selects one of `total` disjoint package partitions (1-based index).
// The zero value (total == 0) means "run every package" — no sharding.
type shardSpec struct {
	index int
	total int
}

func (s shardSpec) enabled() bool { return s.total > 0 }

type shardWeights struct {
	SchemaVersion  int                `json:"schemaVersion"`
	DefaultSeconds float64            `json:"defaultSeconds"`
	Packages       map[string]float64 `json:"packages"`
}

func (w shardWeights) packageSeconds(pkg string) float64 {
	if seconds, ok := w.Packages[pkg]; ok {
		return seconds
	}
	return w.DefaultSeconds
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
	invocation, err := parseInvocation(args)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "hermetic tier: %v\n", err)
		_, _ = fmt.Fprintln(stderr, "usage: go run ./test/hermetic [--go-command <go>] [--timing-job <job> --timing-output <file>] -- <go test arguments>")
		return 2
	}

	root, err := findModuleRoot()
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "hermetic tier: %v\n", err)
		return 1
	}

	tools, compilerName, err := resolveTools(invocation.goCommand)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "hermetic tier: %v\n", err)
		return 1
	}
	goroot, err := resolveGoroot(tools[0].path)
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

	if invocation.shard.enabled() {
		sharded, count, err := shardTestArgs(invocation.goCommand, root, invocation.testArgs, invocation.shard)
		if err != nil {
			_, _ = fmt.Fprintf(stderr, "hermetic tier: %v\n", err)
			return 1
		}
		_, _ = fmt.Fprintf(stdout, "hermetic tier: shard %d/%d runs %d packages\n",
			invocation.shard.index, invocation.shard.total, count)
		invocation.testArgs = sharded
	}

	goArgs := goCommandArgs(invocation)
	command := exec.Command(filepath.Join(toolDir, executableName("go")), goArgs...)
	command.Dir = root
	command.Env = hermeticEnvironment(os.Environ(), toolDir, compilerName, goroot)

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

func parseInvocation(args []string) (invocation, error) {
	flags := flag.NewFlagSet("hermetic", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	goCommand := flags.String("go-command", "go", "Go executable")
	timingJob := flags.String("timing-job", "", "stable timing job name")
	timingOutput := flags.String("timing-output", "", "timing artifact path")
	shard := flags.String("shard", "", "run one package partition as i/n (e.g. 2/3)")
	if err := flags.Parse(args); err != nil {
		return invocation{}, err
	}
	if strings.TrimSpace(*goCommand) == "" {
		return invocation{}, errors.New("--go-command requires an executable")
	}
	if (*timingJob == "") != (*timingOutput == "") {
		return invocation{}, errors.New("--timing-job and --timing-output must be provided together")
	}
	spec, err := parseShard(*shard)
	if err != nil {
		return invocation{}, err
	}
	if spec.enabled() && *timingOutput != "" {
		return invocation{}, errors.New("--shard cannot be combined with timing capture (timing needs the whole suite)")
	}
	if len(flags.Args()) == 0 {
		return invocation{}, errors.New("go test arguments are required")
	}
	return invocation{
		goCommand:    *goCommand,
		timingJob:    *timingJob,
		timingOutput: *timingOutput,
		shard:        spec,
		testArgs:     flags.Args(),
	}, nil
}

// parseShard reads an "i/n" shard selector. Empty means no sharding.
func parseShard(raw string) (shardSpec, error) {
	if strings.TrimSpace(raw) == "" {
		return shardSpec{}, nil
	}
	parts := strings.SplitN(raw, "/", 2)
	if len(parts) != 2 {
		return shardSpec{}, fmt.Errorf("shard %q must be i/n", raw)
	}
	index, err := strconv.Atoi(strings.TrimSpace(parts[0]))
	if err != nil {
		return shardSpec{}, fmt.Errorf("shard %q: %w", raw, err)
	}
	total, err := strconv.Atoi(strings.TrimSpace(parts[1]))
	if err != nil {
		return shardSpec{}, fmt.Errorf("shard %q: %w", raw, err)
	}
	if total < 1 || index < 1 || index > total {
		return shardSpec{}, fmt.Errorf("shard %q must satisfy 1 <= i <= n", raw)
	}
	return shardSpec{index: index, total: total}, nil
}

// selectShard uses longest-processing-time-first assignment so measured slow
// packages are distributed before smaller packages fill the remaining gaps.
func selectShard(pkgs []string, spec shardSpec, weights shardWeights) []string {
	ordered := append([]string(nil), pkgs...)
	sort.Slice(ordered, func(i, j int) bool {
		left := weights.packageSeconds(ordered[i])
		right := weights.packageSeconds(ordered[j])
		if left == right {
			return ordered[i] < ordered[j]
		}
		return left > right
	})

	shards := make([][]string, spec.total)
	totals := make([]float64, spec.total)
	for _, pkg := range ordered {
		target := 0
		for index := 1; index < spec.total; index++ {
			if totals[index] < totals[target] {
				target = index
			}
		}
		shards[target] = append(shards[target], pkg)
		totals[target] += weights.packageSeconds(pkg)
	}
	return shards[spec.index-1]
}

func loadShardWeights(root string) (shardWeights, error) {
	path := filepath.Join(root, shardWeightsPath)
	data, err := os.ReadFile(path)
	if err != nil {
		return shardWeights{}, fmt.Errorf("read shard weights %s: %w", path, err)
	}
	var weights shardWeights
	if err := json.Unmarshal(data, &weights); err != nil {
		return shardWeights{}, fmt.Errorf("parse shard weights %s: %w", path, err)
	}
	if weights.SchemaVersion != 1 {
		return shardWeights{}, fmt.Errorf("shard weights %s: unsupported schemaVersion %d", path, weights.SchemaVersion)
	}
	if !validShardWeight(weights.DefaultSeconds) {
		return shardWeights{}, fmt.Errorf("shard weights %s: defaultSeconds must be finite and positive", path)
	}
	for pkg, seconds := range weights.Packages {
		if strings.TrimSpace(pkg) == "" || !validShardWeight(seconds) {
			return shardWeights{}, fmt.Errorf("shard weights %s: package %q must have a finite positive duration", path, pkg)
		}
	}
	return weights, nil
}

func validShardWeight(seconds float64) bool {
	return seconds > 0 && !math.IsInf(seconds, 0) && !math.IsNaN(seconds)
}

// shardTestArgs replaces the `./...` package spec in testArgs with the subset
// of packages assigned to this shard, discovered via `go list`.
func shardTestArgs(goCommand, root string, testArgs []string, spec shardSpec) ([]string, int, error) {
	list := exec.Command(goCommand, "list", "./...")
	list.Dir = root
	output, err := list.Output()
	if err != nil {
		return nil, 0, fmt.Errorf("list packages for sharding: %w", err)
	}
	packages := strings.Fields(string(output))
	if len(packages) == 0 {
		return nil, 0, errors.New("go list ./... returned no packages to shard")
	}
	weights, err := loadShardWeights(root)
	if err != nil {
		return nil, 0, err
	}
	selected := selectShard(packages, spec, weights)
	if len(selected) == 0 {
		return nil, 0, fmt.Errorf("shard %d/%d selected no packages from %d", spec.index, spec.total, len(packages))
	}
	result := make([]string, 0, len(testArgs)+len(selected))
	replaced := false
	for _, arg := range testArgs {
		if arg == "./..." && !replaced {
			result = append(result, selected...)
			replaced = true
			continue
		}
		result = append(result, arg)
	}
	if !replaced {
		return nil, 0, errors.New("--shard requires a ./... package spec in the go test arguments")
	}
	return result, len(selected), nil
}

func goCommandArgs(invocation invocation) []string {
	if invocation.timingOutput == "" {
		return append([]string{"test"}, invocation.testArgs...)
	}
	args := []string{
		"run", "./test/testtiming", "capture",
		"-job", invocation.timingJob,
		"-out", invocation.timingOutput,
		"--",
	}
	return append(args, invocation.testArgs...)
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
	specs := platformToolSpecs(runtime.GOOS)

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

func platformToolSpecs(goos string) []toolSpec {
	if goos == "windows" {
		return []toolSpec{
			{name: "git", required: true},
			{name: "cmd.exe", required: true},
			// Both spellings: internal/platform/secfile execs bare "icacls",
			// internal/credentials execs "icacls.exe". The audit matches the
			// literal argv[0], so dropping either one fails that package.
			{name: "icacls", required: true},
			{name: "icacls.exe", required: true},
			{name: "node", required: true},
			{name: "npm.cmd", required: true},
			{name: "powershell.exe", required: true},
			// Optional: PowerShell 7 ships as pwsh alongside the built-in
			// Windows PowerShell. Hosted runners have it, developer machines
			// need not.
			{name: "pwsh"},
			{name: "sh", required: true},
		}
	}

	specs := []toolSpec{
		{name: "git", required: true},
		{name: "node", required: true},
		{name: "npm", required: true},
		{name: "sh", required: true},
		{name: "bash"},
		// Optional: PowerShell is cross-platform and preinstalled on hosted
		// runners, so allowlisting it lets the PowerShell quoting tests execute
		// in the hermetic tier instead of skipping. It must stay optional -
		// developer machines without PowerShell simply run those tests as
		// skips, exactly as they do today. Both spellings are listed because
		// the binary is pwsh off Windows and powershell on it.
		{name: "pwsh"},
		{name: "powershell"},
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
	if goos == "linux" {
		specs = append(specs,
			toolSpec{name: "as", required: true},
			toolSpec{name: "ld", required: true},
		)
	}
	return specs
}

func toolNames(tools []resolvedTool) map[string]struct{} {
	names := make(map[string]struct{}, len(tools))
	for _, tool := range tools {
		names[tool.name] = struct{}{}
	}
	return names
}

func populateToolPath(directory string, tools []resolvedTool) error {
	// Distinct allowlist names can normalise to one executable: on Windows
	// executableName maps both "icacls" and "icacls.exe" to icacls.exe. The
	// allowlist needs both spellings so the audit matches either literal
	// argv[0], but the binary must only be linked once — linkTool fails with
	// "The file exists" on the second attempt.
	linked := make(map[string]struct{}, len(tools))
	for _, tool := range tools {
		destination := filepath.Join(directory, executableName(tool.name))
		if _, done := linked[destination]; done {
			continue
		}
		linked[destination] = struct{}{}
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

// resolveGoroot asks the real Go binary where its GOROOT is, before that binary
// is linked into the hermetic tool PATH.
//
// The tool PATH cannot be relied on to answer this. Off Windows linkTool makes
// a symlink, so the Go runtime follows it back to the original binary and infers
// GOROOT by itself. On Windows linkTool hardlinks or copies, and a copied go.exe
// has no path home — it fails with "'go' binary is trimmed and GOROOT is not
// set". Setting GOROOT explicitly makes the environment correct on every
// platform rather than relying on symlink resolution.
func resolveGoroot(goPath string) (string, error) {
	output, err := exec.Command(goPath, "env", "GOROOT").Output()
	if err != nil {
		return "", fmt.Errorf("resolve GOROOT from %s: %w", goPath, err)
	}
	goroot := strings.TrimSpace(string(output))
	if goroot == "" {
		return "", fmt.Errorf("resolve GOROOT from %s: empty result", goPath)
	}
	return goroot, nil
}

func hermeticEnvironment(base []string, toolPath, compilerName, goroot string) []string {
	excluded := map[string]string{
		"GOOBERS_OTLP_ENDPOINT": "",
		"GOOBERS_OTLP_INSECURE": "",
	}
	overrides := map[string]string{
		"CC":          compilerName,
		"GO":          executableName("go"),
		"GOROOT":      goroot,
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
		if _, excluded := environmentOverride(excluded, name); excluded {
			continue
		}
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
