package gaggletemplate

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"reflect"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/goobers/goobers/internal/strictyaml"
)

// Merge never inserts conflict markers. Lists without stable unique names and
// non-YAML files are atomic values; competing edits require human resolution.
func Merge(base, local, upstream Tree) (Tree, []string, error) {
	for _, tree := range []Tree{base, local, upstream} {
		if err := tree.Validate(); err != nil {
			return nil, nil, err
		}
	}
	keys := Tree{}
	for _, tree := range []Tree{base, local, upstream} {
		for key := range tree {
			keys[key] = File{}
		}
	}
	result := Tree{}
	var conflicts []string
	for _, path := range sortedKeys(keys) {
		b, bok := base[path]
		l, lok := local[path]
		u, uok := upstream[path]
		switch {
		case equalFile(l, lok, b, bok):
			if uok {
				result[path] = u
			}
		case equalFile(u, uok, b, bok), equalFile(l, lok, u, uok):
			if lok {
				result[path] = l
			}
		default:
			if !bok || !lok || !uok || l.Mode != b.Mode || u.Mode != b.Mode ||
				(!strings.HasSuffix(path, ".yaml") && !strings.HasSuffix(path, ".yml")) {
				conflicts = append(conflicts, path)
				continue
			}
			data, err := mergeYAML(b.Data, l.Data, u.Data)
			if err != nil {
				conflicts = append(conflicts, path+": "+err.Error())
				continue
			}
			result[path] = File{Data: data, Mode: l.Mode}
		}
	}
	return result, conflicts, nil
}

func parseYAML(data []byte) (*yaml.Node, error) {
	if _, err := strictyaml.YAMLToJSON(data); err != nil {
		return nil, err
	}
	var node yaml.Node
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	if err := decoder.Decode(&node); err != nil {
		return nil, err
	}
	var extra yaml.Node
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return nil, errors.New("template files must contain exactly one YAML document")
	}
	if len(node.Content) != 1 {
		return nil, errors.New("expected one YAML document")
	}
	if hasAlias(&node) {
		return nil, errors.New("template YAML aliases are unsupported; expand anchors before enrollment")
	}
	return node.Content[0], nil
}

func hasAlias(node *yaml.Node) bool {
	if node.Kind == yaml.AliasNode {
		return true
	}
	for _, child := range node.Content {
		if hasAlias(child) {
			return true
		}
	}
	return false
}

func encodeYAML(node *yaml.Node) ([]byte, error) {
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(node); err != nil {
		return nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func nodeEqual(a, b *yaml.Node) bool {
	if a == nil || b == nil {
		return a == b
	}
	var av, bv any
	if a.Decode(&av) != nil || b.Decode(&bv) != nil {
		return false
	}
	return reflect.DeepEqual(av, bv)
}

func mapping(node *yaml.Node) map[string]*yaml.Node {
	out := map[string]*yaml.Node{}
	for i := 0; i+1 < len(node.Content); i += 2 {
		out[node.Content[i].Value] = node.Content[i+1]
	}
	return out
}

func namedSequence(node *yaml.Node) (map[string]*yaml.Node, bool) {
	out := map[string]*yaml.Node{}
	for _, item := range node.Content {
		if item.Kind != yaml.MappingNode {
			return nil, false
		}
		name := mapping(item)["name"]
		if name == nil || name.Kind != yaml.ScalarNode || name.Value == "" || out[name.Value] != nil {
			return nil, false
		}
		out[name.Value] = item
	}
	return out, true
}

func mergeYAML(base, local, upstream []byte) ([]byte, error) {
	b, err := parseYAML(base)
	if err != nil {
		return nil, err
	}
	l, err := parseYAML(local)
	if err != nil {
		return nil, err
	}
	u, err := parseYAML(upstream)
	if err != nil {
		return nil, err
	}
	out, err := mergeNode(b, l, u, "$")
	if err != nil {
		return nil, err
	}
	return encodeYAML(out)
}

func mergeNode(b, l, u *yaml.Node, path string) (*yaml.Node, error) {
	switch {
	case nodeEqual(l, b):
		return u, nil
	case nodeEqual(u, b), nodeEqual(l, u):
		return l, nil
	case b == nil || l == nil || u == nil:
		return nil, fmt.Errorf("conflict at %s (add/delete versus edit)", path)
	case b.Kind != l.Kind || l.Kind != u.Kind:
		return nil, fmt.Errorf("conflict at %s (type changed)", path)
	}
	var bm, lm, um map[string]*yaml.Node
	switch l.Kind {
	case yaml.MappingNode:
		bm, lm, um = mapping(b), mapping(l), mapping(u)
	case yaml.SequenceNode:
		if path != "$.spec.tasks" && path != "$.spec.gates" {
			return nil, fmt.Errorf("conflict at %s (ordered list)", path)
		}
		var bok, lok, uok bool
		bm, bok = namedSequence(b)
		lm, lok = namedSequence(l)
		um, uok = namedSequence(u)
		if !bok || !lok || !uok {
			return nil, fmt.Errorf("conflict at %s (unnamed list)", path)
		}
		if reordered(b, l) || reordered(b, u) {
			return nil, fmt.Errorf("conflict at %s (reordering versus edit)", path)
		}
	default:
		return nil, fmt.Errorf("conflict at %s", path)
	}
	keys := orderedNodeKeys(l)
	for _, key := range orderedNodeKeys(u) {
		if _, ok := lm[key]; !ok {
			keys = append(keys, key)
		}
	}
	out := *l
	out.Content = nil
	for _, key := range keys {
		value, err := mergeNode(bm[key], lm[key], um[key], path+"."+key)
		if err != nil {
			return nil, err
		}
		if value == nil {
			continue
		}
		if l.Kind == yaml.MappingNode {
			out.Content = append(out.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key})
		}
		out.Content = append(out.Content, value)
	}
	return &out, nil
}

func reordered(base, changed *yaml.Node) bool {
	positions := map[string]int{}
	for i, name := range orderedNodeKeys(base) {
		positions[name] = i
	}
	last := -1
	for _, name := range orderedNodeKeys(changed) {
		if position, ok := positions[name]; ok {
			if position < last {
				return true
			}
			last = position
		}
	}
	return false
}

func orderedNodeKeys(node *yaml.Node) []string {
	var keys []string
	if node.Kind == yaml.MappingNode {
		for i := 0; i+1 < len(node.Content); i += 2 {
			keys = append(keys, node.Content[i].Value)
		}
		return keys
	}
	for _, item := range node.Content {
		keys = append(keys, mapping(item)["name"].Value)
	}
	return keys
}

// Changes returns sorted paths whose content, permissions or presence differ.
func Changes(base, other Tree) []string {
	var changes []string
	union := Tree{}
	for key := range base {
		union[key] = File{}
	}

	for key := range other {
		union[key] = File{}
	}
	for key := range union {
		b, bok := base[key]
		o, ook := other[key]
		if !equalFile(b, bok, o, ook) {
			changes = append(changes, key)
		}
	}
	sort.Strings(changes)
	return changes
}

// Equivalent ignores YAML formatting while retaining file and permission changes.
func Equivalent(a, b Tree) bool {
	if len(a) != len(b) {
		return false
	}
	for name, first := range a {
		second, ok := b[name]
		if !ok || first.Mode != second.Mode {
			return false
		}
		if bytes.Equal(first.Data, second.Data) {
			continue
		}
		if !strings.HasSuffix(name, ".yaml") && !strings.HasSuffix(name, ".yml") {
			return false
		}
		an, aerr := parseYAML(first.Data)
		bn, berr := parseYAML(second.Data)
		if aerr != nil || berr != nil || !nodeEqual(an, bn) {
			return false
		}
	}
	return true
}
