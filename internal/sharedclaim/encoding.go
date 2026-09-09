package sharedclaim

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"unicode"
	"unicode/utf8"
)

const coordinationProtocol = "goobers/shared-claim/v1"

// MaxRecordBytes bounds a shared coordination record before JSON decoding.
const MaxRecordBytes = 4096

type coordinationRecord struct {
	Protocol string `json:"protocol"`
	Key      string `json:"key"`
	Lease    Record `json:"lease"`
}

// Encode binds a lease to the canonical repository/item key and the shared
// protocol. Human-readable local markers must never use this encoding.
func Encode(key string, record Record) ([]byte, error) {
	if !validKey(key) {
		return nil, fmt.Errorf("invalid shared coordination key")
	}
	if err := validateRecord(record); err != nil {
		return nil, err
	}
	data, err := json.Marshal(coordinationRecord{Protocol: coordinationProtocol, Key: key, Lease: record})
	if err != nil {
		return nil, err
	}
	if len(data) > MaxRecordBytes {
		return nil, fmt.Errorf("shared coordination record exceeds bound")
	}
	return data, nil
}

// Decode refuses local markers, another item's record, unknown protocol
// versions or fields, trailing documents, and oversized remote data.
func Decode(key string, data []byte) (Record, error) {
	if !validKey(key) || len(data) > MaxRecordBytes || !utf8.Valid(data) {
		return Record{}, fmt.Errorf("invalid shared coordination data")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var envelope coordinationRecord
	if err := decoder.Decode(&envelope); err != nil {
		return Record{}, err
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return Record{}, fmt.Errorf("shared coordination data has trailing content")
	}
	if envelope.Protocol != coordinationProtocol || envelope.Key != key {
		return Record{}, fmt.Errorf("shared coordination identity mismatch")
	}
	if err := validateRecord(envelope.Lease); err != nil {
		return Record{}, err
	}
	// Only this protocol's canonical encoding is admitted. Besides binding
	// serialization, this rejects duplicate keys that encoding/json otherwise
	// silently resolves using the last value.
	canonical, err := Encode(key, envelope.Lease)
	if err != nil || !bytes.Equal(bytes.TrimSpace(data), canonical) {
		return Record{}, fmt.Errorf("noncanonical shared coordination record")
	}
	return envelope.Lease, nil
}

func validKey(key string) bool {
	return validIdentityText(key, 1024)
}

// JSON replaces invalid UTF-8 instead of reporting an encoding error. An
// ownership identity must survive serialization exactly, never be repaired.
func validIdentityText(value string, limit int) bool {
	return value != "" && len(value) <= limit && utf8.ValidString(value) && strings.IndexFunc(value, unicode.IsControl) < 0
}
