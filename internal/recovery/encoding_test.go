package recovery

import (
	"errors"
	"io"
	"strings"
	"testing"
	"time"
)

func TestEncodingRejectsAmbiguousAndOversizedRecords(t *testing.T) {
	now := time.Date(2026, 9, 8, 0, 0, 0, 0, time.UTC)
	record := Record{Version: 1, RunID: "run-1", RepositoryKey: "github|||team|repo|", Ref: "refs/goobers/recovery/run-1", BaseSHA: strings.Repeat("a", 40), SnapshotSHA: strings.Repeat("b", 40), PatchDigest: "sha256:" + strings.Repeat("c", 64), CreatedAt: now, RetainUntil: now.Add(time.Hour)}
	data, err := Encode(record)
	if err != nil {
		t.Fatal(err)
	}
	got, err := Decode(strings.NewReader(string(data)))
	if err != nil || got != record {
		t.Fatalf("round trip: %+v %v", got, err)
	}
	for _, bad := range []string{
		"null", "[]", "{}", string(data) + "{}", strings.Repeat(" ", MaxRecordBytes+1),
		`{"version":1,` + string(data[1:]),
		`{"unexpected":true,` + string(data[1:]),
		`{"Version":1,` + string(data[1:]),
		strings.Replace(string(data), `"version":1`, `"version":2`, 1),
		strings.Replace(string(data), "team", string([]byte{0xff}), 1),
	} {
		got, err := Decode(strings.NewReader(bad))
		if err == nil || got != (Record{}) {
			t.Fatalf("invalid input returned a record: %+v %v", got, err)
		}
	}
}

type failedRecordReader struct{}

func (failedRecordReader) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }

func TestDecodePreservesReadFailure(t *testing.T) {
	got, err := Decode(failedRecordReader{})
	if !errors.Is(err, io.ErrUnexpectedEOF) || got != (Record{}) {
		t.Fatalf("read failure: %+v %v", got, err)
	}
}
