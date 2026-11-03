package onecollector

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"unicode/utf8"
)

func compactJSON(value any) ([]byte, error) {
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buffer.Bytes(), []byte{'\n'}), nil
}

func DecodeObject(text string, limit int) (map[string]any, error) {
	value, err := DecodeJSON(text, limit)
	if err != nil {
		return nil, err
	}
	object, ok := value.(map[string]any)
	if !ok {
		return nil, errors.New("onecollector: JSON object required")
	}
	return object, nil
}

func DecodeJSON(text string, limit int) (any, error) {
	if len(text) > limit || !utf8.ValidString(text) {
		return nil, errors.New("onecollector: JSON byte limit or UTF-8")
	}
	decoder := json.NewDecoder(strings.NewReader(text))
	decoder.UseNumber()
	value, err := decodeValue(decoder, 1)
	if err != nil {
		return nil, err
	}
	if _, err := decoder.Token(); err != io.EOF {
		return nil, errors.New("onecollector: trailing JSON")
	}
	return value, nil
}

func decodeValue(decoder *json.Decoder, depth int) (any, error) {
	token, err := decoder.Token()
	if err != nil {
		return nil, errors.New("onecollector: malformed JSON")
	}
	delimiter, container := token.(json.Delim)
	if !container {
		return token, nil
	}
	if depth > 32 {
		return nil, errors.New("onecollector: JSON nesting limit")
	}
	if delimiter == '{' {
		result := map[string]any{}
		for decoder.More() {
			token, err := decoder.Token()
			if err != nil {
				return nil, errors.New("onecollector: malformed JSON key")
			}
			key, ok := token.(string)
			if !ok {
				return nil, errors.New("onecollector: JSON key type")
			}
			if _, found := result[key]; found {
				return nil, errors.New("onecollector: duplicate JSON key")
			}
			value, err := decodeValue(decoder, depth+1)
			if err != nil {
				return nil, err
			}
			result[key] = value
		}
		if end, err := decoder.Token(); err != nil || end != json.Delim('}') {
			return nil, errors.New("onecollector: malformed JSON object")
		}
		return result, nil
	}
	if delimiter == '[' {
		result := []any{}
		for decoder.More() {
			value, err := decodeValue(decoder, depth+1)
			if err != nil {
				return nil, err
			}
			result = append(result, value)
		}
		if end, err := decoder.Token(); err != nil || end != json.Delim(']') {
			return nil, errors.New("onecollector: malformed JSON array")
		}
		return result, nil
	}
	return nil, errors.New("onecollector: unexpected JSON delimiter")
}
