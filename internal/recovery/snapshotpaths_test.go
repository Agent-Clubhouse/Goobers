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
	var destination bytes.Buffer
	err := writeExistingSnapshotPaths(t.TempDir(), []byte("../escape\x00"), &destination)
	if err == nil || !strings.Contains(err.Error(), `"../escape"`) {
		t.Fatalf("invalid path error = %v; want offending path", err)
	}
	if destination.Len() != 0 {
		t.Fatalf("invalid path wrote %q", destination.Bytes())
	}
}

func TestWriteExistingSnapshotPathsNamesUnterminatedPath(t *testing.T) {
	err := writeExistingSnapshotPaths(t.TempDir(), []byte("unterminated"), io.Discard)
	if err == nil || !strings.Contains(err.Error(), `"unterminated"`) {
		t.Fatalf("unterminated path error = %v; want offending path", err)
	}
}
