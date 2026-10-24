package journal

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestFileBackendLookupPreservesRecordedAndIdentity(t *testing.T) {
	root := t.TempDir()
	backend := NewFileBackend(FileBackendConfig{RunsDir: root})
	key := RunKey{Gaggle: "web", RunID: "file-identity"}
	dir := filepath.Join(root, key.RunID)
	assertRecorded := func() {
		t.Helper()
		got, err := backend.RunRecorded(context.Background(), key)
		if err != nil || got != Recorded(dir) {
			t.Fatalf("backend recorded=%t, %v; file recorded=%t", got, err, Recorded(dir))
		}
	}
	assertRecorded()
	if err := os.MkdirAll(dir, 0750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "residue"), []byte("unpublished"), 0600); err != nil {
		t.Fatal(err)
	}
	assertRecorded()
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	run, err := Create(root, RunIdentity{RunID: key.RunID, Gaggle: key.Gaggle, Workflow: "fixture", WorkflowVersion: 1, WorkflowDigest: "sha256:fixture"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := run.Close(); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(filepath.Join(dir, "run.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	assertRecorded()
	direct, err := OpenReadOnly(dir)
	if err != nil {
		t.Fatal(err)
	}
	throughBackend, err := backend.OpenRunReader(context.Background(), key)
	if err != nil {
		t.Fatal(err)
	}
	want, err := direct.Identity()
	if err != nil {
		t.Fatal(err)
	}
	got, err := throughBackend.Identity()
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("identity=%+v, %v; want %+v", got, err, want)
	}
	after, err := os.ReadFile(filepath.Join(dir, "run.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("backend read changed durable identity bytes")
	}
}
