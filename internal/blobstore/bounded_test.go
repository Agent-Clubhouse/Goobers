package blobstore

import (
	"context"
	"errors"
	"os"
	"testing"
)

func TestDirBoundedRead(t *testing.T) {
	store, err := NewDir(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	data := []byte("evidence")
	digest := digestOf(data)
	if err := store.Put(context.Background(), digest, data); err != nil {
		t.Fatal(err)
	}
	if got, err := store.GetBounded(context.Background(), digest, 100); err != nil || string(got) != string(data) {
		t.Fatalf("bounded read: %q, %v", got, err)
	}
	for _, limit := range []int64{-1, 0, 2, (64 << 20) + 1} {
		if got, err := store.GetBounded(context.Background(), digest, limit); got != nil || !errors.Is(err, ErrTooLarge) {
			t.Fatalf("limit %d: %q, %v", limit, got, err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := store.GetBounded(ctx, digest, 100); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel: %v", err)
	}
	path, err := store.pathFor(digest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("tampered"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := store.GetBounded(context.Background(), digest, 100); got != nil || !errors.Is(err, ErrNotFound) {
		t.Fatalf("tamper: %q, %v", got, err)
	}
}
