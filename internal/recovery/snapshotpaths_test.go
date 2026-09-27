package recovery

import (
	"bytes"
	"errors"
	"io"
	"testing"
)

func TestSnapshotPathBudgetAcceptsMoreThanLegacyLimit(t *testing.T) {
	writer := &snapshotPathBudgetWriter{destination: io.Discard, remaining: maxSnapshotPathBytes}
	data := bytes.Repeat([]byte("x"), (8<<20)+1)

	if n, err := writer.Write(data); err != nil || n != len(data) {
		t.Fatalf("write above legacy limit = %d, %v; want %d, nil", n, err, len(data))
	}
}

func TestSnapshotPathBudgetFailsWithoutPartialWrite(t *testing.T) {
	writer := &snapshotPathBudgetWriter{destination: io.Discard, remaining: 1}
	data := []byte("xx")

	if n, err := writer.Write(data); n != 0 || !errors.Is(err, errSnapshotPathListTooLarge) {
		t.Fatalf("oversized write = %d, %v; want 0, %v", n, err, errSnapshotPathListTooLarge)
	}
}
