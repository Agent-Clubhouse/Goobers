package main

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/goobers/goobers/internal/stateclient"
)

type keyedStateRecordField uint8

const (
	keyedStateRecordFieldRecord keyedStateRecordField = iota
	keyedStateRecordFieldVerdict
)

type keyedStateRecordSpec[T any] struct {
	schema      string
	operation   string
	errorPrefix string
	field       keyedStateRecordField
	stateKey    func(string) string
}

func decodeKeyedStateRecord[T any](
	value stateclient.Value,
	key string,
	spec keyedStateRecordSpec[T],
) (T, bool, error) {
	var zero T
	if !value.Exists() {
		return zero, false, nil
	}

	var schema, documentKey string
	var record T
	switch spec.field {
	case keyedStateRecordFieldRecord:
		var doc struct {
			Schema string `json:"schema"`
			Key    string `json:"key"`
			Record T      `json:"record"`
		}
		if err := json.Unmarshal(value.Data, &doc); err != nil {
			return zero, false, fmt.Errorf("%s: %w", spec.errorPrefix, err)
		}
		schema, documentKey, record = doc.Schema, doc.Key, doc.Record
	case keyedStateRecordFieldVerdict:
		var doc struct {
			Schema  string `json:"schema"`
			Key     string `json:"key"`
			Verdict T      `json:"verdict"`
		}
		if err := json.Unmarshal(value.Data, &doc); err != nil {
			return zero, false, fmt.Errorf("%s: %w", spec.errorPrefix, err)
		}
		schema, documentKey, record = doc.Schema, doc.Key, doc.Verdict
	default:
		return zero, false, fmt.Errorf("%s: unsupported record field", spec.errorPrefix)
	}
	if schema != spec.schema {
		return zero, false, fmt.Errorf(
			"%s: unsupported schema %q, want %q", spec.errorPrefix, schema, spec.schema)
	}
	if documentKey != key {
		return zero, false, fmt.Errorf(
			"%s: record is keyed to %q, not %q", spec.errorPrefix, documentKey, key)
	}
	return record, true, nil
}

func encodeKeyedStateRecord[T any](key string, record T, spec keyedStateRecordSpec[T]) ([]byte, error) {
	switch spec.field {
	case keyedStateRecordFieldRecord:
		return json.Marshal(struct {
			Schema string `json:"schema"`
			Key    string `json:"key"`
			Record T      `json:"record"`
		}{Schema: spec.schema, Key: key, Record: record})
	case keyedStateRecordFieldVerdict:
		return json.Marshal(struct {
			Schema  string `json:"schema"`
			Key     string `json:"key"`
			Verdict T      `json:"verdict"`
		}{Schema: spec.schema, Key: key, Verdict: record})
	default:
		return nil, fmt.Errorf("%s: unsupported record field", spec.errorPrefix)
	}
}

func updateKeyedStateRecord[T any](
	ctx context.Context,
	store stateclient.Store,
	key string,
	spec keyedStateRecordSpec[T],
	fn func(T, bool) (T, bool, error),
) error {
	return store.Update(ctx, spec.stateKey(key), spec.operation,
		func(value stateclient.Value) ([]byte, bool, error) {
			current, exists, err := decodeKeyedStateRecord(value, key, spec)
			if err != nil {
				return nil, false, err
			}
			next, write, err := fn(current, exists)
			if err != nil || !write {
				return nil, false, err
			}
			data, err := encodeKeyedStateRecord(key, next, spec)
			if err != nil {
				return nil, false, err
			}
			return data, true, nil
		})
}
