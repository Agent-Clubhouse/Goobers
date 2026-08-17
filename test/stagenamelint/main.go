// Command stagenamelint rejects config-facing workflow and label literals in stage code.
package main

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"sigs.k8s.io/yaml"
)

type violation struct {
	Path    string
	Line    int
	Message string
}

type exception struct {
	Path   string
	Value  string
	Reason string
}

var exceptions = []exception{
	// Config scaffolding and command/stage registries define these names rather
	// than branching stage behavior on them.
	{Path: "internal/instance/guided.go", Value: "implementation", Reason: "guided-init workflow definition"},
	{Path: "internal/instance/guided.go", Value: "backlog-curation", Reason: "guided-init workflow definition"},
	{Path: "internal/instance/guided.go", Value: "work-nomination", Reason: "guided-init workflow definition"},
	{Path: "internal/instance/instance.go", Value: "docs-updater", Reason: "canonical scaffold directory name"},
	{Path: "cmd/goobers/clisynopsis.go", Value: "self-update", Reason: "CLI command registry key"},
	{Path: "cmd/goobers/runtime_capabilities.go", Value: "self-update", Reason: "CLI command registry key and lookup"},
	{Path: "cmd/goobers/selfupdate.go", Value: "self-update", Reason: "CLI flag-set name and help lookup"},
	{Path: "cmd/goobers/completionmodel.go", Value: "self-update", Reason: "CLI completion flag-spec registry key"},
	{Path: "cmd/goobers/tutorprpolicy.go", Value: "tutor", Reason: "legacy tutor-name compatibility; TutorScope is preferred"},

	// Existing telemetry projections still infer roles from shipped names.
	// #2494 tracks replacing these with a config-sourced workflow role marker.
	{Path: "internal/telemetry/rollup/aggregates.go", Value: "backlog-curation", Reason: "#2494"},
	{Path: "internal/telemetry/rollup/curation.go", Value: "backlog-curation", Reason: "#2494"},
	{Path: "internal/telemetry/rollup/ingest.go", Value: "backlog-curation", Reason: "#2494"},
	{Path: "internal/telemetry/rollup/ingest.go", Value: "implementation", Reason: "#2494"},

	// These are canonical stage-owned status labels, not config-facing routing
	// labels. Configurable approval/readiness labels must come from stage inputs.
	{Path: "cmd/goobers/applyverdict.go", Value: "goobers:blocked-on-sibling", Reason: "canonical verdict label"},
	{Path: "cmd/goobers/applyverdict.go", Value: "goobers:merge-ready", Reason: "canonical verdict label"},
	{Path: "cmd/goobers/applyverdict.go", Value: "goobers:merge-escalated", Reason: "canonical verdict label"},
	{Path: "cmd/goobers/applyverdict.go", Value: "goobers:needs-remediation", Reason: "canonical verdict label"},
	{Path: "cmd/goobers/mergedemotion.go", Value: "goobers:merge-demoted", Reason: "canonical merge-review label"},
	{Path: "cmd/goobers/postmerge.go", Value: "goobers:needs-remediation", Reason: "canonical verdict label"},
	{Path: "cmd/goobers/prselect.go", Value: "goobers:merge-ready,goobers:needs-remediation", Reason: "canonical merge-review labels"},
	{Path: "cmd/goobers/prselect.go", Value: "goobers:no-merge-review", Reason: "canonical merge-review label"},
	{Path: "cmd/goobers/remediationcheckpoint.go", Value: "goobers:merge-escalated", Reason: "canonical verdict label"},
	{Path: "cmd/goobers/runabortlabel.go", Value: "goobers:run-aborted", Reason: "canonical run lifecycle label"},
	{Path: "cmd/goobers/scopedrift.go", Value: "goobers:scope-drift", Reason: "canonical scope-review label"},
	{Path: "cmd/goobers/scopegate1313.go", Value: "goobers:scope-gate", Reason: "canonical scope-review label"},
	{Path: "cmd/goobers/scopegate1313.go", Value: "goobers:scope-gate-ack", Reason: "canonical scope-review label"},
}

var excludedPaths = map[string]string{
	"internal/apicontract/wire.go": "generated portal wire fixtures, not stage behavior",
}

func main() {
	violations, err := checkRepository(".")
	if err != nil {
		fmt.Fprintln(os.Stderr, "stage-name lint:", err)
		os.Exit(1)
	}
	if len(violations) == 0 {
		return
	}
	for _, current := range violations {
		fmt.Fprintf(os.Stderr, "%s:%d: %s\n", current.Path, current.Line, current.Message)
	}
	os.Exit(1)
}

func checkRepository(root string) ([]violation, error) {
	return checkRepositoryWithExceptions(root, exceptions)
}

func checkRepositoryWithExceptions(root string, configured []exception) ([]violation, error) {
	workflowNames, err := loadWorkflowNames(filepath.Join(root, "reference-workflows", "gaggles", "goobers", "workflows"))
	if err != nil {
		return nil, err
	}
	allowed := newExceptionSet(configured)

	var violations []violation
	for _, directory := range []string{"cmd/goobers", "internal"} {
		base := filepath.Join(root, filepath.FromSlash(directory))
		err := filepath.WalkDir(base, func(path string, entry os.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if entry.IsDir() {
				return nil
			}
			if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			relative, err := filepath.Rel(root, path)
			if err != nil {
				return err
			}
			found, err := checkGoFile(path, filepath.ToSlash(relative), workflowNames, allowed)
			if err != nil {
				return err
			}
			violations = append(violations, found...)
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	violations = append(violations, allowed.staleViolations()...)
	sort.Slice(violations, func(i, j int) bool {
		if violations[i].Path == violations[j].Path {
			return violations[i].Line < violations[j].Line
		}
		return violations[i].Path < violations[j].Path
	})
	return violations, nil
}

type exceptionSet struct {
	configured []exception
	byLiteral  map[string]bool
	matched    map[string]bool
}

func newExceptionSet(configured []exception) *exceptionSet {
	set := &exceptionSet{
		configured: configured,
		byLiteral:  make(map[string]bool, len(configured)),
		matched:    make(map[string]bool, len(configured)),
	}
	for _, current := range configured {
		set.byLiteral[exceptionKey(current.Path, current.Value)] = true
	}
	return set
}

func (s *exceptionSet) allow(path, value string) bool {
	key := exceptionKey(path, value)
	if s.byLiteral[key] {
		s.matched[key] = true
		return true
	}
	return false
}

func (s *exceptionSet) staleViolations() []violation {
	var violations []violation
	for _, current := range s.configured {
		if s.matched[exceptionKey(current.Path, current.Value)] {
			continue
		}
		violations = append(violations, violation{
			Path:    current.Path,
			Message: fmt.Sprintf("stale exception for literal %q (%s)", current.Value, current.Reason),
		})
	}
	return violations
}

func loadWorkflowNames(directory string) (map[string]bool, error) {
	entries, err := os.ReadDir(directory)
	if err != nil {
		return nil, fmt.Errorf("read shipped workflows: %w", err)
	}
	names := make(map[string]bool)
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".yaml") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(directory, entry.Name()))
		if err != nil {
			return nil, err
		}
		var document struct {
			Metadata struct {
				Name string `yaml:"name"`
			} `yaml:"metadata"`
		}
		if err := yaml.Unmarshal(data, &document); err != nil {
			return nil, fmt.Errorf("decode shipped workflow %s: %w", entry.Name(), err)
		}
		if document.Metadata.Name == "" {
			return nil, fmt.Errorf("shipped workflow %s has no metadata.name", entry.Name())
		}
		names[document.Metadata.Name] = true
	}
	return names, nil
}

func checkGoFile(path, relative string, workflowNames map[string]bool, allowed *exceptionSet) ([]violation, error) {
	if _, excluded := excludedPaths[relative]; excluded {
		return nil, nil
	}
	files := token.NewFileSet()
	parsed, err := parser.ParseFile(files, path, nil, 0)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", relative, err)
	}
	var violations []violation
	ast.Inspect(parsed, func(node ast.Node) bool {
		literal, ok := node.(*ast.BasicLit)
		if !ok || literal.Kind != token.STRING {
			return true
		}
		value, err := strconv.Unquote(literal.Value)
		if err != nil {
			return true
		}
		isWorkflow := workflowNames[value]
		isLabel := isGoobersLabelList(value)
		if !isWorkflow && !isLabel {
			return true
		}
		if allowed.allow(relative, value) {
			return true
		}
		line := files.Position(literal.Pos()).Line
		alternative := "use a config-sourced workflow role marker"
		if isLabel {
			alternative = "use the stage's label input or a providers label constant sourced through config"
		}
		violations = append(violations, violation{
			Path:    relative,
			Line:    line,
			Message: fmt.Sprintf("literal %q matches a shipped config name; %s", value, alternative),
		})
		return true
	})
	return violations, nil
}

func isGoobersLabelList(value string) bool {
	parts := strings.Split(value, ",")
	for _, part := range parts {
		if !isGoobersLabel(strings.TrimSpace(part)) {
			return false
		}
	}
	return true
}

func isGoobersLabel(value string) bool {
	suffix := strings.TrimPrefix(value, "goobers:")
	if suffix == value || suffix == "" {
		return false
	}
	for _, current := range suffix {
		if current < 'a' || current > 'z' {
			if current < '0' || current > '9' {
				if current != '-' {
					return false
				}
			}
		}
	}
	return true
}

func exceptionKey(path, value string) string {
	return path + "\x00" + value
}
