// Command stackparity keeps the shipped stack examples honest about themselves.
//
// It enforces two properties that were both, separately, silently violable:
//
//   - #2554: a non-Go example gaggle must not carry Go-specific command
//     residue. Copying the Go reference workflow into a Node or .NET gaggle
//     leaves `["make", "ci"]` in its `local-ci` stage, and the gaggle's own
//     `ciCommand` corrects it INVISIBLY at config-load time. The example then
//     reads as a Go project to every operator who opens it, while running
//     something else — the one thing a reference example may not do.
//
//   - #2555: the stack-support tier table is hand-maintained prose, so a row
//     can claim a stack is proven green in CI when nothing in CI executes it.
//     Every row's claim is checked against the shipped reference gaggle it
//     names and against whether .github/workflows/ci.yml actually enables that
//     stack's end-to-end leg.
//
// Run it with `go run ./test/stackparity`; it is a `make ci` check.
package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	yaml "gopkg.in/yaml.v3"
)

func main() {
	findings, err := verify(".")
	if err != nil {
		fmt.Fprintf(os.Stderr, "stackparity: %v\n", err)
		os.Exit(1)
	}
	if len(findings) > 0 {
		for _, finding := range findings {
			fmt.Fprintf(os.Stderr, "stackparity: %s\n", finding)
		}
		os.Exit(1)
	}
}

func verify(root string) ([]string, error) {
	residue, err := verifyExampleCommands(root)
	if err != nil {
		return nil, err
	}
	tiers, err := verifyTierTable(root)
	if err != nil {
		return nil, err
	}
	return append(residue, tiers...), nil
}

// ---------------------------------------------------------------------------
// #2554: Go-specific command residue in non-Go examples
// ---------------------------------------------------------------------------

const gagglesDir = "config-examples/gaggles"

// localCIStage is the stage name a gaggle's ciCommand is resolved into
// (internal/instance.LocalCIStageName). Duplicated as a literal rather than
// imported so this gate reads the shipped YAML the way an operator does.
const localCIStage = "local-ci"

// goFallbackMarker is the documented escape hatch. A command line preceded by
// a comment containing this marker is allowed to carry a Go literal in a
// non-Go example, because the example is deliberately showing the fallback.
// The marker must carry a reason after it, so the exemption explains itself.
const goFallbackMarker = "stackparity:go-fallback"

// goCommandHeads are executables that only exist to drive a Go toolchain.
var goCommandHeads = map[string]bool{
	"go":            true,
	"gofmt":         true,
	"goimports":     true,
	"golangci-lint": true,
	"staticcheck":   true,
}

type exampleGaggle struct {
	name      string
	dir       string
	ciCommand []string
}

func verifyExampleCommands(root string) ([]string, error) {
	gaggles, err := loadExampleGaggles(root)
	if err != nil {
		return nil, err
	}
	var findings []string
	for _, gaggle := range gaggles {
		if len(gaggle.ciCommand) == 0 {
			// No declared ciCommand: the workflow's own literal IS the
			// effective command, so nothing can be silently overridden.
			continue
		}
		commands, err := workflowCommands(filepath.Join(gaggle.dir, "workflows"))
		if err != nil {
			return nil, err
		}
		for _, cmd := range commands {
			findings = append(findings, exampleCommandFindings(gaggle, cmd)...)
		}
	}
	sort.Strings(findings)
	return findings, nil
}

func exampleCommandFindings(gaggle exampleGaggle, cmd declaredCommand) []string {
	if cmd.exempt {
		return nil
	}
	var findings []string
	// The local-ci stage is the one the gaggle's ciCommand replaces, so a
	// disagreement there is exactly the invisible override.
	if cmd.stage == localCIStage && !equalArgv(cmd.argv, gaggle.ciCommand) {
		findings = append(findings, fmt.Sprintf(
			"%s:%d: gaggle %q declares ciCommand %s but its %s stage declares %s; "+
				"the declared command is resolved away at config-load time, so the example reads as a stack it does not run",
			cmd.file, cmd.line, gaggle.name, formatArgv(gaggle.ciCommand), localCIStage, formatArgv(cmd.argv)))
	}
	if isGoCommand(gaggle.ciCommand) {
		return findings
	}
	if literal, ok := goLiteral(cmd.argv); ok {
		findings = append(findings, fmt.Sprintf(
			"%s:%d: gaggle %q is a non-Go stack (ciCommand %s) but stage %q runs the Go-specific command %q; "+
				"replace it with the stack-native command, or mark the line `# %s <reason>` if the example is deliberately showing the Go fallback",
			cmd.file, cmd.line, gaggle.name, formatArgv(gaggle.ciCommand), cmd.stage, literal, goFallbackMarker))
	}
	return findings
}

// goLiteral reports the offending literal in argv, if any. `make` counts: it
// is the Go reference workflow's own default, so a `make` invocation inside a
// gaggle that declares an npm/dotnet/maven/pytest ciCommand is residue from
// copying that reference, not a project that genuinely drives make.
func goLiteral(argv []string) (string, bool) {
	if len(argv) == 0 {
		return "", false
	}
	if goCommandHeads[argv[0]] {
		return strings.Join(argv[:min(2, len(argv))], " "), true
	}
	if argv[0] == "make" {
		return strings.Join(argv[:min(2, len(argv))], " "), true
	}
	return "", false
}

func isGoCommand(argv []string) bool {
	_, ok := goLiteral(argv)
	return ok
}

func loadExampleGaggles(root string) ([]exampleGaggle, error) {
	base := filepath.Join(root, filepath.FromSlash(gagglesDir))
	entries, err := os.ReadDir(base)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", gagglesDir, err)
	}
	var gaggles []exampleGaggle
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		path := filepath.Join(base, entry.Name(), "gaggle.yaml")
		raw, err := os.ReadFile(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		var doc struct {
			Metadata struct {
				Name string `yaml:"name"`
			} `yaml:"metadata"`
			Spec struct {
				CICommand []string `yaml:"ciCommand"`
			} `yaml:"spec"`
		}
		if err := yaml.Unmarshal(raw, &doc); err != nil {
			return nil, fmt.Errorf("parse %s: %w", path, err)
		}
		name := doc.Metadata.Name
		if name == "" {
			name = entry.Name()
		}
		gaggles = append(gaggles, exampleGaggle{
			name:      name,
			dir:       filepath.Join(base, entry.Name()),
			ciCommand: doc.Spec.CICommand,
		})
	}
	return gaggles, nil
}

type declaredCommand struct {
	file  string
	line  int
	stage string
	argv  []string
	// exempt records that the line carries the documented-fallback marker.
	exempt bool
}

func workflowCommands(dir string) ([]declaredCommand, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var commands []declaredCommand
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".yaml" {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		var doc yaml.Node
		if err := yaml.Unmarshal(raw, &doc); err != nil {
			return nil, fmt.Errorf("parse %s: %w", path, err)
		}
		found := collectCommands(&doc, "")
		exempted := exemptedLines(raw)
		for i := range found {
			found[i].file = filepath.ToSlash(path)
			found[i].exempt = exempted[found[i].line]
		}
		commands = append(commands, found...)
	}
	return commands, nil
}

// exemptedLines maps the line number of each command a `# stackparity:go-fallback
// <reason>` comment exempts. The marker applies to the next non-blank,
// non-comment line, which is the idiom a reader expects from a lint pragma.
func exemptedLines(raw []byte) map[int]bool {
	exempt := map[int]bool{}
	lines := strings.Split(string(raw), "\n")
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "#") || !strings.Contains(trimmed, goFallbackMarker) {
			continue
		}
		if strings.TrimSpace(strings.SplitN(trimmed, goFallbackMarker, 2)[1]) == "" {
			// A marker with no reason exempts nothing: the exemption has to
			// say why, or it is just a way to switch the gate off.
			continue
		}
		for j := i + 1; j < len(lines); j++ {
			next := strings.TrimSpace(lines[j])
			if next == "" || strings.HasPrefix(next, "#") {
				continue
			}
			exempt[j+1] = true
			break
		}
	}
	return exempt
}

// collectCommands walks a workflow document for `command:` sequences, carrying
// the enclosing task's `name:` so a finding can say which stage it is about.
func collectCommands(node *yaml.Node, stage string) []declaredCommand {
	var out []declaredCommand
	switch node.Kind {
	case yaml.DocumentNode, yaml.SequenceNode:
		for _, child := range node.Content {
			out = append(out, collectCommands(child, stage)...)
		}
	case yaml.MappingNode:
		nested := stage
		for i := 0; i+1 < len(node.Content); i += 2 {
			key, value := node.Content[i], node.Content[i+1]
			if key.Value == "name" && value.Kind == yaml.ScalarNode {
				nested = value.Value
			}
		}
		for i := 0; i+1 < len(node.Content); i += 2 {
			key, value := node.Content[i], node.Content[i+1]
			if key.Value == "command" && value.Kind == yaml.SequenceNode {
				argv := make([]string, 0, len(value.Content))
				for _, item := range value.Content {
					if item.Kind != yaml.ScalarNode {
						argv = nil
						break
					}
					argv = append(argv, item.Value)
				}
				if len(argv) > 0 {
					out = append(out, declaredCommand{line: key.Line, stage: nested, argv: argv})
				}
				continue
			}
			out = append(out, collectCommands(value, nested)...)
		}
	}
	return out
}

func equalArgv(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}

func formatArgv(argv []string) string {
	quoted := make([]string, 0, len(argv))
	for _, item := range argv {
		quoted = append(quoted, fmt.Sprintf("%q", item))
	}
	return "[" + strings.Join(quoted, ", ") + "]"
}

// ---------------------------------------------------------------------------
// #2555: stack-support tier table parity
// ---------------------------------------------------------------------------

const (
	tierTableDoc = "docs/guides/stack-support.md"
	ciWorkflow   = ".github/workflows/ci.yml"
)

// Status vocabulary. The distinction the tier table could not previously make
// is the whole point: a shipped reference gaggle and an end-to-end leg that CI
// actually runs are different claims.
const (
	statusCIGreen    = "Shipped, CI-green"
	statusLocalGreen = "Shipped, validated locally"
	statusStructural = "Shipped reference, structural checks only"
)

// stackEvidence is what each tier-table row's claim is checked against. It is
// checked-in data on purpose: the point of the gate is that changing the claim
// and changing the evidence must happen in one change.
type stackEvidence struct {
	// stack is the tier table's first column, verbatim.
	stack string
	// status is the tier table's last column, verbatim.
	status string
	// reference is the repo-relative reference gaggle directory the row names.
	reference string
	// e2eTest is the integration test file that executes the reference gaggle
	// end to end. Empty for a row that claims no executing leg.
	e2eTest string
	// e2eEnv is the opt-in environment variable that enables e2eTest. A
	// CI-green row requires ci.yml to set it; a locally-validated row requires
	// ci.yml NOT to set it, so a row cannot quietly become stronger or weaker
	// than its label.
	e2eEnv string
	// repoSuite marks the stack whose end-to-end leg IS this repository's own
	// test suite, so it has no separate opt-in gate to look for.
	repoSuite bool
}

var stackEvidenceTable = []stackEvidence{
	{
		stack:     "Go",
		status:    statusCIGreen,
		reference: "reference-workflows/gaggles/goobers",
		repoSuite: true,
	},
	{
		stack:     "Java",
		status:    statusCIGreen,
		reference: "config-examples/gaggles/java-service",
		e2eTest:   "test/e2e/java_gaggle_integration_test.go",
		e2eEnv:    "GOOBERS_JAVA_E2E",
	},
	{
		stack:     ".NET/C#",
		status:    statusLocalGreen,
		reference: "config-examples/gaggles/dotnet-service",
		e2eTest:   "test/e2e/dotnet_gaggle_integration_test.go",
		e2eEnv:    "GOOBERS_DOTNET_E2E",
	},
	{
		stack:     "Python",
		status:    statusLocalGreen,
		reference: "config-examples/gaggles/python-service",
		e2eTest:   "test/e2e/python_gaggle_integration_test.go",
		e2eEnv:    "GOOBERS_PYTHON_E2E",
	},
	{
		stack:     "Node/TypeScript",
		status:    statusStructural,
		reference: "config-examples/gaggles/acme-web",
	},
}

// tierRow is one parsed row of the published table.
type tierRow struct {
	stack     string
	reference string
	status    string
	line      int
}

var referenceCell = regexp.MustCompile("`([^`]+)`")

func verifyTierTable(root string) ([]string, error) {
	rows, err := parseTierTable(filepath.Join(root, filepath.FromSlash(tierTableDoc)))
	if err != nil {
		return nil, err
	}
	ci, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(ciWorkflow)))
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", ciWorkflow, err)
	}

	byStack := map[string]tierRow{}
	for _, row := range rows {
		byStack[row.stack] = row
	}
	var findings []string
	for _, want := range stackEvidenceTable {
		row, ok := byStack[want.stack]
		if !ok {
			findings = append(findings, fmt.Sprintf(
				"%s: no tier-table row for %q, which this gate has evidence for; add the row or drop the evidence entry",
				tierTableDoc, want.stack))
			continue
		}
		delete(byStack, want.stack)
		findings = append(findings, evidenceFindings(root, want, row, string(ci))...)
	}
	// The reverse direction: a row this gate has no evidence for is a claim
	// nothing checks, which is the state #2555 was filed about.
	for stack, row := range byStack {
		if !claimsShipped(row.status) {
			continue
		}
		findings = append(findings, fmt.Sprintf(
			"%s:%d: tier-table row %q claims %q with no evidence entry in test/stackparity; add one so the claim is checked",
			tierTableDoc, row.line, stack, row.status))
	}
	sort.Strings(findings)
	return findings, nil
}

func claimsShipped(status string) bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(status)), "shipped")
}

func evidenceFindings(root string, want stackEvidence, row tierRow, ci string) []string {
	var findings []string
	if row.status != want.status {
		findings = append(findings, fmt.Sprintf(
			"%s:%d: tier-table row %q says %q but its evidence supports %q",
			tierTableDoc, row.line, want.stack, row.status, want.status))
	}
	if row.reference != want.reference {
		findings = append(findings, fmt.Sprintf(
			"%s:%d: tier-table row %q names reference %q but its evidence names %q",
			tierTableDoc, row.line, want.stack, row.reference, want.reference))
	}
	if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(want.reference), "gaggle.yaml")); err != nil {
		findings = append(findings, fmt.Sprintf(
			"%s: %q names reference gaggle %s, which has no gaggle.yaml",
			tierTableDoc, want.stack, want.reference))
	}
	if want.e2eTest != "" {
		if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(want.e2eTest))); err != nil {
			findings = append(findings, fmt.Sprintf(
				"%s: %q claims an end-to-end leg at %s, which does not exist",
				tierTableDoc, want.stack, want.e2eTest))
		}
	}
	switch want.status {
	case statusCIGreen:
		if want.repoSuite {
			break
		}
		if want.e2eTest == "" || want.e2eEnv == "" {
			findings = append(findings, fmt.Sprintf(
				"%s: %q claims %q but names no end-to-end leg to run",
				tierTableDoc, want.stack, statusCIGreen))
			break
		}
		if !enablesEnv(ci, want.e2eEnv) {
			findings = append(findings, fmt.Sprintf(
				"%s: %q claims %q but %s never sets %s, so nothing in CI executes %s",
				tierTableDoc, want.stack, statusCIGreen, ciWorkflow, want.e2eEnv, want.e2eTest))
		}
	case statusLocalGreen:
		if want.e2eEnv == "" {
			findings = append(findings, fmt.Sprintf(
				"%s: %q claims %q but names no opt-in gate for its end-to-end leg",
				tierTableDoc, want.stack, statusLocalGreen))
			break
		}
		if enablesEnv(ci, want.e2eEnv) {
			findings = append(findings, fmt.Sprintf(
				"%s: %q is labelled %q but %s now sets %s — the leg runs in CI, so promote the row to %q",
				tierTableDoc, want.stack, statusLocalGreen, ciWorkflow, want.e2eEnv, statusCIGreen))
		}
	case statusStructural:
		if want.e2eTest != "" {
			findings = append(findings, fmt.Sprintf(
				"%s: %q is labelled %q but names the end-to-end leg %s; relabel the row",
				tierTableDoc, want.stack, statusStructural, want.e2eTest))
		}
	default:
		findings = append(findings, fmt.Sprintf(
			"test/stackparity: evidence for %q uses unknown status %q", want.stack, want.status))
	}
	return findings
}

// enablesEnv reports whether the CI workflow sets name to a truthy value. It
// looks for the `NAME: "1"`-style assignment the Java leg uses; a commented-out
// line does not count, which is exactly the drift this catches.
func enablesEnv(ci, name string) bool {
	for _, line := range strings.Split(ci, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "#") {
			continue
		}
		key, value, ok := strings.Cut(trimmed, ":")
		if !ok || strings.TrimSpace(key) != name {
			continue
		}
		value = strings.Trim(strings.TrimSpace(value), `"'`)
		if value != "" && value != "0" && !strings.EqualFold(value, "false") {
			return true
		}
	}
	return false
}

func parseTierTable(path string) ([]tierRow, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", tierTableDoc, err)
	}
	var rows []tierRow
	inTable := false
	for i, line := range strings.Split(string(raw), "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "|") {
			inTable = false
			continue
		}
		cells := splitTableRow(trimmed)
		if len(cells) != 4 {
			continue
		}
		if strings.EqualFold(cells[0], "Stack") {
			inTable = true
			continue
		}
		if !inTable || strings.HasPrefix(cells[0], "---") {
			continue
		}
		reference := ""
		if match := referenceCell.FindStringSubmatch(cells[2]); match != nil {
			reference = strings.TrimSuffix(match[1], "/")
		}
		rows = append(rows, tierRow{
			stack:     cells[0],
			reference: reference,
			status:    cells[3],
			line:      i + 1,
		})
	}
	if len(rows) == 0 {
		return nil, fmt.Errorf("%s: no tier table found", tierTableDoc)
	}
	return rows, nil
}

func splitTableRow(line string) []string {
	parts := strings.Split(strings.Trim(line, "|"), "|")
	cells := make([]string, 0, len(parts))
	for _, part := range parts {
		cells = append(cells, strings.TrimSpace(part))
	}
	return cells
}
