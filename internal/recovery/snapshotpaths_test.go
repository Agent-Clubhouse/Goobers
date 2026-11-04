package recovery

import (
	"bytes"
	"errors"
	"io"
	"strings"
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

func TestWriteExistingSnapshotPathsNamesInvalidPath(t *testing.T) {
	const path = "../outside"

	err := writeExistingSnapshotPaths(t.TempDir(), []byte(path+"\x00"), io.Discard)
	if err == nil || !strings.Contains(err.Error(), path) {
		t.Fatalf("invalid path error = %v; want offending path", err)
	}
}
