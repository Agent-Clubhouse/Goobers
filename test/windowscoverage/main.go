// Command windowscoverage enforces #2031's Windows test-surface policy: every
// package in this module is either exercised by a `go test` invocation in
// ci.yml's windows-smoke job, or explicitly recorded in skip-inventory.txt
// with a reason. Before this tool, the windows-smoke allowlist was an
// undocumented accident of history — a package could silently never run on
// Windows with nothing surfacing the gap for review. This makes the boundary
// a reviewed, enumerated, drift-checked artifact instead, mirroring
// test/deadcode's exemptions.txt shape and bidirectional-drift discipline.
//
// It runs on any host (it only ever shells out to `go list`, never `go test`
// or a Windows binary) — the windows-smoke job itself still owns actually
// proving each covered package's tests pass on Windows; this tool only
// audits which packages are accounted for, not whether their tests pass.
package main

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"sigs.k8s.io/yaml"
)

const (
	ciWorkflowPath    = ".github/workflows/ci.yml"
	skipInventoryPath = "test/windowscoverage/skip-inventory.txt"
	windowsSmokeJob   = "windows-smoke"
)

func main() {
	flags := flag.NewFlagSet("windowscoverage", flag.ContinueOnError)
	goCommand := flags.String("go", "go", "Go command to use")
	if err := flags.Parse(os.Args[1:]); err != nil {
		os.Exit(2)
	}
	if err := run(*goCommand); err != nil {
		fmt.Fprintf(os.Stderr, "windowscoverage: %v\n", err)
		os.Exit(1)
	}
	fmt.Println("windowscoverage: every package is covered or explicitly skip-listed")
}

func run(goCommand string) error {
	root, err := repositoryRoot()
	if err != nil {
		return err
	}

	all, err := listPackages(goCommand, root, "windows", "./...")
	if err != nil {
		return fmt.Errorf("list windows/amd64 packages: %w", err)
	}
	covered, err := coveredPackages(goCommand, root)
	if err != nil {
		return err
	}
	skipped, _, err := readSkipInventory(filepath.Join(root, skipInventoryPath))
	if err != nil {
		return err
	}

	var problems []string

	for pkg := range covered {
		if _, ok := skipped[pkg]; ok {
			problems = append(problems, fmt.Sprintf("%s is both a windows-smoke go-test target and a skip-inventory entry — remove the stale skip-inventory line", pkg))
		}
	}
	for pkg := range skipped {
		if !all[pkg] {
			problems = append(problems, fmt.Sprintf("stale skip-inventory entry: %s no longer exists as a windows/amd64 package — remove it from %s", pkg, skipInventoryPath))
		}
	}
	for pkg := range all {
		if covered[pkg] || skipped[pkg] {
			continue
		}
		problems = append(problems, fmt.Sprintf(
			"undocumented Windows coverage gap: %s is not exercised by any go-test step in the %q job (%s) and has no skip-inventory entry — either add it to a windows-smoke test step or record it in %s with a reason",
			pkg, windowsSmokeJob, ciWorkflowPath, skipInventoryPath,
		))
	}

	if len(problems) == 0 {
		return nil
	}
	sort.Strings(problems)
	return fmt.Errorf("%d problem(s):\n  - %s", len(problems), strings.Join(problems, "\n  - "))
}

// ciWorkflow is the minimal shape of ci.yml this tool needs: enough to reach
// a named job's steps and their shell commands.
type ciWorkflow struct {
	Jobs map[string]ciJob `json:"jobs"`
}

type ciJob struct {
	Steps []ciStep `json:"steps"`
}

type ciStep struct {
	Run string `json:"run"`
}

// goTestCommandRE finds each `go test` invocation's trailing arguments on its
// own line (ci.yml's windows-smoke steps are single-line `run:` commands, one
// `go test` per step — this is intentionally narrow rather than a general
// shell parser).
var goTestCommandRE = regexp.MustCompile(`(?m)^\s*go test\b(.*)$`)

// packageTokenRE matches a `./`-relative package pattern token.
var packageTokenRE = regexp.MustCompile(`\./\S+`)

// coveredPackages parses ci.yml's windows-smoke job and resolves every
// package pattern named in a `go test` step (expanding `...` wildcards via
// `go list`) into canonical import paths.
func coveredPackages(goCommand, root string) (map[string]bool, error) {
	data, err := os.ReadFile(filepath.Join(root, ciWorkflowPath))
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", ciWorkflowPath, err)
	}
	var wf ciWorkflow
	if err := yaml.Unmarshal(data, &wf); err != nil {
		return nil, fmt.Errorf("parse %s: %w", ciWorkflowPath, err)
	}
	job, ok := wf.Jobs[windowsSmokeJob]
	if !ok {
		return nil, fmt.Errorf("%s has no %q job", ciWorkflowPath, windowsSmokeJob)
	}

	var patterns []string
	for _, step := range job.Steps {
		for _, match := range goTestCommandRE.FindAllStringSubmatch(step.Run, -1) {
			for _, token := range packageTokenRE.FindAllString(match[1], -1) {
				patterns = append(patterns, strings.Trim(token, `'"`))
			}
		}
	}
	if len(patterns) == 0 {
		return nil, fmt.Errorf("found no `go test ./...`-style package arguments in the %q job — the parser or the job's step shape has drifted", windowsSmokeJob)
	}

	covered := map[string]bool{}
	for _, pattern := range patterns {
		pkgs, err := listPackages(goCommand, root, "windows", pattern)
		if err != nil {
			return nil, fmt.Errorf("resolve covered pattern %q: %w", pattern, err)
		}
		for pkg := range pkgs {
			covered[pkg] = true
		}
	}
	return covered, nil
}

// listPackages runs `go list <pattern>` for goos and returns the resulting
// canonical import paths.
func listPackages(goCommand, root, goos, pattern string) (map[string]bool, error) {
	cmd := exec.Command(goCommand, "list", pattern)
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "GOOS="+goos)
	out, err := cmd.Output()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return nil, fmt.Errorf("go list %s: %s", pattern, strings.TrimSpace(string(exitErr.Stderr)))
		}
		return nil, fmt.Errorf("go list %s: %w", pattern, err)
	}
	pkgs := map[string]bool{}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			pkgs[line] = true
		}
	}
	return pkgs, nil
}

// readSkipInventory parses skip-inventory.txt: one `<import-path> # <reason>`
// entry per line, blank lines and `#`-only comment lines ignored. A missing
// file is fine (an empty inventory) so a brand-new checkout with nothing
// skipped yet doesn't need a placeholder file.
func readSkipInventory(path string) (pkgs map[string]bool, reasons map[string]string, err error) {
	pkgs, reasons = map[string]bool{}, map[string]string{}
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return pkgs, reasons, nil
		}
		return nil, nil, fmt.Errorf("open %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()

	scanner := bufio.NewScanner(f)
	lineNum := 0
	for scanner.Scan() {
		lineNum++
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		pkg, reason, ok := strings.Cut(line, " # ")
		if !ok {
			return nil, nil, fmt.Errorf("%s:%d: want `<import-path> # <reason>`, got %q", path, lineNum, line)
		}
		pkg, reason = strings.TrimSpace(pkg), strings.TrimSpace(reason)
		if pkg == "" || reason == "" {
			return nil, nil, fmt.Errorf("%s:%d: both the package and the reason are required, got %q", path, lineNum, line)
		}
		if pkgs[pkg] {
			return nil, nil, fmt.Errorf("%s:%d: duplicate entry for %s", path, lineNum, pkg)
		}
		pkgs[pkg] = true
		reasons[pkg] = reason
	}
	if err := scanner.Err(); err != nil {
		return nil, nil, fmt.Errorf("scan %s: %w", path, err)
	}
	return pkgs, reasons, nil
}

func repositoryRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("resolve current directory: %w", err)
	}
	return repositoryRootFrom(dir)
}

func repositoryRootFrom(dir string) (string, error) {
	for {
		data, readErr := os.ReadFile(filepath.Join(dir, "go.mod"))
		if readErr == nil && strings.Contains(string(data), "module github.com/goobers/goobers") {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("repository root not found from %s", dir)
		}
		dir = parent
	}
}
