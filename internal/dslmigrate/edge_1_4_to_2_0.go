package dslmigrate

import (
	"bytes"
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
	if err := validateTransformSourceEdits(source, edits); err != nil {
		return nil, nil, err
	}
	edited := applySourceEdits(source, edits)
	if err := validateCommentsPreserved(source, edited); err != nil {
		return nil, nil, err
	}
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

func validateTransformSourceEdits(source []byte, edits []sourceEdit) error {
	for _, edit := range edits {
		if edit.start < 0 || edit.end < edit.start || edit.end > len(source) {
			return fmt.Errorf("source-preserving migration produced invalid edit range [%d,%d)", edit.start, edit.end)
		}
		if edit.end == edit.start {
			continue
		}
		replaced := source[edit.start:edit.end]
		if bytes.Contains(replaced, []byte("#")) && strings.TrimSpace(string(replaced)) != "pollIntervalSeconds:" {
			return errors.New("source-preserving migration edit would replace a YAML comment")
		}
	}
	return nil
}

func validateCommentsPreserved(source, edited []byte) error {
	for _, comment := range yamlCommentTexts(source) {
		if !bytes.Contains(edited, []byte(comment)) {
			return fmt.Errorf("source-preserving migration lost YAML comment %q", comment)
		}
	}
	return nil
}

func yamlCommentTexts(source []byte) []string {
	var comments []string
	inSingle := false
	inDouble := false
	lineStart := 0
	for i := 0; i < len(source); i++ {
		switch source[i] {
		case '\n':
			inSingle = false
			inDouble = false
			lineStart = i + 1
		case '\'':
			if !inDouble {
				if inSingle && i+1 < len(source) && source[i+1] == '\'' {
					i++
					continue
				}
				inSingle = !inSingle
			}
		case '"':
			if !inSingle {
				backslashes := 0
				for j := i - 1; j >= lineStart && source[j] == '\\'; j-- {
					backslashes++
				}
				if backslashes%2 == 0 {
					inDouble = !inDouble
				}
			}
		case '#':
			if !inSingle && !inDouble {
				end := i
				for end < len(source) && source[end] != '\r' && source[end] != '\n' {
					end++
				}
				comments = append(comments, string(source[i:end]))
				i = end - 1
			}
		}
	}
	return comments
}

func expectedSemanticV14ToV20(source []byte, to string) (semanticYAML, error) {
	var normalizeErr error
	source = normalizeValuelessFlowPollInterval(source, &normalizeErr)
	if normalizeErr != nil {
		return semanticYAML{}, normalizeErr
	}
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

func normalizeValuelessFlowPollInterval(source []byte, outErr *error) []byte {
	var doc yaml.Node
	if err := yaml.Unmarshal(source, &doc); err != nil {
		// Leave parse errors to the caller's real parse path.
		return source
	}
	var edits []sourceEdit
	collectValuelessFlowPollIntervalEdits(source, documentRoot(&doc), &edits, outErr)
	if outErr != nil && *outErr != nil {
		return source
	}
	if len(edits) == 0 {
		return source
	}
	return applySourceEdits(source, edits)
}

func collectValuelessFlowPollIntervalEdits(source []byte, node *yaml.Node, edits *[]sourceEdit, outErr *error) {
	if node == nil || (outErr != nil && *outErr != nil) {
		return
	}
	if node.Kind == yaml.MappingNode && node.Style&yaml.FlowStyle != 0 && node.Line > 0 && node.Column > 0 {
		start, err := nodeContentOffset(source, node)
		if err != nil {
			if outErr != nil {
				*outErr = err
			}
			return
		}
		if start < len(source) && source[start] == '{' {
			if closeRel := bytes.IndexByte(source[start+1:], '}'); closeRel >= 0 {
				closeOffset := start + 1 + closeRel
				if strings.TrimSpace(string(source[start+1:closeOffset])) == "pollIntervalSeconds:" {
					*edits = append(*edits, sourceEdit{start: closeOffset, end: closeOffset, replacement: []byte(" ")})
				}
			}
		}
	}
	for _, child := range node.Content {
		collectValuelessFlowPollIntervalEdits(source, child, edits, outErr)
	}
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
	if poll, _ := mapValue(automated, "pollIntervalSeconds"); poll != nil && !isUnsetPollInterval(poll) {
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
	if poll, pollIndex := mapValue(automated, "pollIntervalSeconds"); poll != nil {
		if !isUnsetPollInterval(poll) {
			return sourceEdit{}, "", false, nil
		}
		var edit sourceEdit
		var err error
		if poll.Tag == "!!null" || poll.Value == "" {
			edit, err = nullPollIntervalReplacementEdit(source, automated.Content[pollIndex-1], poll, []byte("10"))
		} else {
			edit, err = scalarReplacementEdit(source, poll, []byte("10"), "pollIntervalSeconds")
		}
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

func isUnsetPollInterval(node *yaml.Node) bool {
	if node == nil {
		return true
	}
	return node.Value == "" || node.Value == "0" || node.Tag == "!!null"
}

func scalarReplacementEdit(source []byte, node *yaml.Node, replacement []byte, field string) (sourceEdit, error) {
	if node.Kind != yaml.ScalarNode || node.Line < 1 || node.Column < 1 {
		return sourceEdit{}, fmt.Errorf("%s must be a scalar", field)
	}
	if node.Style&(yaml.LiteralStyle|yaml.FoldedStyle) != 0 {
		return sourceEdit{}, fmt.Errorf("block-style %s is not supported for source-preserving migration", field)
	}
	start, err := nodeContentOffset(source, node)
	if err != nil {
		return sourceEdit{}, err
	}
	end, err := scalarEnd(source, start, node.Style)
	if err != nil {
		return sourceEdit{}, err
	}
	return sourceEdit{start: start, end: end, replacement: replacement}, nil
}

func nodeContentOffset(source []byte, node *yaml.Node) (int, error) {
	if node == nil || node.Line < 1 || node.Column < 1 {
		return 0, errors.New("YAML node has no source position")
	}
	pos, err := sourceOffset(source, node.Line, node.Column)
	if err != nil {
		return 0, err
	}
	for pos < len(source) {
		pos = skipYAMLWhitespaceAndComments(source, pos)
		if pos >= len(source) {
			break
		}
		switch source[pos] {
		case '!':
			next, err := tagTokenEnd(source, pos)
			if err != nil {
				return 0, err
			}
			pos = next
			continue
		case '&':
			next := yamlTokenEnd(source, pos, len(source))
			if next <= pos {
				return 0, errors.New("YAML anchor token has no end")
			}
			pos = next
			continue
		default:
			return pos, nil
		}
	}
	return 0, errors.New("YAML node content start was not found")
}

func skipYAMLWhitespaceAndComments(source []byte, pos int) int {
	for pos < len(source) {
		switch source[pos] {
		case ' ', '\t', '\r', '\n':
			pos++
		case '#':
			for pos < len(source) && source[pos] != '\n' {
				pos++
			}
		default:
			return pos
		}
	}
	return pos
}

func tagTokenEnd(source []byte, pos int) (int, error) {
	if pos+1 < len(source) && source[pos+1] == '<' {
		end := bytes.IndexByte(source[pos+2:], '>')
		if end < 0 {
			return 0, errors.New("verbatim YAML tag has no closing '>'")
		}
		return pos + 2 + end + 1, nil
	}
	next := yamlTokenEnd(source, pos, len(source))
	if next <= pos {
		return 0, errors.New("YAML tag token has no end")
	}
	return next, nil
}

func yamlTokenEnd(source []byte, pos, limit int) int {
	for pos < limit && !plainScalarDelimiter(source[pos]) {
		pos++
	}
	return pos
}

func nullPollIntervalReplacementEdit(source []byte, key, value *yaml.Node, replacement []byte) (sourceEdit, error) {
	if value.Value != "" {
		return scalarReplacementEdit(source, value, replacement, "pollIntervalSeconds")
	}
	if key == nil || key.Line < 1 || key.Column < 1 {
		return sourceEdit{}, errors.New("pollIntervalSeconds key has no source position")
	}
	keyStart, err := nodeContentOffset(source, key)
	if err != nil {
		return sourceEdit{}, err
	}
	lineEnd := sourceLineEndOffsetFromOffset(source, keyStart)
	searchEnd := lineEnd
	if searchEnd < keyStart {
		return sourceEdit{}, errors.New("pollIntervalSeconds key position is after its line end")
	}
	colonRel := bytes.IndexByte(source[keyStart:searchEnd], ':')
	if colonRel < 0 {
		return sourceEdit{}, errors.New("pollIntervalSeconds key has no colon on its source line")
	}
	colon := keyStart + colonRel
	insert := colon + 1
	for insert < lineEnd && (source[insert] == ' ' || source[insert] == '\t') {
		insert++
	}
	if insert < lineEnd && source[insert] == '#' {
		prefix := []byte{}
		if insert == colon+1 {
			prefix = []byte{' '}
		}
		return sourceEdit{start: insert, end: insert, replacement: append(append(prefix, replacement...), ' ')}, nil
	}
	if insert == colon+1 {
		replacement = append([]byte{' '}, replacement...)
	}
	if value.Column > 0 {
		valueStart, err := nodeContentOffset(source, value)
		if err == nil && valueStart > colon && valueStart < lineEnd {
			insert = valueStart
			replacement = bytes.TrimLeft(replacement, " \t")
		}
	}
	return sourceEdit{start: insert, end: insert, replacement: replacement}, nil
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
		offset, err := nodeContentOffset(source, mapping)
		if err != nil {
			return sourceEdit{}, err
		}
		lineEnd := sourceLineEndOffsetFromOffset(source, offset)
		indent := indentForOffset(source, offset) + "  "
		return sourceEdit{start: lineEnd, end: lineEnd, replacement: append(append([]byte{}, eol...), []byte(indent+line)...)}, nil
	}
	if edit, ok, err := insertAfterLastSingleLineScalarEdit(source, mapping, line, eol); err != nil || ok {
		return edit, err
	}
	firstKey := mapping.Content[0]
	if firstKey.Line < 1 || firstKey.Column < 1 {
		return sourceEdit{}, fmt.Errorf("target mapping for %s has no source position", line)
	}
	offset, err := nodeContentOffset(source, firstKey)
	if err != nil {
		return sourceEdit{}, err
	}
	indent := indentForOffset(source, offset)
	insertion := append([]byte(line), eol...)
	insertion = append(insertion, []byte(indent)...)
	return sourceEdit{start: offset, end: offset, replacement: insertion}, nil
}

func insertAfterLastSingleLineScalarEdit(source []byte, mapping *yaml.Node, line string, eol []byte) (sourceEdit, bool, error) {
	if len(mapping.Content) < 2 {
		return sourceEdit{}, false, nil
	}
	lastKey := mapping.Content[len(mapping.Content)-2]
	lastValue := mapping.Content[len(mapping.Content)-1]
	if lastKey.Line < 1 || lastKey.Column < 1 || lastValue.Line < 1 {
		return sourceEdit{}, false, nil
	}
	if lastValue.Kind != yaml.ScalarNode || lastValue.Line != lastKey.Line {
		return sourceEdit{}, false, nil
	}
	if lastValue.Style&(yaml.LiteralStyle|yaml.FoldedStyle) != 0 {
		return sourceEdit{}, false, nil
	}
	if (lastValue.Style&(yaml.DoubleQuotedStyle|yaml.SingleQuotedStyle) != 0) && strings.Contains(lastValue.Value, "\n") {
		return sourceEdit{}, false, nil
	}
	valueStart, err := nodeContentOffset(source, lastValue)
	if err != nil {
		return sourceEdit{}, false, err
	}
	valueEnd, err := scalarEnd(source, valueStart, lastValue.Style)
	if err != nil {
		return sourceEdit{}, false, err
	}
	lineEnd := sourceLineEndOffsetFromOffset(source, valueStart)
	if valueEnd > lineEnd {
		return sourceEdit{}, false, nil
	}
	offset, hadLineBreak := sourceAfterLineOffsetFromOffset(source, valueStart)
	keyOffset, err := nodeContentOffset(source, lastKey)
	if err != nil {
		return sourceEdit{}, false, err
	}
	indent := indentForOffset(source, keyOffset)
	insertion := []byte(indent + line)
	if hadLineBreak {
		insertion = append(insertion, eol...)
	} else {
		insertion = append(append([]byte{}, eol...), insertion...)
	}
	return sourceEdit{start: offset, end: offset, replacement: insertion}, true, nil
}

func insertFlowMappingScalarEdit(source []byte, mapping *yaml.Node, line string) (sourceEdit, error) {
	if mapping.Line < 1 || mapping.Column < 1 {
		return sourceEdit{}, fmt.Errorf("flow target mapping for %s has no source position", line)
	}
	start, err := nodeContentOffset(source, mapping)
	if err != nil {
		return sourceEdit{}, err
	}
	if start >= len(source) || source[start] != '{' {
		return sourceEdit{}, fmt.Errorf("%w: flow target mapping for %s does not start at an opening brace", errSourcePreserveUnsupported, line)
	}
	closeRel := bytes.IndexByte(source[start+1:], '}')
	if closeRel < 0 {
		return sourceEdit{}, fmt.Errorf("%w: unterminated flow target mapping for %s", errSourcePreserveUnsupported, line)
	}
	closeOffset := start + 1 + closeRel
	if strings.TrimSpace(string(source[start+1:closeOffset])) == "pollIntervalSeconds:" {
		return sourceEdit{start: start + 1, end: closeOffset, replacement: []byte(line)}, nil
	}
	if len(mapping.Content) == 0 {
		body := source[start+1 : closeOffset]
		if strings.TrimSpace(string(body)) != "" {
			eol := sourceEOL(source)
			closeIndent := indentForOffset(source, closeOffset)
			replacement := append([]byte(line), eol...)
			replacement = append(replacement, []byte(closeIndent)...)
			return sourceEdit{start: closeOffset, end: closeOffset, replacement: replacement}, nil
		}
	}
	replacement := []byte(line)
	if len(mapping.Content) > 0 {
		replacement = []byte(line + ", ")
	}
	return sourceEdit{start: start + 1, end: start + 1, replacement: replacement}, nil
}

func sourceLineEndOffsetFromOffset(source []byte, offset int) int {
	for offset < len(source) && source[offset] != '\r' && source[offset] != '\n' {
		offset++
	}
	return offset
}

func indentForOffset(source []byte, offset int) string {
	start := offset
	for start > 0 && source[start-1] != '\n' && source[start-1] != '\r' {
		start--
	}
	return string(source[start:offset])
}

func sourceAfterLineOffsetFromOffset(source []byte, offset int) (int, bool) {
	next := offset
	for next < len(source) && source[next] != '\n' {
		next++
	}
	if next >= len(source) {
		return len(source), false
	}
	return next + 1, true
}
