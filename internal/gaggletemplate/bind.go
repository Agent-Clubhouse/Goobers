package gaggletemplate

import (
	"errors"
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

func setScalar(node *yaml.Node, key, value, tag string) {
	for i := 0; i+1 < len(node.Content); i += 2 {
		if node.Content[i].Value == key {
			node.Content[i+1] = &yaml.Node{Kind: yaml.ScalarNode, Tag: tag, Value: value}
			return
		}
	}
	node.Content = append(node.Content,
		&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key},
		&yaml.Node{Kind: yaml.ScalarNode, Tag: tag, Value: value})
}

// Bind adapts only typed identity fields. Imports start disabled so a template
// cannot begin working against its author's project before operator review.
func Bind(input Tree, name string) (Tree, error) {
	if !namePattern.MatchString(name) || len(name) > 63 {
		return nil, errors.New("invalid destination gaggle name")
	}
	if err := input.Validate(); err != nil {
		return nil, err
	}
	gaggle, err := parseYAML(input["gaggle.yaml"].Data)
	if err != nil {
		return nil, fmt.Errorf("package requires gaggle.yaml: %w", err)
	}
	g := mapping(gaggle)
	if g["kind"] == nil || g["kind"].Value != "Gaggle" || g["metadata"] == nil {
		return nil, errors.New("package gaggle.yaml must define one Gaggle")
	}
	original := mapping(g["metadata"])["name"]
	if original == nil {
		return nil, errors.New("template gaggle has no name")
	}
	oldName := original.Value
	documents, goobers, err := templateDefinitions(input, name)
	if err != nil {
		return nil, err
	}
	result := Tree{}
	for path, file := range input {
		result[path] = file
		doc := documents[path]
		if doc == nil {
			continue
		}
		fields := mapping(doc)
		spec, meta := fields["spec"], fields["metadata"]
		if spec == nil || meta == nil || spec.Kind != yaml.MappingNode || meta.Kind != yaml.MappingNode {
			return nil, fmt.Errorf("%s requires mapping metadata and spec", path)
		}
		switch fields["kind"].Value {
		case "Gaggle":
			setScalar(meta, "name", name, "!!str")
			setScalar(spec, "enabled", "false", "!!bool")
			isolation := mapping(spec)["isolation"]
			if isolation == nil || isolation.Kind != yaml.MappingNode {
				return nil, errors.New("template gaggle requires isolation")
			}
			setScalar(isolation, "namespace", "gaggle-"+name, "!!str")
		case "Goober", "Workflow":
			owner := mapping(spec)["gaggle"]
			if owner == nil || owner.Value != oldName {
				return nil, fmt.Errorf("%s is not owned by the template gaggle", path)
			}
			setScalar(spec, "gaggle", name, "!!str")
			if fields["kind"].Value == "Goober" {
				old := mapping(meta)["name"]
				setScalar(meta, "name", goobers[old.Value], "!!str")
			} else {
				renameGooberReferences(spec, goobers)
			}
		}
		data, err := encodeYAML(doc)
		if err != nil {
			return nil, err
		}
		result[path] = File{Data: data, Mode: file.Mode}
	}
	return result, result.Validate()
}

func templateDefinitions(input Tree, name string) (map[string]*yaml.Node, map[string]string, error) {
	goobers := map[string]string{}
	documents := map[string]*yaml.Node{}
	for path, file := range input {
		if !strings.HasSuffix(path, ".yaml") && !strings.HasSuffix(path, ".yml") {
			continue
		}
		// Skill and asset packages may contain arbitrary YAML.
		if strings.HasPrefix(path, "skills/") || strings.HasPrefix(path, "assets/") || strings.Contains(path, "/assets/") {
			continue
		}
		doc, err := parseYAML(file.Data)
		if err != nil {
			return nil, nil, fmt.Errorf("%s: %w", path, err)
		}
		fields := mapping(doc)
		kind := fields["kind"]
		if kind == nil {
			continue
		}
		switch kind.Value {
		case "Gaggle", "Goober", "Workflow":
			documents[path] = doc
		default:
			return nil, nil, fmt.Errorf("template contains unsupported definition %s in %s", kind.Value, path)
		}
		if kind.Value == "Goober" {
			meta := fields["metadata"]
			if meta == nil || mapping(meta)["name"] == nil {
				return nil, nil, fmt.Errorf("%s has no metadata.name", path)
			}
			old := mapping(meta)["name"].Value
			goobers[old] = name + "-" + old
		}
		if kind.Value == "Gaggle" && path != "gaggle.yaml" {
			return nil, nil, errors.New("a template package must contain exactly one gaggle")
		}
	}
	return documents, goobers, nil
}

func renameGooberReferences(node *yaml.Node, names map[string]string) {
	spec := mapping(node)
	for _, field := range []string{"tasks", "gates"} {
		list := spec[field]
		if list == nil {
			continue
		}
		for _, item := range list.Content {
			subject := item
			if field == "gates" {
				subject = mapping(item)["agentic"]
			}
			if subject == nil {
				continue
			}
			ref := mapping(subject)["goober"]
			if ref != nil && names[ref.Value] != "" {
				ref.Value = names[ref.Value]
			}
		}
	}
}
