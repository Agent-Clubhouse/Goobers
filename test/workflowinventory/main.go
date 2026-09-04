// Command workflowinventory checks the rendered workflow inventory in
// docs/reference/workflow-inventory.md against the workflow files themselves.
//
// Four workflows in .github/workflows/ had never run a single time (#4224):
// their `schedule:` blocks sit commented out pending named blocking issues, and
// nothing distinguished them from gates enforced on every push. This command
// classifies each workflow from its own triggers and fails when the inventory
// disagrees — so a newly added dormant workflow cannot land unrecorded, and a
// dormant one cannot stay dormant without naming what unblocks it.
//
// Run `go run ./test/workflowinventory -write` to regenerate the table;
// blocking-issue references are carried forward because no workflow file
// states them.
package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

const (
	beginMarker  = "<!-- BEGIN GENERATED WORKFLOW INVENTORY -->"
	endMarker    = "<!-- END GENERATED WORKFLOW INVENTORY -->"
	activeStatus = "active"
	// dormantStatus marks a workflow whose only enabled trigger is
	// workflow_dispatch while a commented-out schedule waits on something.
	dormantStatus = "dormant"
	noBlocker     = "—"
	blockerTODO   = "TODO: name the blocking issue"
)

// commentedSchedule matches a `schedule:` key that has been commented out.
var commentedSchedule = regexp.MustCompile(`(?m)^\s*#\s*schedule:\s*$`)

type workflow struct {
	File     string
	Triggers []string
	Status   string
}

type row struct {
	File      string
	Triggers  string
	Status    string
	BlockedOn string
}

func main() {
	write := flag.Bool("write", false, "rewrite the inventory table in place")
	workflowsDir := flag.String("workflows", filepath.Join(".github", "workflows"), "workflow directory to scan")
	inventory := flag.String("inventory", filepath.Join("docs", "reference", "workflow-inventory.md"), "rendered inventory document")
	flag.Parse()

	if err := run(*workflowsDir, *inventory, *write); err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "workflowinventory: %v\n", err)
		os.Exit(1)
	}
}

func run(workflowsDir, inventoryPath string, write bool) error {
	workflows, err := scanWorkflows(workflowsDir)
	if err != nil {
		return err
	}
	document, err := os.ReadFile(inventoryPath)
	if err != nil {
		return err
	}
	recorded, err := parseInventory(string(document))
	if err != nil && !write {
		return fmt.Errorf("%s: %w", inventoryPath, err)
	}

	if write {
		updated, err := renderInventory(string(document), workflows, recorded)
		if err != nil {
			return fmt.Errorf("%s: %w", inventoryPath, err)
		}
		if updated == string(document) {
			return nil
		}
		return os.WriteFile(inventoryPath, []byte(updated), 0o644)
	}

	problems := compare(workflows, recorded)
	if len(problems) == 0 {
		return nil
	}
	sort.Strings(problems)
	return fmt.Errorf("%s is out of date (regenerate with `go run ./test/workflowinventory -write`):\n  %s",
		inventoryPath, strings.Join(problems, "\n  "))
}

// compare reports every disagreement between the workflow files and the
// rendered inventory.
func compare(workflows []workflow, recorded map[string]row) []string {
	var problems []string
	seen := map[string]bool{}
	for _, current := range workflows {
		seen[current.File] = true
		entry, ok := recorded[current.File]
		if !ok {
			problems = append(problems, fmt.Sprintf("%s: %s workflow is missing from the inventory", current.File, current.Status))
			continue
		}
		if triggers := strings.Join(current.Triggers, ", "); entry.Triggers != triggers {
			problems = append(problems, fmt.Sprintf("%s: inventory lists triggers %q, workflow has %q", current.File, entry.Triggers, triggers))
		}
		if entry.Status != current.Status {
			problems = append(problems, fmt.Sprintf("%s: inventory says %q, workflow is %s", current.File, entry.Status, current.Status))
		}
		if current.Status == dormantStatus {
			if entry.BlockedOn == "" || entry.BlockedOn == noBlocker || strings.HasPrefix(entry.BlockedOn, "TODO") {
				problems = append(problems, fmt.Sprintf("%s: dormant workflow names no blocking issue in the inventory", current.File))
			}
			continue
		}
		if entry.BlockedOn != noBlocker {
			problems = append(problems, fmt.Sprintf("%s: %s workflow should record %q as its blocker, has %q", current.File, current.Status, noBlocker, entry.BlockedOn))
		}
	}
	for file := range recorded {
		if !seen[file] {
			problems = append(problems, fmt.Sprintf("%s: inventory lists a workflow that does not exist", file))
		}
	}
	return problems
}

func scanWorkflows(dir string) ([]workflow, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var workflows []workflow
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || (filepath.Ext(name) != ".yml" && filepath.Ext(name) != ".yaml") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			return nil, err
		}
		triggers, err := enabledTriggers(data)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		workflows = append(workflows, workflow{
			File:     name,
			Triggers: triggers,
			Status:   classify(triggers, commentedSchedule.Match(data)),
		})
	}
	sort.Slice(workflows, func(i, j int) bool { return workflows[i].File < workflows[j].File })
	return workflows, nil
}

func classify(triggers []string, hasCommentedSchedule bool) string {
	if hasCommentedSchedule && len(triggers) == 1 && triggers[0] == "workflow_dispatch" {
		return dormantStatus
	}
	return activeStatus
}

// enabledTriggers reads the workflow's `on:` key from the document node rather
// than unmarshalling into a struct, because YAML resolves a bare `on` key to
// the boolean true.
func enabledTriggers(data []byte) ([]string, error) {
	var document yaml.Node
	if err := yaml.Unmarshal(data, &document); err != nil {
		return nil, err
	}
	if len(document.Content) == 0 || document.Content[0].Kind != yaml.MappingNode {
		return nil, errors.New("not a workflow mapping")
	}
	top := document.Content[0]
	for i := 0; i+1 < len(top.Content); i += 2 {
		if top.Content[i].Value != "on" {
			continue
		}
		return triggerNames(top.Content[i+1])
	}
	return nil, errors.New("no `on:` triggers")
}

func triggerNames(node *yaml.Node) ([]string, error) {
	var names []string
	switch node.Kind {
	case yaml.MappingNode:
		for i := 0; i+1 < len(node.Content); i += 2 {
			names = append(names, node.Content[i].Value)
		}
	case yaml.SequenceNode:
		for _, child := range node.Content {
			names = append(names, child.Value)
		}
	case yaml.ScalarNode:
		names = append(names, node.Value)
	default:
		return nil, errors.New("unsupported `on:` node")
	}
	sort.Strings(names)
	return names, nil
}

// parseInventory reads the generated table between the markers.
func parseInventory(document string) (map[string]row, error) {
	body, err := generatedSection(document)
	if err != nil {
		return nil, err
	}
	recorded := map[string]row{}
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "|") {
			continue
		}
		cells := tableCells(line)
		if len(cells) != 4 || cells[0] == "Workflow" || strings.HasPrefix(cells[0], "---") {
			continue
		}
		file := strings.Trim(cells[0], "`")
		recorded[file] = row{File: file, Triggers: cells[1], Status: cells[2], BlockedOn: cells[3]}
	}
	if len(recorded) == 0 {
		return nil, errors.New("generated section contains no workflow rows")
	}
	return recorded, nil
}

func tableCells(line string) []string {
	fields := strings.Split(strings.Trim(line, "|"), "|")
	cells := make([]string, 0, len(fields))
	for _, field := range fields {
		cells = append(cells, strings.TrimSpace(field))
	}
	return cells
}

// renderInventory replaces the generated section, carrying each dormant
// workflow's recorded blocking issue forward.
func renderInventory(document string, workflows []workflow, recorded map[string]row) (string, error) {
	begin := strings.Index(document, beginMarker)
	end := strings.Index(document, endMarker)
	if begin < 0 || end < 0 || end < begin {
		return "", fmt.Errorf("missing %s / %s markers", beginMarker, endMarker)
	}
	var table strings.Builder
	table.WriteString("| Workflow | Enabled triggers | Status | Blocked on |\n")
	table.WriteString("| --- | --- | --- | --- |\n")
	for _, current := range workflows {
		blocker := noBlocker
		if current.Status == dormantStatus {
			blocker = blockerTODO
			if entry, ok := recorded[current.File]; ok && entry.BlockedOn != "" && entry.BlockedOn != noBlocker {
				blocker = entry.BlockedOn
			}
		}
		fmt.Fprintf(&table, "| `%s` | %s | %s | %s |\n",
			current.File, strings.Join(current.Triggers, ", "), current.Status, blocker)
	}
	return document[:begin+len(beginMarker)] + "\n\n" + table.String() + "\n" + document[end:], nil
}

func generatedSection(document string) (string, error) {
	begin := strings.Index(document, beginMarker)
	end := strings.Index(document, endMarker)
	if begin < 0 || end < 0 || end < begin {
		return "", fmt.Errorf("missing %s / %s markers", beginMarker, endMarker)
	}
	return document[begin+len(beginMarker) : end], nil
}
