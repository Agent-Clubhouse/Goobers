package main

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/stateclient"
)

type keyedStateTestRecord struct {
	Count int `json:"count"`
}

var keyedStateTestSpec = keyedStateRecordSpec[keyedStateTestRecord]{
	schema:      "test/v1",
	operation:   "test.update",
	errorPrefix: "decode test state",
	field:       keyedStateRecordFieldRecord,
	stateKey:    func(key string) string { return "test/" + key },
}

func TestDecodeKeyedStateRecord(t *testing.T) {
	tests := []struct {
		name       string
		value      stateclient.Value
		key        string
		want       keyedStateTestRecord
		wantExists bool
		wantErr    string
	}{
		{name: "absent value", key: "item"},
		{
			name:    "malformed JSON",
			value:   stateclient.Value{Data: []byte("{"), ETag: "etag"},
			key:     "item",
			wantErr: "decode test state: unexpected end of JSON input",
		},
		{
			name:    "bad schema",
			value:   stateclient.Value{Data: []byte(`{"schema":"test/v0","key":"item","record":{"count":1}}`), ETag: "etag"},
			key:     "item",
			wantErr: `decode test state: unsupported schema "test/v0", want "test/v1"`,
		},
		{
			name:    "wrong embedded key",
			value:   stateclient.Value{Data: []byte(`{"schema":"test/v1","key":"other","record":{"count":1}}`), ETag: "etag"},
			key:     "item",
			wantErr: `decode test state: record is keyed to "other", not "item"`,
		},
		{
			name:       "valid value",
			value:      stateclient.Value{Data: []byte(`{"schema":"test/v1","key":"item","record":{"count":2}}`), ETag: "etag"},
			key:        "item",
			want:       keyedStateTestRecord{Count: 2},
			wantExists: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, exists, err := decodeKeyedStateRecord(tt.value, tt.key, keyedStateTestSpec)
			if tt.wantErr != "" {
				if err == nil || err.Error() != tt.wantErr {
					t.Fatalf("error = %v, want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil || got != tt.want || exists != tt.wantExists {
				t.Fatalf("decode = %+v, %v, %v; want %+v, %v, nil", got, exists, err, tt.want, tt.wantExists)
			}
		})
	}
}

type keyedStateTestStore struct {
	values    []stateclient.Value
	getValue  stateclient.Value
	key       string
	operation string
	writes    [][]byte
}

func (s *keyedStateTestStore) Get(_ context.Context, key string) (stateclient.Value, error) {
	s.key = key
	return s.getValue, nil
}

func (s *keyedStateTestStore) Put(context.Context, string, []byte, string) (stateclient.Value, error) {
	return stateclient.Value{}, errors.New("unexpected Put")
}

func (s *keyedStateTestStore) Update(
	_ context.Context,
	key, operation string,
	fn func(stateclient.Value) ([]byte, bool, error),
) error {
	s.key, s.operation = key, operation
	for _, value := range s.values {
		data, write, err := fn(value)
		if err != nil {
			return err
		}
		if !write {
			return nil
		}
		s.writes = append(s.writes, data)
	}
	return nil
}

func (s *keyedStateTestStore) Section(context.Context, string, string, func() error) error {
	return errors.New("unexpected Section")
}

func TestReadJSONState(t *testing.T) {
	store := &keyedStateTestStore{
		getValue: stateclient.Value{Data: []byte(`{"count":3}`), ETag: "etag"},
	}
	got, err := readJSONState(t.Context(), store, "test/cursor",
		func(value stateclient.Value) (keyedStateTestRecord, error) {
			var record keyedStateTestRecord
			err := json.Unmarshal(value.Data, &record)
			return record, err
		})
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got.Count != 3 || store.key != "test/cursor" {
		t.Fatalf("read = %+v from %q, want count 3 from test/cursor", got, store.key)
	}
}

func TestUpdateJSONState(t *testing.T) {
	store := &keyedStateTestStore{values: []stateclient.Value{
		{Data: []byte(`{"count":1}`), ETag: "old"},
		{Data: []byte(`{"count":4}`), ETag: "winner"},
	}}
	callbacks := 0
	err := updateJSONState(
		t.Context(),
		store,
		"test/cursor",
		"test.update",
		func(value stateclient.Value) (keyedStateTestRecord, error) {
			var record keyedStateTestRecord
			err := json.Unmarshal(value.Data, &record)
			return record, err
		},
		func(record keyedStateTestRecord) ([]byte, error) {
			return json.Marshal(record)
		},
		func(current keyedStateTestRecord) (keyedStateTestRecord, bool, error) {
			callbacks++
			current.Count++
			return current, true, nil
		},
	)
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if callbacks != 2 || len(store.writes) != 2 || string(store.writes[1]) != `{"count":5}` {
		t.Fatalf("callbacks/writes = %d/%q, want 2/[... count 5]", callbacks, store.writes)
	}

	store = &keyedStateTestStore{values: []stateclient.Value{{Data: []byte(`{"count":2}`)}}}
	err = updateJSONState(
		t.Context(),
		store,
		"test/cursor",
		"test.update",
		func(value stateclient.Value) (keyedStateTestRecord, error) {
			var record keyedStateTestRecord
			err := json.Unmarshal(value.Data, &record)
			return record, err
		},
		func(record keyedStateTestRecord) ([]byte, error) {
			return json.Marshal(record)
		},
		func(current keyedStateTestRecord) (keyedStateTestRecord, bool, error) {
			return current, false, nil
		},
	)
	if err != nil {
		t.Fatalf("no-write update: %v", err)
	}
	if len(store.writes) != 0 {
		t.Fatalf("no-write update wrote %q", store.writes)
	}
}

func TestUpdateKeyedStateRecord(t *testing.T) {
	tests := []struct {
		name          string
		values        []stateclient.Value
		write         bool
		wantCallbacks int
		wantWrites    int
		wantFinal     string
	}{
		{
			name:          "no-write callback",
			values:        []stateclient.Value{{}},
			wantCallbacks: 1,
		},
		{
			name:          "successful write",
			values:        []stateclient.Value{{}},
			write:         true,
			wantCallbacks: 1,
			wantWrites:    1,
			wantFinal:     `{"schema":"test/v1","key":"item","record":{"count":1}}`,
		},
		{
			name: "CAS callback re-execution",
			values: []stateclient.Value{
				{Data: []byte(`{"schema":"test/v1","key":"item","record":{"count":1}}`), ETag: "old"},
				{Data: []byte(`{"schema":"test/v1","key":"item","record":{"count":4}}`), ETag: "winner"},
			},
			write:         true,
			wantCallbacks: 2,
			wantWrites:    2,
			wantFinal:     `{"schema":"test/v1","key":"item","record":{"count":5}}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := &keyedStateTestStore{values: tt.values}
			callbacks := 0
			err := updateKeyedStateRecord(context.Background(), store, "item", keyedStateTestSpec,
				func(current keyedStateTestRecord, _ bool) (keyedStateTestRecord, bool, error) {
					callbacks++
					current.Count++
					return current, tt.write, nil
				})
			if err != nil {
				t.Fatalf("update: %v", err)
			}
			if callbacks != tt.wantCallbacks || len(store.writes) != tt.wantWrites {
				t.Fatalf("callbacks = %d, writes = %d; want %d, %d", callbacks, len(store.writes), tt.wantCallbacks, tt.wantWrites)
			}
			if store.key != "test/item" || store.operation != "test.update" {
				t.Fatalf("Update key/operation = %q/%q, want test/item/test.update", store.key, store.operation)
			}
			if tt.wantFinal != "" && (len(store.writes) == 0 || string(store.writes[len(store.writes)-1]) != tt.wantFinal) {
				t.Fatalf("final write = %q, want %q", store.writes[len(store.writes)-1], tt.wantFinal)
			}
		})
	}
}

func TestKeyedStateRecordVerdictField(t *testing.T) {
	spec := keyedStateTestSpec
	spec.field = keyedStateRecordFieldVerdict
	data, err := encodeKeyedStateRecord("item", keyedStateTestRecord{Count: 3}, spec)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if got := string(data); got != `{"schema":"test/v1","key":"item","verdict":{"count":3}}` {
		t.Fatalf("encoded verdict field = %s", got)
	}
	if strings.Contains(string(data), `"record"`) {
		t.Fatalf("encoded verdict contains record field: %s", data)
	}
}
