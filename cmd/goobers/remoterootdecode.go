package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"

	"github.com/goobers/goobers/internal/readservice"
)

func decodeRemoteRoot(body []byte) (string, *readservice.RootIdentity, error) {
	fields, err := remoteRootObject(body)
	if err != nil {
		return "", nil, err
	}
	for key := range fields {
		if (strings.EqualFold(key, "instanceRoot") && key != "instanceRoot") || (strings.EqualFold(key, "rootIdentity") && key != "rootIdentity") {
			return "", nil, fmt.Errorf("ambiguous daemon root identity field")
		}
	}
	var root string
	if err := json.Unmarshal(fields["instanceRoot"], &root); err != nil {
		return "", nil, fmt.Errorf("daemon has no usable durable root identity")
	}
	identityFields, err := remoteRootObject(fields["rootIdentity"])
	if err != nil {
		return "", nil, fmt.Errorf("daemon has no usable durable root identity")
	}
	for key := range identityFields {
		switch key {
		case "id", "identityProblem", "decommissionedAt", "decommissionReason", "lifecycleProblem":
		default:
			return "", nil, fmt.Errorf("unrecognized daemon root identity field")
		}
	}
	var identity readservice.RootIdentity
	if err := json.Unmarshal(fields["rootIdentity"], &identity); err != nil {
		return "", nil, fmt.Errorf("invalid daemon root identity response")
	}
	return root, &identity, nil
}

// Decode only exact object keys. Go's default struct decoder accepts duplicate
// and case-insensitive fields, which can conceal conflicting identity evidence.
func remoteRootObject(body []byte) (map[string]json.RawMessage, error) {
	if !utf8.Valid(body) {
		return nil, fmt.Errorf("invalid UTF-8 in daemon root identity response")
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return nil, fmt.Errorf("daemon root identity response must be an object")
	}
	fields := make(map[string]json.RawMessage)
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return nil, fmt.Errorf("invalid daemon root identity field")
		}
		key, ok := token.(string)
		if !ok || fields[key] != nil {
			return nil, fmt.Errorf("duplicate daemon root identity field")
		}
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return nil, fmt.Errorf("invalid daemon root identity value")
		}
		fields[key] = value
	}
	if _, err := decoder.Token(); err != nil {
		return nil, fmt.Errorf("invalid daemon root identity object")
	}
	if _, err := decoder.Token(); err != io.EOF {
		return nil, fmt.Errorf("trailing daemon root identity data")
	}
	return fields, nil
}
