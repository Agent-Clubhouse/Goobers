package recovery

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"unicode/utf8"
)

// MaxRecordBytes bounds metadata only, independently of the retained patch.
const MaxRecordBytes = 16 << 10

// Encode validates before publishing a recovery record.
func Encode(record Record) ([]byte, error) {
	if err := record.Validate(); err != nil {
		return nil, err
	}
	data, err := json.Marshal(record)
	if err != nil {
		return nil, fmt.Errorf("encode recovery record: %w", err)
	}
	if len(data) > MaxRecordBytes {
		return nil, fmt.Errorf("recovery record exceeds byte limit")
	}
	return data, nil
}

// Decode rejects oversized, ambiguous, unknown-version and malformed metadata.
// No partial record is returned on failure. It does not read referenced objects.
func Decode(reader io.Reader) (Record, error) {
	data, err := io.ReadAll(io.LimitReader(reader, MaxRecordBytes+1))
	if err != nil {
		return Record{}, fmt.Errorf("read recovery record: %w", err)
	}
	if len(data) > MaxRecordBytes {
		return Record{}, fmt.Errorf("recovery record exceeds byte limit")
	}
	if !utf8.Valid(data) {
		return Record{}, fmt.Errorf("recovery record is not valid UTF-8")
	}
	if err := uniqueRecordFields(data); err != nil {
		return Record{}, err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var record Record
	if err := decoder.Decode(&record); err != nil {
		return Record{}, fmt.Errorf("decode recovery record: %w", err)
	}
	if err := record.Validate(); err != nil {
		return Record{}, err
	}
	return record, nil
}

func uniqueRecordFields(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	token, err := decoder.Token()
	if err != nil {
		return fmt.Errorf("read recovery object: %w", err)
	}
	if token != json.Delim('{') {
		return fmt.Errorf("recovery record must be an object")
	}
	seen := make(map[string]bool)
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return fmt.Errorf("read recovery field: %w", err)
		}
		key, ok := token.(string)
		if !ok || seen[key] {
			return fmt.Errorf("duplicate or invalid recovery field")
		}
		seen[key] = true
		if !knownRecordField(key) {
			return fmt.Errorf("unknown recovery field")
		}
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return fmt.Errorf("read recovery value: %w", err)
		}
	}
	if _, err := decoder.Token(); err != nil {
		return fmt.Errorf("close recovery object: %w", err)
	}
	if _, err := decoder.Token(); err != io.EOF {
		return fmt.Errorf("trailing recovery record data")
	}
	return nil
}

func knownRecordField(key string) bool {
	switch key {
	case "version", "runId", "repositoryKey", "ref", "baseRef", "baseSha", "snapshotSha", "patchDigest", "archiveDigest", "archiveBytes", "archiveFormat", "createdAt", "retainUntil":
		return true
	default:
		return false
	}
}
