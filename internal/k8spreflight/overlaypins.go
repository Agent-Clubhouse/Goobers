package k8spreflight

import (
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

var overlayCommit = regexp.MustCompile(`^([0-9a-f]{40})(-[A-Za-z0-9_.-]+)?$`)

type overlayPin struct {
	Path, Kind, Image, Commit, Suffix string
	Line                              int
}

type overlayReader struct {
	pins    []overlayPin
	visited map[string]bool
	bytes   int64
	depth   int
}

// collectOverlayPins follows the actual local kustomization inputs. It neither
// greps unrelated files/comments nor fetches remote bases merely to inspect a
// ref. Local bases outside the overlay directory are included, with bounded
// file and byte counts and cycle detection.
func collectOverlayPins(root string) ([]overlayPin, error) {
	r := overlayReader{visited: map[string]bool{}}
	if err := r.visit(root); err != nil {
		return nil, err
	}
	return r.pins, nil
}

func (r *overlayReader) visit(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if info.IsDir() {
		for _, name := range []string{"kustomization.yaml", "kustomization.yml", "Kustomization"} {
			candidate := filepath.Join(path, name)
			if _, err := os.Stat(candidate); err == nil {
				return r.visit(candidate)
			}
		}
		return fmt.Errorf("%s: no kustomization file", path)
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return err
	}
	resolved, err = filepath.Abs(resolved)
	if err != nil {
		return err
	}
	if r.visited[resolved] {
		return nil
	}
	if len(r.visited) >= 512 || !info.Mode().IsRegular() || info.Size() > 4<<20 || r.bytes+info.Size() > 32<<20 {
		return fmt.Errorf("%s: overlay input is not a bounded regular file (512 files, 4 MiB/file, 32 MiB total)", path)
	}
	r.visited[resolved] = true
	r.bytes += info.Size()
	file, err := os.Open(resolved)
	if err != nil {
		return err
	}
	defer func() { _ = file.Close() }()
	data, err := io.ReadAll(io.LimitReader(file, (4<<20)+1))
	if err != nil {
		return err
	}
	if len(data) > 4<<20 {
		return fmt.Errorf("%s: overlay input grew beyond 4 MiB", path)
	}
	return r.documents(resolved, data)
}

func (r *overlayReader) documents(path string, data []byte) error {
	if r.depth >= 16 {
		return fmt.Errorf("%s: nested YAML exceeds the overlay inspection depth", path)
	}
	r.depth++
	defer func() { r.depth-- }()
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	for {
		var doc yaml.Node
		if err := decoder.Decode(&doc); err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return fmt.Errorf("%s: %w", path, err)
		}
		if len(doc.Content) == 0 {
			continue
		}
		root := doc.Content[0]
		if err := r.secretData(path, root); err != nil {
			return err
		}
		if err := r.scanPins(path, root, ""); err != nil {
			return err
		}
		name := filepath.Base(path)
		if nodeValue(root, "kind") == "Kustomization" || name == "kustomization.yaml" || name == "kustomization.yml" || name == "Kustomization" {
			if err := r.kustomization(path, root); err != nil {
				return err
			}
		}
	}
}

func nodeField(node *yaml.Node, key string) *yaml.Node {
	if node == nil || node.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		if node.Content[i].Value == key {
			return node.Content[i+1]
		}
	}
	return nil
}

func (r *overlayReader) secretData(path string, root *yaml.Node) error {
	if nodeValue(root, "kind") != "Secret" {
		return nil
	}
	data := nodeField(root, "data")
	if data == nil {
		return nil
	}
	if data.Kind != yaml.MappingNode {
		return fmt.Errorf("%s: Secret data must be a mapping", path)
	}
	for i := 0; i+1 < len(data.Content); i += 2 {
		key, value := data.Content[i].Value, data.Content[i+1]
		if !yamlFilename(key) {
			continue
		}
		decoded, err := base64.StdEncoding.Strict().DecodeString(value.Value)
		if err != nil {
			return fmt.Errorf("%s: Secret YAML entry %s is not valid base64", path, key)
		}
		if err := r.documents(path+"[Secret "+key+"]", decoded); err != nil {
			return err
		}
	}
	return nil
}

func nodeValue(node *yaml.Node, key string) string {
	if field := nodeField(node, key); field != nil {
		return field.Value
	}
	return ""
}

func (r *overlayReader) kustomization(path string, root *yaml.Node) error {
	for _, key := range []string{"resources", "bases", "components", "patchesStrategicMerge"} {
		if entries := nodeField(root, key); entries != nil {
			for _, entry := range entries.Content {
				if err := r.resource(path, entry); err != nil {
					return err
				}
			}
		}
	}
	for _, key := range []string{"patches", "patchesJson6902"} {
		if entries := nodeField(root, key); entries != nil {
			for _, entry := range entries.Content {
				if patch := nodeField(entry, "patch"); patch != nil {
					if err := r.documents(path+"[inline patch]", []byte(patch.Value)); err != nil {
						return err
					}
				}
				if source := nodeField(entry, "path"); source != nil {
					if err := r.resource(path, source); err != nil {
						return err
					}
				}
			}
		}
	}
	for _, key := range []string{"configMapGenerator", "secretGenerator"} {
		if generators := nodeField(root, key); generators != nil {
			for _, generator := range generators.Content {
				if err := r.generator(path, generator); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func (r *overlayReader) generator(path string, generator *yaml.Node) error {
	if literals := nodeField(generator, "literals"); literals != nil {
		for _, literal := range literals.Content {
			name, value, ok := strings.Cut(literal.Value, "=")
			if ok && yamlFilename(name) {
				if err := r.documents(path+"[literal "+name+"]", []byte(value)); err != nil {
					return err
				}
			}
		}
	}
	if files := nodeField(generator, "files"); files != nil {
		for _, file := range files.Content {
			alias, source, ok := strings.Cut(file.Value, "=")
			if !ok {
				source = alias
			}
			if yamlFilename(alias) || yamlFilename(source) {
				if err := r.visit(filepath.Join(filepath.Dir(path), source)); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func yamlFilename(path string) bool {
	ext := strings.ToLower(filepath.Ext(path))
	return ext == ".yaml" || ext == ".yml"
}

func (r *overlayReader) resource(path string, entry *yaml.Node) error {
	value := entry.Value
	if strings.Contains(strings.ToLower(value), "github.com/agent-clubhouse/goobers//") {
		parsed, err := url.Parse(value)
		if err != nil {
			return fmt.Errorf("%s:%d: invalid upstream base URL", path, entry.Line)
		}
		refs := parsed.Query()["ref"]
		if len(refs) != 1 || len(refs[0]) != 40 || !overlayCommit.MatchString(refs[0]) {
			return fmt.Errorf("%s:%d: upstream base must carry exactly one full 40-hex commit ref", path, entry.Line)
		}
		r.pins = append(r.pins, overlayPin{Path: path, Line: entry.Line, Kind: "base-ref", Commit: refs[0]})
		return nil
	}
	if strings.Contains(value, "://") || strings.Contains(value, "git@") || strings.HasPrefix(value, "github.com/") || strings.HasPrefix(value, "git::") {
		return nil // Not an upstream Goobers pin; rendering validates this base.
	}
	if value == "" || strings.Contains(value, "\n") {
		return fmt.Errorf("%s:%d: unsupported non-path kustomization input", path, entry.Line)
	}
	return r.visit(filepath.Join(filepath.Dir(path), value))
}

func goobersImage(image string) bool {
	name := strings.Split(image, "@")[0]
	name = name[strings.LastIndex(name, "/")+1:]
	name = strings.Split(name, ":")[0]
	return name == "goobers" || strings.HasPrefix(name, "goobers-")
}

func (r *overlayReader) imagePin(path string, node *yaml.Node, kind, image, tag string) error {
	if tag == "" {
		imageWithoutDigest := strings.Split(image, "@")[0]
		index := strings.LastIndex(imageWithoutDigest, ":")
		if index > strings.LastIndex(imageWithoutDigest, "/") {
			tag = imageWithoutDigest[index+1:]
		}
	}
	match := overlayCommit.FindStringSubmatch(tag)
	if match == nil {
		return fmt.Errorf("%s:%d: %s image requires a full commit tag, optionally followed by an identity suffix", path, node.Line, kind)
	}
	r.pins = append(r.pins, overlayPin{Path: path, Line: node.Line, Kind: kind, Image: image, Commit: match[1], Suffix: match[2]})
	return nil
}

func (r *overlayReader) scanPins(path string, node *yaml.Node, parent string) error {
	if node.Kind == yaml.ScalarNode && parent == "images" && goobersImage(node.Value) {
		return r.imagePin(path, node, "image", node.Value, "")
	}
	if node.Kind == yaml.AliasNode {
		return fmt.Errorf("%s:%d: YAML aliases are not supported for overlay pin inspection", path, node.Line)
	}
	switch node.Kind {
	case yaml.MappingNode:
		if err := r.patchPin(path, node); err != nil {
			return err
		}
		if parent == "images" {
			if err := r.imageEntry(path, node); err != nil {
				return err
			}
		}
		for i := 0; i+1 < len(node.Content); i += 2 {
			key, value := node.Content[i].Value, node.Content[i+1]
			if (parent == "data" || parent == "stringData") && yamlFilename(key) && value.Kind == yaml.ScalarNode {
				if err := r.documents(path+"["+key+"]", []byte(value.Value)); err != nil {
					return err
				}
			}
			if (key == "host" && parent == "runners" || key == "image") && goobersImage(value.Value) {
				if err := r.imagePin(path, value, key, value.Value, ""); err != nil {
					return err
				}
			}
			if err := r.scanPins(path, value, key); err != nil {
				return err
			}
		}
	case yaml.SequenceNode:
		for _, child := range node.Content {
			if err := r.scanPins(path, child, parent); err != nil {
				return err
			}
		}
	}
	return nil
}

func (r *overlayReader) imageEntry(path string, node *yaml.Node) error {
	if !goobersImage(nodeValue(node, "name")) && !goobersImage(nodeValue(node, "newName")) {
		return nil
	}
	image := nodeValue(node, "newName")
	if image == "" {
		image = nodeValue(node, "name")
	}
	tag := nodeValue(node, "newTag")
	if tag != "" {
		image += ":" + tag
	}
	return r.imagePin(path, node, "image", image, tag)
}

func (r *overlayReader) patchPin(path string, node *yaml.Node) error {
	operation, target := nodeValue(node, "op"), nodeValue(node, "path")
	if operation != "add" && operation != "replace" {
		return nil
	}
	value := nodeField(node, "value")
	if value == nil || !goobersImage(value.Value) {
		return nil
	}
	if strings.HasSuffix(target, "/image") || strings.Contains(target, "/runners/") && strings.HasSuffix(target, "/host") {
		return r.imagePin(path, value, "patch", value.Value, "")
	}
	return nil
}
