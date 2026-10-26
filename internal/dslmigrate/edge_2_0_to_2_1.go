package dslmigrate

import (
	"fmt"
	"reflect"
	"strings"

	"gopkg.in/yaml.v3"
)

// Match the compiler's policyCommand contract exactly, including argv[0].
// Shell snippets and similarly named commands are not inferred permissions.
func commentWatchTasks(root *yaml.Node) []*yaml.Node {
	spec, _ := mapValue(root, "spec")
	tasks, _ := mapValue(spec, "tasks")
	if tasks == nil || tasks.Kind != yaml.SequenceNode {
		return nil
	}
	var found []*yaml.Node
	for _, task := range tasks.Content {
		run, _ := mapValue(task, "run")
		command, _ := mapValue(run, "command")
		if command != nil && command.Kind == yaml.SequenceNode && len(command.Content) >= 2 &&
			command.Content[0].Value == "goobers" && command.Content[1].Value == "pr-comment-watch" {
			found = append(found, task)
		}
	}
	return found
}

func hasCommentAction(task *yaml.Node) bool {
	actions, _ := mapValue(task, "policyActions")
	if actions != nil && actions.Kind == yaml.SequenceNode {
		for _, action := range actions.Content {
			if action.Value == "comment-on-issue" {
				return true
			}
		}
	}
	return false
}

func policyAdditionsV21(root *yaml.Node) []PolicyAddition {
	var additions []PolicyAddition
	for _, task := range commentWatchTasks(root) {
		if hasCommentAction(task) {
			continue
		}
		name, _ := mapValue(task, "name")
		taskName := "<unnamed>"
		if name != nil {
			taskName = name.Value
		}
		additions = append(additions, PolicyAddition{Task: taskName, Action: "comment-on-issue"})
	}
	return additions
}

func applyV20ToV21SourcePreserving(source []byte, root, version *yaml.Node, to string) ([]byte, []string, error) {
	pin, err := versionPinEdit(source, version, root, to)
	if err != nil {
		return nil, nil, err
	}
	edits := []sourceEdit{pin}
	notes := []string{"DSL 2.1 is PREVIEW: each migrated Workflow.metadata.annotations must explicitly set goobers.dev/allow-preview-features to \"true\" before validation or execution; Instance/Gaggle annotations do not opt in. Migration does not add this annotation.", "DSL 2.1 opts pr-comment-watch into its durable three-route budget; DSL 2.0 retains its stable behavior."}
	for _, task := range commentWatchTasks(root) {
		if hasCommentAction(task) {
			continue
		}
		edit, err := commentActionEdit(source, task)
		if err != nil {
			return nil, nil, err
		}
		edits = append(edits, edit)
		// Mutate only the expected tree; the source edits preserve every other byte.
		actions, _ := mapValue(task, "policyActions")
		if actions == nil {
			actions = &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
			task.Content = append(task.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "policyActions"}, actions)
		}
		actions.Content = append([]*yaml.Node{{Kind: yaml.ScalarNode, Tag: "!!str", Value: "comment-on-issue"}}, actions.Content...)
	}
	setScalar(root, "dslVersion", to, "!!str")
	if err := validateTransformSourceEdits(source, edits); err != nil {
		return nil, nil, err
	}
	after := applySourceEdits(source, edits)
	got, err := parseSemanticYAML(after)
	if err != nil {
		return nil, nil, fmt.Errorf("validate source-preserving migration: %w", err)
	}
	if !reflect.DeepEqual(got, semanticNode(root, "")) {
		return nil, nil, fmt.Errorf("source-preserving migration changed unrelated YAML semantics")
	}
	if err := validateCommentsPreserved(source, after); err != nil {
		return nil, nil, err
	}
	return after, notes, nil
}

func commentActionEdit(source []byte, task *yaml.Node) (sourceEdit, error) {
	actions, _ := mapValue(task, "policyActions")
	if actions == nil {
		if task.Style&yaml.FlowStyle != 0 {
			pos, err := nodeContentOffset(source, task)
			if err != nil {
				return sourceEdit{}, err
			}
			if source[pos] != '{' {
				return sourceEdit{}, fmt.Errorf("unsupported flow task mapping")
			}
			return sourceEdit{start: pos + 1, end: pos + 1, replacement: []byte("policyActions: [comment-on-issue], ")}, nil
		}
		first := task.Content[0]
		pos, err := nodeContentOffset(source, first)
		if err != nil {
			return sourceEdit{}, err
		}
		insertion := "policyActions: [comment-on-issue]" + string(sourceEOL(source)) + strings.Repeat(" ", first.Column-1)
		return sourceEdit{start: pos, end: pos, replacement: []byte(insertion)}, nil
	}
	if actions.Kind != yaml.SequenceNode {
		return sourceEdit{}, fmt.Errorf("pr-comment-watch policyActions must be a sequence; repair it before migration")
	}
	if actions.Style&yaml.FlowStyle != 0 {
		pos, err := nodeContentOffset(source, actions)
		if err != nil {
			return sourceEdit{}, err
		}
		if source[pos] != '[' {
			return sourceEdit{}, fmt.Errorf("unsupported flow policyActions")
		}
		insertion := "comment-on-issue"
		if len(actions.Content) > 0 {
			insertion += ", "
		}
		return sourceEdit{start: pos + 1, end: pos + 1, replacement: []byte(insertion)}, nil
	}
	if len(actions.Content) == 0 {
		return sourceEdit{}, fmt.Errorf("empty block policyActions has no source position")
	}
	first := actions.Content[0]
	if first.Kind != yaml.ScalarNode {
		return sourceEdit{}, fmt.Errorf("policyActions must contain scalar action names")
	}
	pos, err := nodeContentOffset(source, first)
	if err != nil {
		return sourceEdit{}, err
	}
	if first.Column < 3 {
		return sourceEdit{}, fmt.Errorf("unsupported policyActions indentation")
	}
	insertion := "comment-on-issue" + string(sourceEOL(source)) + strings.Repeat(" ", first.Column-3) + "- "
	return sourceEdit{start: pos, end: pos, replacement: []byte(insertion)}, nil
}
