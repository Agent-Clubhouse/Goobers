package onecollector

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/santhosh-tekuri/jsonschema/v5"
)

const MaxInputBytes = 16 << 20

type Input struct {
	Name               string                     `json:"name"`
	Schema             json.RawMessage            `json:"schema"`
	NormalizeKeys      bool                       `json:"normalize_keys,omitempty"`
	CollisionCheckOnly bool                       `json:"collision_check_only,omitempty"`
	RemoveBlank        []string                   `json:"remove_blank,omitempty"`
	Defaults           map[string]json.RawMessage `json:"defaults,omitempty"`
	ItemsPath          string                     `json:"items_path,omitempty"`
	RecordSchema       json.RawMessage            `json:"record_schema,omitempty"`
	Rules              []string                   `json:"rules,omitempty"`
	RecordRules        []string                   `json:"record_rules,omitempty"`
	SerializeFields    []string                   `json:"serialize_fields,omitempty"`
}

type Tool struct {
	Name            string          `json:"name"`
	Description     string          `json:"description"`
	InputSchema     json.RawMessage `json:"input_schema"`
	Input           string          `json:"input"`
	PayloadPath     string          `json:"payload_path,omitempty"`
	ValidationField string          `json:"validation_field,omitempty"`
}

func compileSchema(raw json.RawMessage) (*jsonschema.Schema, error) {
	compiler := jsonschema.NewCompiler()
	compiler.AssertContent = true
	compiler.AssertFormat = true
	compiler.LoadURL = func(string) (io.ReadCloser, error) {
		return nil, errors.New("onecollector: remote schema references forbidden")
	}
	if err := compiler.AddResource("urn:onecollector:input", bytes.NewReader(raw)); err != nil {
		return nil, errors.New("onecollector: invalid input schema")
	}
	schema, err := compiler.Compile("urn:onecollector:input")
	if err != nil {
		return nil, errors.New("onecollector: invalid input schema")
	}
	return schema, nil
}

func (p Profile) validateInputs() error {
	if len(p.Inputs) > 16 || len(p.Tools) > 32 {
		return errors.New("onecollector: input/tool count limit")
	}
	names := map[string]bool{}
	for _, input := range p.Inputs {
		if input.Name == "" || names[input.Name] || len(input.Rules) > 64 || len(input.RecordRules) > 64 || len(input.Defaults) > 128 {
			return errors.New("onecollector: invalid input declaration")
		}
		names[input.Name] = true
		if _, err := compileSchema(input.Schema); err != nil {
			return err
		}
		if len(input.RecordSchema) > 0 {
			if _, err := compileSchema(input.RecordSchema); err != nil {
				return err
			}
		}
		for _, rule := range append(append([]string{}, input.Rules...), input.RecordRules...) {
			if _, err := compileRule(rule); err != nil {
				return err
			}
		}
		for _, raw := range input.Defaults {
			if _, err := DecodeObject(`{"value":`+string(raw)+`}`, MaxProfileBytes); err != nil {
				return err
			}
		}
	}
	tools := map[string]bool{}
	for _, tool := range p.Tools {
		if tool.Name == "" || tools[tool.Name] || !names[tool.Input] {
			return errors.New("onecollector: invalid tool declaration")
		}
		tools[tool.Name] = true
		if _, err := compileSchema(tool.InputSchema); err != nil {
			return err
		}
	}
	return nil
}

func NormalizeKey(key string) (string, error) {
	first := normalizeKey(key)
	if !regexp.MustCompile(`^[a-z][A-Za-z0-9]*$`).MatchString(first) {
		return "", errors.New("onecollector: invalid normalized key")
	}
	return first, nil
}

func normalizeKey(key string) string {
	words := strings.FieldsFunc(key, func(r rune) bool { return r == '-' || r == '_' || unicode.IsSpace(r) })
	if len(words) == 0 {
		return ""
	}
	normalizeWord := func(first string) string {
		runes := []rune(first)
		upper := 0
		for upper < len(runes) && unicode.IsUpper(runes[upper]) {
			upper++
		}
		lowerCount := max(1, upper-1)
		if upper == len(runes) {
			lowerCount = upper
		}
		for i := 0; i < lowerCount; i++ {
			runes[i] = unicode.ToLower(runes[i])
		}
		return string(runes)
	}
	first := normalizeWord(words[0])
	for _, word := range words[1:] {
		word = normalizeWord(word)
		runes := []rune(word)
		runes[0] = unicode.ToUpper(runes[0])
		first += string(runes)
	}
	return first
}

func normalizeValue(value any, rewrite bool) error {
	switch typed := value.(type) {
	case map[string]any:
		normalized := make(map[string]bool, len(typed))
		result := make(map[string]any, len(typed))
		for key, child := range typed {
			name := normalizeKey(key)
			if rewrite {
				var err error
				name, err = NormalizeKey(key)
				if err != nil {
					return err
				}
			}
			if normalized[name] {
				return errors.New("onecollector: normalized key collision")
			}
			normalized[name] = true
			if err := normalizeValue(child, rewrite); err != nil {
				return err
			}
			if rewrite {
				result[name] = child
			} else {
				result[key] = child
			}
		}
		clear(typed)
		for key, child := range result {
			typed[key] = child
		}
	case []any:
		for _, child := range typed {
			if err := normalizeValue(child, rewrite); err != nil {
				return err
			}
		}
	}
	return nil
}

func selectValue(value any, path string) (any, bool) {
	if path == "" || path == "$" {
		return value, true
	}
	for _, part := range strings.Split(strings.TrimPrefix(path, "$."), ".") {
		switch typed := value.(type) {
		case map[string]any:
			value = typed[part]
		case []any:
			index, err := strconv.Atoi(part)
			if err != nil || index < 0 || index >= len(typed) {
				return nil, false
			}
			value = typed[index]
		default:
			return nil, false
		}
		if value == nil {
			return nil, false
		}
	}
	return value, true
}

func validateInputSchema(raw json.RawMessage, value any) error {
	schema, err := compileSchema(raw)
	if err != nil {
		return err
	}
	if err := schema.Validate(value); err != nil {
		return errors.New("onecollector: input schema validation failed")
	}
	return nil
}

func inputFacts(value any, name string) Facts {
	facts := Facts{"input_name": JSON(name)}
	var visit func(any, string)
	visit = func(value any, path string) {
		facts[path] = JSON(value)
		switch typed := value.(type) {
		case map[string]any:
			for key, child := range typed {
				visit(child, path+"."+key)
			}
		case []any:
			for i, child := range typed {
				visit(child, path+"."+strconv.Itoa(i))
			}
		}
	}
	visit(value, "input")
	if object, ok := value.(map[string]any); ok {
		for key, value := range object {
			if text, ok := value.(string); ok {
				if decoded, err := DecodeObject(text, MaxNativeBytes); err == nil {
					visit(decoded, "input_json."+key)
				}
			}
		}
	}
	return facts
}

func prepareInputRoot(config Input, raw []byte) (any, error) {
	valueRoot, err := DecodeJSON(string(raw), MaxInputBytes)
	if err != nil {
		return nil, err
	}
	object, isObject := valueRoot.(map[string]any)
	if !isObject && (len(config.RemoveBlank) > 0 || len(config.Defaults) > 0) {
		return nil, errors.New("onecollector: object required for defaults")
	}
	if config.NormalizeKeys || config.CollisionCheckOnly {
		if err := normalizeValue(valueRoot, config.NormalizeKeys); err != nil {
			return nil, err
		}
	}
	for _, field := range config.RemoveBlank {
		if value, ok := object[field].(string); ok && strings.TrimSpace(value) == "" {
			delete(object, field)
		}
	}
	for key, raw := range config.Defaults {
		if _, exists := object[key]; !exists {
			decoder := json.NewDecoder(bytes.NewReader(raw))
			decoder.UseNumber()
			var value any
			if decoder.Decode(&value) != nil {
				return nil, errors.New("onecollector: invalid default")
			}
			object[key] = value
		}
	}
	if err := validateInputSchema(config.Schema, valueRoot); err != nil {
		return nil, err
	}
	if err := evaluateRules(config.Rules, valueRoot); err != nil {
		return nil, err
	}
	return valueRoot, nil
}

func prepareInputRecord(config Input, item any) (map[string]any, error) {
	record, ok := item.(map[string]any)
	if !ok {
		return nil, errors.New("onecollector: input record must be object")
	}
	if len(config.RecordSchema) > 0 {
		if err := validateInputSchema(config.RecordSchema, record); err != nil {
			return nil, err
		}
	}
	if err := evaluateRules(config.RecordRules, record); err != nil {
		return nil, err
	}
	for _, field := range config.SerializeFields {
		if value, found := record[field]; found {
			if _, isString := value.(string); isString {
				continue
			}
			raw, err := compactJSON(value)
			if err != nil {
				return nil, errors.New("onecollector: cannot serialize field")
			}
			record[field] = string(raw)
		}
	}
	return record, nil
}

// PrepareInput performs all validation and serialization before any opt-in or
// credential lookup. Output remains bounded even for profile-selected batches.
func (p Profile) PrepareInput(name string, raw []byte) ([]Envelope, error) {
	var config *Input
	for i := range p.Inputs {
		if p.Inputs[i].Name == name {
			config = &p.Inputs[i]
			break
		}
	}
	if config == nil {
		return nil, errors.New("onecollector: unknown input")
	}
	valueRoot, err := prepareInputRoot(*config, raw)
	if err != nil {
		return nil, err
	}
	value, found := selectValue(valueRoot, config.ItemsPath)
	if !found {
		return nil, errors.New("onecollector: missing items path")
	}
	items := []any{value}
	if config.ItemsPath != "" {
		var ok bool
		items, ok = value.([]any)
		if !ok {
			return nil, errors.New("onecollector: items path must select array")
		}
	}
	if len(items) < 1 || len(items) > MaxRecords {
		return nil, errors.New("onecollector: input record limit")
	}
	var envelopes []Envelope
	timestamp := time.Now().UTC().Format(time.RFC3339Nano)
	for _, item := range items {
		record, err := prepareInputRecord(*config, item)
		if err != nil {
			return nil, err
		}
		facts := inputFacts(record, name)
		matched := false
		for _, mapping := range p.Mappings {
			if mapping.Signal != "input" || !mapping.Matches(facts) {
				continue
			}
			envelope, err := mapping.Envelope(facts, "", timestamp)
			if err != nil {
				return nil, err
			}
			envelopes = append(envelopes, envelope)
			matched = true
			if len(envelopes) > MaxRecords {
				return nil, errors.New("onecollector: mapped record limit")
			}
		}
		if !matched {
			return nil, errors.New("onecollector: no input mapping matched")
		}
	}
	if _, err := Serialize(envelopes); err != nil {
		return nil, err
	}
	return envelopes, nil
}

func (p Profile) ToolPayload(name string, raw json.RawMessage) (string, []byte, bool, error) {
	for _, tool := range p.Tools {
		if tool.Name != name {
			continue
		}
		object, err := DecodeObject(string(raw), MaxInputBytes)
		if err != nil {
			return "", nil, false, err
		}
		if err := validateInputSchema(tool.InputSchema, object); err != nil {
			return "", nil, false, err
		}
		validate := false
		if tool.ValidationField != "" {
			if value, found := object[tool.ValidationField]; found {
				var ok bool
				validate, ok = value.(bool)
				if !ok {
					return "", nil, false, errors.New("onecollector: validation switch must be Boolean")
				}
				delete(object, tool.ValidationField)
			}
		}
		payload, found := selectValue(object, tool.PayloadPath)
		if !found {
			return "", nil, false, errors.New("onecollector: missing tool payload")
		}
		body, err := json.Marshal(payload)
		return tool.Input, body, validate, err
	}
	return "", nil, false, fmt.Errorf("onecollector: unknown tool")
}
