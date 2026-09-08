package harness

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestFileTranscriptCheckpointCopiesOnlyAcknowledgedDeltas(t *testing.T) {
	dir := t.TempDir()
	root, err := os.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = root.Close() })
	var got []TranscriptDelta
	failure := errors.New("storage failure")
	state := fileTranscriptCheckpoint{root: root, path: "native.jsonl", limit: 10,
		sink: func(d TranscriptDelta) error { got = append(got, d); return failure },
	}
	if err := state.capture("checkpoint"); err != nil || len(got) != 0 {
		t.Fatalf("missing initial log: %v", err)
	}
	f, err := os.OpenFile(filepath.Join(dir, state.path), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close() })
	if _, err := f.WriteString("hello"); err != nil {
		t.Fatal(err)
	}
	if err := state.capture("checkpoint"); !errors.Is(err, failure) || state.offset != 0 {
		t.Fatalf("unacknowledged cursor: %d %v", state.offset, err)
	}
	failure = nil
	if err := state.capture("checkpoint"); err != nil {
		t.Fatal(err)
	}
	if string(got[1].Data) != "hello" || got[1].Offset != 0 {
		t.Fatalf("lost retry bytes: %+v", got)
	}
	for range 20 {
		if _, err := f.WriteString("x"); err != nil {
			t.Fatal(err)
		}
		if err := state.capture("checkpoint"); err != nil {
			t.Fatal(err)
		}
	}
	bytes := 0
	for _, d := range got[1:] {
		bytes += len(d.Data)
	}
	if bytes != 10 || state.offset != 10 || got[len(got)-1].DroppedBytes != 15 {
		t.Fatalf("capture not bounded: bytes=%d state=%+v", bytes, state)
	}
	count := len(got)
	if err := state.capture("checkpoint"); err != nil || len(got) != count {
		t.Fatalf("unchanged capture wrote again: %v", err)
	}
	if err := state.capture("canceled"); err != nil || len(got) != count+1 || got[count].Reason != "canceled" {
		t.Fatalf("missing final reason: %v", err)
	}
}

func TestFileTranscriptCheckpointRejectsChangedLog(t *testing.T) {
	for _, mode := range []string{"replace", "truncate", "remove"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			root, err := os.Open(dir)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = root.Close() })
			path := filepath.Join(dir, "native.jsonl")
			if err := os.WriteFile(path, []byte("original"), 0o600); err != nil {
				t.Fatal(err)
			}
			calls := 0
			state := fileTranscriptCheckpoint{root: root, path: "native.jsonl", sink: func(TranscriptDelta) error { calls++; return nil }}
			if err := state.capture("checkpoint"); err != nil {
				t.Fatal(err)
			}
			switch mode {
			case "replace":
				if err := os.Rename(path, path+".old"); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte("different"), 0o600); err != nil {
					t.Fatal(err)
				}
			case "truncate":
				if err := os.Truncate(path, 1); err != nil {
					t.Fatal(err)
				}
			case "remove":
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			}
			if err := state.capture("checkpoint"); err == nil || calls != 1 {
				t.Fatalf("changed log accepted: calls=%d err=%v", calls, err)
			}
		})
	}
}

func TestFileTranscriptCheckpointRejectsUnsafePaths(t *testing.T) {
	dir := t.TempDir()
	root, err := os.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = root.Close() })
	if err := os.Mkdir(filepath.Join(dir, "directory"), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"../outside", dir, ".", "directory"} {
		state := fileTranscriptCheckpoint{root: root, path: path, sink: func(TranscriptDelta) error { t.Fatal("unsafe path reached sink"); return nil }}
		if err := state.capture("checkpoint"); err == nil {
			t.Fatalf("accepted unsafe path %q", path)
		}
	}
}
