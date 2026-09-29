package dslmigrate

import (
	"errors"
	"fmt"
	"reflect"
	"strings"

	"gopkg.in/yaml.v3"
)

var errSourcePreserveUnsupported = errors.New("source-preserving edit unsupported")

const (
	taskKindInput  = "kind"
	ciPollTaskKind = "ci-poll"
)

// applyV14ToV20 implements the DVL-5 (#865, PR #1353) v_current→v_2_0
// edge's one concrete semantic delta: internal/workflow/v_2_0's ci-poll
// input builder injects a "10s" pollIntervalSeconds default onto a ci-poll
// task's invocation whenever its downstream automated gate leaves
// PollIntervalSeconds unset, where v_current's builder leaves the input
// absent entirely (see internal/workflow/{v_current,v_2_0}/inputs.go). That
// default lives in compiled-invocation code, not in the workflow schema, so
// migrating a workflow forward would otherwise silently change its runtime
// polling cadence. This transform makes the new default explicit in the YAML
// — pinning `automated.pollIntervalSeconds: 10` on every such gate — so the
// compiled behavior is identical immediately before and after the version
// bump, and the change is visible in the migration diff for review.
func applyV14ToV20(_ []byte, root *yaml.Node) (bool, []string, error) {
	var notes []string
	changed := false
	spec, _ := mapValue(root, "spec")
	if spec != nil {
		gatesByName := map[string]*yaml.Node{}
		if gates, _ := mapValue(spec, "gates"); gates != nil {
			for _, gate := range gates.Content {
				if name, _ := mapValue(gate, "name"); name != nil {
					gatesByName[name.Value] = gate
				}
			}
		}
		if tasks, _ := mapValue(spec, "tasks"); tasks != nil {
			for _, task := range tasks.Content {
				note, ok := pinCIPollInterval(task, gatesByName)
				if ok {
					changed = true
					notes = append(notes, note)
				}
			}
		}
	}
	return changed, notes, nil
}

func applyV14ToV20SourcePreserving(source []byte, root *yaml.Node, versionNode *yaml.Node, to string) ([]byte, []string, error) {
	versionEdit, err := versionPinEdit(source, versionNode, root, to)
	if err != nil {
		return nil, nil, err
	}
	edits := []sourceEdit{versionEdit}
	var notes []string
	gateEdited := map[*yaml.Node]bool{}
	spec, _ := mapValue(root, "spec")
	if spec != nil {
		gatesByName := map[string]*yaml.Node{}
		if gates, _ := mapValue(spec, "gates"); gates != nil {
			for _, gate := range gates.Content {
				if name, _ := mapValue(gate, "name"); name != nil {
					gatesByName[name.Value] = gate
				}
			}
		}
		if tasks, _ := mapValue(spec, "tasks"); tasks != nil {
			for _, task := range tasks.Content {
				edit, note, ok, err := ciPollIntervalSourceEdit(source, task, gatesByName)
				if err != nil {
					return nil, nil, err
				}
				if !ok {
					continue
				}
				gate, _ := ciPollTargetGate(task, gatesByName)
				if gateEdited[gate] {
					continue
				}
				gateEdited[gate] = true
				edits = append(edits, edit)
				notes = append(notes, note)
			}
		}
	}
	edited := applySourceEdits(source, edits)
	ok, err := sourceEditMatchesV14ToV20(source, edited, to)
	if err != nil {
		return nil, nil, err
	}
	if !ok {
		return nil, nil, errors.New("source-preserving migration produced YAML with different semantics than the node-tree transform")
	}
	return edited, notes, nil
}

func sourceEditMatchesV14ToV20(source, edited []byte, to string) (bool, error) {
	got, err := parseSemanticYAML(edited)
	if err != nil {
		return false, nil
	}
	want, err := expectedSemanticV14ToV20(source, to)
	if err != nil {
		return false, err
	}
	return reflect.DeepEqual(got, want), nil
}

func expectedSemanticV14ToV20(source []byte, to string) (semanticYAML, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(source, &doc); err != nil {
		return semanticYAML{}, fmt.Errorf("parse workflow for semantic check: %w", err)
	}
	root := documentRoot(&doc)
	if root == nil || root.Kind != yaml.MappingNode {
		return semanticYAML{}, errors.New("workflow document has no top-level mapping")
	}
	if _, _, err := applyV14ToV20(source, root); err != nil {
		return semanticYAML{}, err
	}
	setScalar(root, "dslVersion", to, "!!str")
	return semanticNode(root, ""), nil
}

type semanticYAML struct {
	Kind     yaml.Kind
	Tag      string
	Value    string
	Sequence []semanticYAML
	Mapping  map[string]semanticYAML
}

func parseSemanticYAML(source []byte) (semanticYAML, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(source, &doc); err != nil {
		return semanticYAML{}, err
	}
	return semanticNode(documentRoot(&doc), ""), nil
}

func semanticNode(node *yaml.Node, key string) semanticYAML {
	if node == nil {
		return semanticYAML{}
	}
	out := semanticYAML{Kind: node.Kind, Tag: node.Tag, Value: node.Value}
	if node.Kind == yaml.ScalarNode && key == "dslVersion" {
		out.Tag = "!!str"
	}
	switch node.Kind {
	case yaml.MappingNode:
		out.Mapping = map[string]semanticYAML{}
		for i := 0; i+1 < len(node.Content); i += 2 {
			k := node.Content[i].Value
			out.Mapping[k] = semanticNode(node.Content[i+1], k)
		}
	case yaml.SequenceNode:
		out.Sequence = make([]semanticYAML, 0, len(node.Content))
		for _, child := range node.Content {
			out.Sequence = append(out.Sequence, semanticNode(child, ""))
		}
	}
	return out
}

func pinCIPollInterval(task *yaml.Node, gatesByName map[string]*yaml.Node) (string, bool) {
	gate, note := ciPollTargetGate(task, gatesByName)
	if gate == nil {
		return "", false
	}
	automated, _ := mapValue(gate, "automated")
	if automated == nil {
		return "", false
	}
	if poll, _ := mapValue(automated, "pollIntervalSeconds"); poll != nil && poll.Value != "" && poll.Value != "0" {
		return "", false
	}
	setScalar(automated, "pollIntervalSeconds", "10", "!!int")
	return note, true
}

func ciPollTargetGate(task *yaml.Node, gatesByName map[string]*yaml.Node) (*yaml.Node, string) {
	inputs, _ := mapValue(task, "inputs")
	if inputs == nil {
		return nil, ""
	}
	kind, _ := mapValue(inputs, taskKindInput)
	if kind == nil || kind.Value != ciPollTaskKind {
		return nil, ""
	}
	next, _ := mapValue(task, "next")
	if next == nil || next.Value == "" {
		return nil, ""
	}
	gate, ok := gatesByName[next.Value]
	if !ok {
		return nil, ""
	}
	evaluator, _ := mapValue(gate, "evaluator")
	if evaluator == nil || evaluator.Value != "automated" {
		return nil, ""
	}
	taskName, _ := mapValue(task, "name")
	name := ciPollTaskKind
	if taskName != nil {
		name = taskName.Value
	}
	note := fmt.Sprintf(
		"gate %q: pinned automated.pollIntervalSeconds: 10 explicitly — DSL 2.0's ci-poll input builder "+
			"injects this default for task %q where DSL 1.4 left it unset", next.Value, name,
	)
	return gate, note
}

func ciPollIntervalSourceEdit(source []byte, task *yaml.Node, gatesByName map[string]*yaml.Node) (sourceEdit, string, bool, error) {
	gate, note := ciPollTargetGate(task, gatesByName)
	if gate == nil {
		return sourceEdit{}, "", false, nil
	}
	automated, _ := mapValue(gate, "automated")
	if automated == nil {
		return sourceEdit{}, "", false, nil
	}
	if poll, _ := mapValue(automated, "pollIntervalSeconds"); poll != nil {
		if poll.Value != "" && poll.Value != "0" {
			return sourceEdit{}, "", false, nil
		}
		edit, err := scalarReplacementEdit(source, poll, []byte("10"), "pollIntervalSeconds")
		if err != nil {
			return sourceEdit{}, "", false, err
		}
		return edit, note, true, nil
	}
	edit, err := insertMappingScalarEdit(source, automated, "pollIntervalSeconds: 10")
	if err != nil {
		return sourceEdit{}, "", false, err
	}
	return edit, note, true, nil
}

func scalarReplacementEdit(source []byte, node *yaml.Node, replacement []byte, field string) (sourceEdit, error) {
	if node.Kind != yaml.ScalarNode || node.Line < 1 || node.Column < 1 {
		return sourceEdit{}, fmt.Errorf("%s must be a scalar", field)
	}
	if node.Style&(yaml.LiteralStyle|yaml.FoldedStyle) != 0 {
		return sourceEdit{}, fmt.Errorf("block-style %s is not supported for source-preserving migration", field)
	}
	if node.Style&yaml.TaggedStyle != 0 {
		return sourceEdit{}, fmt.Errorf("tagged %s is not supported for source-preserving migration", field)
	}
	if node.Anchor != "" {
		return sourceEdit{}, fmt.Errorf("anchored %s is not supported for source-preserving migration", field)
	}
	start, err := sourceOffset(source, node.Line, node.Column)
	if err != nil {
		return sourceEdit{}, err
	}
	end, err := scalarEnd(source, start, node.Style)
	if err != nil {
		return sourceEdit{}, err
	}
	return sourceEdit{start: start, end: end, replacement: replacement}, nil
}

func insertMappingScalarEdit(source []byte, mapping *yaml.Node, line string) (sourceEdit, error) {
	if mapping == nil || mapping.Kind != yaml.MappingNode {
		return sourceEdit{}, fmt.Errorf("target mapping for %s is not a YAML mapping", line)
	}
	if mapping.Style&yaml.FlowStyle != 0 {
		return insertFlowMappingScalarEdit(source, mapping, line)
	}
	eol := sourceEOL(source)
	if len(mapping.Content) == 0 {
		if mapping.Line < 1 || mapping.Column < 1 {
			return sourceEdit{}, fmt.Errorf("empty target mapping for %s has no source position", line)
		}
		lineEnd, err := sourceLineEndOffset(source, mapping.Line)
		if err != nil {
			return sourceEdit{}, err
		}
		indent := strings.Repeat(" ", mapping.Column+1)
		return sourceEdit{start: lineEnd, end: lineEnd, replacement: append(append([]byte{}, eol...), []byte(indent+line)...)}, nil
	}
	firstKey := mapping.Content[0]
	if firstKey.Line < 1 || firstKey.Column < 1 {
		return sourceEdit{}, fmt.Errorf("target mapping for %s has no source position", line)
	}
	offset, err := sourceOffset(source, firstKey.Line, firstKey.Column)
	if err != nil {
		return sourceEdit{}, err
	}
	indent := strings.Repeat(" ", firstKey.Column-1)
	insertion := append([]byte(line), eol...)
	insertion = append(insertion, []byte(indent)...)
	return sourceEdit{start: offset, end: offset, replacement: insertion}, nil
}

func insertFlowMappingScalarEdit(source []byte, mapping *yaml.Node, line string) (sourceEdit, error) {
	if mapping.Line < 1 || mapping.Column < 1 {
		return sourceEdit{}, fmt.Errorf("flow target mapping for %s has no source position", line)
	}
	start, err := sourceOffset(source, mapping.Line, mapping.Column)
	if err != nil {
		return sourceEdit{}, err
	}
	if start >= len(source) || source[start] != '{' {
		return sourceEdit{}, fmt.Errorf("%w: flow target mapping for %s does not start at an opening brace", errSourcePreserveUnsupported, line)
	}
	replacement := []byte(line)
	if len(mapping.Content) > 0 {
		replacement = []byte(line + ", ")
	}
	return sourceEdit{start: start + 1, end: start + 1, replacement: replacement}, nil
}

func sourceLineEndOffset(source []byte, line int) (int, error) {
	start, err := sourceOffset(source, line, 1)
	if err != nil {
		return 0, err
	}
	next := start
	for next < len(source) && source[next] != '\r' && source[next] != '\n' {
		next++
	}
	return next, nil
}
