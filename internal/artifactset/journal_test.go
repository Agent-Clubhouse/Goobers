package artifactset

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

func TestJournalReader(t *testing.T) {
	root := t.TempDir()
	pointer, err := apiv1.WriteArtifact(root, "artifacts/evidence", []byte("evidence"), "text/plain")
	if err != nil {
		t.Fatal(err)
	}
	r, err := OpenJournal(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := r.Close(); err != nil {
			t.Error(err)
		}
	})
	data, err := r.ReadArtifact(context.Background(), pointer, 100)
	if err != nil || string(data) != "evidence" {
		t.Fatalf("read = %q, %v", data, err)
	}
	for _, limit := range []int64{0, -1, 2, MaxPayloadBytes + 1} {
		if _, err := r.ReadArtifact(context.Background(), pointer, limit); !errors.Is(err, ErrInvalid) {
			t.Fatalf("limit %d: %v", limit, err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := r.ReadArtifact(ctx, pointer, 100); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, pointer.Path), []byte("modified"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := r.ReadArtifact(context.Background(), pointer, 100); !errors.Is(err, ErrInvalid) {
		t.Fatalf("tampering: %v", err)
	}
	pointer.Path = "artifacts/missing"
	if _, err := r.ReadArtifact(context.Background(), pointer, 100); !errors.Is(err, ErrInvalid) {
		t.Fatalf("missing: %v", err)
	}
	pointer.Path = "../outside"
	if _, err := r.ReadArtifact(context.Background(), pointer, 100); !errors.Is(err, ErrInvalid) {
		t.Fatalf("escape: %v", err)
	}
	pointer.Path = "artifacts"
	if _, err := r.ReadArtifact(context.Background(), pointer, 100); !errors.Is(err, ErrInvalid) {
		t.Fatalf("directory: %v", err)
	}
}
