// Package tutorclass implements TUT-A6's change-type classifier (design:
// docs/design/tutor-redesign.md §5 item 5 / D5, issue #1218): the tutor's own
// PRs get differentiated review, not the single undifferentiated CODEOWNERS
// gate every other config change gets. Workflow topology changes, gate
// removals/loosening, and skill-list changes require explicit human
// sign-off and must never be auto-merged; ordinary persona-prompt edits and
// gate tuning may follow the normal review path.
//
// This package only classifies what a diff observably did — same
// observable-diff discipline as internal/tutorguard's gate-edit
// classification, which this package composes with (a gate removal/
// loosening always escalates to CategoryStructure, since it is exactly the
// kind of change item 5 lists as sign-off-required).
package tutorclass

import (
	"bytes"
	"fmt"

	"sigs.k8s.io/yaml"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

// Category is a tutor PR's change-type classification.
type Category string

const (
	// CategoryPersona is the default, lightest-review category: only
	// goober instructions/prompt text or other non-structural content
	// changed. Follows the normal review path (TUT-A6 acceptance).
	CategoryPersona Category = "persona"
	// CategoryGateTune means an existing gate's non-topology fields
	// changed (its check, params, timeout) without adding/removing any
	// task or gate and without loosening/removing the gate. Also follows
	// the normal review path.
	CategoryGateTune Category = "gate-tune"
	// CategoryStructure covers everything item 5 requires sign-off for:
	// workflow topology changes (a task or gate added/removed, or a
	// task's wiring rewired), a gate removed/loosened, or a goober's
	// skill list changed. Never auto-merged.
	CategoryStructure Category = "structure"
)

// RequiresSignoff reports whether c is one of the categories TUT-A6 requires
// explicit human sign-off for and forbids auto-merging.
func (c Category) RequiresSignoff() bool {
	return c == CategoryStructure
}

// Escalate returns the more sign-off-demanding of a and b — CategoryStructure
// dominates CategoryGateTune, which dominates CategoryPersona — for folding
// several changed files' individual classifications into one PR-level
// category (the most demanding file wins, same "most severe survives"
// convention as tutorguard's worseGateEdit).
func Escalate(a, b Category) Category {
	rank := map[Category]int{CategoryPersona: 0, CategoryGateTune: 1, CategoryStructure: 2}
	if rank[b] > rank[a] {
		return b
	}
	return a
}

// WorkflowTopologyChanged reports whether oldYAML and newYAML — a workflow
// definition's content at two revisions — differ in their task or gate name
// sets, or in any task's Next wiring: adding/removing a stage, or rewiring
// which stage follows which, is a topology change regardless of which
// stage's own fields also changed. nil bytes model a revision where the file
// did not exist (a brand-new or deleted file is never itself "topology
// changed" by this function — the changed-file-list already captures that
// distinctly; callers that want "file added/removed" treated as structural
// should check for empty content and escalate directly).
func WorkflowTopologyChanged(oldYAML, newYAML []byte) (bool, error) {
	oldWF, err := parseWorkflow(oldYAML)
	if err != nil {
		return false, fmt.Errorf("tutorclass: parse old workflow: %w", err)
	}
	newWF, err := parseWorkflow(newYAML)
	if err != nil {
		return false, fmt.Errorf("tutorclass: parse new workflow: %w", err)
	}
	if oldWF == nil || newWF == nil {
		return false, nil
	}

	if !sameNameSet(taskNames(oldWF.Spec.Tasks), taskNames(newWF.Spec.Tasks)) {
		return true, nil
	}
	if !sameNameSet(gateNames(oldWF.Spec.Gates), gateNames(newWF.Spec.Gates)) {
		return true, nil
	}

	oldTasks := taskByName(oldWF.Spec.Tasks)
	for _, newTask := range newWF.Spec.Tasks {
		oldTask, ok := oldTasks[newTask.Name]
		if ok && oldTask.Next != newTask.Next {
			return true, nil
		}
	}
	return false, nil
}

// GateFieldsChanged reports whether any gate present in both revisions has a
// different definition — a tuning signal — without itself indicating which
// kind of change (tutorguard.ClassifyGateEdit distinguishes tuning from
// loosened/removed for a specific named gate; this function is the broader
// "did any shared gate's fields change at all" check used to decide between
// CategoryPersona and CategoryGateTune when no topology change occurred).
func GateFieldsChanged(oldYAML, newYAML []byte) (bool, error) {
	oldWF, err := parseWorkflow(oldYAML)
	if err != nil {
		return false, fmt.Errorf("tutorclass: parse old workflow: %w", err)
	}
	newWF, err := parseWorkflow(newYAML)
	if err != nil {
		return false, fmt.Errorf("tutorclass: parse new workflow: %w", err)
	}
	if oldWF == nil || newWF == nil {
		return false, nil
	}

	oldGates := gateByName(oldWF.Spec.Gates)
	for _, newGate := range newWF.Spec.Gates {
		oldGate, ok := oldGates[newGate.Name]
		if ok && !gatesEqual(oldGate, newGate) {
			return true, nil
		}
	}
	return false, nil
}

// GooberSkillsChanged reports whether a goober definition's declared skill
// list changed between revisions — order-independent, since reordering an
// unordered set of skill names is not itself a meaningful change.
func GooberSkillsChanged(oldYAML, newYAML []byte) (bool, error) {
	oldGoober, err := parseGoober(oldYAML)
	if err != nil {
		return false, fmt.Errorf("tutorclass: parse old goober: %w", err)
	}
	newGoober, err := parseGoober(newYAML)
	if err != nil {
		return false, fmt.Errorf("tutorclass: parse new goober: %w", err)
	}
	if oldGoober == nil || newGoober == nil {
		return false, nil
	}
	return !sameNameSet(oldGoober.Spec.Skills, newGoober.Spec.Skills), nil
}

func parseWorkflow(raw []byte) (*apiv1.Workflow, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, nil
	}
	var wf apiv1.Workflow
	if err := yaml.Unmarshal(raw, &wf); err != nil {
		return nil, err
	}
	return &wf, nil
}

func parseGoober(raw []byte) (*apiv1.Goober, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, nil
	}
	var g apiv1.Goober
	if err := yaml.Unmarshal(raw, &g); err != nil {
		return nil, err
	}
	return &g, nil
}

func taskNames(tasks []apiv1.Task) []string {
	names := make([]string, 0, len(tasks))
	for _, t := range tasks {
		names = append(names, t.Name)
	}
	return names
}

func gateNames(gates []apiv1.Gate) []string {
	names := make([]string, 0, len(gates))
	for _, g := range gates {
		names = append(names, g.Name)
	}
	return names
}

func taskByName(tasks []apiv1.Task) map[string]apiv1.Task {
	m := make(map[string]apiv1.Task, len(tasks))
	for _, t := range tasks {
		m[t.Name] = t
	}
	return m
}

func gateByName(gates []apiv1.Gate) map[string]apiv1.Gate {
	m := make(map[string]apiv1.Gate, len(gates))
	for _, g := range gates {
		m[g.Name] = g
	}
	return m
}

func toSet(items []string) map[string]bool {
	m := make(map[string]bool, len(items))
	for _, item := range items {
		m[item] = true
	}
	return m
}

func sameNameSet(a, b []string) bool {
	sa, sb := toSet(a), toSet(b)
	if len(sa) != len(sb) {
		return false
	}
	for k := range sa {
		if !sb[k] {
			return false
		}
	}
	return true
}

func gatesEqual(a, b apiv1.Gate) bool {
	aj, aErr := yaml.Marshal(a)
	bj, bErr := yaml.Marshal(b)
	if aErr != nil || bErr != nil {
		return false
	}
	return bytes.Equal(aj, bj)
}
