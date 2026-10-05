// Package eventing defines the bounded, gaggle-scoped event publication contract.
// It deliberately has no provider, HTTP authentication or execution dependency.
package eventing

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"
)

// MaxEnvelopeBytes bounds the structured CloudEvents JSON profile used by Goobers.
// Binary payloads must be supplied through authorized artifact references in data.
const MaxEnvelopeBytes = 16 << 10

// Envelope contains immutable canonical JSON and the routing attributes. Data and
// extensions remain in JSON; they never carry trusted producer or gaggle scope.
type Envelope struct {
	ID, Source, Type, Subject string
	JSON                      []byte
	Digest                    string
}

// Parse accepts CloudEvents 1.0 structured JSON with JSON data. Object key order
// and insignificant whitespace are normalized; number spellings are preserved.
// Duplicate keys (including in data) and overly nested inputs are refused.
// This is a bounded JSON-only profile, not a generic CloudEvents transport SDK.
func Parse(raw []byte) (Envelope, error) {
	if len(raw) == 0 || len(raw) > MaxEnvelopeBytes || !utf8.Valid(raw) {
		return Envelope{}, errors.New("eventing: invalid envelope size or UTF-8")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	value, err := decodeValue(decoder, 0)
	if err != nil {
		return Envelope{}, fmt.Errorf("eventing: invalid JSON: %w", err)
	}
	if _, err = decoder.Token(); !errors.Is(err, io.EOF) {
		return Envelope{}, errors.New("eventing: expected one JSON object")
	}
	object, ok := value.(map[string]any)
	if !ok {
		return Envelope{}, errors.New("eventing: expected an envelope object")
	}
	if err := validateAttributes(object); err != nil {
		return Envelope{}, err
	}
	canonical, err := json.Marshal(object)
	if err != nil || len(canonical) > MaxEnvelopeBytes {
		return Envelope{}, errors.New("eventing: canonical envelope exceeds size limit")
	}
	result := Envelope{ID: object["id"].(string), Source: object["source"].(string), Type: object["type"].(string), JSON: canonical}
	result.Subject, _ = object["subject"].(string)
	result.Digest = fmt.Sprintf("sha256:%x", sha256.Sum256(canonical))
	return result, nil
}

func validateAttributes(object map[string]any) error {
	for key, value := range object {
		if err := validateAttribute(key, value); err != nil {
			return err
		}
	}
	for _, key := range []string{"specversion", "id", "source", "type"} {
		value, ok := object[key].(string)
		if !ok || value == "" {
			return fmt.Errorf("eventing: %s must be a nonempty string", key)
		}
	}
	if object["specversion"] != "1.0" {
		return errors.New("eventing: specversion must be 1.0")
	}
	if value, exists := object["datacontenttype"]; exists {
		mediaType, _, err := mime.ParseMediaType(value.(string))
		if err != nil || (mediaType != "application/json" && !strings.HasSuffix(mediaType, "+json")) {
			return errors.New("eventing: only JSON data content types are supported")
		}
	}
	return nil
}

func validateAttribute(key string, value any) error {
	if key == "data" {
		return nil
	}
	if !attributeName(key) || key == "data_base64" {
		return errors.New("eventing: unsupported attribute name")
	}
	// Trusted authority is carried separately, never accepted as an extension.
	if strings.HasPrefix(key, "goobers") {
		return errors.New("eventing: goobers attributes are server-owned")
	}
	if err := attributeScalar(value); err != nil {
		return fmt.Errorf("eventing: %s: %w", key, err)
	}
	switch key {
	case "specversion", "id", "source", "type", "subject", "time", "dataschema", "datacontenttype":
		text, ok := value.(string)
		if !ok || text == "" {
			return fmt.Errorf("eventing: %s must be a nonempty string", key)
		}
		return validateKnownString(key, text)
	}
	return nil
}

func validateKnownString(key, value string) error {
	if (key == "id" && len(value) > 256) || (key == "type" && len(value) > 256) || len(value) > 1024 {
		return fmt.Errorf("eventing: %s exceeds attribute size limit", key)
	}
	if key == "time" {
		if _, err := time.Parse(time.RFC3339Nano, value); err != nil {
			return errors.New("eventing: time must be RFC 3339")
		}
	}
	if key == "source" || key == "dataschema" {
		parsed, err := url.Parse(value)
		if err != nil || strings.ContainsAny(value, " \t\r\n\\") || (key == "dataschema" && !parsed.IsAbs()) {
			return fmt.Errorf("eventing: invalid %s URI", key)
		}
	}
	return nil
}

func attributeName(key string) bool {
	if len(key) == 0 || len(key) > 64 {
		return false
	}
	for _, r := range key {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') {
			return false
		}
	}
	return true
}

func attributeScalar(value any) error {
	switch v := value.(type) {
	case string:
		if len(v) > 1024 {
			return errors.New("attribute exceeds size limit")
		}
		for _, r := range v {
			if r < 0x20 || (r >= 0x7f && r <= 0x9f) || r == utf8.RuneError || (r >= 0xfdd0 && r <= 0xfdef) || r&0xffff >= 0xfffe {
				return errors.New("invalid attribute character")
			}
		}
	case bool:
	case json.Number:
		n, err := v.Int64()
		if err != nil || n < -2147483648 || n > 2147483647 {
			return errors.New("extension integer is outside int32 range")
		}
	default:
		return errors.New("extension must be a string, boolean or int32")
	}
	return nil
}

func decodeValue(decoder *json.Decoder, depth int) (any, error) {
	if depth > 32 {
		return nil, errors.New("JSON nesting exceeds 32")
	}
	token, err := decoder.Token()
	if err != nil {
		return nil, err
	}
	switch token {
	case json.Delim('{'):
		return decodeObject(decoder, depth)
	case json.Delim('['):
		values := make([]any, 0)
		for decoder.More() {
			value, err := decodeValue(decoder, depth+1)
			if err != nil {
				return nil, err
			}
			values = append(values, value)
		}
		_, err = decoder.Token()
		return values, err
	default:
		return token, nil
	}
}

func decodeObject(decoder *json.Decoder, depth int) (map[string]any, error) {
	values := make(map[string]any)
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return nil, err
		}
		key, ok := token.(string)
		if !ok {
			return nil, errors.New("object key must be a string")
		}
		if _, exists := values[key]; exists {
			return nil, fmt.Errorf("duplicate key %q", key)
		}
		values[key], err = decodeValue(decoder, depth+1)
		if err != nil {
			return nil, err
		}
	}
	_, err := decoder.Token()
	return values, err
}
