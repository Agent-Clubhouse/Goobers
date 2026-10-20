package interactiveagent

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"strings"

	"github.com/santhosh-tekuri/jsonschema/v5"

	"github.com/goobers/goobers/internal/secretpattern"
)

var toolName = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)

type compiledTool struct {
	definition Tool
	schema     *jsonschema.Schema
}

func compileTools(definitions []Tool) (map[string]compiledTool, []Tool, error) {
	if len(definitions) == 0 || len(definitions) > 32 {
		return nil, nil, ErrInvalidConfiguration
	}
	compiled := map[string]compiledTool{}
	retained := make([]Tool, 0, len(definitions))
	for i, definition := range definitions {
		if !toolName.MatchString(definition.Name) || forbiddenTool(definition.Name) || len(definition.Description) > 4096 {
			return nil, nil, ErrInvalidConfiguration
		}
		if _, exists := compiled[definition.Name]; exists {
			return nil, nil, ErrInvalidConfiguration
		}
		raw, err := json.Marshal(definition)
		if err != nil || len(raw) > MaxRequestBytes || unsafeText(raw) {
			return nil, nil, ErrInvalidConfiguration
		}
		var copy Tool
		if json.Unmarshal(raw, &copy) != nil {
			return nil, nil, ErrInvalidConfiguration
		}
		if copy.InputSchema["type"] != "object" || copy.InputSchema["additionalProperties"] != false {
			return nil, nil, ErrInvalidConfiguration
		}
		if !schemaPropertiesSafe(copy.InputSchema) {
			return nil, nil, ErrInvalidConfiguration
		}
		schemaJSON, _ := json.Marshal(copy.InputSchema)
		compiler := jsonschema.NewCompiler()
		compiler.Draft = jsonschema.Draft2020
		compiler.LoadURL = func(string) (io.ReadCloser, error) { return nil, ErrInvalidConfiguration }
		uri := fmt.Sprintf("https://goobers.invalid/interactive-tool-%d.json", i)
		if compiler.AddResource(uri, bytes.NewReader(schemaJSON)) != nil {
			return nil, nil, ErrInvalidConfiguration
		}
		schema, err := compiler.Compile(uri)
		if err != nil {
			return nil, nil, ErrInvalidConfiguration
		}
		compiled[copy.Name] = compiledTool{definition: copy, schema: schema}
		retained = append(retained, copy)
	}
	return compiled, retained, nil
}

func forbiddenTool(name string) bool {
	for _, part := range strings.Split(name, "_") {
		switch part {
		case "approve", "approval", "authorize", "grant", "credential", "credentials", "token", "reauthenticate":
			return true
		}
	}
	return false
}

func reservedArgument(name string) bool {
	key := strings.ToLower(strings.NewReplacer("_", "", "-", "").Replace(name))
	switch key {
	case "owner", "ownerid", "session", "sessionid", "principal", "principalid", "actor", "actorid", "authority", "authorityref", "connectionid", "lease", "leaseid", "turnid", "fence", "executionfence", "token", "accesstoken", "refreshtoken", "credentials", "authorization", "approval", "approved", "decision", "decisionid":
		return true
	}
	return false
}

// Every declared object is closed. The host still validates domain-specific
// arguments; this layer forbids transport callers from providing authority.
func schemaPropertiesSafe(value any) bool {
	switch node := value.(type) {
	case map[string]any:
		if node["type"] == "object" && node["additionalProperties"] != false {
			return false
		}
		if properties, ok := node["properties"].(map[string]any); ok {
			for name := range properties {
				if reservedArgument(name) {
					return false
				}
			}
		}
		for _, v := range node {
			if !schemaPropertiesSafe(v) {
				return false
			}
		}
	case []any:
		for _, v := range node {
			if !schemaPropertiesSafe(v) {
				return false
			}
		}
	}
	return true
}

func unsafeText(raw []byte) bool { return !bytes.Equal(raw, secretpattern.NewScrubber().Scrub(raw)) }

// uniqueJSON rejects duplicate keys and excessive nesting before either the
// protocol decoder or schema validator can reinterpret the caller's intent.
func uniqueJSON(raw []byte) (any, error) {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	v, err := jsonValue(d, 0)
	if err != nil {
		return nil, ErrInvalidMessage
	}
	if _, err = d.Token(); err != io.EOF {
		return nil, ErrInvalidMessage
	}
	return v, nil
}

func jsonValue(d *json.Decoder, depth int) (any, error) {
	if depth > 32 {
		return nil, ErrInvalidMessage
	}
	token, err := d.Token()
	if err != nil {
		return nil, err
	}
	delim, isDelim := token.(json.Delim)
	if !isDelim {
		return token, nil
	}
	switch delim {
	case '{':
		object := map[string]any{}
		for d.More() {
			keyToken, err := d.Token()
			if err != nil {
				return nil, err
			}
			key, ok := keyToken.(string)
			if !ok {
				return nil, ErrInvalidMessage
			}
			if _, exists := object[key]; exists {
				return nil, ErrInvalidMessage
			}
			v, err := jsonValue(d, depth+1)
			if err != nil {
				return nil, err
			}
			object[key] = v
		}
		end, err := d.Token()
		if err != nil || end != json.Delim('}') {
			return nil, ErrInvalidMessage
		}
		return object, nil
	case '[':
		array := []any{}
		for d.More() {
			v, err := jsonValue(d, depth+1)
			if err != nil {
				return nil, err
			}
			array = append(array, v)
		}
		end, err := d.Token()
		if err != nil || end != json.Delim(']') {
			return nil, ErrInvalidMessage
		}
		return array, nil
	default:
		return nil, ErrInvalidMessage
	}
}

func strictDecode(raw []byte, target any) error {
	if _, err := uniqueJSON(raw); err != nil {
		return err
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if d.Decode(target) != nil {
		return ErrInvalidMessage
	}
	if d.Decode(new(any)) != io.EOF {
		return ErrInvalidMessage
	}
	return nil
}
