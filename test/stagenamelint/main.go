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
	Line   int
	Value  string
	Reason string
}

var exceptions = []exception{
	// Config scaffolding and command/stage registries define these names rather
	// than branching stage behavior on them.
	{Path: "internal/instance/guided.go", Line: 28, Value: "implementation", Reason: "guided-init workflow definition"},
	{Path: "internal/instance/guided.go", Line: 30, Value: "backlog-curation", Reason: "guided-init workflow definition"},
	{Path: "internal/instance/guided.go", Line: 32, Value: "work-nomination", Reason: "guided-init workflow definition"},
	{Path: "internal/instance/instance.go", Line: 26, Value: "docs-updater", Reason: "canonical scaffold directory name"},
	{Path: "internal/providerstage/manifest.go", Line: 180, Value: "post-merge", Reason: "stage command registry key"},
	{Path: "cmd/goobers/clisynopsis.go", Line: 32, Value: "self-update", Reason: "CLI command registry key"},
	{Path: "cmd/goobers/runtime_capabilities.go", Line: 271, Value: "self-update", Reason: "CLI command registry key"},
	{Path: "cmd/goobers/runtime_capabilities.go", Line: 272, Value: "self-update", Reason: "CLI command registry lookup"},
	{Path: "cmd/goobers/selfupdate.go", Line: 27, Value: "self-update", Reason: "CLI flag-set name"},
	{Path: "cmd/goobers/selfupdate.go", Line: 34, Value: "self-update", Reason: "CLI help lookup"},
	{Path: "cmd/goobers/completionmodel.go", Line: 175, Value: "self-update", Reason: "CLI completion flag-spec registry key"},
	{Path: "cmd/goobers/tutorprpolicy.go", Line: 76, Value: "tutor", Reason: "legacy tutor-name compatibility; TutorScope is preferred"},

	// Existing telemetry projections still infer roles from shipped names.
	// #2494 tracks replacing these with a config-sourced workflow role marker.
	{Path: "internal/telemetry/rollup/aggregates.go", Line: 419, Value: "backlog-curation", Reason: "#2494"},
	{Path: "internal/telemetry/rollup/curation.go", Line: 172, Value: "backlog-curation", Reason: "#2494"},
	{Path: "internal/telemetry/rollup/ingest.go", Line: 539, Value: "backlog-curation", Reason: "#2494"},
	{Path: "internal/telemetry/rollup/ingest.go", Line: 550, Value: "implementation", Reason: "#2494"},

	// These are canonical stage-owned status labels, not config-facing routing
	// labels. Configurable approval/readiness labels must come from stage inputs.
	{Path: "cmd/goobers/applyverdict.go", Line: 28, Value: "goobers:blocked-on-sibling", Reason: "canonical verdict label"},
	{Path: "cmd/goobers/applyverdict.go", Line: 64, Value: "goobers:merge-ready", Reason: "canonical verdict label"},
	{Path: "cmd/goobers/applyverdict.go", Line: 66, Value: "goobers:merge-escalated", Reason: "canonical verdict label"},
	{Path: "cmd/goobers/applyverdict.go", Line: 71, Value: "goobers:needs-remediation", Reason: "canonical verdict label"},
	{Path: "cmd/goobers/mergedemotion.go", Line: 33, Value: "goobers:merge-demoted", Reason: "canonical merge-review label"},
	{Path: "cmd/goobers/postmerge.go", Line: 69, Value: "goobers:needs-remediation", Reason: "canonical verdict label"},
	{Path: "cmd/goobers/prselect.go", Line: 33, Value: "goobers:merge-ready,goobers:needs-remediation", Reason: "canonical merge-review labels"},
	{Path: "cmd/goobers/prselect.go", Line: 34, Value: "goobers:no-merge-review", Reason: "canonical merge-review label"},
	{Path: "cmd/goobers/remediationcheckpoint.go", Line: 25, Value: "goobers:merge-escalated", Reason: "canonical verdict label"},
	{Path: "cmd/goobers/runabortlabel.go", Line: 32, Value: "goobers:run-aborted", Reason: "canonical run lifecycle label"},
	{Path: "cmd/goobers/scopedrift.go", Line: 17, Value: "goobers:scope-drift", Reason: "canonical scope-review label"},
	{Path: "cmd/goobers/scopegate1313.go", Line: 13, Value: "goobers:scope-gate", Reason: "canonical scope-review label"},
	{Path: "cmd/goobers/scopegate1313.go", Line: 20, Value: "goobers:scope-gate-ack", Reason: "canonical scope-review label"},
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
	workflowNames, err := loadWorkflowNames(filepath.Join(root, "reference-workflows", "gaggles", "goobers", "workflows"))
	if err != nil {
		return nil, err
	}
	allowed := make(map[string]exception, len(exceptions))
	for _, current := range exceptions {
		allowed[exceptionKey(current.Path, current.Line, current.Value)] = current
	}

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
	sort.Slice(violations, func(i, j int) bool {
		if violations[i].Path == violations[j].Path {
			return violations[i].Line < violations[j].Line
		}
		return violations[i].Path < violations[j].Path
	})
	return violations, nil
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

func checkGoFile(path, relative string, workflowNames map[string]bool, allowed map[string]exception) ([]violation, error) {
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
		line := files.Position(literal.Pos()).Line
		if _, ok := allowed[exceptionKey(relative, line, value)]; ok {
			return true
		}
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

func exceptionKey(path string, line int, value string) string {
	return fmt.Sprintf("%s:%d:%s", path, line, value)
}
