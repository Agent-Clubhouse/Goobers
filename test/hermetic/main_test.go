package main

import (
	"bytes"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"
)

func TestParseInvocationPreservesConfiguredGoCommand(t *testing.T) {
	got, err := parseInvocation([]string{"--go-command", "/tools/custom-go", "--", "-race", "./..."})
	if err != nil {
		t.Fatal(err)
	}
	if got.goCommand != "/tools/custom-go" {
		t.Fatalf("Go command = %q, want /tools/custom-go", got.goCommand)
	}
	if want := []string{"-race", "./..."}; !reflect.DeepEqual(got.testArgs, want) {
		t.Fatalf("test arguments = %q, want %q", got.testArgs, want)
	}
}

func TestParseInvocationPreservesTimingConfiguration(t *testing.T) {
	got, err := parseInvocation([]string{
		"--timing-job", "unit",
		"--timing-output", "test-timings/unit.json",
		"--",
		"-race", "./...",
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.timingJob != "unit" || got.timingOutput != "test-timings/unit.json" {
		t.Fatalf("timing configuration = %q, %q", got.timingJob, got.timingOutput)
	}
}

func TestGoCommandArgsRoutesTimedTestsThroughCapture(t *testing.T) {
	got := goCommandArgs(invocation{
		timingJob:    "unit",
		timingOutput: "test-timings/unit.json",
		testArgs:     []string{"-race", "./..."},
	})
	want := []string{
		"run", "./test/testtiming", "capture",
		"-job", "unit",
		"-out", "test-timings/unit.json",
		"--",
		"-race", "./...",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("timed Go arguments = %q, want %q", got, want)
	}
}

func TestResolveToolsUsesConfiguredGoExecutable(t *testing.T) {
	ambientGo, err := exec.LookPath("go")
	if err != nil {
		t.Fatal(err)
	}
	configuredGo := filepath.Join(t.TempDir(), executableName("configured-go"))
	if err := linkTool(ambientGo, configuredGo); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		deadline := time.Now().Add(3 * time.Second)
		for {
			err := os.Remove(configuredGo)
			if err == nil || os.IsNotExist(err) {
				return
			}
			if time.Now().After(deadline) {
				t.Errorf("remove configured Go executable: %v", err)
				return
			}
			time.Sleep(25 * time.Millisecond) // Polling interval for the external test process result.
		}
	})
	goroot, err := exec.Command(ambientGo, "env", "GOROOT").Output()
	if err != nil {
		t.Fatalf("resolve ambient GOROOT: %v", err)
	}
	t.Setenv("GOROOT", strings.TrimSpace(string(goroot)))

	tools, _, err := resolveTools(configuredGo)
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range tools {
		if tool.name == "go" {
			if tool.path != configuredGo {
				t.Fatalf("resolved Go path = %q, want %q", tool.path, configuredGo)
			}
			return
		}
	}
	t.Fatal("resolved tools do not contain Go")
}

func TestPlatformToolSpecsIncludeRequiredStackTools(t *testing.T) {
	for _, tt := range []struct {
		goos  string
		tools []string
	}{
		{goos: "linux", tools: []string{"as", "ld", "node", "npm"}},
		{goos: "darwin", tools: []string{"node", "npm"}},
		{goos: "windows", tools: []string{"icacls", "icacls.exe", "node", "npm.cmd", "powershell.exe", "sh"}},
	} {
		t.Run(tt.goos, func(t *testing.T) {
			required := make(map[string]bool)
			for _, spec := range platformToolSpecs(tt.goos) {
				required[spec.name] = spec.required
			}
			for _, name := range tt.tools {
				if !required[name] {
					t.Errorf("%s tool %q is not required", tt.goos, name)
				}
			}
		})
	}
}

// PowerShell is allowlisted so the cmd/goobers PowerShell quoting tests execute
// under the hermetic tier, but it must never be mandatory: a machine without it
// has to keep running the suite, with those tests skipping themselves.
func TestPlatformToolSpecsAllowPowerShellWithoutRequiringIt(t *testing.T) {
	for _, tt := range []struct {
		goos  string
		tools []string
	}{
		{goos: "linux", tools: []string{"pwsh", "powershell"}},
		{goos: "darwin", tools: []string{"pwsh", "powershell"}},
		{goos: "windows", tools: []string{"pwsh"}},
	} {
		t.Run(tt.goos, func(t *testing.T) {
			specs := make(map[string]toolSpec)
			for _, spec := range platformToolSpecs(tt.goos) {
				specs[spec.name] = spec
			}
			for _, name := range tt.tools {
				spec, listed := specs[name]
				if !listed {
					t.Errorf("%s tool %q is not allowlisted", tt.goos, name)
					continue
				}
				if spec.required {
					t.Errorf("%s tool %q is required, want optional", tt.goos, name)
				}
			}
		})
	}
}

func TestHermeticEnvironmentReplacesAmbientToolAndNetworkSettings(t *testing.T) {
	got := hermeticEnvironment([]string{
		"HOME=/home/tester",
		"PATH=/ambient/bin",
		"GOPROXY=https://proxy.example",
		"GOPRIVATE=example.com",
		"GOFLAGS=-mod=mod",
		"CC=ambient-cc",
		"GOOBERS_OTLP_ENDPOINT=http://127.0.0.1:4317",
		"GOOBERS_OTLP_INSECURE=true",
		"GOROOT=/ambient/goroot",
	}, "/isolated/tools", "hermetic-cc", "/isolated/goroot")

	values := environmentMap(got)
	for _, name := range []string{"GOOBERS_OTLP_ENDPOINT", "GOOBERS_OTLP_INSECURE"} {
		if _, ok := values[name]; ok {
			t.Errorf("%s leaked into hermetic test environment", name)
		}
	}
	for name, want := range map[string]string{
		"CC":          "hermetic-cc",
		"GO":          executableName("go"),
		"GOENV":       "off",
		"GOFLAGS":     "-mod=readonly",
		"GONOPROXY":   "none",
		"GONOSUMDB":   "none",
		"GOPRIVATE":   "",
		"GOPROXY":     "off",
		"GOROOT":      "/isolated/goroot",
		"GOSUMDB":     "off",
		"GOTOOLCHAIN": "local",
		"GOVCS":       "*:off",
		"HOME":        "/home/tester",
		"PATH":        "/isolated/tools",
	} {
		if values[name] != want {
			t.Errorf("%s = %q, want %q", name, values[name], want)
		}
	}
}

func TestAuditTestExecsRejectsNonAllowlistedTools(t *testing.T) {
	root := t.TempDir()
	writeFixture(t, filepath.Join(root, "go.mod"), "module example.com/hermetic\n\ngo 1.26\n")
	writeFixture(t, filepath.Join(root, "unit_test.go"), `package fixture

import (
	"context"
	command "os/exec"
)

func unit() {
	_ = command.Command("copilot", "--version")
	_ = command.CommandContext(context.Background(), "docker", "version")
	_ = command.Command("git", "status")
	_ = command.Command("./fixture-tool")
}
`)
	writeFixture(t, filepath.Join(root, "live_test.go"), `//go:build integration

package fixture

import "os/exec"

func live() {
	_ = exec.Command("copilot")
}
`)

	got, err := auditTestExecs(root, map[string]struct{}{"git": {}})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("violations = %#v, want copilot and docker", got)
	}
	if got[0].tool != "copilot" || got[1].tool != "docker" {
		t.Fatalf("tools = %q, %q, want copilot, docker", got[0].tool, got[1].tool)
	}
	for _, item := range got {
		if item.position.Filename != "unit_test.go" {
			t.Errorf("position filename = %q, want unit_test.go", item.position.Filename)
		}
	}
}

func TestReportViolationsDirectsAuthorToIntegrationTier(t *testing.T) {
	var output bytes.Buffer
	reportViolations(&output, []violation{{
		position: token.Position{Filename: "fixture_test.go", Line: 12, Column: 3},
		tool:     "copilot",
	}})
	for _, want := range []string{"fixture_test.go:12:3", "copilot not allowlisted", "//go:build integration", "integration tier"} {
		if !strings.Contains(output.String(), want) {
			t.Errorf("diagnostic %q does not contain %q", output.String(), want)
		}
	}
}

func TestPopulateToolPathLinksSharedDestinationOnce(t *testing.T) {
	// Two allowlist names can normalise to a single executable — on Windows
	// executableName maps both "icacls" and "icacls.exe" to icacls.exe. Linking
	// the second one used to fail with "The file exists", taking every Windows
	// behavioral shard red. Same-named entries reproduce that collision on any
	// GOOS.
	sourceDir := t.TempDir()
	source := filepath.Join(sourceDir, executableName("icacls"))
	writeFixture(t, source, "icacls")

	destination := t.TempDir()
	if err := populateToolPath(destination, []resolvedTool{
		{name: "icacls", path: source},
		{name: "icacls", path: source},
	}); err != nil {
		t.Fatalf("populateToolPath with a shared destination: %v", err)
	}

	entries, err := os.ReadDir(destination)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != executableName("icacls") {
		var names []string
		for _, entry := range entries {
			names = append(names, entry.Name())
		}
		t.Fatalf("tool PATH entries = %q, want exactly [%q]", names, executableName("icacls"))
	}
}

func TestPopulateToolPathContainsOnlyResolvedTools(t *testing.T) {
	sourceDir := t.TempDir()
	first := filepath.Join(sourceDir, executableName("go"))
	second := filepath.Join(sourceDir, executableName("git"))
	writeFixture(t, first, "go")
	writeFixture(t, second, "git")

	destination := t.TempDir()
	if err := populateToolPath(destination, []resolvedTool{
		{name: "go", path: first},
		{name: "git", path: second},
	}); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(destination)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	want := []string{executableName("git"), executableName("go")}
	if !reflect.DeepEqual(names, want) {
		t.Fatalf("tool PATH entries = %q, want %q", names, want)
	}
	if runtime.GOOS != "windows" {
		for _, entry := range entries {
			info, err := os.Lstat(filepath.Join(destination, entry.Name()))
			if err != nil {
				t.Fatal(err)
			}
			if info.Mode()&os.ModeSymlink == 0 {
				t.Errorf("%s is not a symlink", entry.Name())
			}
		}
	}
}

func TestMissingExecToolDiagnostics(t *testing.T) {
	tests := map[string]string{
		`exec: "copilot": executable file not found in $PATH`: "copilot",
		"bash: docker: command not found":                     "docker",
		"sh: 1: gh: not found":                                "gh",
		`exec: "git": permission denied`:                      "",
	}
	for line, want := range tests {
		if got := missingExecTool(line); got != want {
			t.Errorf("missingExecTool(%q) = %q, want %q", line, got, want)
		}
	}
}

func TestDiagnosticCollectorReportsOnlyNonAllowlistedTools(t *testing.T) {
	collector := &diagnosticCollector{
		allowed: map[string]struct{}{"git": {}},
		tools:   make(map[string]struct{}),
	}
	var output bytes.Buffer
	writer := &diagnosticWriter{destination: &output, collector: collector}
	_, _ = writer.Write([]byte("exec: \"git\": executable file not found in $PATH\n"))
	_, _ = writer.Write([]byte("exec: \"copilot\": executable file not found in $PATH\n"))
	writer.flush()

	if got := collector.missingTools(); !reflect.DeepEqual(got, []string{"copilot"}) {
		t.Fatalf("missing tools = %q, want [copilot]", got)
	}
}

func TestRunRequiresGoTestArguments(t *testing.T) {
	var stderr bytes.Buffer
	if code := run(nil, &bytes.Buffer{}, &stderr); code != 2 {
		t.Fatalf("run exit = %d, want 2", code)
	}
	if !strings.Contains(stderr.String(), "usage:") {
		t.Fatalf("stderr = %q, want usage", stderr.String())
	}
}

func TestParseInvocationRejectsIncompleteTimingConfiguration(t *testing.T) {
	if _, err := parseInvocation([]string{"--timing-output", "timing.json", "--", "./..."}); err == nil {
		t.Fatal("parseInvocation() accepted timing output without a job")
	}
}

func environmentMap(environment []string) map[string]string {
	result := make(map[string]string, len(environment))
	for _, variable := range environment {
		name := environmentName(variable)
		result[name] = strings.TrimPrefix(variable, name+"=")
	}
	return result
}

func writeFixture(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
}

func TestParseShardValidatesSelector(t *testing.T) {
	t.Parallel()
	for _, ok := range []string{"", "1/3", "3/3", "1/1", " 2 / 4 "} {
		if _, err := parseShard(ok); err != nil {
			t.Errorf("parseShard(%q) unexpected error: %v", ok, err)
		}
	}
	for _, bad := range []string{"0/3", "4/3", "1/0", "abc", "1/", "/3", "-1/3", "1/2/3"} {
		if _, err := parseShard(bad); err == nil {
			t.Errorf("parseShard(%q) = nil error, want rejection", bad)
		}
	}
}

// TestSelectShardPartitionsExactly is the coverage-integrity guard: for any
// package set and shard count, the shards must be disjoint and their union must
// equal the whole set — so fanning the unit suite across runners never silently
// drops or double-runs a package.
func TestSelectShardPartitionsExactly(t *testing.T) {
	t.Parallel()
	weights := shardWeights{
		DefaultSeconds: 1,
		Packages: map[string]float64{
			"pkg/a/1": 20,
			"pkg/b/2": 10,
		},
	}
	for _, total := range []int{1, 2, 3, 5, 8} {
		for _, size := range []int{0, 1, 7, 40, 137} {
			packages := make([]string, size)
			for i := range packages {
				// Deliberately unsorted input to prove selectShard sorts.
				packages[i] = "pkg/" + string(rune('a'+i%26)) + "/" + itoa(size-i)
			}
			seen := map[string]int{}
			for index := 1; index <= total; index++ {
				for _, pkg := range selectShard(packages, shardSpec{index: index, total: total}, weights) {
					seen[pkg]++
				}
			}
			// Deduplicate the input (generated names can collide) before comparing.
			want := map[string]bool{}
			for _, pkg := range packages {
				want[pkg] = true
			}
			if len(seen) != len(want) {
				t.Fatalf("total=%d size=%d: union has %d packages, want %d", total, size, len(seen), len(want))
			}
			for pkg, count := range seen {
				if !want[pkg] {
					t.Fatalf("total=%d: shard produced unknown package %q", total, pkg)
				}
				if count != 1 {
					t.Fatalf("total=%d: package %q appears in %d shards, want exactly 1", total, pkg, count)
				}
			}
		}
	}
}

func TestSelectShardIsDeterministicRegardlessOfInputOrder(t *testing.T) {
	t.Parallel()
	forward := []string{"a", "b", "c", "d", "e"}
	reversed := []string{"e", "d", "c", "b", "a"}
	spec := shardSpec{index: 1, total: 2}
	weights := shardWeights{DefaultSeconds: 1}
	if !reflect.DeepEqual(selectShard(forward, spec, weights), selectShard(reversed, spec, weights)) {
		t.Fatalf("selectShard depends on input order: %v vs %v",
			selectShard(forward, spec, weights), selectShard(reversed, spec, weights))
	}
}

func TestCheckedInShardWeightsBalanceRepresentativeRun(t *testing.T) {
	root, err := findModuleRoot()
	if err != nil {
		t.Fatal(err)
	}
	weights, err := loadShardWeights(root)
	if err != nil {
		t.Fatal(err)
	}
	list := exec.Command("go", "list", "./...")
	list.Dir = root
	output, err := list.Output()
	if err != nil {
		t.Fatal(err)
	}
	packages := strings.Fields(string(output))

	totals := make([]float64, 3)
	for index := 1; index <= len(totals); index++ {
		for _, pkg := range selectShard(packages, shardSpec{index: index, total: len(totals)}, weights) {
			totals[index-1] += weights.packageSeconds(pkg)
		}
	}
	sort.Float64s(totals)
	if ratio := totals[len(totals)-1] / totals[0]; ratio > 2 {
		t.Fatalf("measured shard ratio = %.2fx (%v), want no more than 2x", ratio, totals)
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var digits []byte
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	if neg {
		digits = append([]byte{'-'}, digits...)
	}
	return string(digits)
}
