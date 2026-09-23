package main

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"maps"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

func TestCIWindowsJournalOTLPCoverage(t *testing.T) {
	t.Parallel()
	workflow := loadCIWorkflow(t)
	job := workflow.Jobs["windows-smoke"]
	step := job.step(t, "Windows journal OTLP export")
	if job.RunsOn != "windows-latest" || job.If != "" || job.ContinueOnError ||
		step.If != "" || step.ContinueOnError || !slices.Contains(workflow.Jobs["required-ci"].Needs, "windows-smoke") {
		t.Fatal("journal export must run unconditionally in the required Windows gate")
	}
	required := journalOTLPTestInventory(t)
	if err := journalOTLPSelectionError(step.Run, required); err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, tests := range required {
		count += len(tests)
	}
	t.Logf("Windows journal selection covers %d regression tests across %d packages", count, len(required))
}

func journalOTLPTestInventory(t *testing.T) map[string][]string {
	t.Helper()
	root := moduleRoot(t)
	required := map[string][]string{
		"./cmd/goobers": {"TestRunNoWaitReturnsAfterStandaloneDispatch"},
	}
	for _, pattern := range []string{
		"internal/journal/committed_test.go",
		"internal/livejournal/committed_test.go",
		"internal/telemetry/journallogs*_test.go",
		"internal/instance/journallogs_test.go",
		"api/schemas/journallogs_test.go",
		"internal/engine/projection_export_test.go",
		"cmd/goobers/journaltelemetry_test.go",
	} {
		paths, err := filepath.Glob(filepath.Join(root, filepath.FromSlash(pattern)))
		if err != nil || len(paths) == 0 {
			t.Fatalf("journal test inventory %s: files=%v, error=%v", pattern, paths, err)
		}
		for _, path := range paths {
			file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
			if err != nil {
				t.Fatal(err)
			}
			rel, err := filepath.Rel(root, filepath.Dir(path))
			if err != nil {
				t.Fatal(err)
			}
			pkg := "./" + filepath.ToSlash(rel)
			count := 0
			for _, decl := range file.Decls {
				fn, ok := decl.(*ast.FuncDecl)
				if ok && fn.Recv == nil && strings.HasPrefix(fn.Name.Name, "Test") {
					required[pkg] = append(required[pkg], fn.Name.Name)
					count++
				}
			}
			if count == 0 {
				t.Fatalf("journal regression file %s contains no tests", path)
			}
		}
	}
	return required
}

func journalOTLPSelectionError(command string, required map[string][]string) error {
	// Only a single, uncached test command is accepted; shell fallbacks must not
	// turn a failing or empty selection into a passing Windows step.
	shape := regexp.MustCompile(`^go test ((?:\./[a-zA-Z0-9/]+ )+)-run '([^']+)' -count=1 -timeout=3m$`)
	parts := shape.FindStringSubmatch(strings.TrimSpace(command))
	if parts == nil {
		return fmt.Errorf("journal Windows step must use explicit packages, a quoted -run filter, -count=1 and -timeout=3m: %s", command)
	}
	selection, err := regexp.Compile(parts[2])
	if err != nil {
		return err
	}
	packages := strings.Fields(parts[1])
	for _, pkg := range slices.Sorted(maps.Keys(required)) {
		if !slices.Contains(packages, pkg) {
			return fmt.Errorf("journal Windows step omits package %s", pkg)
		}
		for _, name := range required[pkg] {
			if !selection.MatchString(name) {
				return fmt.Errorf("journal Windows step omits %s in %s", name, pkg)
			}
		}
	}
	if selection.MatchString("TestUnrelatedCLIBehavior") {
		return fmt.Errorf("journal Windows step must not select the entire CLI suite")
	}
	return nil
}

func TestCIWindowsJournalOTLPCoverageRejectsGaps(t *testing.T) {
	t.Parallel()
	command := loadCIWorkflow(t).Jobs["windows-smoke"].step(t, "Windows journal OTLP export").Run
	required := journalOTLPTestInventory(t)
	for name, broken := range map[string]string{
		"missing exporter package": strings.Replace(command, "./internal/telemetry ", "", 1),
		"missing CLI lifetime":     strings.Replace(command, "|TestCommandJournalTelemetrySharesRootLifetime", "", 1),
		"missing no-wait lifetime": strings.Replace(command, "|TestRunNoWaitReturnsAfterStandaloneDispatch", "", 1),
		"missing retry tests":      strings.Replace(command, "TestJournalLogs.*", "TestJournalLogsOTLPWireContract", 1),
		"cached execution":         strings.Replace(command, "-count=1", "-count=0", 1),
		"ignored failure":          command + " || true",
		"whole CLI suite":          strings.Replace(command, "TestCommitted.*", "Test.*", 1),
	} {
		t.Run(name, func(t *testing.T) {
			if err := journalOTLPSelectionError(broken, required); err == nil {
				t.Fatal("coverage guard accepted a broken journal selection")
			}
		})
	}
}

func TestCILinuxJournalOTLPRaceCoverage(t *testing.T) {
	t.Parallel()
	workflow := loadCIWorkflow(t)
	job := workflow.Jobs["unit"]
	step := job.step(t, "Unit suite (-race, shard ${{ matrix.shard }})")
	if job.RunsOn != "ubuntu-latest" || job.If != "" || job.ContinueOnError ||
		step.If != "" || step.ContinueOnError || !slices.Contains(workflow.Jobs["required-ci"].Needs, "unit") {
		t.Fatal("Linux race shards must remain an unconditional required gate")
	}
	if step.Run != "go run ./test/ci group unit" || step.Env["GOOBERS_CI_SHARD"] != "${{ matrix.shard }}" {
		t.Fatal("Linux race job must run the real unit group with its shard matrix")
	}
	if !slices.Equal(job.Strategy.Matrix.Shard, []string{"1/3", "2/3", "3/3"}) {
		t.Fatalf("Linux race shard matrix is incomplete: %v", job.Strategy.Matrix.Shard)
	}
	env := maps.Clone(workflow.Env)
	maps.Copy(env, job.Env)
	maps.Copy(env, step.Env)
	if env["CGO_ENABLED"] == "0" {
		t.Fatal("Linux race job disables cgo")
	}
	for _, shard := range job.Strategy.Matrix.Shard {
		env["GOOBERS_CI_SHARD"] = shard
		unit := applyRuntimeToggles(groupChecksOnly(mergeGateChecks(), groupUnit), func(key string) string { return env[key] })
		args := checkByLabel(t, unit, "test").args
		for _, required := range []string{"./test/hermetic", "--shard", shard, "-race", "-count=1", "./..."} {
			if !slices.Contains(args, required) {
				t.Fatalf("Linux shard %s omits %s: %v", shard, required, args)
			}
		}
		for _, arg := range args {
			if arg == "-run" || strings.HasPrefix(arg, "-run=") || arg == "-skip" || strings.HasPrefix(arg, "-skip=") {
				t.Fatalf("Linux shard %s filters out unit tests: %v", shard, args)
			}
		}
	}
}
